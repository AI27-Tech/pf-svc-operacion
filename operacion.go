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
	"encoding/json"
	"net/http"

	operclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/operacion"
	"github.com/AI27-Tech/pf-pkg-platform-go/auth"
	"github.com/AI27-Tech/pf-pkg-platform-go/bus"
	platformerrors "github.com/AI27-Tech/pf-pkg-platform-go/errors"
	"github.com/AI27-Tech/pf-pkg-platform-go/outbox"
	"github.com/AI27-Tech/pf-pkg-platform-go/pgxpool"
	"github.com/AI27-Tech/pf-pkg-platform-go/worker"
	"github.com/AI27-Tech/pf-svc-operacion/internal/asignacion"
	"github.com/AI27-Tech/pf-svc-operacion/internal/viajes"
	"github.com/google/uuid"
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
	// Verificador autentica las rutas protegidas que operacion monta por sí misma
	// (como bff): la interfaz común Routes(mux) no lleva verificador, así que el
	// módulo se autentica solo. Si es nil, no se monta ninguna ruta protegida (no
	// se crea un endpoint sin autenticar). La raíz lo inyecta (el autenticador real
	// o el stub de desarrollo).
	Verificador auth.Authenticator
	// BusPublisher es el publicador del bus que usa el Relay de outbox para
	// drenar los eventos viaje.* del esquema `viajes` (ADR 35, el único outbox de
	// operacion). Agnóstico al transporte (bus/memory en desarrollo; broker en
	// espera de ADR 32). Si es nil, Workers() no arranca el relay: el módulo
	// escribe eventos a la tabla pero no los publica (arranque standalone sin bus).
	BusPublisher bus.Publisher
}

// Module es la instancia construida del módulo operacion.
type Module struct {
	pools      Pools
	deps       Deps
	viajes     *viajes.Servicio
	asignacion *asignacion.Servicio
	// relay drena viajes.outbox al bus. nil si Deps.BusPublisher no se inyectó
	// (arranque standalone sin bus); en ese caso Workers() no lo arranca.
	relay worker.Worker
	// Aquí vivirán los demás casos de uso (bitacora) al implementarse.
}

// New recibe los pools por rol y los puertos ya resueltos. No lee entorno
// (00 regla 4): la raíz cablea todo. Construye el publicador de outbox y el
// caso de uso de viajes sobre el pool del rol op_viajes.
func New(p Pools, d Deps) (*Module, error) {
	pub := outbox.NewPublisher()
	m := &Module{
		pools:      p,
		deps:       d,
		viajes:     viajes.NewServicio(p.Viajes, pub),
		asignacion: asignacion.NewServicio(p.Asignacion, pub),
	}
	// El único outbox de operacion es el del esquema `viajes` (ADR 35, DEC-068):
	// lleva todos los eventos viaje.* (incluidos los que asignacion escribe ahí
	// por la excepción de GRANT). Un solo relay sobre el pool op_viajes lo drena.
	// Corre con search_path=viajes, igual que el Publisher que ya lo escribe.
	if d.BusPublisher != nil {
		m.relay = outbox.NewRelay("outbox-relay:viajes", p.Viajes, d.BusPublisher, outbox.RelayOpciones{})
	}
	return m, nil
}

// Routes monta las rutas HTTP del módulo bajo /api/v1/... en el mux dado.
//
// Monta SOLO las rutas internas que bff no proxea (hoy: transicionar un viaje).
// crear/listar/obtener NO se montan aquí: los sirve bff vía Consultas() en core,
// y montarlos también provocaría una colisión de patrón en el mux.
//
// operacion se autentica a sí misma (como bff): la interfaz común Routes(mux) no
// lleva verificador, así que se envuelve cada ruta protegida con el verificador
// de Deps. Sin verificador no se monta nada (nunca un endpoint sin autenticar).
// La consulta de revocación va nil por ahora (la proyección de tombstones de
// operacion es DEC-057/058, aparte): se verifica la firma del token, no el
// tombstone.
func (m *Module) Routes(mux *http.ServeMux) {
	if m.deps.Verificador == nil {
		return
	}
	protegido := auth.Middleware(m.deps.Verificador, nil)
	mux.Handle("POST /api/v1/viajes/{id}/transiciones", protegido(http.HandlerFunc(m.handleTransicionar)))
	mux.Handle("POST /api/v1/viajes/{id}/asignacion", protegido(http.HandlerFunc(m.handleAsignar)))
	mux.Handle("PATCH /api/v1/viajes/{id}/asignacion", protegido(http.HandlerFunc(m.handleReasignar)))
}

// handleTransicionar atiende POST /api/v1/viajes/{id}/transiciones: lee el estado
// destino y la causa del cuerpo (TransicionEntrada) y delega en el caso de uso de
// viajes, que es la única autoridad de transición.
func (m *Module) handleTransicionar(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		escribirError(w, platformerrors.NewValidacion("operacion: id de viaje inválido"))
		return
	}
	var body operclient.TransicionEntrada
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		escribirError(w, platformerrors.NewValidacion("operacion: cuerpo de la petición inválido: %v", err))
		return
	}
	v, err := m.viajes.Transicionar(r.Context(), id, body)
	if err != nil {
		escribirError(w, err)
		return
	}
	escribirJSON(w, http.StatusOK, v)
}

// handleAsignar atiende POST /api/v1/viajes/{id}/asignacion: lee las unidades de
// la escolta (AsignacionEntrada) y delega en el caso de uso de asignación, que
// reserva + transiciona + audita en una transacción indivisible.
func (m *Module) handleAsignar(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		escribirError(w, platformerrors.NewValidacion("operacion: id de viaje inválido"))
		return
	}
	var body operclient.AsignacionEntrada
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		escribirError(w, platformerrors.NewValidacion("operacion: cuerpo de la petición inválido: %v", err))
		return
	}
	var actor string
	if claims := auth.ClaimsFromContext(r.Context()); claims != nil {
		actor = claims.Subject
	}
	a, err := m.asignacion.Asignar(r.Context(), id, body, actor)
	if err != nil {
		escribirError(w, err)
		return
	}
	escribirJSON(w, http.StatusOK, a)
}

// handleReasignar atiende PATCH /api/v1/viajes/{id}/asignacion: lee la escolta
// completa y el motivo (ReasignacionEntrada) y delega en el caso de uso, que
// libera la asignación vigente + reserva la nueva + audita en una tx indivisible.
func (m *Module) handleReasignar(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		escribirError(w, platformerrors.NewValidacion("operacion: id de viaje inválido"))
		return
	}
	var body operclient.ReasignacionEntrada
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		escribirError(w, platformerrors.NewValidacion("operacion: cuerpo de la petición inválido: %v", err))
		return
	}
	var actor string
	if claims := auth.ClaimsFromContext(r.Context()); claims != nil {
		actor = claims.Subject
	}
	a, err := m.asignacion.Reasignar(r.Context(), id, body, actor)
	if err != nil {
		escribirError(w, err)
		return
	}
	escribirJSON(w, http.StatusOK, a)
}

// escribirJSON serializa v como respuesta JSON con el status dado.
func escribirJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// escribirError traduce un error de la taxonomía de la plataforma al Error del
// contrato y a su status HTTP, reusando el mapeo de la plataforma.
func escribirError(w http.ResponseWriter, err error) {
	cat := platformerrors.AsCategory(err)
	escribirJSON(w, cat.HTTPStatus(), operclient.Error{Codigo: cat.String(), Mensaje: err.Error()})
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
// Hoy arranca el relay de viajes.outbox cuando la raíz inyectó el BusPublisher
// (bus/memory en desarrollo; broker real en espera de ADR 32). Sin BusPublisher,
// no hay relay y la lista va vacía (arranque standalone sin bus). Los dos
// consumidores durables siguen pendientes (DEC-057).
func (m *Module) Workers() []worker.Worker {
	if m.relay == nil {
		return nil
	}
	return []worker.Worker{m.relay}
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
