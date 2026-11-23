package operacion_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	operclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/operacion"
	"github.com/AI27-Tech/pf-pkg-platform-go/auth"
	platformerrors "github.com/AI27-Tech/pf-pkg-platform-go/errors"
	"github.com/AI27-Tech/pf-svc-operacion"
	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"
)

// TestConsultasDeSolicitudesRecorreLosDoceMetodos (I3): los 12 métodos de
// ConsultasAPI hacen lo mismo que las rutas HTTP, con los claims del ctx como
// actor, y terminan en el mismo estado. Es el ciclo completo de una solicitud.
func TestConsultasDeSolicitudesRecorreLosDoceMetodos(t *testing.T) {
	ctx := context.Background()
	tenant := uuid.New()
	m := moduloCompletoCon(t, ctx, operacion.Deps{Verificador: autenticadorRoles{tenant: tenant.String()}, Atomos: catalogo})
	api := m.Consultas()
	como := func(rol string) context.Context {
		c := &auth.Claims{Subject: "00000000-0000-4000-8000-0000000000" + map[string]string{"cliente": "01", "cotizacion": "02", "jefe_operativo": "03", "administrador": "04"}[rol], Rol: rol, Audiencia: auth.AudienciaInterno}
		if rol == "cliente" {
			c.Audiencia, c.TenantID = auth.AudienciaCliente, tenant.String()
		}
		return auth.ContextWithClaims(ctx, c)
	}
	futuro := time.Now().Add(72 * time.Hour)
	hoy := openapi_types.Date{Time: time.Now().UTC().Truncate(24 * time.Hour)}
	estado := func(s *operclient.Solicitud, err error) string {
		t.Helper()
		if err != nil {
			t.Fatalf("falló: %v", err)
		}
		return string(s.Estado)
	}

	s, err := api.CrearSolicitud(como("cliente"), operclient.CrearSolicitudEntrada{Origen: "CDMX", Destinos: []operclient.Destino{{Nombre: "GDL"}}})
	if got := estado(s, err); got != "SOLICITADO" {
		t.Fatalf("crear: %s", got)
	}
	id := s.Id
	if got := estado(api.CotizarSolicitud(como("cotizacion"), id, operclient.CotizacionEntrada{Monto: "100", Moneda: "MXN", FechaHoraServicio: futuro})); got != "COTIZADO" {
		t.Errorf("cotizar: %s", got)
	}
	if got := estado(api.ConfirmarPago(como("cotizacion"), id, operclient.PagoEntrada{FechaPago: hoy})); got != "PAGADO" {
		t.Errorf("confirmar pago: %s", got)
	}
	if got := estado(api.ReprogramarSolicitud(como("cotizacion"), id, operclient.ReprogramacionEntrada{FechaHoraServicio: futuro.Add(24 * time.Hour), Motivo: "m"})); got != "PAGADO" {
		t.Errorf("reprogramar: %s", got)
	}
	if got := estado(api.RevertirPago(como("administrador"), id, operclient.MotivoEntrada{Motivo: "error"})); got != "COTIZADO" {
		t.Errorf("revertir pago: %s", got)
	}
	if got := estado(api.ConfirmarPago(como("cotizacion"), id, operclient.PagoEntrada{FechaPago: hoy})); got != "PAGADO" {
		t.Errorf("confirmar pago otra vez: %s", got)
	}
	if p, err := api.ListarSolicitudes(como("jefe_operativo"), operclient.ListarSolicitudesParams{}); err != nil || len(p.Items) != 1 || p.Items[0].Id != id {
		t.Errorf("bandeja del jefe: %v %+v", err, p)
	}
	if got := estado(api.AprobarSolicitud(como("jefe_operativo"), id)); got != "APROBADO" {
		t.Errorf("aprobar: %s", got)
	}
	if viajesDe(t, ctx, id) != 1 {
		t.Error("la aprobación por la fachada no creó el viaje")
	}
	if got := estado(api.ObtenerSolicitud(como("cliente"), id)); got != "APROBADO" {
		t.Errorf("obtener: %s", got)
	}
	h, err := api.ListarHistoricoSolicitud(como("administrador"), id)
	if err != nil {
		t.Fatalf("histórico: %v", err)
	}
	var acciones []string
	for _, it := range h.Items {
		acciones = append(acciones, it.Accion)
	}
	if want := "[creada cotizada pago_confirmado reprogramada pago_revertido pago_confirmado aprobada]"; fmt.Sprint(acciones) != want {
		t.Errorf("histórico = %v, quería %s", acciones, want)
	}

	// Las otras tres salidas del cliente y el rechazo de AI27.
	nueva := func() uuid.UUID {
		s, err := api.CrearSolicitud(como("cliente"), operclient.CrearSolicitudEntrada{Origen: "A", Destinos: []operclient.Destino{{Nombre: "B"}}})
		if err != nil {
			t.Fatal(err)
		}
		return s.Id
	}
	cotizar := func(id uuid.UUID) {
		if _, err := api.CotizarSolicitud(como("cotizacion"), id, operclient.CotizacionEntrada{Monto: "0", Moneda: "MXN", FechaHoraServicio: futuro}); err != nil {
			t.Fatal(err)
		}
	}
	a := nueva()
	cotizar(a)
	if got := estado(api.RechazarCotizacion(como("cliente"), a, operclient.AccionClienteEntrada{})); got != "RECHAZADO_POR_CLIENTE" {
		t.Errorf("rechazar cotización: %s", got)
	}
	b := nueva()
	if got := estado(api.CancelarSolicitud(como("cliente"), b, operclient.AccionClienteEntrada{})); got != "CANCELADO_POR_CLIENTE" {
		t.Errorf("cancelar: %s", got)
	}
	c := nueva()
	cotizar(c)
	if _, err := api.ConfirmarPago(como("cotizacion"), c, operclient.PagoEntrada{FechaPago: hoy}); err != nil {
		t.Fatal(err)
	}
	if got := estado(api.RechazarSolicitud(como("jefe_operativo"), c, operclient.MotivoEntrada{Motivo: "sin escolta"})); got != "RECHAZADO_POR_AI27" {
		t.Errorf("rechazar solicitud: %s", got)
	}
	// Sin la facultad, el mismo 403 que la ruta.
	if _, err := api.AprobarSolicitud(como("cotizacion"), id); platformerrors.AsCategory(err) != platformerrors.CategoryProhibido {
		t.Errorf("aprobar como cotizacion: %v", err)
	}
	// Sin claims: 401.
	if _, err := api.ListarSolicitudes(ctx, operclient.ListarSolicitudesParams{}); platformerrors.AsCategory(err) != platformerrors.CategoryNoAutorizado {
		t.Errorf("sin claims: %v", err)
	}
}

// TestConsultasDeSolicitudesSinPool: sin el pool de op_solicitudes la fachada
// responde 503 en lugar de fallar con un puntero nulo.
func TestConsultasDeSolicitudesSinPool(t *testing.T) {
	m, err := operacion.New(operacion.Pools{}, operacion.Deps{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := auth.ContextWithClaims(context.Background(), &auth.Claims{Subject: "x", Rol: "cotizacion", Audiencia: auth.AudienciaInterno})
	if _, err := m.Consultas().ListarSolicitudes(ctx, operclient.ListarSolicitudesParams{}); platformerrors.AsCategory(err) != platformerrors.CategoryDegradado {
		t.Errorf("esperaba 503 (degradado), obtuve %v", err)
	}
}
