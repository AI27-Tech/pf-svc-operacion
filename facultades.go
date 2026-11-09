package operacion

import (
	"context"
	"slices"

	"github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/roles"
	"github.com/AI27-Tech/pf-pkg-platform-go/auth"
	platformerrors "github.com/AI27-Tech/pf-pkg-platform-go/errors"
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
	"guardarFicha": {roles.RolMonitorista, roles.RolSupervisor, roles.RolCoordinador, roles.RolAdministrador},
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
