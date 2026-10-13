package operacion_test

import (
	"context"
	"testing"

	"github.com/AI27-Tech/pf-pkg-platform-go/bus"
	operacion "github.com/AI27-Tech/pf-svc-operacion"
)

// busFake es un bus.Publisher que no hace nada: basta para construir el módulo y
// comprobar el CABLEADO del relay (New → Workers). La mecánica del relay en sí
// (drenado, marcar publicado, recuperación ante fallo del bus = prueba 7 de G2)
// ya está probada en pf-pkg-platform-go/outbox/relay_test.go; no se reprueba.
type busFake struct{}

func (busFake) Publish(ctx context.Context, s bus.Sobre) error { return nil }

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
