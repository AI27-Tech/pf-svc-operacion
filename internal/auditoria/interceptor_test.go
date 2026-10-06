package auditoria

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

// txFalso solo implementa Exec; el resto de pgx.Tx entra en pánico si se usa.
type txFalso struct {
	execs int
}

func (t *txFalso) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	t.execs++
	return pgconn.CommandTag{}, nil
}
func (t *txFalso) Begin(context.Context) (pgx.Tx, error) { panic("no usado") }
func (t *txFalso) Commit(context.Context) error          { return nil }
func (t *txFalso) Rollback(context.Context) error        { return nil }
func (t *txFalso) CopyFrom(context.Context, pgx.Identifier, []string, pgx.CopyFromSource) (int64, error) {
	panic("no usado")
}
func (t *txFalso) SendBatch(context.Context, *pgx.Batch) pgx.BatchResults { panic("no usado") }
func (t *txFalso) LargeObjects() pgx.LargeObjects                         { panic("no usado") }
func (t *txFalso) Prepare(context.Context, string, string) (*pgconn.StatementDescription, error) {
	panic("no usado")
}
func (t *txFalso) Query(context.Context, string, ...any) (pgx.Rows, error) { panic("no usado") }
func (t *txFalso) QueryRow(context.Context, string, ...any) pgx.Row        { panic("no usado") }
func (t *txFalso) Conn() *pgx.Conn                                         { return nil }
func (t *txFalso) TypeMap() *pgtype.Map                                    { return nil }

var _ pgx.Tx = (*txFalso)(nil)

// TestInterceptar_EscribeCambioYRegistro: el camino feliz escribe el cambio y
// luego el registro de auditoría en la misma tx (dos Execs).
func TestInterceptar_EscribeCambioYRegistro(t *testing.T) {
	tx := &txFalso{}
	err := Interceptar(context.Background(), tx, Registro{
		ViajeID: "v1", Modulo: "viajes", Accion: "crear",
	}, func(ctx context.Context, tx pgx.Tx) error {
		_, e := tx.Exec(ctx, "INSERT INTO viajes.viaje ...")
		return e
	})
	if err != nil {
		t.Fatalf("Interceptar: %v", err)
	}
	if tx.execs != 2 {
		t.Errorf("se esperaban 2 Execs (cambio + registro), hubo %d", tx.execs)
	}
}

// TestInterceptar_FalloEnElCambioNoEscribeRegistro: si el cambio falla, el
// registro NO se escribe (el llamador hará rollback: ni cambio ni registro, P3).
func TestInterceptar_FalloEnElCambioNoEscribeRegistro(t *testing.T) {
	tx := &txFalso{}
	fallo := errors.New("fallo inyectado antes del commit")
	err := Interceptar(context.Background(), tx, Registro{
		ViajeID: "v1", Modulo: "viajes", Accion: "crear",
	}, func(ctx context.Context, tx pgx.Tx) error {
		return fallo // el cambio falla antes de escribir nada
	})
	if !errors.Is(err, fallo) {
		t.Fatalf("se esperaba el error del cambio, got %v", err)
	}
	if tx.execs != 0 {
		t.Errorf("no debió escribirse el registro si el cambio falló; Execs=%d", tx.execs)
	}
}

// TestInterceptar_RegistroInvalido: sin modulo/accion el registro probatorio se
// rechaza (no se permite auditoría vacía).
func TestInterceptar_RegistroInvalido(t *testing.T) {
	tx := &txFalso{}
	err := Interceptar(context.Background(), tx, Registro{ViajeID: "v1"},
		func(ctx context.Context, tx pgx.Tx) error { return nil })
	if err == nil {
		t.Fatal("un registro sin modulo/accion debería rechazarse")
	}
}
