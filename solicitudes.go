package operacion

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"

	operclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/operacion"
	"github.com/AI27-Tech/pf-pkg-platform-go/auth"
	platformerrors "github.com/AI27-Tech/pf-pkg-platform-go/errors"
	"github.com/AI27-Tech/pf-svc-operacion/internal/solicitudes"
	"github.com/google/uuid"
)

// rutasSolicitudes monta las rutas del módulo `solicitudes` (OpenAPI de
// tracer.45) solo si la raíz inyectó el pool de op_solicitudes.
func (m *Module) rutasSolicitudes(mux *http.ServeMux, protegido func(http.Handler) http.Handler) {
	if m.solicitudes == nil {
		return
	}
	mux.Handle("POST /api/v1/solicitudes", protegido(http.HandlerFunc(m.handleCrearSolicitud)))
	mux.Handle("GET /api/v1/solicitudes", protegido(http.HandlerFunc(m.handleListarSolicitudes)))
	mux.Handle("GET /api/v1/solicitudes/{id}", protegido(http.HandlerFunc(m.handleObtenerSolicitud)))
	mux.Handle("PUT /api/v1/solicitudes/{id}/cotizacion", protegido(http.HandlerFunc(m.handleCotizarSolicitud)))
	mux.Handle("PUT /api/v1/solicitudes/{id}/fecha-hora", protegido(http.HandlerFunc(m.handleReprogramarSolicitud)))
	mux.Handle("POST /api/v1/solicitudes/{id}/rechazo-cotizacion", protegido(http.HandlerFunc(m.handleRechazarCotizacion)))
	mux.Handle("POST /api/v1/solicitudes/{id}/cancelacion", protegido(http.HandlerFunc(m.handleCancelarSolicitud)))
	mux.Handle("PUT /api/v1/solicitudes/{id}/pago", protegido(http.HandlerFunc(m.handleConfirmarPago)))
	mux.Handle("POST /api/v1/solicitudes/{id}/reversa-pago", protegido(http.HandlerFunc(m.handleRevertirPago)))
	mux.Handle("POST /api/v1/solicitudes/{id}/aprobacion", protegido(http.HandlerFunc(m.handleAprobarSolicitud)))
	mux.Handle("POST /api/v1/solicitudes/{id}/rechazo", protegido(http.HandlerFunc(m.handleRechazarSolicitud)))
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
	a, err := actorConFacultad(ctx, "crearSolicitud")
	if err != nil {
		return nil, err
	}
	return m.solicitudes.Crear(ctx, a, body)
}

// idSolicitud lee el {id} de la ruta; un id mal formado es 400.
func idSolicitud(r *http.Request) (uuid.UUID, error) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		return uuid.Nil, platformerrors.NewValidacion("operacion: id de solicitud inválido")
	}
	return id, nil
}

// responder escribe la solicitud resultante (200) o el error de la plataforma.
func responder(w http.ResponseWriter, s *operclient.Solicitud, err error) {
	if err != nil {
		escribirError(w, err)
		return
	}
	escribirJSON(w, http.StatusOK, s)
}

// decodificarOpcional lee un cuerpo que el contrato deja opcional: en blanco es
// válido (entrada vacía); si trae algo, es estricto.
func decodificarOpcional(r *http.Request, v any) error {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return platformerrors.NewValidacion("operacion: cuerpo de la petición inválido: %v", err)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return platformerrors.NewValidacion("operacion: cuerpo de la petición inválido: %v", err)
	}
	return nil
}

// handleCotizarSolicitud atiende PUT /api/v1/solicitudes/{id}/cotizacion.
func (m *Module) handleCotizarSolicitud(w http.ResponseWriter, r *http.Request) {
	id, err := idSolicitud(r)
	if err != nil {
		escribirError(w, err)
		return
	}
	var body operclient.CotizacionEntrada
	if err := decodificarEstricto(r, &body); err != nil {
		escribirError(w, err)
		return
	}
	s, err := m.cotizarSolicitud(r.Context(), id, body)
	responder(w, s, err)
}

// handleReprogramarSolicitud atiende PUT /api/v1/solicitudes/{id}/fecha-hora.
func (m *Module) handleReprogramarSolicitud(w http.ResponseWriter, r *http.Request) {
	id, err := idSolicitud(r)
	if err != nil {
		escribirError(w, err)
		return
	}
	var body operclient.ReprogramacionEntrada
	if err := decodificarEstricto(r, &body); err != nil {
		escribirError(w, err)
		return
	}
	s, err := m.reprogramarSolicitud(r.Context(), id, body)
	responder(w, s, err)
}

// handleRechazarCotizacion atiende POST /api/v1/solicitudes/{id}/rechazo-cotizacion.
func (m *Module) handleRechazarCotizacion(w http.ResponseWriter, r *http.Request) {
	id, err := idSolicitud(r)
	if err != nil {
		escribirError(w, err)
		return
	}
	var body operclient.AccionClienteEntrada
	if err := decodificarOpcional(r, &body); err != nil {
		escribirError(w, err)
		return
	}
	s, err := m.rechazarCotizacion(r.Context(), id, body)
	responder(w, s, err)
}

// handleCancelarSolicitud atiende POST /api/v1/solicitudes/{id}/cancelacion.
func (m *Module) handleCancelarSolicitud(w http.ResponseWriter, r *http.Request) {
	id, err := idSolicitud(r)
	if err != nil {
		escribirError(w, err)
		return
	}
	var body operclient.AccionClienteEntrada
	if err := decodificarOpcional(r, &body); err != nil {
		escribirError(w, err)
		return
	}
	s, err := m.cancelarSolicitud(r.Context(), id, body)
	responder(w, s, err)
}

// Las fachadas aplican la facultad del comando y delegan en el módulo; las
// comparten HTTP y la fachada en proceso (I3).

func (m *Module) cotizarSolicitud(ctx context.Context, id operclient.Id, body operclient.CotizacionEntrada) (*operclient.Solicitud, error) {
	a, err := actorConFacultad(ctx, "cotizarSolicitud")
	if err != nil {
		return nil, err
	}
	return m.solicitudes.Cotizar(ctx, a, id, body)
}

func (m *Module) reprogramarSolicitud(ctx context.Context, id operclient.Id, body operclient.ReprogramacionEntrada) (*operclient.Solicitud, error) {
	a, err := actorConFacultad(ctx, "reprogramarSolicitud")
	if err != nil {
		return nil, err
	}
	return m.solicitudes.Reprogramar(ctx, a, id, body)
}

func (m *Module) rechazarCotizacion(ctx context.Context, id operclient.Id, body operclient.AccionClienteEntrada) (*operclient.Solicitud, error) {
	a, err := actorConFacultad(ctx, "rechazarCotizacion")
	if err != nil {
		return nil, err
	}
	return m.solicitudes.RechazarCotizacion(ctx, a, id, body)
}

func (m *Module) cancelarSolicitud(ctx context.Context, id operclient.Id, body operclient.AccionClienteEntrada) (*operclient.Solicitud, error) {
	a, err := actorConFacultad(ctx, "cancelarSolicitud")
	if err != nil {
		return nil, err
	}
	return m.solicitudes.Cancelar(ctx, a, id, body)
}

// actorConFacultad exige la facultad del comando y arma el actor.
func actorConFacultad(ctx context.Context, op string) (solicitudes.Actor, error) {
	if err := exigirFacultad(ctx, op); err != nil {
		return solicitudes.Actor{}, err
	}
	return actorDe(ctx, op)
}

// handleConfirmarPago atiende PUT /api/v1/solicitudes/{id}/pago.
func (m *Module) handleConfirmarPago(w http.ResponseWriter, r *http.Request) {
	id, err := idSolicitud(r)
	if err != nil {
		escribirError(w, err)
		return
	}
	var body operclient.PagoEntrada
	if err := decodificarEstricto(r, &body); err != nil {
		escribirError(w, err)
		return
	}
	s, err := m.confirmarPago(r.Context(), id, body)
	responder(w, s, err)
}

// handleRevertirPago atiende POST /api/v1/solicitudes/{id}/reversa-pago.
func (m *Module) handleRevertirPago(w http.ResponseWriter, r *http.Request) {
	id, err := idSolicitud(r)
	if err != nil {
		escribirError(w, err)
		return
	}
	var body operclient.MotivoEntrada
	if err := decodificarEstricto(r, &body); err != nil {
		escribirError(w, err)
		return
	}
	s, err := m.revertirPago(r.Context(), id, body)
	responder(w, s, err)
}

func (m *Module) confirmarPago(ctx context.Context, id operclient.Id, body operclient.PagoEntrada) (*operclient.Solicitud, error) {
	a, err := actorConFacultad(ctx, "confirmarPago")
	if err != nil {
		return nil, err
	}
	return m.solicitudes.ConfirmarPago(ctx, a, id, body)
}

func (m *Module) revertirPago(ctx context.Context, id operclient.Id, body operclient.MotivoEntrada) (*operclient.Solicitud, error) {
	a, err := actorConFacultad(ctx, "revertirPago")
	if err != nil {
		return nil, err
	}
	return m.solicitudes.RevertirPago(ctx, a, id, body)
}

// handleAprobarSolicitud atiende POST /api/v1/solicitudes/{id}/aprobacion (sin cuerpo).
func (m *Module) handleAprobarSolicitud(w http.ResponseWriter, r *http.Request) {
	id, err := idSolicitud(r)
	if err != nil {
		escribirError(w, err)
		return
	}
	s, err := m.aprobarSolicitud(r.Context(), id)
	responder(w, s, err)
}

// handleRechazarSolicitud atiende POST /api/v1/solicitudes/{id}/rechazo.
func (m *Module) handleRechazarSolicitud(w http.ResponseWriter, r *http.Request) {
	id, err := idSolicitud(r)
	if err != nil {
		escribirError(w, err)
		return
	}
	var body operclient.MotivoEntrada
	if err := decodificarEstricto(r, &body); err != nil {
		escribirError(w, err)
		return
	}
	s, err := m.rechazarSolicitud(r.Context(), id, body)
	responder(w, s, err)
}

func (m *Module) aprobarSolicitud(ctx context.Context, id operclient.Id) (*operclient.Solicitud, error) {
	a, err := actorConFacultad(ctx, "aprobarSolicitud")
	if err != nil {
		return nil, err
	}
	return m.solicitudes.Aprobar(ctx, a, id)
}

func (m *Module) rechazarSolicitud(ctx context.Context, id operclient.Id, body operclient.MotivoEntrada) (*operclient.Solicitud, error) {
	a, err := actorConFacultad(ctx, "rechazarSolicitud")
	if err != nil {
		return nil, err
	}
	return m.solicitudes.Rechazar(ctx, a, id, body)
}

// handleListarSolicitudes atiende GET /api/v1/solicitudes?estado=&cursor=.
func (m *Module) handleListarSolicitudes(w http.ResponseWriter, r *http.Request) {
	var params operclient.ListarSolicitudesParams
	if v := r.URL.Query().Get("estado"); v != "" {
		e := operclient.EstadoSolicitud(v)
		params.Estado = &e
	}
	if v := r.URL.Query().Get("cursor"); v != "" {
		params.Cursor = &v
	}
	p, err := m.listarSolicitudes(r.Context(), params)
	if err != nil {
		escribirError(w, err)
		return
	}
	escribirJSON(w, http.StatusOK, p)
}

// handleObtenerSolicitud atiende GET /api/v1/solicitudes/{id}.
func (m *Module) handleObtenerSolicitud(w http.ResponseWriter, r *http.Request) {
	id, err := idSolicitud(r)
	if err != nil {
		escribirError(w, err)
		return
	}
	s, err := m.obtenerSolicitud(r.Context(), id)
	responder(w, s, err)
}

func (m *Module) listarSolicitudes(ctx context.Context, params operclient.ListarSolicitudesParams) (*operclient.PaginaSolicitudes, error) {
	a, err := actorConFacultad(ctx, "listarSolicitudes")
	if err != nil {
		return nil, err
	}
	return m.solicitudes.Listar(ctx, a, params.Estado, params.Cursor)
}

func (m *Module) obtenerSolicitud(ctx context.Context, id operclient.Id) (*operclient.Solicitud, error) {
	a, err := actorConFacultad(ctx, "obtenerSolicitud")
	if err != nil {
		return nil, err
	}
	return m.solicitudes.Obtener(ctx, a, id)
}
