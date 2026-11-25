package viajes_test

import (
	"context"
	"testing"
	"time"

	operclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/operacion"
	platformerrors "github.com/AI27-Tech/pf-pkg-platform-go/errors"
	"github.com/AI27-Tech/pf-pkg-platform-go/outbox"
	"github.com/AI27-Tech/pf-svc-operacion/internal/dbtest"
	"github.com/AI27-Tech/pf-svc-operacion/internal/viajes"
	"github.com/AI27-Tech/pf-svc-operacion/migrations"
	"github.com/google/uuid"
)

func ptr[T any](v T) *T { return &v }

// transicionarInterno llama a Transicionar con audiencia Interno (tenant vacío:
// el aislamiento no aplica a interno). Envuelve el patrón de las transiciones de
// operador para no repetir los argumentos de audiencia en cada caso, tras E9-T7
// (DEC-111) que amplió la firma con audiencia + tenant. Las transiciones de
// cliente se llaman directo con sus argumentos (audiencia "Cliente" + tenant).
func transicionarInterno(svc *viajes.Servicio, ctx context.Context, id operclient.Id, in operclient.TransicionEntrada, rol string) (*operclient.Viaje, error) {
	return svc.Transicionar(ctx, id, in, rol, "Interno", "")
}

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
	// Igual con op_solicitudes (viajes/0008, ADR 39): lo crea solicitudes/0001.
	dbtest.Exec(t, ctx, admin,
		`DO $$ BEGIN IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname='op_solicitudes') THEN CREATE ROLE op_solicitudes LOGIN; END IF; END $$;`)

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
		"TRUNCATE viajes.viaje, viajes.destino, viajes.condicion_viaje, viajes.estadia, viajes.transicion_viaje, viajes.outbox")

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

	// --- Crear como Operación: el viaje nace en APROBADO (ADR 38) ---
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

	// --- viaje.mensajeria_habilitada también, en la misma tx (DEC-016 #1): la
	// conversación se habilita al crear el viaje, sin importar el estado inicial
	// (aquí APROBADO, creado por interno); mensajeria lo consume para crear la
	// conversación (E3-T1). ---
	if n := contarEventos(t, ctx, "viaje.mensajeria_habilitada", v.Id.String()); n != 1 {
		t.Errorf("viaje.mensajeria_habilitada en outbox: got %d, want 1", n)
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

	// --- Un viaje de Operación no nació de una solicitud: sin penalización
	// (DEC-148 #27). Obtener y Listar lo reflejan desde la columna. ---
	if v.CancelacionConPenalizacion == nil || *v.CancelacionConPenalizacion {
		t.Errorf("cancelacion_con_penalizacion al crear: got %v, want false", v.CancelacionConPenalizacion)
	}
	if det.Viaje.CancelacionConPenalizacion == nil || *det.Viaje.CancelacionConPenalizacion {
		t.Errorf("cancelacion_con_penalizacion al obtener: got %v, want false", det.Viaje.CancelacionConPenalizacion)
	}

	// --- Un viaje que nació de una solicitud pagada lleva la marca (la pone la
	// aprobación, E10-T8; aquí se simula con la columna) en Obtener y en Listar. ---
	admin, cerrar := dbtest.AdminConn(t, ctx)
	dbtest.Exec(t, ctx, admin, "UPDATE viajes.viaje SET nacio_de_solicitud = true WHERE id = '"+v.Id.String()+"'")
	cerrar()
	det, err = svc.Obtener(ctx, v.Id)
	if err != nil || det.Viaje.CancelacionConPenalizacion == nil || !*det.Viaje.CancelacionConPenalizacion {
		t.Errorf("cancelacion_con_penalizacion de un viaje de solicitud (Obtener): got %+v err %v, want true", det.Viaje.CancelacionConPenalizacion, err)
	}

	// --- El cliente ya no crea viajes (ADR 38): 403, y no deja nada escrito ---
	cli := uuid.New()
	if _, err := svc.Crear(ctx, operclient.CrearViajeEntrada{
		ClienteId: &cli, Origen: "O", Destinos: []operclient.Destino{{Nombre: "D"}},
	}, "cliente"); platformerrors.AsCategory(err) != platformerrors.CategoryProhibido {
		t.Errorf("Crear (cliente): got %v, want Prohibido", err)
	}

	// --- Listar filtra por estado: APROBADO contiene v y no v2 ---
	estadoAprobado := operclient.APROBADO
	pag, err := svc.Listar(ctx, operclient.ListarViajesParams{Estado: &estadoAprobado})
	if err != nil {
		t.Fatalf("Listar: %v", err)
	}
	var vistoV, penalizaEnLista bool
	var destinosDeV int
	for _, it := range pag.Items {
		if it.Id == v.Id {
			vistoV = true
			destinosDeV = len(it.Destinos)
			penalizaEnLista = it.CancelacionConPenalizacion != nil && *it.CancelacionConPenalizacion
		}
	}
	if !vistoV {
		t.Error("Listar(APROBADO) no incluyó el viaje aprobado")
	}
	if !penalizaEnLista {
		t.Error("Listar: cancelacion_con_penalizacion de un viaje de solicitud debió ser true")
	}
	if destinosDeV != 2 {
		t.Errorf("Listar: destinos del viaje aprobado: got %d, want 2", destinosDeV)
	}
}

func TestCrearValidaciones(t *testing.T) {
	ctx := context.Background()
	svc := prepara(t, ctx)
	cli := uuid.New()

	// Rol interno sin cliente_id → inválido (el operador debe elegir el cliente).
	if _, err := svc.Crear(ctx, operclient.CrearViajeEntrada{
		Origen: "O", Destinos: []operclient.Destino{{Nombre: "D"}},
	}, "jefe_operativo"); err == nil {
		t.Error("Crear interno sin cliente_id debió fallar")
	}

	// Sin destinos → inválido.
	if _, err := svc.Crear(ctx, operclient.CrearViajeEntrada{
		ClienteId: &cli, Origen: "O",
	}, "jefe_operativo"); err == nil {
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

func TestCompletarFechaHoraExigeInstanteFuturo(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 25, 6, 0, 0, 0, time.UTC)
	svc := prepara(t, ctx).ConReloj(func() time.Time { return now })
	zona, err := time.LoadLocation("America/Mexico_City")
	if err != nil {
		t.Fatal(err)
	}
	cliente := uuid.New()
	fechaOriginal := now.Add(time.Hour)
	v, err := svc.Crear(ctx, operclient.CrearViajeEntrada{
		ClienteId: &cliente, Origen: "Origen", Destinos: []operclient.Destino{{Nombre: "Destino"}},
		FechaHoraServicio: &fechaOriginal,
	}, "jefe_operativo")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		nombre string
		fecha  time.Time
	}{
		{"anterior", now.Add(-time.Second).In(zona)},
		{"mismo_instante_en_otra_zona", now.In(zona)},
		{"posterior_pero_igual_al_persistir", now.Add(time.Nanosecond).In(zona)},
	} {
		t.Run(tc.nombre, func(t *testing.T) {
			_, err := svc.CompletarFechaHora(ctx, v.Id, operclient.FechaHoraEntrada{FechaHoraServicio: tc.fecha})
			if err == nil || platformerrors.AsCategory(err) != platformerrors.CategoryValidacion {
				t.Fatalf("fecha %v: error %v; quiere validación 400", tc.fecha, err)
			}
			out, err := svc.Obtener(ctx, v.Id)
			if err != nil || out.Viaje.FechaHoraServicio == nil || !out.Viaje.FechaHoraServicio.Equal(fechaOriginal) {
				t.Fatalf("el rechazo debe conservar %v: viaje %+v, error %v", fechaOriginal, out, err)
			}
		})
	}
	futura := now.Add(time.Second).In(zona)
	out, err := svc.CompletarFechaHora(ctx, v.Id, operclient.FechaHoraEntrada{FechaHoraServicio: futura})
	if err != nil || out.FechaHoraServicio == nil || !out.FechaHoraServicio.Equal(futura) {
		t.Fatalf("fecha futura %v: viaje %+v, error %v", futura, out, err)
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
