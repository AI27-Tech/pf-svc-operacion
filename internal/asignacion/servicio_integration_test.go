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
	// La respuesta afirma el estado tras la reserva (tracer.20): ASIGNADO.
	if out.Estado != operclient.ASIGNADO {
		t.Errorf("estado en respuesta: got %q, want ASIGNADO", out.Estado)
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

// asignaViaje siembra un viaje APROBADO y lo asigna con las unidades dadas,
// dejándolo ASIGNADO. Devuelve el id del viaje. Base para probar la reasignación.
func asignaViaje(t *testing.T, ctx context.Context, svc *asignacion.Servicio, unidades ...operclient.Id) operclient.Id {
	t.Helper()
	viajeID := sembrarViaje(t, ctx, "APROBADO", true)
	if _, err := svc.Asignar(ctx, viajeID, operclient.AsignacionEntrada{UnidadesIds: unidades}, "actor-1"); err != nil {
		t.Fatalf("asignación previa: %v", err)
	}
	return viajeID
}

func TestReasignarEscolta(t *testing.T) {
	ctx := context.Background()
	svc := prepara(t, ctx)

	u1, u2, u3 := uuid.New(), uuid.New(), uuid.New()
	viajeID := asignaViaje(t, ctx, svc, u1, u2)

	out, err := svc.Reasignar(ctx, viajeID, operclient.ReasignacionEntrada{
		UnidadesIds: []operclient.Id{u3}, Motivo: "u1 averiada",
	}, "actor-2")
	if err != nil {
		t.Fatalf("Reasignar: %v", err)
	}
	if len(out.Unidades) != 1 || out.Unidades[0].UnidadId != u3 {
		t.Errorf("escolta resultante: got %+v, want [u3]", out.Unidades)
	}
	// La respuesta afirma el estado actual, que la reasignación no cambia (tracer.20).
	if out.Estado != operclient.ASIGNADO {
		t.Errorf("estado en respuesta: got %q, want ASIGNADO", out.Estado)
	}

	// El estado del viaje NO cambia: sigue ASIGNADO.
	if e := estadoDe(t, ctx, viajeID); e != "ASIGNADO" {
		t.Errorf("reasignar no debe cambiar el estado: got %q, want ASIGNADO", e)
	}
	// Una sola asignación vigente (la nueva); la anterior quedó liberada.
	if n := contar(t, ctx, "SELECT count(*) FROM asignacion.asignacion_viaje WHERE viaje_id=$1 AND liberada_en IS NULL", viajeID); n != 1 {
		t.Errorf("asignaciones vigentes: got %d, want 1", n)
	}
	if n := contar(t, ctx, "SELECT count(*) FROM asignacion.asignacion_viaje WHERE viaje_id=$1 AND liberada_en IS NOT NULL", viajeID); n != 1 {
		t.Errorf("asignaciones liberadas: got %d, want 1", n)
	}
	// La reserva vigente es solo u3 (las de u1/u2 se borraron al liberar).
	if n := contar(t, ctx, "SELECT count(*) FROM asignacion.reserva_recurso r JOIN asignacion.asignacion_viaje a ON a.id=r.asignacion_id WHERE a.viaje_id=$1 AND a.liberada_en IS NULL", viajeID); n != 1 {
		t.Errorf("reservas vigentes: got %d, want 1", n)
	}
	// Un evento viaje.reasignado y un registro de auditoría de la reasignación.
	if n := contar(t, ctx, "SELECT count(*) FROM viajes.outbox WHERE tipo='viaje.reasignado' AND agregado_id=$1", viajeID.String()); n != 1 {
		t.Errorf("viaje.reasignado en outbox: got %d, want 1", n)
	}
	if n := contar(t, ctx, "SELECT count(*) FROM asignacion.historia_asignacion WHERE viaje_id=$1 AND accion='reasignar'", viajeID); n != 1 {
		t.Errorf("historia de reasignación: got %d, want 1", n)
	}
}

// Reusar una unidad de la escolta anterior en la reasignación debe funcionar:
// la reserva vieja se liberó, así que no choca contra la exclusión GiST.
func TestReasignarPuedeConservarUnidad(t *testing.T) {
	ctx := context.Background()
	svc := prepara(t, ctx)

	u1, u2 := uuid.New(), uuid.New()
	viajeID := asignaViaje(t, ctx, svc, u1)

	if _, err := svc.Reasignar(ctx, viajeID, operclient.ReasignacionEntrada{
		UnidadesIds: []operclient.Id{u1, u2}, Motivo: "sumar una unidad",
	}, "a"); err != nil {
		t.Fatalf("reasignar conservando u1 debió funcionar: %v", err)
	}
}

func TestReasignarSinAsignacionVigenteEsConflicto(t *testing.T) {
	ctx := context.Background()
	svc := prepara(t, ctx)

	// Viaje APROBADO pero nunca asignado: no hay nada que reasignar.
	vAprobado := sembrarViaje(t, ctx, "APROBADO", true)
	if _, err := svc.Reasignar(ctx, vAprobado, operclient.ReasignacionEntrada{
		UnidadesIds: []operclient.Id{uuid.New()}, Motivo: "x",
	}, "a"); err == nil {
		t.Error("reasignar un viaje sin asignación vigente debió fallar")
	}

	// Viaje inexistente.
	if _, err := svc.Reasignar(ctx, uuid.New(), operclient.ReasignacionEntrada{
		UnidadesIds: []operclient.Id{uuid.New()}, Motivo: "x",
	}, "a"); err == nil {
		t.Error("reasignar un viaje inexistente debió fallar")
	}

	// Sin motivo → inválido (aunque haya asignación vigente).
	viajeID := asignaViaje(t, ctx, svc, uuid.New())
	if _, err := svc.Reasignar(ctx, viajeID, operclient.ReasignacionEntrada{
		UnidadesIds: []operclient.Id{uuid.New()}, Motivo: "  ",
	}, "a"); err == nil {
		t.Error("reasignar sin motivo debió fallar")
	}
}

func TestReasignarViajeTerminalEsConflicto(t *testing.T) {
	ctx := context.Background()
	svc := prepara(t, ctx)

	// Un viaje que llegó a asignarse y luego cerró: no se reasigna.
	viajeID := asignaViaje(t, ctx, svc, uuid.New())
	admin, cerrar := dbtest.AdminConn(t, ctx)
	defer cerrar()
	if _, err := admin.Exec(ctx, "UPDATE viajes.viaje SET estado='CERRADO' WHERE id=$1", viajeID); err != nil {
		t.Fatalf("cerrar viaje: %v", err)
	}

	if _, err := svc.Reasignar(ctx, viajeID, operclient.ReasignacionEntrada{
		UnidadesIds: []operclient.Id{uuid.New()}, Motivo: "tarde",
	}, "a"); err == nil {
		t.Error("reasignar un viaje terminal debió fallar")
	}
}

// Si la reserva de la nueva escolta choca (unidad ocupada por otro viaje en la
// ventana), toda la reasignación revierte: la asignación ORIGINAL sigue vigente
// y sin liberar. Es el invariante atómico de la operación.
func TestReasignarConflictoRevierteAsignacionOriginal(t *testing.T) {
	ctx := context.Background()
	svc := prepara(t, ctx)

	u1 := uuid.New()
	viajeA := asignaViaje(t, ctx, svc, u1)

	// Otro viaje ocupa u2 en la misma ventana (misma fecha de servicio).
	u2 := uuid.New()
	_ = asignaViaje(t, ctx, svc, u2)

	// Reasignar A a u2 debe chocar (u2 ya reservada) y revertir todo.
	_, err := svc.Reasignar(ctx, viajeA, operclient.ReasignacionEntrada{
		UnidadesIds: []operclient.Id{u2}, Motivo: "intento fallido",
	}, "a")
	if err == nil {
		t.Fatal("reasignar a una unidad ocupada debió fallar (409)")
	}

	// La asignación original de A sigue vigente y con su reserva de u1.
	if n := contar(t, ctx, "SELECT count(*) FROM asignacion.asignacion_viaje WHERE viaje_id=$1 AND liberada_en IS NULL", viajeA); n != 1 {
		t.Errorf("la asignación original de A debió seguir vigente, vigentes=%d", n)
	}
	if n := contar(t, ctx, "SELECT count(*) FROM asignacion.asignacion_viaje WHERE viaje_id=$1", viajeA); n != 1 {
		t.Errorf("A no debió ganar una asignación nueva tras el rollback, total=%d", n)
	}
	if n := contar(t, ctx, "SELECT count(*) FROM asignacion.reserva_recurso r JOIN asignacion.asignacion_viaje a ON a.id=r.asignacion_id WHERE a.viaje_id=$1 AND a.liberada_en IS NULL AND r.unidad_id=$2", viajeA, u1); n != 1 {
		t.Errorf("la reserva original de u1 debió sobrevivir al rollback, got %d", n)
	}
	if n := contar(t, ctx, "SELECT count(*) FROM viajes.outbox WHERE tipo='viaje.reasignado' AND agregado_id=$1", viajeA.String()); n != 0 {
		t.Errorf("no debió emitirse viaje.reasignado tras el rollback, got %d", n)
	}
}
