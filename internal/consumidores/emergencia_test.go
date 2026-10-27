package consumidores_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/AI27-Tech/pf-pkg-platform-go/bus"
	"github.com/AI27-Tech/pf-svc-operacion/internal/consumidores"
)

// historicoFake captura las llamadas a RegistrarDesdeEvento para afirmar qué se
// proyectó, sin tocar Postgres.
type historicoFake struct {
	llamadas []registro
}

type registro struct {
	viajeID, eventoID, texto, autorID string
}

func (h *historicoFake) RegistrarDesdeEvento(ctx context.Context, viajeID, eventoID, texto, autorID string) (bool, error) {
	h.llamadas = append(h.llamadas, registro{viajeID, eventoID, texto, autorID})
	return true, nil
}

func sobreNota(payload map[string]any) bus.Sobre {
	b, _ := json.Marshal(payload)
	return bus.Sobre{
		EventoID:     "ev-1",
		Tipo:         consumidores.TipoEmergenciaNotaAgregada,
		Version:      1,
		AgregadoID:   "caso-1",
		Sensibilidad: bus.SensibilidadRestringida,
		Payload:      b,
	}
}

// TestProyectaNotaAlHistorico: un evento bien formado proyecta la nota al viaje,
// con el nota_id como clave de idempotencia (evento_id).
func TestProyectaNotaAlHistorico(t *testing.T) {
	h := &historicoFake{}
	cons := consumidores.NuevoConsumidorEmergenciaNota(h)

	err := cons.Procesar(context.Background(), sobreNota(map[string]any{
		"caso_emergencia_id": "caso-1",
		"nota_id":            "nota-7",
		"viaje_id":           "viaje-9",
		"audiencia_autor":    "Cliente",
		"cuerpo":             "el conductor reporta un bloqueo",
		"es_resolucion":      false,
	}))
	if err != nil {
		t.Fatalf("Procesar: %v", err)
	}
	if len(h.llamadas) != 1 {
		t.Fatalf("se esperaba 1 proyección, hubo %d", len(h.llamadas))
	}
	got := h.llamadas[0]
	if got.viajeID != "viaje-9" || got.eventoID != "nota-7" || got.texto != "el conductor reporta un bloqueo" {
		t.Fatalf("proyección incorrecta: %+v", got)
	}
}

// TestIgnoraOtroTipo: un sobre de otro tipo se acusa sin proyectar.
func TestIgnoraOtroTipo(t *testing.T) {
	h := &historicoFake{}
	cons := consumidores.NuevoConsumidorEmergenciaNota(h)

	s := sobreNota(map[string]any{"viaje_id": "v", "nota_id": "n", "cuerpo": "x"})
	s.Tipo = "catalogo.actualizado"
	if err := cons.Procesar(context.Background(), s); err != nil {
		t.Fatalf("Procesar: %v", err)
	}
	if len(h.llamadas) != 0 {
		t.Fatalf("un tipo ajeno no debe proyectar, hubo %d", len(h.llamadas))
	}
}

// TestPayloadIncompletoNoProyecta: sin viaje_id (o sin cuerpo) se acusa sin
// escribir, porque no hay a qué histórico copiar.
func TestPayloadIncompletoNoProyecta(t *testing.T) {
	h := &historicoFake{}
	cons := consumidores.NuevoConsumidorEmergenciaNota(h)

	// Falta viaje_id.
	if err := cons.Procesar(context.Background(), sobreNota(map[string]any{
		"nota_id": "n", "cuerpo": "x",
	})); err != nil {
		t.Fatalf("Procesar: %v", err)
	}
	if len(h.llamadas) != 0 {
		t.Fatalf("sin viaje_id no debe proyectar, hubo %d", len(h.llamadas))
	}
}
