package operacion

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/AI27-Tech/pf-svc-operacion/internal/roster"
	"github.com/google/uuid"
)

// TestCampoNulo fija el parseo de un PATCH: `"campo": null` es quitar; la
// propiedad ausente o con valor no lo es (DEC-149 #2, ficha técnica).
func TestCampoNulo(t *testing.T) {
	for cuerpo, quiere := range map[string]bool{
		`{}`:                      false,
		`{"activa":true}`:         false,
		`{"cliente_id":null}`:     true,
		`{"cliente_id" :  null }`: true,
		`{"cliente_id":"` + uuid.NewString() + `"}`: false,
		`no es json`: false,
	} {
		if got := campoNulo([]byte(cuerpo), "cliente_id"); got != quiere {
			t.Errorf("campoNulo(%s) = %v; quiere %v", cuerpo, got, quiere)
		}
	}
}

// TestUnidadModificacionClienteId fija cómo llega cliente_id al roster desde el
// JSON del PATCH (leerUnidadModificacion, la lectura de handleModificarUnidad):
// ausente = nil (no se toca), null = &roster.QuitarCliente, un id = ese id.
func TestUnidadModificacionClienteId(t *testing.T) {
	id := uuid.New()
	for cuerpo, quiere := range map[string]*uuid.UUID{
		`{"activa":false}`:                     nil,
		`{"cliente_id":null}`:                  &roster.QuitarCliente,
		`{"cliente_id":"` + id.String() + `"}`: &id,
	} {
		body, err := leerUnidadModificacion(httptest.NewRequest("PATCH", "/", strings.NewReader(cuerpo)))
		if err != nil {
			t.Fatalf("%s: %v", cuerpo, err)
		}
		switch {
		case quiere == nil && body.ClienteId != nil:
			t.Errorf("%s: ClienteId %v; quiere nil", cuerpo, *body.ClienteId)
		case quiere != nil && (body.ClienteId == nil || *body.ClienteId != *quiere):
			t.Errorf("%s: ClienteId %v; quiere %v", cuerpo, body.ClienteId, *quiere)
		}
	}
	if _, err := leerUnidadModificacion(httptest.NewRequest("PATCH", "/", strings.NewReader(`{"tipo":"moto"}`))); err == nil {
		t.Error("propiedad fuera del contrato: quiere 400")
	}
}
