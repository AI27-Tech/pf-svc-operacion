// Package dbtest ofrece utilidades para las pruebas de integración de operacion
// que requieren Postgres. Replica el patrón de pf-pkg-platform-go/internal/dbtest
// (no importable por ser internal de otro módulo) acotado a lo que operacion
// necesita: aplicar sus migraciones y abrir pools por rol para comprobar los
// GRANT de E0-T5.
//
// Todas las pruebas que lo usan se saltan (t.Skip) cuando OPERACION_TEST_DB no
// está definido, de modo que `go test ./...` sigue pasando en CI sin Postgres.
//
// OPERACION_TEST_DB debe ser un DSN con privilegios para CREATE ROLE y CREATE
// SCHEMA (superusuario o equivalente), porque las migraciones crean los roles
// op_* y los esquemas de operacion.
//
// Ejemplo:
//
//	OPERACION_TEST_DB="postgres:///postgres" go test ./...
package dbtest

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// EnvDSN es la variable de entorno que activa las pruebas de integración de BD.
const EnvDSN = "OPERACION_TEST_DB"

// DSN devuelve el DSN de administración, o salta la prueba si no está definido.
func DSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv(EnvDSN)
	if dsn == "" {
		t.Skipf("se salta la prueba de integración: %s no está definido", EnvDSN)
	}
	return dsn
}

// Serializar toma un lock de sesión de Postgres (pg_advisory_lock) para que las
// pruebas de integración de DISTINTOS paquetes, que comparten una sola base, no
// corran a la vez y se pisen el esquema (go test corre los paquetes en paralelo).
// El lock se libera al cerrar la conexión dedicada, en el Cleanup de la prueba.
func Serializar(t *testing.T, ctx context.Context) {
	t.Helper()
	conn, err := pgx.Connect(ctx, DSN(t))
	if err != nil {
		t.Fatalf("dbtest: conectar para serializar: %v", err)
	}
	// Clave arbitraria pero COMPARTIDA por todas las pruebas de integración de
	// operacion; cualquier otra que toque la misma base debe tomar esta misma.
	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock(727224)"); err != nil {
		_ = conn.Close(ctx)
		t.Fatalf("dbtest: tomar lock de serialización: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(ctx) }) // cerrar la conexión libera el lock
}

// AdminConn abre una conexión de administración para crear o eliminar roles y
// esquemas. El llamador la cierra con la función devuelta.
func AdminConn(t *testing.T, ctx context.Context) (*pgx.Conn, func()) {
	t.Helper()
	conn, err := pgx.Connect(ctx, DSN(t))
	if err != nil {
		t.Fatalf("dbtest: conectar como admin: %v", err)
	}
	return conn, func() { _ = conn.Close(ctx) }
}

// Exec ejecuta una sentencia con la conexión de administración y falla la prueba
// si hay error. Útil para setup y teardown.
func Exec(t *testing.T, ctx context.Context, conn *pgx.Conn, sql string) {
	t.Helper()
	if _, err := conn.Exec(ctx, sql); err != nil {
		t.Fatalf("dbtest: exec %q: %v", sql, err)
	}
}

// PoolAs abre un pool que asume el rol dado en cada conexión (SET ROLE), de modo
// que los privilegios efectivos sean los del rol y no los del admin que lo abrió.
// searchPath, si no está vacío, se fija por conexión como en producción (p. ej.
// "viajes" para que el Publisher de outbox resuelva la tabla sin calificar).
func PoolAs(t *testing.T, ctx context.Context, role, searchPath string) (*pgxpool.Pool, func()) {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(DSN(t))
	if err != nil {
		t.Fatalf("dbtest: parsear DSN: %v", err)
	}
	if searchPath != "" {
		cfg.ConnConfig.RuntimeParams["search_path"] = searchPath
	}
	cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		_, err := c.Exec(ctx, "SET ROLE "+quoteIdent(role))
		return err
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("dbtest: crear pool como %s: %v", role, err)
	}
	return pool, pool.Close
}

// quoteIdent escapa un identificador SQL con doble comilla.
func quoteIdent(ident string) string {
	out := make([]byte, 0, len(ident)+2)
	out = append(out, '"')
	for i := 0; i < len(ident); i++ {
		if ident[i] == '"' {
			out = append(out, '"')
		}
		out = append(out, ident[i])
	}
	out = append(out, '"')
	return string(out)
}
