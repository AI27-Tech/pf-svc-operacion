package asignacion_test

import (
	"context"
	"testing"
	"time"

	operclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/operacion"
	"github.com/AI27-Tech/pf-pkg-platform-go/outbox"
	"github.com/AI27-Tech/pf-svc-operacion/internal/asignacion"
	"github.com/AI27-Tech/pf-svc-operacion/internal/dbtest"
	"github.com/AI27-Tech/pf-svc-operacion/migrations"
	"github.com/google/uuid"
)

// prepara aplica TODAS las migraciones de operacion en dos fases (todos los 0001
// de estructura/roles primero, luego el resto por esquema), de modo que
// op_asignacion exista con su excepción de GRANT sobre viajes/auditoria/roster.
// Devuelve el servicio sobre un pool de op_asignacion con search_path=viajes
// (para que el Publisher de outbox resuelva viajes.outbox sin calificar).
func prepara(t *testing.T, ctx context.Context) *asignacion.Servicio {
	t.Helper()
	dbtest.Serializar(t, ctx)
	admin, cerrar := dbtest.AdminConn(t, ctx)
	defer cerrar()

	aplica := func(f string) {
		sql, err := migrations.FS.ReadFile(f)
		if err != nil {
			t.Fatalf("leer %s: %v", f, err)
		}
		dbtest.Exec(t, ctx, admin, string(sql))
	}
	// Fase 1: 0001 de cada esquema (crea esquemas y roles).
	for _, esquema := range migrations.Orden {
		archivos, err := migrations.ArchivosDe(esquema)
		if err != nil || len(archivos) == 0 {
			t.Fatalf("archivos de %s: %v", esquema, err)
		}
		aplica(archivos[0])
	}
	// Fase 2: 0002.. de cada esquema (grants cruzados y tablas posteriores).
	for _, esquema := range migrations.Orden {
		archivos, _ := migrations.ArchivosDe(esquema)
		for _, f := range archivos[1:] {
			aplica(f)
		}
	}
	// Datos limpios en los esquemas que toca la prueba.
	dbtest.Exec(t, ctx, admin,
		"TRUNCATE viajes.viaje, viajes.transicion_viaje, viajes.outbox, asignacion.asignacion_viaje, asignacion.reserva_recurso, asignacion.historia_asignacion, auditoria.registro_auditoria")

	pool, cerrarPool := dbtest.PoolAs(t, ctx, "op_asignacion", "viajes")
	t.Cleanup(cerrarPool)
	return asignacion.NewServicio(pool, outbox.NewPublisher())
}

// sembrarViaje inserta un viaje directamente (la creación vive en el paquete
// viajes; aquí solo se necesita un viaje en el estado dado para asignarlo).
func sembrarViaje(t *testing.T, ctx context.Context, estado string, conFecha bool) operclient.Id {
	t.Helper()
	admin, cerrar := dbtest.AdminConn(t, ctx)
	defer cerrar()
	id := uuid.New()
	var fecha any
	if conFecha {
		fecha = time.Now().Add(24 * time.Hour).UTC()
	}
	dbtest.Exec(t, ctx, admin, "SET search_path TO viajes")
	if _, err := admin.Exec(ctx,
		`INSERT INTO viajes.viaje (id, estado, cliente_id, origen, fecha_hora_servicio, creado_en)
		 VALUES ($1, $2, $3, 'Origen', $4, now())`,
		id, estado, uuid.New(), fecha,
	); err != nil {
		t.Fatalf("sembrar viaje: %v", err)
	}
	return id
}

func contar(t *testing.T, ctx context.Context, sql string, args ...any) int {
	t.Helper()
	admin, cerrar := dbtest.AdminConn(t, ctx)
	defer cerrar()
	var n int
	if err := admin.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		t.Fatalf("contar: %v", err)
	}
	return n
}

func estadoDe(t *testing.T, ctx context.Context, id operclient.Id) string {
	t.Helper()
	admin, cerrar := dbtest.AdminConn(t, ctx)
	defer cerrar()
	var e string
	if err := admin.QueryRow(ctx, "SELECT estado FROM viajes.viaje WHERE id=$1", id).Scan(&e); err != nil {
		t.Fatalf("estado de viaje: %v", err)
	}
	return e
}

func TestAsignarEscolta(t *testing.T) {
	ctx := context.Background()
	svc := prepara(t, ctx)

	viajeID := sembrarViaje(t, ctx, "APROBADO", true)
	u1, u2 := uuid.New(), uuid.New()

	out, err := svc.Asignar(ctx, viajeID, operclient.AsignacionEntrada{UnidadesIds: []operclient.Id{u1, u2}}, "actor-1")
	if err != nil {
		t.Fatalf("Asignar: %v", err)
	}
	if len(out.Unidades) != 2 {
		t.Errorf("unidades en respuesta: got %d, want 2", len(out.Unidades))
	}
	if out.ViajeId != viajeID {
		t.Errorf("viaje_id en respuesta: got %v, want %v", out.ViajeId, viajeID)
	}

	// El viaje quedó ASIGNADO.
	if e := estadoDe(t, ctx, viajeID); e != "ASIGNADO" {
		t.Errorf("estado del viaje: got %q, want ASIGNADO", e)
	}
	// Dos reservas, un evento viaje.asignado, un registro de auditoría.
	if n := contar(t, ctx, "SELECT count(*) FROM asignacion.reserva_recurso r JOIN asignacion.asignacion_viaje a ON a.id=r.asignacion_id WHERE a.viaje_id=$1", viajeID); n != 2 {
		t.Errorf("reservas: got %d, want 2", n)
	}
	if n := contar(t, ctx, "SELECT count(*) FROM viajes.outbox WHERE tipo='viaje.asignado' AND agregado_id=$1", viajeID.String()); n != 1 {
		t.Errorf("viaje.asignado en outbox: got %d, want 1", n)
	}
	if n := contar(t, ctx, "SELECT count(*) FROM auditoria.registro_auditoria WHERE viaje_id=$1 AND modulo='asignacion'", viajeID.String()); n != 1 {
		t.Errorf("registro de auditoría: got %d, want 1", n)
	}
}

func TestAsignarUnidadYaReservadaEsConflicto(t *testing.T) {
	ctx := context.Background()
	svc := prepara(t, ctx)

	unidad := uuid.New() // misma unidad para ambos viajes (ventanas solapadas por misma fecha)

	v1 := sembrarViaje(t, ctx, "APROBADO", true)
	if _, err := svc.Asignar(ctx, v1, operclient.AsignacionEntrada{UnidadesIds: []operclient.Id{unidad}}, "a"); err != nil {
		t.Fatalf("primera asignación: %v", err)
	}

	v2 := sembrarViaje(t, ctx, "APROBADO", true)
	_, err := svc.Asignar(ctx, v2, operclient.AsignacionEntrada{UnidadesIds: []operclient.Id{unidad}}, "a")
	if err == nil {
		t.Fatal("asignar la misma unidad en ventana solapada debió fallar (409)")
	}
	// Atomicidad: el segundo viaje NO quedó ASIGNADO y no dejó rastros.
	if e := estadoDe(t, ctx, v2); e != "APROBADO" {
		t.Errorf("v2 debió seguir APROBADO tras el conflicto, está en %q", e)
	}
	if n := contar(t, ctx, "SELECT count(*) FROM asignacion.asignacion_viaje WHERE viaje_id=$1", v2); n != 0 {
		t.Errorf("v2 no debió dejar asignación tras el rollback, hay %d", n)
	}
	if n := contar(t, ctx, "SELECT count(*) FROM viajes.outbox WHERE tipo='viaje.asignado' AND agregado_id=$1", v2.String()); n != 0 {
		t.Errorf("v2 no debió emitir viaje.asignado tras el rollback, hay %d", n)
	}
}

func TestAsignarRequiereAprobado(t *testing.T) {
	ctx := context.Background()
	svc := prepara(t, ctx)

	vSolicitado := sembrarViaje(t, ctx, "SOLICITADO", true)
	if _, err := svc.Asignar(ctx, vSolicitado, operclient.AsignacionEntrada{UnidadesIds: []operclient.Id{uuid.New()}}, "a"); err == nil {
		t.Error("asignar un viaje no APROBADO debió fallar")
	}

	if _, err := svc.Asignar(ctx, uuid.New(), operclient.AsignacionEntrada{UnidadesIds: []operclient.Id{uuid.New()}}, "a"); err == nil {
		t.Error("asignar un viaje inexistente debió fallar")
	}

	// Sin unidades → inválido.
	vAprobado := sembrarViaje(t, ctx, "APROBADO", true)
	if _, err := svc.Asignar(ctx, vAprobado, operclient.AsignacionEntrada{UnidadesIds: nil}, "a"); err == nil {
		t.Error("asignar sin unidades debió fallar")
	}
}
