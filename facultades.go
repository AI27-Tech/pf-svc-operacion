package operacion

import (
	"context"
	"net/http"
	"slices"

	operclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/operacion"
	"github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/roles"
	"github.com/AI27-Tech/pf-pkg-platform-go/auth"
	platformerrors "github.com/AI27-Tech/pf-pkg-platform-go/errors"
	"github.com/AI27-Tech/pf-svc-operacion/internal/alcance"
	"github.com/google/uuid"
)

// facultades fija, por operationId, qué roles pueden invocar las operaciones del
// flujo interno del panel que gatean por rol (DEC-060). Es la única copia de la
// regla: la consultan el handler HTTP y la fachada en proceso (I3). La audiencia
// Interno no se acota por tenant (DEC-129): el límite es el rol.
var facultades = map[string][]roles.Rol{
	// DEC-060 #1: el jefe_operativo arma el roster y asigna.
	"listarUnidades": {roles.RolJefeOperativo, roles.RolAdministrador},
	// DEC-142 #3: armar el roster en /roster es del jefe_operativo y del administrador.
	"crearUnidad":     {roles.RolJefeOperativo, roles.RolAdministrador},
	"obtenerUnidad":   {roles.RolJefeOperativo, roles.RolAdministrador},
	"modificarUnidad": {roles.RolJefeOperativo, roles.RolAdministrador},
	// DEC-060 #1: el jefe_operativo aprueba, y completa la fecha/hora antes de
	// aprobar (E1-T3, guarda fecha_hora_servicio_definida).
	"completarFechaHora": {roles.RolJefeOperativo, roles.RolAdministrador},
	// DEC-060 #2: la ficha técnica es del monitorista, y del supervisor/coordinador en su alcance.
	// E10 (ADR 38, DEC-148): el cliente crea la solicitud. El resto de las acciones
	// de la solicitud se agregan con su historia; el alcance fino por rol es E10-T9.
	"crearSolicitud": {roles.RolCliente},
	// cotizacion cotiza, recotiza y reprograma; el administrador hereda sus
	// facultades (DEC-148 #20). El cliente rechaza la cotización y cancela.
	"cotizarSolicitud":     {roles.RolCotizacion, roles.RolAdministrador},
	"reprogramarSolicitud": {roles.RolCotizacion, roles.RolAdministrador},
	"rechazarCotizacion":   {roles.RolCliente},
	"cancelarSolicitud":    {roles.RolCliente},
	// cotizacion confirma el pago (el administrador hereda); solo el administrador
	// lo revierte (DEC-148 #37).
	"confirmarPago": {roles.RolCotizacion, roles.RolAdministrador},
	"revertirPago":  {roles.RolAdministrador},
	"guardarFicha":  {roles.RolMonitorista, roles.RolSupervisor, roles.RolCoordinador, roles.RolAdministrador},
}

// exigirFacultad rechaza sin claims (401) y con un rol fuera de facultades[op] (403).
func exigirFacultad(ctx context.Context, op string) error {
	claims := auth.ClaimsFromContext(ctx)
	if claims == nil {
		return platformerrors.NewNoAutorizado("operacion: %s sin claims en el contexto", op)
	}
	if !slices.Contains(facultades[op], roles.Rol(claims.Rol)) {
		return platformerrors.NewProhibido("operacion: el rol %q no puede invocar %s", claims.Rol, op)
	}
	return nil
}

// exigirInterno es la guarda de las lecturas internas del viaje (histórico de
// incidencias, ficha): sin claims 401; audiencia Cliente 404, como
// obtenerAsignacion (DEC-132). No filtra por rol interno: DEC-060 reparte
// facultades de escritura, no de lectura.
func exigirInterno(ctx context.Context, op string) error {
	claims := auth.ClaimsFromContext(ctx)
	if claims == nil {
		return platformerrors.NewNoAutorizado("operacion: %s sin claims en el contexto", op)
	}
	if claims.Audiencia == auth.AudienciaCliente {
		return platformerrors.NewNoEncontrado("operacion: viaje no encontrado")
	}
	return nil
}

// exigirFacultadInterna es exigirFacultad para recursos que el Cliente no debe
// saber que existen (las unidades del roster): su audiencia recibe 404, como
// obtenerAsignacion (DEC-132); otro rol interno fuera de facultades[op], 403.
func exigirFacultadInterna(ctx context.Context, op string) error {
	if claims := auth.ClaimsFromContext(ctx); claims != nil && claims.Audiencia == auth.AudienciaCliente {
		return platformerrors.NewNoEncontrado("operacion: %s: recurso no encontrado", op)
	}
	return exigirFacultad(ctx, op)
}

// exigirAlcance es la guarda única del alcance sobre un viaje (DEC-149,
// operacion.yaml de tracer.41). La corren, antes de cualquier efecto, todas las
// rutas /api/v1/viajes/{id}/... (enAlcance) y los métodos de la fachada que
// operan sobre un viaje (I3).
//
//   - Sin claims (llamada de sistema dentro de core) o audiencia Cliente: no
//     acota; al Cliente lo acota su tenant en cada caso de uso (DEC-112).
//   - Rol sin la acción (ejecutivo; reaccion fuera de su caso; rol desconocido): 403.
//   - jefe_operativo, administrador: todos.
//   - monitorista, supervisor, coordinador: el viaje debe ser de sus clientes
//     efectivos (proyección de alcance.cambiado); si no, 404 igual que si no existiera.
//
// El alcance se busca por claims.Subject: la proyección se indexa por el
// idp_subject que publica identidad, igual al `sub` del token (DEC-149 #22).
func (m *Module) exigirAlcance(ctx context.Context, id operclient.Id, a alcance.Acceso) error {
	claims := auth.ClaimsFromContext(ctx)
	if claims == nil || claims.Audiencia == auth.AudienciaCliente {
		return nil
	}
	switch alcance.Para(claims.Rol, a) {
	case alcance.Todos:
		return nil
	case alcance.PorClientes:
		clientes, err := m.alcance.Clientes(ctx, claims.Subject)
		if err != nil {
			return err
		}
		h, err := m.viajes.HechosAlcance(ctx, id)
		if err != nil {
			return err
		}
		if !alcance.Visible(clientes, h) {
			return platformerrors.NewNoEncontrado("operacion: viaje %s no encontrado", id)
		}
		return nil
	}
	return platformerrors.NewProhibido("operacion: el rol %q no opera sobre viajes", claims.Rol)
}

// enAlcance envuelve una ruta /api/v1/viajes/{id}/... con exigirAlcance.
func (m *Module) enAlcance(a alcance.Acceso, h http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, err := uuid.Parse(r.PathValue("id"))
		if err != nil {
			escribirError(w, platformerrors.NewValidacion("operacion: id de viaje inválido"))
			return
		}
		if err := m.exigirAlcance(r.Context(), id, a); err != nil {
			escribirError(w, err)
			return
		}
		h(w, r)
	})
}
