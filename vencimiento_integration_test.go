package operacion_test

import (
	"context"
	"testing"
	"time"

	"github.com/AI27-Tech/pf-svc-operacion"
	"github.com/AI27-Tech/pf-svc-operacion/internal/dbtest"
	"github.com/google/uuid"
)

// TestWorkerVencimientoCableado: con el pool de op_solicitudes, Workers() trae el
// worker de vencimiento (sin necesitar bus) y su primera corrida cierra una
// cotización de 2020 sin pago. Las fechas están escritas a mano.
func TestWorkerVencimientoCableado(t *testing.T) {
	ctx, cancelar := context.WithCancel(context.Background())
	defer cancelar()
	m := moduloCompletoCon(t, ctx, operacion.Deps{Verificador: autenticadorRoles{tenant: uuid.NewString()}, Atomos: catalogo})

	id := uuid.New()
	admin, cerrar := dbtest.AdminConn(t, ctx)
	defer cerrar()
	dbtest.Exec(t, ctx, admin, `INSERT INTO solicitudes.solicitud (id, cliente_id, estado, marca_penalizacion, origen, destinos, creado_en)
	  VALUES ('`+id.String()+`', gen_random_uuid(), 'COTIZADO', false, 'A', '[{"nombre":"B"}]', '2020-01-01T00:00:00Z')`)
	dbtest.Exec(t, ctx, admin, `INSERT INTO solicitudes.cotizacion (solicitud_id, version, monto, moneda, fecha_hora_servicio, entregada_en, vigente_hasta)
	  VALUES ('`+id.String()+`', 1, 100, 'MXN', '2020-02-01T00:00:00Z', '2020-01-01T12:00:00Z', '2020-01-16')`)

	encontrado := false
	for _, w := range m.Workers() {
		if w.Name() == "vencimiento:solicitudes" {
			encontrado = true
			go func() { _ = w.Run(ctx) }()
		}
	}
	if !encontrado {
		t.Fatal("Workers() no incluye vencimiento:solicitudes")
	}
	for fin := time.Now().Add(10 * time.Second); time.Now().Before(fin); time.Sleep(50 * time.Millisecond) {
		var estado string
		if err := admin.QueryRow(ctx, `SELECT estado FROM solicitudes.solicitud WHERE id=$1`, id).Scan(&estado); err != nil {
			t.Fatal(err)
		}
		if estado == "VENCIDA" {
			return
		}
	}
	t.Fatal("el worker no cerró la solicitud vencida")
}
