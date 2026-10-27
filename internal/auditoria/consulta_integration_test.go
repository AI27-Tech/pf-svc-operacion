package auditoria_test

import (
	"context"
	"testing"
	"time"

	operclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/operacion"
	"github.com/AI27-Tech/pf-svc-operacion/internal/auditoria"
	"github.com/AI27-Tech/pf-svc-operacion/internal/dbtest"
	"github.com/AI27-Tech/pf-svc-operacion/migrations"
	"github.com/google/uuid"
)

// prepara aplica todas las migraciones de operacion (estructura+roles, luego el
// resto por esquema) y devuelve el servicio de consulta sobre el pool op_auditoria
// (SELECT sobre registro_auditoria). Se salta sin la base de pruebas.
func prepara(t *testing.T, ctx context.Context) *auditoria.ServicioConsulta {
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
	for _, esquema := range migrations.Orden {
		archivos, err := migrations.ArchivosDe(esquema)
		if err != nil || len(archivos) == 0 {
			t.Fatalf("archivos de %s: %v", esquema, err)
		}
		aplica(archivos[0])
	}
	for _, esquema := range migrations.Orden {
		archivos, _ := migrations.ArchivosDe(esquema)
		for _, f := range archivos[1:] {
			aplica(f)
		}
	}
	dbtest.Exec(t, ctx, admin, "TRUNCATE auditoria.registro_auditoria")

	pool, cerrarP := dbtest.PoolAs(t, ctx, "op_auditoria", "auditoria")
	t.Cleanup(cerrarP)
	return auditoria.NewServicioConsulta(pool)
}

// sembrarRegistro inserta un registro de auditoría directamente (el alta real la
// hace el interceptor en la tx del cambio; aquí solo se necesitan filas que leer).
func sembrarRegistro(t *testing.T, ctx context.Context, viajeID operclient.Id, modulo, accion, actor string, detalle string) {
	t.Helper()
	admin, cerrar := dbtest.AdminConn(t, ctx)
	defer cerrar()
	dbtest.Exec(t, ctx, admin, "SET search_path TO auditoria")
	var det any
	if detalle != "" {
		det = detalle
	}
	_, err := admin.Exec(ctx,
		`INSERT INTO auditoria.registro_auditoria (viaje_id, modulo, accion, actor_id, detalle)
		 VALUES ($1, $2, $3, $4, $5)`,
		viajeID, modulo, accion, actor, det)
	if err != nil {
		t.Fatalf("sembrar registro: %v", err)
	}
}

// TestConsultar_OrdenYMapeo: la consulta devuelve los registros del viaje en
// orden cronológico, mapeando modulo→entidad, viaje_id→entidad_id, actor_id→actor,
// y extrayendo de/a del detalle cuando es una transición de estado.
func TestConsultar_OrdenYMapeo(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	svc := prepara(t, ctx)

	viaje := uuid.New()
	otro := uuid.New()
	sembrarRegistro(t, ctx, viaje, "viajes", "crear", "actor-1", "")
	sembrarRegistro(t, ctx, viaje, "viajes", "transicionar", "actor-2", `{"de":"APROBADO","a":"ASIGNADO"}`)
	sembrarRegistro(t, ctx, otro, "viajes", "crear", "actor-x", "") // otro viaje: no debe salir

	pag, err := svc.Consultar(ctx, viaje, operclient.ConsultarAuditoriaParams{})
	if err != nil {
		t.Fatalf("Consultar: %v", err)
	}
	if len(pag.Items) != 2 {
		t.Fatalf("se esperaban 2 registros del viaje, hubo %d", len(pag.Items))
	}
	// Orden cronológico: crear primero, transición después.
	if pag.Items[0].Entidad != "viajes" || pag.Items[0].Actor != "actor-1" {
		t.Fatalf("primer registro inesperado: %+v", pag.Items[0])
	}
	if pag.Items[0].EntidadId != viaje {
		t.Fatalf("entidad_id debía ser el viaje %s, fue %s", viaje, pag.Items[0].EntidadId)
	}
	// El segundo trae de/a del detalle.
	segundo := pag.Items[1]
	if segundo.De == nil || *segundo.De != "APROBADO" || segundo.A == nil || *segundo.A != "ASIGNADO" {
		t.Fatalf("de/a esperados APROBADO→ASIGNADO, fue de=%v a=%v", segundo.De, segundo.A)
	}
	// El primero NO trae de/a (detalle vacío).
	if pag.Items[0].De != nil || pag.Items[0].A != nil {
		t.Fatalf("el primer registro no debía traer de/a: de=%v a=%v", pag.Items[0].De, pag.Items[0].A)
	}
}

// TestConsultar_PaginaPorCursor: con más registros que el tamaño de página, la
// primera trae `paginaMax` y un cursor; la segunda el resto, sin repetir.
func TestConsultar_PaginaPorCursor(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	svc := prepara(t, ctx)

	viaje := uuid.New()
	const total = 51
	for i := 0; i < total; i++ {
		sembrarRegistro(t, ctx, viaje, "viajes", "accion", "actor", "")
	}

	pag1, err := svc.Consultar(ctx, viaje, operclient.ConsultarAuditoriaParams{})
	if err != nil {
		t.Fatalf("Consultar p1: %v", err)
	}
	if len(pag1.Items) != 50 {
		t.Fatalf("primera página: se esperaban 50, hubo %d", len(pag1.Items))
	}
	if pag1.CursorSiguiente == nil {
		t.Fatalf("primera página debía traer cursor")
	}

	cursor := operclient.Cursor(*pag1.CursorSiguiente)
	pag2, err := svc.Consultar(ctx, viaje, operclient.ConsultarAuditoriaParams{Cursor: &cursor})
	if err != nil {
		t.Fatalf("Consultar p2: %v", err)
	}
	if len(pag2.Items) != 1 {
		t.Fatalf("segunda página: se esperaba 1, hubo %d", len(pag2.Items))
	}
	if pag2.CursorSiguiente != nil {
		t.Fatalf("segunda página no debía traer cursor")
	}
}

// TestConsultar_Vacio: un viaje sin registros devuelve página vacía (Items no
// nil) y sin cursor.
func TestConsultar_Vacio(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	svc := prepara(t, ctx)

	pag, err := svc.Consultar(ctx, uuid.New(), operclient.ConsultarAuditoriaParams{})
	if err != nil {
		t.Fatalf("Consultar: %v", err)
	}
	if pag.Items == nil {
		t.Fatalf("Items debía ser lista vacía, no nil")
	}
	if len(pag.Items) != 0 || pag.CursorSiguiente != nil {
		t.Fatalf("se esperaba página vacía sin cursor, fue %d items / cursor=%v", len(pag.Items), pag.CursorSiguiente)
	}
}
