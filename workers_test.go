package operacion_test

import (
	"context"
	"testing"

	catalogos "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/catalogos"
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

// suscriptorFake es un bus.Subscriber mínimo para comprobar el CABLEADO del
// consumidor durable de catalogo.actualizado (New → Workers). Devuelve un worker
// cuyo nombre refleja el durableName pedido, igual que haría el adaptador real
// ("bus-durable:"+durableName); así el test fija el nombre observable del worker.
// Guarda el filtro recibido para comprobar que se suscribe al tipo correcto.
type suscriptorFake struct {
	durableName string
	superficie  bus.Sensibilidad
	filtro      []string
}

func (s *suscriptorFake) Subscribe(durableName string, superficie bus.Sensibilidad, filtroTipos []string, h bus.Handler) (worker.Worker, error) {
	s.durableName = durableName
	s.superficie = superficie
	s.filtro = filtroTipos
	return workerConNombre{nombre: "bus-durable:" + durableName}, nil
}

type workerConNombre struct{ nombre string }

func (w workerConNombre) Name() string                  { return w.nombre }
func (workerConNombre) Ready(ctx context.Context) error { return nil }
func (workerConNombre) Run(ctx context.Context) error   { <-ctx.Done(); return nil }

// aptitudFake satisface operacion.AptitudRecursos para que New envuelva un puerto
// real con la caché. No necesita devolver nada útil: el test solo comprueba el
// cableado del worker, no la lectura de aptitud.
type aptitudFake struct{}

func (aptitudFake) AptitudVehiculo(ctx context.Context, vehiculoID, clienteID catalogos.Id) (*catalogos.Aptitud, error) {
	return &catalogos.Aptitud{}, nil
}
func (aptitudFake) AptitudCustodio(ctx context.Context, custodioID, clienteID catalogos.Id) (*catalogos.Aptitud, error) {
	return &catalogos.Aptitud{}, nil
}

// TestWorkersConSubscriberArrancaConsumidorCatalogos fija que, con Subscriber y
// un puerto Aptitud que envolver, operacion expone el consumidor durable de
// catalogo.actualizado (E5-T5) además del relay, suscrito al tipo correcto sobre
// la superficie amplia.
func TestWorkersConSubscriberArrancaConsumidorCatalogos(t *testing.T) {
	sub := &suscriptorFake{}
	m, err := operacion.New(operacion.Pools{}, operacion.Deps{
		BusPublisher: busFake{},
		Subscriber:   sub,
		Aptitud:      aptitudFake{},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Se suscribió al tipo correcto, sobre la superficie amplia, con el durable
	// del módulo.
	if sub.durableName != "operacion-catalogos" {
		t.Errorf("durableName = %q, want operacion-catalogos", sub.durableName)
	}
	if sub.superficie != bus.SensibilidadAmplia {
		t.Errorf("superficie = %q, want amplia", sub.superficie)
	}
	if len(sub.filtro) != 1 || sub.filtro[0] != "catalogo.actualizado" {
		t.Errorf("filtro = %v, want [catalogo.actualizado]", sub.filtro)
	}

	// El worker aparece en Workers() con el nombre durable del bus.
	var nombres []string
	for _, x := range m.Workers() {
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
	if !tiene("bus-durable:operacion-catalogos") {
		t.Errorf("falta el consumidor de catálogos en Workers: %v", nombres)
	}
}

// TestWorkersSinSubscriberNoArrancaConsumidorCatalogos fija que sin Subscriber no
// hay consumidor de catálogos (y por tanto Aptitud se usa sin caché).
func TestWorkersSinSubscriberNoArrancaConsumidorCatalogos(t *testing.T) {
	m, err := operacion.New(operacion.Pools{}, operacion.Deps{
		BusPublisher: busFake{},
		Aptitud:      aptitudFake{},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, x := range m.Workers() {
		if x.Name() == "bus-durable:operacion-catalogos" {
			t.Error("sin Subscriber no debe haber consumidor de catálogos")
		}
	}
}
