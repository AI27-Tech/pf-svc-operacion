// Package operacion es el dueño del progreso de un viaje —su creación, su
// máquina de estados, su asignación de recursos, su bitácora y su auditoría
// probatoria— dentro de UNA sola frontera transaccional indivisible
// (operacion.md (a)). Además es dueño del roster de `unidades` (ADR 31 §5).
//
// Cuatro módulos internos (viajes, asignacion, bitacora, auditoria) bajo cuatro
// esquemas y cuatro roles, que comparten la TRANSACCIÓN, no el acoplamiento
// (operacion.md (e)). No es un error que sean cuatro esquemas en un servicio:
// repartirlos entre servicios convierte la asignación en sagas que sobrevenden.
//
// Capa de dominio. Arista síncrona saliente: una sola, hacia catalogos (y la
// restringida a identidad para revocación, DEC-051). No llama a mensajeria,
// reportes, realtime ni procesamiento: todo les llega por evento.
//
// Vive en el deployable `core`; cmd/api solo existe para arranque standalone
// (I5). Scaffolding inicial: el cableado y la estructura existen; la lógica de
// negocio real todavía no.
//
// V2 (no scaffoldeado): sin_senal.detectado/.recuperado y telemetria.alerta no
// están en el inventario del tracer (operacion.md (g), ADR 5).
package operacion

import (
	"context"
	"net/http"

	operclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/operacion"
	"github.com/AI27-Tech/pf-pkg-platform-go/auth"
	platformerrors "github.com/AI27-Tech/pf-pkg-platform-go/errors"
	"github.com/AI27-Tech/pf-pkg-platform-go/outbox"
	"github.com/AI27-Tech/pf-pkg-platform-go/pgxpool"
	"github.com/AI27-Tech/pf-pkg-platform-go/worker"
	"github.com/AI27-Tech/pf-svc-operacion/internal/viajes"
)

// Pools agrupa los cinco pools, uno por rol/esquema (operacion.md (e), ADR 33).
// Es el único servicio con esta forma. Nunca se comparte un pool entre roles,
// aunque los módulos compartan proceso y transacción (I1).
type Pools struct {
	Viajes     *pgxpool.Pool // rol op_viajes
	Asignacion *pgxpool.Pool // rol op_asignacion (la única excepción de GRANT cruzado)
	Bitacora   *pgxpool.Pool // rol op_bitacora
	Auditoria  *pgxpool.Pool // rol op_auditoria
	Roster     *pgxpool.Pool // rol op_roster (roster de unidades, ADR 33)
}

// Deps agrupa los puertos de salida ya resueltos por la raíz.
type Deps struct {
	// Aptitud es la arista a catalogos (lectura de átomos + aptitud informativa).
	Aptitud AptitudRecursos
	// Revocacion es la arista restringida a identidad (DEC-051). Opcional en el
	// esqueleto; sin ella, la autorización corre sin proyección de tombstones.
	Revocacion Revocacion
}

// Module es la instancia construida del módulo operacion.
type Module struct {
	pools  Pools
	deps   Deps
	viajes *viajes.Servicio
	// Aquí vivirán los demás casos de uso (asignacion, bitacora) al implementarse.
}

// New recibe los pools por rol y los puertos ya resueltos. No lee entorno
// (00 regla 4): la raíz cablea todo. Construye el publicador de outbox y el
// caso de uso de viajes sobre el pool del rol op_viajes.
func New(p Pools, d Deps) (*Module, error) {
	return &Module{
		pools:  p,
		deps:   d,
		viajes: viajes.NewServicio(p.Viajes, outbox.NewPublisher()),
	}, nil
}

// Routes monta las rutas HTTP del módulo bajo /api/v1/... en el mux dado.
// Scaffolding: hoy no monta rutas de negocio. Se agregan una a una conforme se
// implementen los casos de uso, y SOLO las presentes en operacion.yaml de la
// versión fijada. Las guardas y el 409 por aptitud NO se montan (DEC-055),
// aunque el contrato de tracer.3 todavía los traiga.
func (m *Module) Routes(mux *http.ServeMux) {
	// TODO(operacion): montar handlers de crearViaje, transicionarViaje,
	// asignarRecursos, bitacora, incidencias y auditoria, delegando en los
	// mismos casos de uso que Consultas() (I3).
}

// Workers devuelve los workers embebidos (ADR 29 §6): UN publisher de outbox,
// el del esquema `viajes` —que lleva todos los eventos viaje.* (ADR 35, DEC-068),
// incluidos viaje.asignado/reasignado que asignacion escribe ahí— más los dos
// consumidores durables V1 (catalogo.actualizado, sesion.revocada). asignacion,
// bitacora y auditoria no tienen outbox ni worker. Cada worker usa el rol y pool
// de su módulo; ninguno accede a varios esquemas.
//
// Los dos consumidores deduplican por evento_id EN MEMORIA (ADR 34, DEC-067): su
// efecto es en memoria (invalidar caché / proyección de revocación reconstruida
// desde snapshot, ADR 26), así que no hay tabla inbox_evento.
//
// Scaffolding: hoy devuelve lista vacía. El publisher de viajes se cablea cuando
// la raíz inyecte el adaptador de bus (bus/memory en desarrollo; broker real en
// espera de ADR 32); los consumidores, con su suscripción durable propia
// (DEC-057).
func (m *Module) Workers() []worker.Worker {
	return nil
}

// Consultas expone los casos de uso que bff consume en proceso (arista síncrona
// entrante, tabla de servicios de 00). Tipada solo con tipos de
// pf-pkg-contracts, nunca con tipos de internal/.
func (m *Module) Consultas() ConsultasAPI {
	return &consultas{m: m}
}

// ConsultasAPI es la superficie síncrona de operacion. Cada método corresponde
// 1:1 a un operationId presente en operacion.yaml de la versión fijada (00
// regla 6): crearViaje, listarViajes y obtenerViaje. Su forma es exactamente el
// puerto que bff declara (bff.OperacionPort): crear un viaje, listar por alcance
// y leer el detalle para la composición de hitos (ADR 27).
//
// obtenerViaje devuelve ViajeDetalle (no Viaje): bff compone los hitos a partir
// del detalle, que incluye las condiciones activas (E2-T2/E2-T3). Las escrituras
// internas (transición, asignación) NO entran aquí: no son arista de bff
// (bff.md (f)); se agregan a sus propios handlers cuando se implementen.
//
// SCAFFOLD: el servicio de dominio todavía no existe, así que los tres métodos
// devuelven "no implementado" en runtime. La fachada fija la FORMA tipada con el
// contrato para que core cablee el adaptador en proceso con paridad I3; la
// lógica real se agrega con los casos de uso de viajes/asignacion.
type ConsultasAPI interface {
	CrearViaje(ctx context.Context, body operclient.CrearViajeEntrada) (*operclient.Viaje, error)
	ListarViajes(ctx context.Context, params operclient.ListarViajesParams) (*operclient.PaginaViajes, error)
	ObtenerViaje(ctx context.Context, id operclient.Id) (*operclient.ViajeDetalle, error)
}

type consultas struct {
	m *Module
}

func (c *consultas) CrearViaje(ctx context.Context, body operclient.CrearViajeEntrada) (*operclient.Viaje, error) {
	// El estado inicial lo fija el rol del creador (DEC-026). El rol viaja en los
	// claims del token, que el middleware inyecta en el ctx (en core, vía bff).
	claims := auth.ClaimsFromContext(ctx)
	if claims == nil {
		return nil, platformerrors.NewNoAutorizado("operacion: crearViaje sin claims en el contexto")
	}
	return c.m.viajes.Crear(ctx, body, claims.Rol)
}

func (c *consultas) ListarViajes(ctx context.Context, params operclient.ListarViajesParams) (*operclient.PaginaViajes, error) {
	return c.m.viajes.Listar(ctx, params)
}

func (c *consultas) ObtenerViaje(ctx context.Context, id operclient.Id) (*operclient.ViajeDetalle, error) {
	return c.m.viajes.Obtener(ctx, id)
}
