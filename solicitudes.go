package operacion

import (
	"context"
	"net/http"

	operclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/operacion"
	"github.com/AI27-Tech/pf-pkg-platform-go/auth"
	platformerrors "github.com/AI27-Tech/pf-pkg-platform-go/errors"
	"github.com/AI27-Tech/pf-svc-operacion/internal/solicitudes"
)

// rutasSolicitudes monta las rutas del módulo `solicitudes` (OpenAPI de
// tracer.45) solo si la raíz inyectó el pool de op_solicitudes.
func (m *Module) rutasSolicitudes(mux *http.ServeMux, protegido func(http.Handler) http.Handler) {
	if m.solicitudes == nil {
		return
	}
	mux.Handle("POST /api/v1/solicitudes", protegido(http.HandlerFunc(m.handleCrearSolicitud)))
}

// actorDe arma el actor de un comando desde los claims. Sin claims es 401: una
// solicitud siempre la origina alguien con rol (el vencimiento del sistema no
// pasa por aquí).
func actorDe(ctx context.Context, op string) (solicitudes.Actor, error) {
	claims := auth.ClaimsFromContext(ctx)
	if claims == nil {
		return solicitudes.Actor{}, platformerrors.NewNoAutorizado("operacion: %s sin claims en el contexto", op)
	}
	return solicitudes.Actor{Subject: claims.Subject, Rol: claims.Rol, TenantID: claims.TenantID}, nil
}

// handleCrearSolicitud atiende POST /api/v1/solicitudes. El cuerpo es estricto
// (additionalProperties: false): un `cliente_id` en el cuerpo es 400; la
// organización siempre sale del token.
func (m *Module) handleCrearSolicitud(w http.ResponseWriter, r *http.Request) {
	var body operclient.CrearSolicitudEntrada
	if err := decodificarEstricto(r, &body); err != nil {
		escribirError(w, err)
		return
	}
	s, err := m.crearSolicitud(r.Context(), body)
	if err != nil {
		escribirError(w, err)
		return
	}
	escribirJSON(w, http.StatusCreated, s)
}

// crearSolicitud aplica la facultad (solo el cliente) y delega en el módulo. Lo
// comparten HTTP y la fachada en proceso (I3).
func (m *Module) crearSolicitud(ctx context.Context, body operclient.CrearSolicitudEntrada) (*operclient.Solicitud, error) {
	if err := exigirFacultad(ctx, "crearSolicitud"); err != nil {
		return nil, err
	}
	a, err := actorDe(ctx, "crearSolicitud")
	if err != nil {
		return nil, err
	}
	return m.solicitudes.Crear(ctx, a, body)
}
