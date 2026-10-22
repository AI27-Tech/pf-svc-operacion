package viajes_test

import (
	"context"
	"testing"

	operclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/operacion"
	platformerrors "github.com/AI27-Tech/pf-pkg-platform-go/errors"
	"github.com/AI27-Tech/pf-svc-operacion/internal/dbtest"
)

// estadoDe lee el viaje.estado actual por la vía de administración. Sirve para
// comprobar el invariante central de E1-T6: abrir/resolver una incidencia NO
// cambia el estado de progreso del viaje (§C.2, P15).
func estadoDe(t *testing.T, ctx context.Context, id operclient.Id) string {
	t.Helper()
	admin, cerrar := dbtest.AdminConn(t, ctx)
	defer cerrar()
	var estado string
	if err := admin.QueryRow(ctx,
		"SELECT estado FROM viajes.viaje WHERE id=$1", id,
	).Scan(&estado); err != nil {
		t.Fatalf("leer estado del viaje: %v", err)
	}
	return estado
}

// TestAbrirIncidenciaNoCambiaEstado — abrir una incidencia inserta la condición
// INCIDENCIA, publica viaje.condicion_abierta y NO cambia viaje.estado (se lee el
// estado antes y después). Devuelve la incidencia en estado en_atencion.
func TestAbrirIncidenciaNoCambiaEstado(t *testing.T) {
	ctx := context.Background()
	svc := prepara(t, ctx)

	v := crearInterno(t, ctx, svc)
	estadoAntes := estadoDe(t, ctx, v.Id)

	inc, err := svc.AbrirIncidencia(ctx, v.Id,
		operclient.IncidenciaEntrada{TipoInterno: "robo_tentativa", Descripcion: ptrS("intento en caseta")}, "operador")
	if err != nil {
		t.Fatalf("AbrirIncidencia: %v", err)
	}
	if inc.Estado != operclient.IncidenciaEstadoEnAtencion {
		t.Errorf("estado de la incidencia: got %q, want en_atencion", inc.Estado)
	}
	if inc.TipoInterno != "robo_tentativa" {
		t.Errorf("tipo_interno: got %q, want robo_tentativa", inc.TipoInterno)
	}
	if inc.ViajeId != v.Id {
		t.Errorf("viaje_id: got %v, want %v", inc.ViajeId, v.Id)
	}

	estadoDespues := estadoDe(t, ctx, v.Id)
	if estadoAntes != estadoDespues {
		t.Errorf("abrir incidencia cambió el estado: antes %q, después %q", estadoAntes, estadoDespues)
	}
	if n := contarEventos(t, ctx, "viaje.condicion_abierta", v.Id.String()); n != 1 {
		t.Errorf("viaje.condicion_abierta en outbox: got %d, want 1", n)
	}

	// La condición quedó abierta (cerrada_en NULL) y aparece en el detalle del viaje.
	det, err := svc.Obtener(ctx, v.Id)
	if err != nil {
		t.Fatalf("Obtener: %v", err)
	}
	if len(det.CondicionesActivas) != 1 || det.CondicionesActivas[0].Tipo != operclient.INCIDENCIA {
		t.Errorf("condiciones activas: got %+v, want una INCIDENCIA", det.CondicionesActivas)
	}
}

// TestAbrirIncidenciaRolReaccionRechazado — reacción NO puede abrir incidencias
// (DEC-028): su visibilidad es solo del caso alertado. Rechazo por rol, sin tocar
// la base (ninguna condición, ningún evento).
func TestAbrirIncidenciaRolReaccionRechazado(t *testing.T) {
	ctx := context.Background()
	svc := prepara(t, ctx)

	v := crearInterno(t, ctx, svc)
	if _, err := svc.AbrirIncidencia(ctx, v.Id,
		operclient.IncidenciaEntrada{TipoInterno: "x"}, "reaccion"); err == nil {
		t.Error("abrir incidencia por reacción debió rechazarse (DEC-028)")
	}
	if n := contarEventos(t, ctx, "viaje.condicion_abierta", v.Id.String()); n != 0 {
		t.Errorf("no debió publicarse viaje.condicion_abierta: got %d, want 0", n)
	}
}

// TestResolverIncidenciaSoloSupervisorOAdmin — un operador puede ABRIR pero NO
// resolver; resolver es SOLO de supervisor o administrador. El intento del
// operador se rechaza por rol y la condición sigue abierta.
func TestResolverIncidenciaSoloSupervisorOAdmin(t *testing.T) {
	ctx := context.Background()
	svc := prepara(t, ctx)

	v := crearInterno(t, ctx, svc)
	inc, err := svc.AbrirIncidencia(ctx, v.Id,
		operclient.IncidenciaEntrada{TipoInterno: "falla_mecanica"}, "operador")
	if err != nil {
		t.Fatalf("AbrirIncidencia: %v", err)
	}

	// Operador NO puede resolver: rechazo por rol.
	if _, err := svc.ResolverIncidencia(ctx, v.Id, inc.Id,
		operclient.ResolucionIncidenciaEntrada{Resolucion: "atendida"}, "operador"); err == nil {
		t.Error("resolver por un operador debió rechazarse (solo supervisor/administrador)")
	}
	// Sigue abierta: ningún viaje.condicion_cerrada.
	if n := contarEventos(t, ctx, "viaje.condicion_cerrada", v.Id.String()); n != 0 {
		t.Errorf("no debió publicarse viaje.condicion_cerrada tras rechazo: got %d, want 0", n)
	}

	// Supervisor SÍ puede resolver: cierra la condición, marca resuelta_en y
	// publica viaje.condicion_cerrada. Sigue sin cambiar el estado.
	estadoAntes := estadoDe(t, ctx, v.Id)
	out, err := svc.ResolverIncidencia(ctx, v.Id, inc.Id,
		operclient.ResolucionIncidenciaEntrada{Resolucion: "grúa en sitio"}, "supervisor")
	if err != nil {
		t.Fatalf("ResolverIncidencia (supervisor): %v", err)
	}
	if out.Estado != operclient.IncidenciaEstadoResuelta {
		t.Errorf("estado tras resolver: got %q, want resuelta", out.Estado)
	}
	if out.ResueltaEn == nil {
		t.Error("resuelta_en debió fijarse al resolver")
	}
	if estadoDespues := estadoDe(t, ctx, v.Id); estadoAntes != estadoDespues {
		t.Errorf("resolver incidencia cambió el estado: antes %q, después %q", estadoAntes, estadoDespues)
	}
	if n := contarEventos(t, ctx, "viaje.condicion_cerrada", v.Id.String()); n != 1 {
		t.Errorf("viaje.condicion_cerrada en outbox: got %d, want 1", n)
	}

	// Ya no hay condición abierta en el detalle.
	det, err := svc.Obtener(ctx, v.Id)
	if err != nil {
		t.Fatalf("Obtener: %v", err)
	}
	if len(det.CondicionesActivas) != 0 {
		t.Errorf("condiciones activas tras resolver: got %d, want 0", len(det.CondicionesActivas))
	}
}

// TestDosIncidenciasAbiertasConflicto — el índice único parcial
// `una_condicion_abierta_por_tipo` impide dos INCIDENCIA abiertas del mismo
// viaje. La segunda apertura se traduce a Conflicto (409), no a un 500.
func TestDosIncidenciasAbiertasConflicto(t *testing.T) {
	ctx := context.Background()
	svc := prepara(t, ctx)

	v := crearInterno(t, ctx, svc)
	if _, err := svc.AbrirIncidencia(ctx, v.Id,
		operclient.IncidenciaEntrada{TipoInterno: "robo"}, "operador"); err != nil {
		t.Fatalf("primera AbrirIncidencia: %v", err)
	}
	_, err := svc.AbrirIncidencia(ctx, v.Id,
		operclient.IncidenciaEntrada{TipoInterno: "robo"}, "operador")
	if err == nil {
		t.Fatal("abrir una segunda incidencia debió fallar por el índice único parcial")
	}
	if platformerrors.AsCategory(err) != platformerrors.CategoryConflicto {
		t.Errorf("se esperaba un Conflicto (409), se obtuvo: %v", err)
	}
	// Solo una condición abierta y un solo evento de apertura.
	if n := contarEventos(t, ctx, "viaje.condicion_abierta", v.Id.String()); n != 1 {
		t.Errorf("viaje.condicion_abierta en outbox: got %d, want 1", n)
	}
}

// TestCoexistenciaIncidenciaYEstadia — un viaje puede tener a la vez una
// incidencia abierta y una estadía abierta (ambas condiciones ortogonales
// válidas); ninguna cambia el estado.
func TestCoexistenciaIncidenciaYEstadia(t *testing.T) {
	ctx := context.Background()
	svc := prepara(t, ctx)

	v := crearInterno(t, ctx, svc)
	estadoAntes := estadoDe(t, ctx, v.Id)

	if _, err := svc.AbrirIncidencia(ctx, v.Id,
		operclient.IncidenciaEntrada{TipoInterno: "bloqueo_vial"}, "operador"); err != nil {
		t.Fatalf("AbrirIncidencia: %v", err)
	}
	est, err := svc.AbrirEstadia(ctx, v.Id,
		operclient.EstadiaEntrada{Motivo: operclient.Descanso, EjeAutorizacion: operclient.Autorizada})
	if err != nil {
		t.Fatalf("AbrirEstadia: %v", err)
	}
	if est.Cierre != nil {
		t.Error("una estadía recién abierta no debe tener cierre")
	}

	// Ambas coexisten; el estado no cambió por ninguna.
	if estadoDespues := estadoDe(t, ctx, v.Id); estadoAntes != estadoDespues {
		t.Errorf("abrir incidencia+estadía cambió el estado: antes %q, después %q", estadoAntes, estadoDespues)
	}

	// La incidencia aparece como condición activa; la estadía aparece en su propio
	// listado. Son dimensiones ortogonales distintas.
	det, err := svc.Obtener(ctx, v.Id)
	if err != nil {
		t.Fatalf("Obtener: %v", err)
	}
	if len(det.CondicionesActivas) != 1 || det.CondicionesActivas[0].Tipo != operclient.INCIDENCIA {
		t.Errorf("condiciones activas: got %+v, want una INCIDENCIA", det.CondicionesActivas)
	}
	pag, err := svc.ListarEstadias(ctx, v.Id, operclient.ListarEstadiasParams{})
	if err != nil {
		t.Fatalf("ListarEstadias: %v", err)
	}
	if len(pag.Items) != 1 {
		t.Errorf("estadías del viaje: got %d, want 1", len(pag.Items))
	}
}

// TestAbrirIncidenciaTipoInternoObligatorio — tipo_interno vacío se rechaza por
// validación, sin tocar la base.
func TestAbrirIncidenciaTipoInternoObligatorio(t *testing.T) {
	ctx := context.Background()
	svc := prepara(t, ctx)

	v := crearInterno(t, ctx, svc)
	if _, err := svc.AbrirIncidencia(ctx, v.Id,
		operclient.IncidenciaEntrada{TipoInterno: "   "}, "operador"); err == nil {
		t.Error("abrir incidencia sin tipo_interno debió fallar")
	}
}
