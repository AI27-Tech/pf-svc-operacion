package migrations_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/AI27-Tech/pf-svc-operacion/internal/dbtest"
	"github.com/AI27-Tech/pf-svc-operacion/migrations"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestGrantsPorTabla verifica el invariante I1 (ADR 17) al nivel que cierra
// E0-T5: el privilegio mínimo por tabla dentro de operacion. Aplica las
// migraciones REALES (0001 + 0002 de cada esquema, en el orden de
// migrations.Orden) sobre una base efímera, y luego comprueba, con INSERT/DELETE
// reales ejecutados como cada rol, qué permite y qué rechaza Postgres.
//
// Prueba comportamiento observable, no bits de catálogo: hace la escritura y
// afirma el resultado que vería el caso de uso. El caso central es la ÚNICA
// excepción de GRANT del sistema (op_asignacion), que debe poder escribir la
// transición, el outbox y la auditoría del viaje en su transacción, y NADA más.
//
// Se salta si OPERACION_TEST_DB no está definido (CI sin Postgres).
func TestGrantsPorTabla(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	dbtest.Serializar(t, ctx) // no correr a la vez que otras pruebas de BD

	admin, closeAdmin := dbtest.AdminConn(t, ctx)
	defer closeAdmin()

	// Teardown de roles antes y después: los roles son globales al cluster, los
	// esquemas los limpiamos con CASCADE. Idempotente ante corridas previas a medias.
	roles := []string{"op_viajes", "op_asignacion", "op_bitacora", "op_auditoria", "op_roster"}
	esquemas := []string{"viajes", "asignacion", "bitacora", "auditoria", "roster"}
	limpiar := func() {
		for _, s := range esquemas {
			dbtest.Exec(t, ctx, admin, fmt.Sprintf("DROP SCHEMA IF EXISTS %s CASCADE", s))
		}
		// REASSIGN/DROP OWNED no aplica: no creamos objetos fuera de estos
		// esquemas. Basta con DROP ROLE tras soltar los esquemas.
		for _, r := range roles {
			dbtest.Exec(t, ctx, admin, fmt.Sprintf("DROP ROLE IF EXISTS %s", r))
		}
	}
	limpiar()
	defer limpiar()

	// Aplicar las migraciones reales como admin, EN DOS FASES:
	//   fase 1: todos los 0001 (estructura: esquemas, roles, tablas),
	//   fase 2: todos los 0002 (grants por tabla).
	// Los grants de la excepción de op_asignacion (en viajes/auditoria/roster)
	// referencian un rol que crea asignacion/0001, el último del orden por
	// esquema. Separar por fase garantiza que todos los roles existan antes de
	// cualquier grant cruzado. Dentro de cada fase, migrations.Orden resuelve las
	// dependencias de esquema. En producción el aplicador debe hacer lo mismo.
	for _, version := range []string{"0001", "0002"} {
		for _, esquema := range migrations.Orden {
			archivos, err := migrations.ArchivosDe(esquema)
			if err != nil {
				t.Fatalf("listar migraciones de %s: %v", esquema, err)
			}
			for _, archivo := range archivos {
				if !strings.Contains(archivo, "/"+version) {
					continue
				}
				sql, err := migrations.FS.ReadFile(archivo)
				if err != nil {
					t.Fatalf("leer %s: %v", archivo, err)
				}
				if _, err := admin.Exec(ctx, string(sql)); err != nil {
					t.Fatalf("aplicar %s: %v", archivo, err)
				}
			}
		}
	}

	// --- op_asignacion: la excepción. Debe poder lo justo de su transacción. ---
	poolAsig, closeAsig := dbtest.PoolAs(t, ctx, "op_asignacion", "")
	defer closeAsig()

	// Puede INSERT en viajes.transicion_viaje (escribe la transición).
	mustExec(t, ctx, poolAsig,
		`INSERT INTO viajes.transicion_viaje (viaje_id, a_estado) VALUES (gen_random_uuid(), 'ASIGNADO')`,
		"op_asignacion debe poder insertar la transición del viaje")

	// Puede INSERT en viajes.outbox (publica viaje.asignado en la misma tx).
	mustExec(t, ctx, poolAsig,
		`INSERT INTO viajes.outbox (evento_id, tipo, agregado_id, ocurrido_en, payload)
		 VALUES (gen_random_uuid(), 'viaje.asignado', gen_random_uuid()::text, now(), '{}'::jsonb)`,
		"op_asignacion debe poder escribir el evento en el outbox único")

	// Puede INSERT en auditoria.registro_auditoria (interceptor en la misma tx).
	mustExec(t, ctx, poolAsig,
		`INSERT INTO auditoria.registro_auditoria (modulo, accion) VALUES ('asignacion', 'asignar')`,
		"op_asignacion debe poder escribir el registro de auditoría")

	// Puede SELECT en roster (lee la composición de la unidad antes de reservar).
	mustExec(t, ctx, poolAsig,
		`SELECT 1 FROM roster.unidad`,
		"op_asignacion debe poder leer el roster")

	// Puede administrar su propio esquema dentro de lo documentado: crea la
	// asignación vigente, la reserva por unidad y la historia, en su tx.
	asignacionID := "33333333-3333-3333-3333-333333333333"
	mustExec(t, ctx, poolAsig,
		fmt.Sprintf(`INSERT INTO asignacion.asignacion_viaje (id, viaje_id) VALUES ('%s', gen_random_uuid())`, asignacionID),
		"op_asignacion debe poder insertar la asignación vigente")
	mustExec(t, ctx, poolAsig,
		`UPDATE asignacion.asignacion_viaje SET liberada_en = now() WHERE id = '`+asignacionID+`'`,
		"op_asignacion debe poder liberar la asignación marcando liberada_en")
	mustExec(t, ctx, poolAsig,
		`INSERT INTO asignacion.historia_asignacion (viaje_id, accion) VALUES (gen_random_uuid(), 'asignar')`,
		"op_asignacion debe poder registrar la historia de asignación")

	reservaID := "44444444-4444-4444-4444-444444444444"
	mustExec(t, ctx, poolAsig,
		fmt.Sprintf(`INSERT INTO asignacion.reserva_recurso (id, asignacion_id, unidad_id, ventana)
		 VALUES ('%s', '%s', gen_random_uuid(), tstzrange(now(), now() + interval '1 hour'))`, reservaID, asignacionID),
		"op_asignacion debe poder reservar una unidad")

	// DEBE PASAR: liberar una reserva es un DELETE físico (decisión del Lead
	// E0-T5). Es el ÚNICO DELETE permitido en todo operacion.
	mustExec(t, ctx, poolAsig,
		`DELETE FROM asignacion.reserva_recurso WHERE id = '`+reservaID+`'`,
		"op_asignacion debe poder liberar (DELETE) una reserva de recurso")

	// NO puede borrar ni editar la asignación vigente: se libera por marca, no
	// con DELETE; la fila es historia de qué estuvo asignado.
	mustDenegar(t, ctx, poolAsig,
		`DELETE FROM asignacion.asignacion_viaje`,
		"permission denied for table asignacion_viaje",
		"op_asignacion NO debe poder borrar la asignación vigente (se libera por marca)")

	// NO puede borrar ni editar la historia de asignación: es permanente, append-only.
	mustDenegar(t, ctx, poolAsig,
		`DELETE FROM asignacion.historia_asignacion`,
		"permission denied for table historia_asignacion",
		"op_asignacion NO debe poder borrar la historia de asignación (append-only)")
	mustDenegar(t, ctx, poolAsig,
		`UPDATE asignacion.historia_asignacion SET motivo = 'x'`,
		"permission denied for table historia_asignacion",
		"op_asignacion NO debe poder editar la historia de asignación (append-only)")

	// NO puede borrar la auditoría: es probatoria, append-only.
	mustDenegar(t, ctx, poolAsig,
		`DELETE FROM auditoria.registro_auditoria`,
		"permission denied",
		"op_asignacion NO debe poder borrar la auditoría")

	// NO puede mutar el roster: su acceso es solo lectura.
	mustDenegar(t, ctx, poolAsig,
		`DELETE FROM roster.unidad`,
		"permission denied",
		"op_asignacion NO debe poder mutar el roster")

	// NO puede borrar en viajes: su excepción es INSERT (transición/outbox) y
	// UPDATE/SELECT del viaje, nunca DELETE.
	mustDenegar(t, ctx, poolAsig,
		`DELETE FROM viajes.transicion_viaje`,
		"permission denied for table transicion_viaje",
		"op_asignacion NO debe poder borrar transiciones del viaje (append-only)")
	mustDenegar(t, ctx, poolAsig,
		`DELETE FROM viajes.outbox`,
		"permission denied for table outbox",
		"op_asignacion NO debe poder borrar del outbox (append-only)")

	// NO toca bitacora: su transacción nunca escribe la bitácora. Aislamiento
	// de esquema (I1): ni siquiera entra al esquema.
	mustDenegar(t, ctx, poolAsig,
		`SELECT 1 FROM bitacora.entrada_bitacora`,
		"permission denied for schema bitacora",
		"op_asignacion NO debe alcanzar el esquema bitacora (I1)")

	// NO toca catalogos: el acceso cruzado a catalogos falla en la base; la
	// aptitud se consulta por el puerto, no por SQL (asignacion/0001, operacion.md).
	// El esquema catalogos no existe en esta base efímera; el error es "no
	// existe el esquema", que igualmente prueba que op_asignacion no lo alcanza.
	mustDenegar(t, ctx, poolAsig,
		`SELECT 1 FROM catalogos.cliente`,
		"catalogos",
		"op_asignacion NO debe alcanzar el esquema catalogos")

	// --- op_auditoria: probatorio, append-only. INSERT sí, DELETE no. ---
	poolAud, closeAud := dbtest.PoolAs(t, ctx, "op_auditoria", "")
	defer closeAud()
	mustExec(t, ctx, poolAud,
		`INSERT INTO auditoria.registro_auditoria (modulo, accion) VALUES ('viajes', 'transicionar')`,
		"op_auditoria debe poder insertar un registro")
	mustDenegar(t, ctx, poolAud,
		`DELETE FROM auditoria.registro_auditoria`,
		"permission denied",
		"op_auditoria NO debe poder borrar (append-only)")
	mustDenegar(t, ctx, poolAud,
		`UPDATE auditoria.registro_auditoria SET accion = 'x'`,
		"permission denied",
		"op_auditoria NO debe poder actualizar (append-only)")

	// --- op_viajes: dueño de viajes, pero aislado de los demás esquemas. ---
	poolViajes, closeViajes := dbtest.PoolAs(t, ctx, "op_viajes", "")
	defer closeViajes()
	viajeID := "55555555-5555-5555-5555-555555555555"
	mustExec(t, ctx, poolViajes,
		fmt.Sprintf(`INSERT INTO viajes.viaje (id, estado, cliente_id, origen) VALUES ('%s', 'SOLICITADO', gen_random_uuid(), 'MX')`, viajeID),
		"op_viajes debe poder insertar un viaje")
	mustExec(t, ctx, poolViajes,
		`UPDATE viajes.viaje SET estado = 'APROBADO' WHERE id = '`+viajeID+`'`,
		"op_viajes debe poder transicionar el estado del viaje")
	mustExec(t, ctx, poolViajes,
		`INSERT INTO viajes.transicion_viaje (viaje_id, a_estado) VALUES ('`+viajeID+`', 'APROBADO')`,
		"op_viajes debe poder registrar la transición")

	// NO borra: viaje y condicion_viaje se cierran por estado/marca; transicion y
	// outbox son append-only. Ningún DELETE en viajes.
	mustDenegar(t, ctx, poolViajes,
		`DELETE FROM viajes.viaje`,
		"permission denied for table viaje",
		"op_viajes NO debe poder borrar un viaje (se cierra por estado)")
	mustDenegar(t, ctx, poolViajes,
		`DELETE FROM viajes.transicion_viaje`,
		"permission denied for table transicion_viaje",
		"op_viajes NO debe poder borrar transiciones (append-only)")
	mustDenegar(t, ctx, poolViajes,
		`UPDATE viajes.transicion_viaje SET a_estado = 'x'`,
		"permission denied for table transicion_viaje",
		"op_viajes NO debe poder editar transiciones (append-only)")
	mustDenegar(t, ctx, poolViajes,
		`DELETE FROM viajes.condicion_viaje`,
		"permission denied for table condicion_viaje",
		"op_viajes NO debe poder borrar condiciones (se cierran por marca)")
	mustDenegar(t, ctx, poolViajes,
		`DELETE FROM viajes.outbox`,
		"permission denied for table outbox",
		"op_viajes NO debe poder borrar del outbox (append-only)")

	// NO alcanza el esquema de asignacion: aislamiento por schema (I1).
	mustDenegar(t, ctx, poolViajes,
		`SELECT 1 FROM asignacion.reserva_recurso`,
		"permission denied for schema",
		"op_viajes NO debe poder leer el esquema de asignacion")

	// --- op_bitacora: entrada append-only; incidencia se cierra por marca. ---
	poolBit, closeBit := dbtest.PoolAs(t, ctx, "op_bitacora", "")
	defer closeBit()
	mustExec(t, ctx, poolBit,
		`INSERT INTO bitacora.entrada_bitacora (viaje_id, texto) VALUES (gen_random_uuid(), 'nota')`,
		"op_bitacora debe poder insertar una entrada")
	incidenciaID := "66666666-6666-6666-6666-666666666666"
	mustExec(t, ctx, poolBit,
		fmt.Sprintf(`INSERT INTO bitacora.incidencia (id, viaje_id) VALUES ('%s', gen_random_uuid())`, incidenciaID),
		"op_bitacora debe poder abrir una incidencia")
	mustExec(t, ctx, poolBit,
		`UPDATE bitacora.incidencia SET resuelta_en = now() WHERE id = '`+incidenciaID+`'`,
		"op_bitacora debe poder cerrar una incidencia por marca")

	// NO borra ni edita entradas: append-only.
	mustDenegar(t, ctx, poolBit,
		`DELETE FROM bitacora.entrada_bitacora`,
		"permission denied for table entrada_bitacora",
		"op_bitacora NO debe poder borrar entradas (append-only)")
	mustDenegar(t, ctx, poolBit,
		`UPDATE bitacora.entrada_bitacora SET texto = 'x'`,
		"permission denied for table entrada_bitacora",
		"op_bitacora NO debe poder editar entradas (append-only)")
	// NO borra incidencias: se cierran por marca, no con DELETE.
	mustDenegar(t, ctx, poolBit,
		`DELETE FROM bitacora.incidencia`,
		"permission denied for table incidencia",
		"op_bitacora NO debe poder borrar incidencias (se cierran por marca)")

	// --- op_roster: dueño del roster; SÍ borra (baja de unidad). ---
	poolRoster, closeRoster := dbtest.PoolAs(t, ctx, "op_roster", "")
	defer closeRoster()
	unidadID := "77777777-7777-7777-7777-777777777777"
	mustExec(t, ctx, poolRoster,
		fmt.Sprintf(`INSERT INTO roster.unidad (id, tipo, vehiculo_id) VALUES ('%s', 'patrulla', 'veh-1')`, unidadID),
		"op_roster debe poder dar de alta una unidad")
	// El Jefe Operativo administra el roster: SÍ puede dar de baja (DELETE).
	mustExec(t, ctx, poolRoster,
		`DELETE FROM roster.unidad WHERE id = '`+unidadID+`'`,
		"op_roster debe poder dar de baja (DELETE) una unidad del roster")
}

func mustExec(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql, msg string) {
	t.Helper()
	if _, err := pool.Exec(ctx, sql); err != nil {
		t.Fatalf("%s: %v", msg, err)
	}
}

func mustDenegar(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql, fragmentoError, msg string) {
	t.Helper()
	_, err := pool.Exec(ctx, sql)
	if err == nil {
		t.Fatalf("%s: la operación tuvo éxito pero debía ser denegada", msg)
	}
	if !strings.Contains(err.Error(), fragmentoError) {
		t.Fatalf("%s: se esperaba un error con %q, se obtuvo: %v", msg, fragmentoError, err)
	}
}
