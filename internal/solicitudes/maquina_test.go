package solicitudes

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/estados"
)

// aristasADR38 es la tabla de transiciones del ADR 38, escrita aparte de `aristas`
// para que una divergencia entre el ADR, el dato generado y los metadatos falle.
var aristasADR38 = [][2]estados.EstadoSolicitud{
	{estados.EstadoSolicitudSolicitado, estados.EstadoSolicitudCotizado},
	{estados.EstadoSolicitudSolicitado, estados.EstadoSolicitudCanceladoPorCliente},
	{estados.EstadoSolicitudCotizado, estados.EstadoSolicitudPagado},
	{estados.EstadoSolicitudCotizado, estados.EstadoSolicitudRechazadoPorCliente},
	{estados.EstadoSolicitudCotizado, estados.EstadoSolicitudCanceladoPorCliente},
	{estados.EstadoSolicitudCotizado, estados.EstadoSolicitudVencida},
	{estados.EstadoSolicitudVencida, estados.EstadoSolicitudPagado},
	{estados.EstadoSolicitudPagado, estados.EstadoSolicitudCotizado},
	{estados.EstadoSolicitudPagado, estados.EstadoSolicitudAprobado},
	{estados.EstadoSolicitudPagado, estados.EstadoSolicitudRechazadoPorAi27},
	{estados.EstadoSolicitudPagado, estados.EstadoSolicitudCanceladoPorCliente},
}

func esAristaADR38(de, a estados.EstadoSolicitud) bool {
	for _, p := range aristasADR38 {
		if p[0] == de && p[1] == a {
			return true
		}
	}
	return false
}

// TestAristasCoincidenConElContrato recorre los 64 pares de estados: el dato
// generado, el ADR 38 y la tabla de metadatos tienen exactamente las mismas
// aristas. Si el YAML cambia una arista, esta prueba falla.
func TestAristasCoincidenConElContrato(t *testing.T) {
	for _, de := range estados.EstadosSolicitud {
		for _, a := range estados.EstadosSolicitud {
			generada := estados.PuedeTransicionarSolicitud(de, a)
			_, enTabla := aristas[arista{de, a}]
			if generada != esAristaADR38(de, a) || generada != enTabla {
				t.Errorf("%s -> %s: generada=%v adr38=%v metadatos=%v", de, a, generada, esAristaADR38(de, a), enTabla)
			}
		}
	}
	if len(aristas) != len(aristasADR38) {
		t.Errorf("aristas: %d metadatos, %d en el ADR 38", len(aristas), len(aristasADR38))
	}
}

// TestTerminales: los cuatro terminales no admiten salida y VENCIDA solo admite
// VENCIDA -> PAGADO (DEC-148 #35).
func TestTerminales(t *testing.T) {
	terminales := []estados.EstadoSolicitud{
		estados.EstadoSolicitudAprobado, estados.EstadoSolicitudRechazadoPorAi27,
		estados.EstadoSolicitudRechazadoPorCliente, estados.EstadoSolicitudCanceladoPorCliente,
	}
	for _, de := range terminales {
		if !de.EsTerminal() {
			t.Errorf("%s debería ser terminal", de)
		}
		for _, a := range estados.EstadosSolicitud {
			if estados.PuedeTransicionarSolicitud(de, a) {
				t.Errorf("el terminal %s admite salida a %s", de, a)
			}
		}
	}
	for _, a := range estados.EstadosSolicitud {
		if want := a == estados.EstadoSolicitudPagado; estados.PuedeTransicionarSolicitud(estados.EstadoSolicitudVencida, a) != want {
			t.Errorf("VENCIDA -> %s: debería ser %v", a, want)
		}
	}
}

// TestSoloMaquinaEscribeEstado: ningún archivo del módulo, salvo maquina.go,
// actualiza `estado` (no existe escritura directa, E10-T2).
func TestSoloMaquinaEscribeEstado(t *testing.T) {
	archivos, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range archivos {
		if f == "maquina.go" || strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(strings.ToLower(string(b)), "set estado") {
			t.Errorf("%s escribe `estado` fuera de maquina.go", f)
		}
	}
}

// TestMetadatosDeAristasADR38 fija disparador, acción, guarda de motivo y marca
// de cada arista con valores escritos a mano desde el ADR 38 (tabla de
// transiciones) y el vocabulario de HistoricoSolicitudItem.accion del OpenAPI.
// Es independiente de `aristas`: si alguien cambia un valor ahí, falla aquí.
func TestMetadatosDeAristasADR38(t *testing.T) {
	const (
		sol  = estados.EstadoSolicitudSolicitado
		cot  = estados.EstadoSolicitudCotizado
		pag  = estados.EstadoSolicitudPagado
		venc = estados.EstadoSolicitudVencida
		apr  = estados.EstadoSolicitudAprobado
		rai  = estados.EstadoSolicitudRechazadoPorAi27
		rcl  = estados.EstadoSolicitudRechazadoPorCliente
		ccl  = estados.EstadoSolicitudCanceladoPorCliente
	)
	esperado := []struct {
		de, a       estados.EstadoSolicitud
		disparador  Disparador
		accion      string
		exigeMotivo bool
		marca       bool
	}{
		{sol, cot, "cotizacion", "cotizada", false, false},
		{sol, ccl, "cliente", "cancelada", false, false},
		{cot, pag, "cotizacion", "pago_confirmado", false, false},
		{cot, rcl, "cliente", "cotizacion_rechazada", false, false},
		{cot, ccl, "cliente", "cancelada", false, false},
		{cot, venc, "sistema", "vencida", false, false},
		{venc, pag, "cotizacion", "pago_confirmado_tardio", false, false},
		{pag, cot, "administrador", "pago_revertido", true, false},
		{pag, apr, "jefe_operativo", "aprobada", false, false},
		{pag, rai, "jefe_operativo", "rechazada", true, false},
		{pag, ccl, "cliente", "cancelada", false, true},
	}
	if len(aristas) != len(esperado) {
		t.Fatalf("aristas tiene %d entradas, el ADR 38 tiene %d", len(aristas), len(esperado))
	}
	for _, e := range esperado {
		r, ok := aristas[arista{e.de, e.a}]
		if !ok {
			t.Errorf("%s -> %s: falta en aristas", e.de, e.a)
			continue
		}
		if r.disparador != e.disparador || r.accion != e.accion || r.exigeMotivo != e.exigeMotivo || r.marca != e.marca {
			t.Errorf("%s -> %s: aristas = {%s %s motivo=%v marca=%v}, ADR 38 = {%s %s motivo=%v marca=%v}",
				e.de, e.a, r.disparador, r.accion, r.exigeMotivo, r.marca, e.disparador, e.accion, e.exigeMotivo, e.marca)
		}
	}
}
