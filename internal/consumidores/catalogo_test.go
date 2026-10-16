package consumidores

import (
	"context"
	"encoding/json"
	"testing"

	catalogos "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/catalogos"
	"github.com/AI27-Tech/pf-pkg-platform-go/bus"
	"github.com/google/uuid"
)

// invalidadorFake registra las invalidaciones recibidas para poder contarlas y
// verificar la entidad/recurso exactos.
type invalidadorFake struct {
	llamadas []invocacion
}

type invocacion struct {
	entidad   string
	recursoID catalogos.Id
}

func (f *invalidadorFake) Invalidar(entidad string, recursoID catalogos.Id) {
	f.llamadas = append(f.llamadas, invocacion{entidad: entidad, recursoID: recursoID})
}

// sobreCatalogo arma un bus.Sobre de catalogo.actualizado con el payload que
// publica catalogos (entidad + entidad_id).
func sobreCatalogo(t *testing.T, eventoID, entidad, entidadID string) bus.Sobre {
	t.Helper()
	payload, err := json.Marshal(map[string]string{"entidad": entidad, "entidad_id": entidadID})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return bus.Sobre{
		EventoID:     eventoID,
		Tipo:         TipoCatalogoActualizado,
		Version:      1,
		AgregadoID:   entidadID,
		Sensibilidad: bus.SensibilidadAmplia,
		Payload:      payload,
	}
}

// TestProcesarInvalidaUnaVez: un catalogo.actualizado válido invalida la entrada
// de esa entidad+recurso exactamente una vez.
func TestProcesarInvalidaUnaVez(t *testing.T) {
	inv := &invalidadorFake{}
	c := NuevoConsumidorCatalogo(inv)

	vehID := uuid.New()
	s := sobreCatalogo(t, "ev-1", "vehiculo", vehID.String())
	if err := c.Procesar(context.Background(), s); err != nil {
		t.Fatalf("Procesar: %v", err)
	}
	if len(inv.llamadas) != 1 {
		t.Fatalf("se esperaba 1 invalidación; hubo %d", len(inv.llamadas))
	}
	if inv.llamadas[0].entidad != "vehiculo" || inv.llamadas[0].recursoID != vehID {
		t.Errorf("invalidación inesperada: %+v (esperaba vehiculo/%s)", inv.llamadas[0], vehID)
	}
}

// TestDobleEntregaInvalidaUnaVez es la propiedad P13: reentregar el MISMO
// evento_id solo surte efecto una vez (dedup en memoria por evento_id).
func TestDobleEntregaInvalidaUnaVez(t *testing.T) {
	inv := &invalidadorFake{}
	c := NuevoConsumidorCatalogo(inv)

	vehID := uuid.New()
	s := sobreCatalogo(t, "ev-dup", "vehiculo", vehID.String())

	if err := c.Procesar(context.Background(), s); err != nil {
		t.Fatalf("primera entrega: %v", err)
	}
	if err := c.Procesar(context.Background(), s); err != nil {
		t.Fatalf("segunda entrega (duplicada): %v", err)
	}
	if len(inv.llamadas) != 1 {
		t.Errorf("P13: doble entrega del mismo evento_id debe invalidar una vez; hubo %d", len(inv.llamadas))
	}
}

// TestEventosDistintosInvalidanCadaUno: dos eventos con evento_id distinto (aun
// de la misma entidad) invalidan cada uno; el dedup es por evento_id, no por
// recurso.
func TestEventosDistintosInvalidanCadaUno(t *testing.T) {
	inv := &invalidadorFake{}
	c := NuevoConsumidorCatalogo(inv)

	vehID := uuid.New()
	if err := c.Procesar(context.Background(), sobreCatalogo(t, "ev-1", "vehiculo", vehID.String())); err != nil {
		t.Fatalf("ev-1: %v", err)
	}
	if err := c.Procesar(context.Background(), sobreCatalogo(t, "ev-2", "vehiculo", vehID.String())); err != nil {
		t.Fatalf("ev-2: %v", err)
	}
	if len(inv.llamadas) != 2 {
		t.Errorf("dos evento_id distintos deben invalidar dos veces; hubo %d", len(inv.llamadas))
	}
}

// TestTipoDistintoNoInvalida: un sobre de otro tipo se acusa sin invalidar (el
// consumidor es defensivo aunque el filtro del Subscribe ya restrinja el tipo).
func TestTipoDistintoNoInvalida(t *testing.T) {
	inv := &invalidadorFake{}
	c := NuevoConsumidorCatalogo(inv)

	s := sobreCatalogo(t, "ev-x", "vehiculo", uuid.New().String())
	s.Tipo = "viaje.estado_cambiado" // no es lo nuestro
	if err := c.Procesar(context.Background(), s); err != nil {
		t.Fatalf("Procesar: %v", err)
	}
	if len(inv.llamadas) != 0 {
		t.Errorf("un tipo distinto no debe invalidar; hubo %d", len(inv.llamadas))
	}
}

// TestPayloadInvalidoNoInvalidaYAcusa: payloads que no sirven para invalidar
// (JSON ilegible, entidad vacía, entidad_id vacío, UUID mal formado) se acusan
// (return nil) sin invalidar y sin reintentarse. Tolerable porque la aptitud es
// informativa (DEC-055): el peor efecto es servir algo obsoleto hasta el TTL.
func TestPayloadInvalidoNoInvalidaYAcusa(t *testing.T) {
	casos := []struct {
		nombre  string
		payload []byte
	}{
		{"json ilegible", []byte("{no es json")},
		{"entidad vacía", []byte(`{"entidad":"","entidad_id":"` + uuid.New().String() + `"}`)},
		{"entidad_id vacío", []byte(`{"entidad":"vehiculo","entidad_id":""}`)},
		{"uuid mal formado", []byte(`{"entidad":"vehiculo","entidad_id":"no-es-uuid"}`)},
	}
	for _, caso := range casos {
		t.Run(caso.nombre, func(t *testing.T) {
			inv := &invalidadorFake{}
			c := NuevoConsumidorCatalogo(inv)
			s := bus.Sobre{
				EventoID:     "ev-" + caso.nombre,
				Tipo:         TipoCatalogoActualizado,
				Version:      1,
				Sensibilidad: bus.SensibilidadAmplia,
				Payload:      caso.payload,
			}
			if err := c.Procesar(context.Background(), s); err != nil {
				t.Fatalf("Procesar debe acusar (nil) un payload inválido; got %v", err)
			}
			if len(inv.llamadas) != 0 {
				t.Errorf("un payload inválido no debe invalidar; hubo %d", len(inv.llamadas))
			}
		})
	}
}
