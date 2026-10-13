package viajes_test

import (
	"context"
	"testing"
	"time"

	operclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/operacion"
	"github.com/AI27-Tech/pf-pkg-platform-go/outbox"
	"github.com/AI27-Tech/pf-svc-operacion/internal/dbtest"
	"github.com/AI27-Tech/pf-svc-operacion/internal/viajes"
	"github.com/AI27-Tech/pf-svc-operacion/migrations"
	"github.com/google/uuid"
)

func ptr[T any](v T) *T { return &v }

// prepara aplica las migraciones de viajes (idempotentes) y limpia los datos,
// luego devuelve el servicio sobre un pool del rol op_viajes con search_path a
// viajes (para que el Publisher de outbox resuelva la tabla sin calificar).
func prepara(t *testing.T, ctx context.Context) *viajes.Servicio {
	t.Helper()
	dbtest.Serializar(t, ctx) // no correr a la vez que el grants test (misma base)
	admin, cerrar := dbtest.AdminConn(t, ctx)
	defer cerrar()

	// op_asignacion lo crea asignacion/0001 en el despliegue real; aquí solo se
	// aplican las migraciones de viajes, así que se pre-crea el rol bare para que
	// 0002_grants (que le otorga el tramo `viajes` de la excepción) no falle.
	dbtest.Exec(t, ctx, admin,
		`DO $$ BEGIN IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname='op_asignacion') THEN CREATE ROLE op_asignacion LOGIN; END IF; END $$;`)

	archivos, err := migrations.ArchivosDe("viajes")
	if err != nil {
		t.Fatalf("listar migraciones viajes: %v", err)
	}
	for _, f := range archivos {
		sql, err := migrations.FS.ReadFile(f)
		if err != nil {
			t.Fatalf("leer %s: %v", f, err)
		}
		dbtest.Exec(t, ctx, admin, string(sql))
	}
	dbtest.Exec(t, ctx, admin,
		"TRUNCATE viajes.viaje, viajes.destino, viajes.condicion_viaje, viajes.transicion_viaje, viajes.outbox")

	pool, cerrarPool := dbtest.PoolAs(t, ctx, "op_viajes", "viajes")
	t.Cleanup(cerrarPool)
	return viajes.NewServicio(pool, outbox.NewPublisher())
}

// contarEventos cuenta las filas de outbox de un tipo y agregado dado.
func contarEventos(t *testing.T, ctx context.Context, tipo, agregadoID string) int {
	t.Helper()
	admin, cerrar := dbtest.AdminConn(t, ctx)
	defer cerrar()
	var n int
	if err := admin.QueryRow(ctx,
		"SELECT count(*) FROM viajes.outbox WHERE tipo=$1 AND agregado_id=$2", tipo, agregadoID,
	).Scan(&n); err != nil {
		t.Fatalf("contar eventos: %v", err)
	}
	return n
}

func TestCrearObtenerListar(t *testing.T) {
	ctx := context.Background()
	svc := prepara(t, ctx)

	// --- Crear como rol interno: estado inicial APROBADO (DEC-026) ---
	clienteID := uuid.New()
	v, err := svc.Crear(ctx, operclient.CrearViajeEntrada{
		ClienteId: &clienteID,
		Origen:    "Bodega CDMX",
		Destinos: []operclient.Destino{
			{Nombre: "Guadalajara", NumeroEnvio: ptr("ENV-1")},
			{Nombre: "Tepic"},
		},
	}, "jefe_operativo")
	if err != nil {
		t.Fatalf("Crear (interno): %v", err)
	}
	if v.Estado != operclient.APROBADO {
		t.Errorf("estado inicial interno: got %q, want APROBADO", v.Estado)
	}
	if len(v.Destinos) != 2 {
		t.Errorf("destinos devueltos: got %d, want 2", len(v.Destinos))
	}

	// --- viaje.creado quedó en el outbox, en la misma tx ---
	if n := contarEventos(t, ctx, "viaje.creado", v.Id.String()); n != 1 {
		t.Errorf("viaje.creado en outbox: got %d, want 1", n)
	}

	// --- Obtener devuelve el detalle con destinos en orden y sin condiciones ---
	det, err := svc.Obtener(ctx, v.Id)
	if err != nil {
		t.Fatalf("Obtener: %v", err)
	}
	if det.Viaje.Estado != operclient.APROBADO {
		t.Errorf("estado obtenido: got %q, want APROBADO", det.Viaje.Estado)
	}
	if det.Viaje.Origen != "Bodega CDMX" {
		t.Errorf("origen: got %q", det.Viaje.Origen)
	}
	if det.Viaje.ClienteId == nil || *det.Viaje.ClienteId != clienteID {
		t.Errorf("cliente_id no round-trip: got %v, want %v", det.Viaje.ClienteId, clienteID)
	}
	if len(det.Viaje.Destinos) != 2 || det.Viaje.Destinos[0].Nombre != "Guadalajara" || det.Viaje.Destinos[1].Nombre != "Tepic" {
		t.Errorf("destinos: got %+v", det.Viaje.Destinos)
	}
	if len(det.CondicionesActivas) != 0 {
		t.Errorf("condiciones activas: got %d, want 0", len(det.CondicionesActivas))
	}

	// --- Crear como cliente: estado inicial SOLICITADO (DEC-026) ---
	cli := uuid.New()
	v2, err := svc.Crear(ctx, operclient.CrearViajeEntrada{
		ClienteId: &cli, Origen: "O", Destinos: []operclient.Destino{{Nombre: "D"}},
	}, "cliente")
	if err != nil {
		t.Fatalf("Crear (cliente): %v", err)
	}
	if v2.Estado != operclient.SOLICITADO {
		t.Errorf("estado inicial cliente: got %q, want SOLICITADO", v2.Estado)
	}

	// --- Listar filtra por estado: APROBADO contiene v y no v2 ---
	estadoAprobado := operclient.APROBADO
	pag, err := svc.Listar(ctx, operclient.ListarViajesParams{Estado: &estadoAprobado})
	if err != nil {
		t.Fatalf("Listar: %v", err)
	}
	var vistoV, vistoV2 bool
	var destinosDeV int
	for _, it := range pag.Items {
		if it.Id == v.Id {
			vistoV = true
			destinosDeV = len(it.Destinos)
		}
		if it.Id == v2.Id {
			vistoV2 = true
		}
	}
	if !vistoV {
		t.Error("Listar(APROBADO) no incluyó el viaje aprobado")
	}
	if vistoV2 {
		t.Error("Listar(APROBADO) incluyó un viaje SOLICITADO")
	}
	if destinosDeV != 2 {
		t.Errorf("Listar: destinos del viaje aprobado: got %d, want 2", destinosDeV)
	}
}

func TestCrearValidaciones(t *testing.T) {
	ctx := context.Background()
	svc := prepara(t, ctx)
	cli := uuid.New()

	// Sin cliente_id → inválido (flujo del operador interno exige cliente_id).
	if _, err := svc.Crear(ctx, operclient.CrearViajeEntrada{
		Origen: "O", Destinos: []operclient.Destino{{Nombre: "D"}},
	}, "cliente"); err == nil {
		t.Error("Crear sin cliente_id debió fallar")
	}

	// Sin destinos → inválido.
	if _, err := svc.Crear(ctx, operclient.CrearViajeEntrada{
		ClienteId: &cli, Origen: "O",
	}, "cliente"); err == nil {
		t.Error("Crear sin destinos debió fallar")
	}

	// Obtener un id inexistente → NoEncontrado (error no nil).
	if _, err := svc.Obtener(ctx, uuid.New()); err == nil {
		t.Error("Obtener de un viaje inexistente debió fallar")
	}
}

func TestFijarEta(t *testing.T) {
	ctx := context.Background()
	svc := prepara(t, ctx)
	cli := uuid.New()

	v, err := svc.Crear(ctx, operclient.CrearViajeEntrada{
		ClienteId: &cli, Origen: "O", Destinos: []operclient.Destino{{Nombre: "D"}},
	}, "jefe_operativo")
	if err != nil {
		t.Fatalf("Crear: %v", err)
	}

	// Fijar un ETA: se guarda y se devuelve, sin cambiar el estado.
	eta := time.Now().Add(6 * time.Hour).UTC().Truncate(time.Second)
	out, err := svc.FijarEta(ctx, v.Id, operclient.EtaEntrada{EtaEstimado: eta})
	if err != nil {
		t.Fatalf("FijarEta: %v", err)
	}
	if out.EtaEstimado == nil || !out.EtaEstimado.Equal(eta) {
		t.Errorf("eta devuelto: got %v, want %v", out.EtaEstimado, eta)
	}
	if out.Estado != operclient.APROBADO {
		t.Errorf("fijar eta no debe cambiar el estado: got %q, want APROBADO", out.Estado)
	}

	// Persistió: Obtener lo trae.
	det, err := svc.Obtener(ctx, v.Id)
	if err != nil {
		t.Fatalf("Obtener: %v", err)
	}
	if det.Viaje.EtaEstimado == nil || !det.Viaje.EtaEstimado.Equal(eta) {
		t.Errorf("eta persistido: got %v, want %v", det.Viaje.EtaEstimado, eta)
	}

	// Re-fijar lo reemplaza (captura manual, no acumulativa).
	eta2 := eta.Add(2 * time.Hour)
	if _, err := svc.FijarEta(ctx, v.Id, operclient.EtaEntrada{EtaEstimado: eta2}); err != nil {
		t.Fatalf("FijarEta (segunda): %v", err)
	}
	det2, _ := svc.Obtener(ctx, v.Id)
	if det2.Viaje.EtaEstimado == nil || !det2.Viaje.EtaEstimado.Equal(eta2) {
		t.Errorf("eta re-fijado: got %v, want %v", det2.Viaje.EtaEstimado, eta2)
	}
}

func TestFijarEtaRechazos(t *testing.T) {
	ctx := context.Background()
	svc := prepara(t, ctx)

	// Viaje inexistente → NoEncontrado.
	if _, err := svc.FijarEta(ctx, uuid.New(), operclient.EtaEntrada{EtaEstimado: time.Now().UTC()}); err == nil {
		t.Error("FijarEta sobre un viaje inexistente debió fallar")
	}

	// Viaje en estado terminal → conflicto (contrato: viaje_en_estado_terminal).
	cli := uuid.New()
	v, err := svc.Crear(ctx, operclient.CrearViajeEntrada{
		ClienteId: &cli, Origen: "O", Destinos: []operclient.Destino{{Nombre: "D"}},
	}, "jefe_operativo")
	if err != nil {
		t.Fatalf("Crear: %v", err)
	}
	admin, cerrar := dbtest.AdminConn(t, ctx)
	defer cerrar()
	if _, err := admin.Exec(ctx, "UPDATE viajes.viaje SET estado='CERRADO' WHERE id=$1", v.Id); err != nil {
		t.Fatalf("cerrar viaje: %v", err)
	}
	if _, err := svc.FijarEta(ctx, v.Id, operclient.EtaEntrada{EtaEstimado: time.Now().UTC()}); err == nil {
		t.Error("FijarEta sobre un viaje terminal debió fallar")
	}
}
