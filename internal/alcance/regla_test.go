package alcance

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestPara fija la tabla rol × acceso de DEC-149 #8 y #16 sobre los viajes.
func TestPara(t *testing.T) {
	quiere := map[string][3]Ambito{ // Lista, Caso, Subrecurso
		"jefe_operativo": {Todos, Todos, Todos},
		"administrador":  {Todos, Todos, Todos},
		"monitorista":    {PorClientes, PorClientes, PorClientes},
		"supervisor":     {PorClientes, PorClientes, PorClientes},
		"coordinador":    {PorClientes, PorClientes, PorClientes},
		"ejecutivo":      {Prohibido, Prohibido, Prohibido},
		"reaccion":       {Prohibido, Todos, Prohibido},
		"operador":       {Prohibido, Prohibido, Prohibido}, // rol retirado
		"":               {Prohibido, Prohibido, Prohibido},
	}
	for rol, q := range quiere {
		for i, a := range []Acceso{Lista, Caso, Subrecurso} {
			if got := Para(rol, a); got != q[i] {
				t.Errorf("Para(%q, %d) = %d; quiere %d", rol, a, got, q[i])
			}
		}
	}
}

// TestPasoPorAprobado recorre los 13 tokens: solo SOLICITADO, RECHAZADO_POR_AI27
// y la cancelación desde SOLICITADO no pasaron por APROBADO.
func TestPasoPorAprobado(t *testing.T) {
	no := map[string]bool{"SOLICITADO": true, "RECHAZADO_POR_AI27": true}
	for _, e := range []string{"SOLICITADO", "APROBADO", "RECHAZADO_POR_AI27", "RECHAZADO_POR_CLIENTE",
		"ASIGNADO", "APTO_PARA_SALIDA", "EN_EMBARQUE", "PATRULLA_EN_CAMINO", "EN_TRANSITO",
		"EN_DESTINO", "EN_DESCARGA", "CERRADO", "CANCELADO"} {
		if got := PasoPorAprobado(Hechos{Estado: e}); got == no[e] {
			t.Errorf("PasoPorAprobado(%s) = %v; quiere %v", e, got, !no[e])
		}
	}
	if PasoPorAprobado(Hechos{Estado: "CANCELADO", CanceladoDesdeSolicitado: true}) {
		t.Error("CANCELADO desde SOLICITADO: quiere false")
	}
}

// TestVisible: cliente en el conjunto y paso por APROBADO; vacío no ve nada. En
// un préstamo la unidad no cuenta: decide el cliente del viaje (DEC-149 #5).
func TestVisible(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	viajeA := Hechos{ClienteID: a, Estado: "EN_TRANSITO"}
	casos := []struct {
		nombre   string
		clientes []uuid.UUID
		h        Hechos
		quiere   bool
	}{
		{"su cliente", []uuid.UUID{a}, viajeA, true},
		{"unión de clientes (supervisor)", []uuid.UUID{b, a}, viajeA, true},
		{"otro cliente", []uuid.UUID{b}, viajeA, false},
		{"préstamo: unidad de B en viaje de A, monitorista de B", []uuid.UUID{b}, viajeA, false},
		{"sin alcance (nil)", nil, viajeA, false},
		{"sin alcance (vacío)", []uuid.UUID{}, viajeA, false},
		{"SOLICITADO de su cliente", []uuid.UUID{a}, Hechos{ClienteID: a, Estado: "SOLICITADO"}, false},
		{"RECHAZADO_POR_AI27 de su cliente", []uuid.UUID{a}, Hechos{ClienteID: a, Estado: "RECHAZADO_POR_AI27"}, false},
		{"cancelado desde SOLICITADO", []uuid.UUID{a}, Hechos{ClienteID: a, Estado: "CANCELADO", CanceladoDesdeSolicitado: true}, false},
		{"cancelado tras APROBADO", []uuid.UUID{a}, Hechos{ClienteID: a, Estado: "CANCELADO"}, true},
		{"APROBADO sin escolta", []uuid.UUID{a}, Hechos{ClienteID: a, Estado: "APROBADO"}, true},
	}
	for _, c := range casos {
		if got := Visible(c.clientes, c.h); got != c.quiere {
			t.Errorf("%s: Visible = %v; quiere %v", c.nombre, got, c.quiere)
		}
	}
}

// TestLeerPayload fija el parseo de alcance.cambiado tal como lo publica
// identidad; lo ilegible se descarta.
func TestLeerPayload(t *testing.T) {
	c := uuid.New()
	a, ok := leer([]byte(`{"sujeto_id":"11111111-1111-1111-1111-111111111111","idp_subject":"s-1","clientes_efectivos_ids":["` + c.String() + `"],"cambiado_en":"2026-11-17T10:00:00.123456Z"}`))
	if !ok || a.IdpSubject != "s-1" || len(a.Clientes) != 1 || a.Clientes[0] != c ||
		!a.CambiadoEn.Equal(time.Date(2026, 11, 17, 10, 0, 0, 123456000, time.UTC)) {
		t.Errorf("payload válido: %+v ok=%v", a, ok)
	}
	if a, ok := leer([]byte(`{"sujeto_id":"11111111-1111-1111-1111-111111111111","idp_subject":"s-1","clientes_efectivos_ids":[],"cambiado_en":"2026-11-17T10:00:00Z"}`)); !ok || a.Clientes == nil || len(a.Clientes) != 0 {
		t.Errorf("vacío = quitar: %+v ok=%v; quiere lista vacía", a, ok)
	}
	for _, malo := range []string{
		`no json`,
		`{"clientes_efectivos_ids":[],"cambiado_en":"2026-11-17T10:00:00Z"}`,
		// Un alcance de tracer.41 sin idp_subject no se puede indexar (DEC-149 #22).
		`{"sujeto_id":"11111111-1111-1111-1111-111111111111","clientes_efectivos_ids":[],"cambiado_en":"2026-11-17T10:00:00Z"}`,
		`{"sujeto_id":"11111111-1111-1111-1111-111111111111","idp_subject":"s-1","clientes_efectivos_ids":[]}`,
		`{"sujeto_id":"11111111-1111-1111-1111-111111111111","idp_subject":"s-1","clientes_efectivos_ids":["x"],"cambiado_en":"2026-11-17T10:00:00Z"}`,
	} {
		if _, ok := leer([]byte(malo)); ok {
			t.Errorf("payload ilegible aceptado: %s", malo)
		}
	}
}
