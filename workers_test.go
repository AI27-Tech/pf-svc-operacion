package operacion_test

import (
	"context"
	"testing"

	"github.com/AI27-Tech/pf-pkg-platform-go/bus"
	"github.com/AI27-Tech/pf-pkg-platform-go/worker"
	operacion "github.com/AI27-Tech/pf-svc-operacion"
)

// busFake es un bus.Publisher que no hace nada: basta para construir el módulo y
// comprobar el CABLEADO del relay (New → Workers). La mecánica del relay en sí
// (drenado, marcar publicado, recuperación ante fallo del bus = prueba 7 de G2)
// ya está probada en pf-pkg-platform-go/outbox/relay_test.go; no se reprueba.
type busFake struct{}

func (busFake) Publish(ctx context.Context, s bus.Sobre) error { return nil }

// difusorFake es un bus.Broadcaster mínimo para comprobar el cableado del
// consumidor de sesion.revocada (New → Workers → Routes). No entrega sobres; la
// mecánica de la proyección ya está probada en internal/autorizacion.
type difusorFake struct{}

func (difusorFake) Broadcast(ctx context.Context, s bus.Sobre) error { return nil }
func (difusorFake) SubscribeBroadcast(h bus.Handler) (worker.Worker, error) {
	return workerNoop{}, nil
}

type workerNoop struct{}

func (workerNoop) Name() string                    { return "noop" }
func (workerNoop) Ready(ctx context.Context) error { return nil }
func (workerNoop) Run(ctx context.Context) error   { <-ctx.Done(); return nil }

// revocacionFake satisface operacion.Revocacion para el snapshot/fallback.
type revocacionFake struct{}

func (revocacionFake) ListarRevocadas(ctx context.Context) ([]string, error) { return nil, nil }
func (revocacionFake) ObtenerSesion(ctx context.Context, s string) (bool, error) {
	return false, nil
}

// TestWorkersSinBusPublisherEsNil fija que, sin BusPublisher, operacion no
// arranca ningún worker (arranque standalone sin bus): escribe eventos a la
// tabla pero no los publica.
func TestWorkersSinBusPublisherEsNil(t *testing.T) {
	m, err := operacion.New(operacion.Pools{}, operacion.Deps{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if w := m.Workers(); w != nil {
		t.Errorf("sin BusPublisher, Workers() debe ser nil; got %d workers", len(w))
	}
}

// TestWorkersConBusPublisherArrancaRelayDeViajes fija que, con BusPublisher,
// operacion expone exactamente un worker: el relay del outbox único de viajes
// (ADR 35). Es el cableado que destraba la publicación de los eventos viaje.*.
func TestWorkersConBusPublisherArrancaRelayDeViajes(t *testing.T) {
	m, err := operacion.New(operacion.Pools{}, operacion.Deps{BusPublisher: busFake{}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	w := m.Workers()
	if len(w) != 1 {
		t.Fatalf("con BusPublisher, Workers() debe tener 1 worker; got %d", len(w))
	}
	if got := w[0].Name(); got != "outbox-relay:viajes" {
		t.Errorf("nombre del relay: got %q, want outbox-relay:viajes", got)
	}
}

// TestWorkersConBroadcasterArrancaConsumidorRevocacion fija que, con Broadcaster
// y Revocacion, operacion expone el consumidor de sesion.revocada (envuelto por
// el reconstructor de snapshot) además del relay. Es el cableado que hace
// efectiva la revocación en las rutas de operacion (ADR 26).
func TestWorkersConBroadcasterArrancaConsumidorRevocacion(t *testing.T) {
	m, err := operacion.New(operacion.Pools{}, operacion.Deps{
		BusPublisher: busFake{},
		Broadcaster:  difusorFake{},
		Revocacion:   revocacionFake{},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	w := m.Workers()
	// Dos workers: el relay y el consumidor de revocación.
	if len(w) != 2 {
		t.Fatalf("con Broadcaster + BusPublisher, Workers() debe tener 2; got %d", len(w))
	}
	var nombres []string
	for _, x := range w {
		nombres = append(nombres, x.Name())
	}
	tiene := func(n string) bool {
		for _, x := range nombres {
			if x == n {
				return true
			}
		}
		return false
	}
	if !tiene("outbox-relay:viajes") {
		t.Errorf("falta el relay en Workers: %v", nombres)
	}
	if !tiene("operacion-revocacion") {
		t.Errorf("falta el consumidor de revocación en Workers: %v", nombres)
	}
}

// TestWorkersSinBroadcasterNoArrancaConsumidor fija que sin Broadcaster no hay
// consumidor de revocación (solo el relay si hay BusPublisher): desarrollo sin
// bus, rutas protegidas solo por firma.
func TestWorkersSinBroadcasterNoArrancaConsumidor(t *testing.T) {
	m, err := operacion.New(operacion.Pools{}, operacion.Deps{BusPublisher: busFake{}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, x := range m.Workers() {
		if x.Name() == "operacion-revocacion" {
			t.Error("sin Broadcaster no debe haber consumidor de revocación")
		}
	}
}
