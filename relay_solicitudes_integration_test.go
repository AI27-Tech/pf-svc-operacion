package operacion_test

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/AI27-Tech/pf-pkg-platform-go/bus"
	"github.com/AI27-Tech/pf-svc-operacion"
	"github.com/google/uuid"
)

// busGrabador guarda los sobres que el relay publica.
type busGrabador struct {
	mu     sync.Mutex
	sobres []bus.Sobre
}

func (b *busGrabador) Publish(_ context.Context, s bus.Sobre) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sobres = append(b.sobres, s)
	return nil
}

func (b *busGrabador) tipos(agregado string) (n int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, s := range b.sobres {
		if s.AgregadoID == agregado && s.Tipo == "solicitud.creada" {
			n++
		}
	}
	return n
}

// TestRelayDeSolicitudesEntregaAlBus: crear una solicitud deja solicitud.creada en
// solicitudes.outbox y el worker outbox-relay:solicitudes lo entrega al bus. Cubre
// el cableado (pool op_solicitudes con search_path=solicitudes, relay con su pool).
func TestRelayDeSolicitudesEntregaAlBus(t *testing.T) {
	ctx, cancelar := context.WithCancel(context.Background())
	defer cancelar()
	tenant := uuid.New()
	grabador := &busGrabador{}
	m := moduloCompletoCon(t, ctx, operacion.Deps{
		Verificador: autenticadorRoles{tenant: tenant.String()}, Atomos: catalogo, BusPublisher: grabador,
	})
	mux := http.NewServeMux()
	m.Routes(mux)
	rec := llamar(t, mux, http.MethodPost, "/api/v1/solicitudes", "cliente", `{"origen":"CDMX","destinos":[{"nombre":"Guadalajara"}]}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("crear: status %d, cuerpo %s", rec.Code, rec.Body)
	}
	var id struct {
		Id string `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &id); err != nil || id.Id == "" {
		t.Fatalf("id de la solicitud: %v", err)
	}

	encontrado := false
	for _, w := range m.Workers() {
		if w.Name() == "outbox-relay:solicitudes" {
			encontrado = true
			go func() { _ = w.Run(ctx) }()
		}
	}
	if !encontrado {
		t.Fatal("Workers() no incluye outbox-relay:solicitudes")
	}
	for fin := time.Now().Add(10 * time.Second); time.Now().Before(fin); time.Sleep(50 * time.Millisecond) {
		if grabador.tipos(id.Id) == 1 {
			return
		}
	}
	t.Fatalf("el bus no recibió solicitud.creada de %s (recibidos %d)", id.Id, grabador.tipos(id.Id))
}
