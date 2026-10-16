package consumidores_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	catalogos "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/catalogos"
	"github.com/AI27-Tech/pf-pkg-platform-go/bus"
	"github.com/AI27-Tech/pf-pkg-platform-go/bus/memory"
	"github.com/AI27-Tech/pf-pkg-platform-go/worker"
	"github.com/AI27-Tech/pf-svc-operacion/internal/catalogoscache"
	"github.com/AI27-Tech/pf-svc-operacion/internal/consumidores"
	"github.com/google/uuid"
)

// Esta prueba de EXTREMO A EXTREMO ejerce el camino completo del DoD de E5-T5
// con el bus en memoria real (no un doble): un catalogo.actualizado publicado
// por el bus llega al consumidor durable, que invalida la caché de aptitud, y la
// siguiente lectura de aptitud reconsulta a catalogos. Verifica el cableado
// caché ↔ consumidor ↔ bus sin pasar por el Module (sus campos son privados);
// que el Module exponga el worker con Subscriber lo cubre workers_test.go.

// registroPrueba es el bus.Registry mínimo que el módulo aporta al bus: declara
// válido catalogo.actualizado v1 y usa el AgregadoID como clave de orden.
type registroPrueba struct{}

func (registroPrueba) Allowed(tipo string, version int) bool {
	return tipo == consumidores.TipoCatalogoActualizado && version == 1
}
func (registroPrueba) OrderingKey(s bus.Sobre) string { return s.AgregadoID }

// subyacenteContador es un AptitudRecursos que cuenta consultas de forma segura
// para concurrencia (el worker corre en otra goroutine).
type subyacenteContador struct {
	mu       sync.Mutex
	llamadas int
}

func (s *subyacenteContador) AptitudVehiculo(ctx context.Context, vehiculoID, clienteID catalogos.Id) (*catalogos.Aptitud, error) {
	s.mu.Lock()
	s.llamadas++
	s.mu.Unlock()
	return &catalogos.Aptitud{Apto: true, RecursoId: vehiculoID}, nil
}
func (s *subyacenteContador) AptitudCustodio(ctx context.Context, custodioID, clienteID catalogos.Id) (*catalogos.Aptitud, error) {
	s.mu.Lock()
	s.llamadas++
	s.mu.Unlock()
	return &catalogos.Aptitud{Apto: true, RecursoId: custodioID}, nil
}

func (s *subyacenteContador) conteo() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.llamadas
}

func TestInvalidacionDeExtremoAExtremo(t *testing.T) {
	sub := &subyacenteContador{}
	// TTL largo: la ÚNICA razón de una reconsulta en esta prueba debe ser la
	// invalidación por evento, no el vencimiento del TTL.
	cache := catalogoscache.Nueva(sub, time.Hour)
	cons := consumidores.NuevoConsumidorCatalogo(cache)

	b := memory.Nuevo(registroPrueba{}, memory.Opciones{})
	w, err := b.Subscribe(
		"operacion-catalogos",
		bus.SensibilidadAmplia,
		[]string{consumidores.TipoCatalogoActualizado},
		func(ctx context.Context, s bus.Sobre) error { return cons.Procesar(ctx, s) },
	)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hecho := make(chan struct{})
	go func() {
		// reg nil: RunGroup omite el registro de readiness (no lo necesitamos aquí).
		_ = worker.RunGroup(ctx, nil, w)
		close(hecho)
	}()

	vehID := uuid.New()
	cliID := uuid.New()

	// Primera lectura: va a catalogos y cachea (1 consulta).
	if _, err := cache.AptitudVehiculo(ctx, vehID, cliID); err != nil {
		t.Fatalf("primera lectura: %v", err)
	}
	if got := sub.conteo(); got != 1 {
		t.Fatalf("tras primera lectura, consultas=%d, want 1", got)
	}

	// Publicar catalogo.actualizado del vehículo por el bus.
	payload, _ := json.Marshal(map[string]string{"entidad": "vehiculo", "entidad_id": vehID.String()})
	if err := b.Publish(ctx, bus.Sobre{
		EventoID:     "ev-e2e-1",
		Tipo:         consumidores.TipoCatalogoActualizado,
		Version:      1,
		AgregadoID:   vehID.String(),
		Sensibilidad: bus.SensibilidadAmplia,
		Payload:      payload,
	}); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	// Esperar a que el consumidor invalide: la relectura debe reconsultar. Como el
	// bus entrega async, hacemos polling corto en vez de dormir fijo.
	if !esperarReconsulta(ctx, cache, sub, vehID, cliID, 2) {
		t.Fatalf("tras el evento, la caché no se invalidó: consultas=%d, want 2", sub.conteo())
	}

	cancel()
	<-hecho
}

// esperarReconsulta reintenta leer la aptitud hasta que el contador del
// subyacente alcance quiere (señal de que la caché se invalidó y reconsultó), o
// agota un plazo corto. Devuelve true si lo alcanzó. Lee en cada vuelta porque
// la lectura es la que dispara la reconsulta una vez que la entrada fue
// invalidada por el worker.
func esperarReconsulta(ctx context.Context, cache *catalogoscache.CacheAptitud, sub *subyacenteContador, vehID, cliID catalogos.Id, quiere int) bool {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := cache.AptitudVehiculo(ctx, vehID, cliID); err != nil {
			return false
		}
		if sub.conteo() >= quiere {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}
