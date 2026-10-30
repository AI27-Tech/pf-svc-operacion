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
	"time"

	operclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/operacion"
	"github.com/AI27-Tech/pf-pkg-platform-go/auth"
	"github.com/AI27-Tech/pf-pkg-platform-go/bus"
	platformerrors "github.com/AI27-Tech/pf-pkg-platform-go/errors"
	"github.com/AI27-Tech/pf-pkg-platform-go/outbox"
	"github.com/AI27-Tech/pf-pkg-platform-go/pgxpool"
	"github.com/AI27-Tech/pf-pkg-platform-go/worker"
	"github.com/AI27-Tech/pf-svc-operacion/internal/asignacion"
	"github.com/AI27-Tech/pf-svc-operacion/internal/auditoria"
	"github.com/AI27-Tech/pf-svc-operacion/internal/autorizacion"
	"github.com/AI27-Tech/pf-svc-operacion/internal/bitacora"
	"github.com/AI27-Tech/pf-svc-operacion/internal/catalogoscache"
	"github.com/AI27-Tech/pf-svc-operacion/internal/consumidores"
	"github.com/AI27-Tech/pf-svc-operacion/internal/descargas"
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
	// Broadcaster es el puerto de difusión del bus (ADR 29 §3). Si no es nil,
	// operacion construye internamente su proyección de tombstones y arranca el
	// consumidor de sesion.revocada que la alimenta; el middleware de Routes
	// consulta esa MISMA proyección (rechaza una sesión revocada, ADR 26 §3). La
	// proyección es asunto interno de operacion (no la construye la raíz): así el
	// middleware y el worker comparten instancia por construcción. Nil en
	// desarrollo sin bus: las rutas se protegen solo con verificación de firma.
	Broadcaster bus.Broadcaster
	// Subscriber es el bus durable del que operacion consume catalogo.actualizado
	// (DEC-057, E5-T5) para invalidar su caché en proceso de aptitud. Si no es
	// nil, New envuelve el puerto Aptitud con la caché (internal/catalogoscache) y
	// arranca el consumidor durable que la invalida; así la lectura de aptitud y
	// la invalidación comparten la MISMA instancia de caché por construcción. Nil
	// en arranque standalone sin bus: entonces Aptitud se usa sin caché (lectura
	// directa a catalogos), que es correcto —sin bus no hay evento que invalide,
	// y una caché sin invalidación solo acumularía obsolescencia—.
	Subscriber bus.Subscriber
}

// Module es la instancia construida del módulo operacion.
type Module struct {
	pools      Pools
	deps       Deps
	viajes     *viajes.Servicio
	asignacion *asignacion.Servicio
	bitacora   *bitacora.Servicio
	descargas  *descargas.Servicio
	// auditoriaConsulta lee la bitácora probatoria para la composición del
	// expediente de descarga (E6-T3). Solo lectura, con el rol op_auditoria.
	auditoriaConsulta *auditoria.ServicioConsulta
	// relay drena viajes.outbox al bus. nil si Deps.BusPublisher no se inyectó
	// (arranque standalone sin bus); en ese caso Workers() no lo arranca.
	relay worker.Worker
	// revocacion es la proyección local de tombstones (ADR 26 §3). La construye
	// New cuando hay Broadcaster; el middleware la consulta y el consumidor la
	// alimenta. Nil sin bus. Es un tipo de internal/: por eso operacion la crea y
	// la retiene aquí, no la raíz.
	revocacion *autorizacion.Proyeccion
	// consumidor es el worker de sesion.revocada (envuelto por el reconstructor de
	// snapshot). nil sin Broadcaster.
	consumidor worker.Worker
	// consumidorCatalogos es el worker durable de catalogo.actualizado que
	// invalida la caché de aptitud (E5-T5). nil sin Subscriber.
	consumidorCatalogos worker.Worker
	// consumidorEmergencia es el worker durable de emergencia.nota_agregada que
	// proyecta cada nota de emergencia al histórico/bitácora del viaje (E7,
	// tracer.17, DEC-100). nil sin Subscriber.
	consumidorEmergencia worker.Worker
}

// umbralFrescura es la ventana tras la cual el canal de revocación se considera
// obsoleto y las rutas protegidas caen al fallback durable con identidad
// (ADR 26 §5). Normativo, no configurable por despliegue.
const umbralFrescura = 15 * time.Second

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
		bitacora:   bitacora.NewServicio(p.Bitacora),
		// registrarDescargaViaje (AI27-80): lee el viaje con op_viajes y escribe el
		// rastro de auditoría con el rol dueño op_auditoria vía el interceptor. Dos
		// pools, uno por rol (I1); sin GRANT cruzado a op_viajes (ADR 31/33).
		descargas: descargas.NewServicio(p.Viajes, p.Auditoria),
		// Consulta de la auditoría probatoria para componer la descarga (E6-T3).
		// Lee con op_auditoria (SELECT sobre registro_auditoria); no escribe.
		auditoriaConsulta: auditoria.NewServicioConsulta(p.Auditoria),
	}
	// El único outbox de operacion es el del esquema `viajes` (ADR 35, DEC-068):
	// lleva todos los eventos viaje.* (incluidos los que asignacion escribe ahí
	// por la excepción de GRANT). Un solo relay sobre el pool op_viajes lo drena.
	// Corre con search_path=viajes, igual que el Publisher que ya lo escribe.
	if d.BusPublisher != nil {
		m.relay = outbox.NewRelay("outbox-relay:viajes", p.Viajes, d.BusPublisher, outbox.RelayOpciones{})
	}
	// Proyección de tombstones y su consumidor (ADR 26 §3): solo con Broadcaster.
	// operacion es dueño de su proyección para que el middleware (Routes) y el
	// consumidor (Workers) compartan la MISMA instancia por construcción.
	if d.Broadcaster != nil {
		proy := autorizacion.NuevaProyeccion()
		m.revocacion = proy
		senal := autorizacion.NuevaSenalCanal(time.Now)
		// La regla de frescura/fallback (ADR 26 §5) necesita la arista a identidad
		// (puerto Revocacion). Sin ella, la proyección queda en solo-tombstones
		// locales: marca lo que llega por el canal, pero no puede caer al fallback
		// durable. Degradación segura y documentada, no fallo de arranque.
		if d.Revocacion != nil {
			proy.ConFrescura(time.Now, senal, autorizacion.NuevaAutoridad(d.Revocacion), umbralFrescura)
		}
		cons, err := autorizacion.NuevoConsumidorRevocacion(d.Broadcaster, proy, senal)
		if err != nil {
			return nil, err
		}
		// Reconstrucción por snapshot (ADR 26 §4): precarga las revocadas vigentes
		// de identidad y queda not-ready hasta converger. Sin Revocacion no hay de
		// dónde precargar; se arranca el consumidor desnudo (sin snapshot inicial).
		if d.Revocacion != nil {
			m.consumidor = autorizacion.NuevoReconstructor(cons, d.Revocacion, proy)
		} else {
			m.consumidor = cons
		}
	}
	// Caché de aptitud + su consumidor de invalidación (E5-T5): solo con Subscriber
	// y con un puerto Aptitud que envolver. Se envuelve el Aptitud inyectado con la
	// caché en proceso y se guarda de vuelta en deps, de modo que todo caso de uso
	// que lea aptitud por m.deps.Aptitud use la MISMA instancia que el consumidor
	// invalida (igual que la proyección de tombstones: construida aquí, compartida
	// por construcción). Sin Subscriber no se cachea (ver Deps.Subscriber).
	if d.Subscriber != nil && d.Aptitud != nil {
		cache := catalogoscache.Nueva(d.Aptitud, 0) // 0 ⇒ TTL por defecto
		m.deps.Aptitud = cache
		cons := consumidores.NuevoConsumidorCatalogo(cache)
		handler := func(ctx context.Context, s bus.Sobre) error {
			return cons.Procesar(ctx, s)
		}
		w, err := d.Subscriber.Subscribe(
			"operacion-catalogos",
			bus.SensibilidadAmplia,
			[]string{consumidores.TipoCatalogoActualizado},
			handler,
		)
		if err != nil {
			return nil, err
		}
		m.consumidorCatalogos = w
	}
	// Consumidor durable de emergencia.nota_agregada (E7, tracer.17, DEC-100):
	// proyecta cada nota de emergencia al histórico/bitácora del viaje. Primer
	// consumidor durable RESTRINGIDO de operacion (el evento viaja por el tema
	// restringido). La idempotencia es durable (RegistrarDesdeEvento por
	// evento_id), no en memoria. Solo con Subscriber.
	if d.Subscriber != nil {
		cons := consumidores.NuevoConsumidorEmergenciaNota(m.bitacora)
		handler := func(ctx context.Context, s bus.Sobre) error {
			return cons.Procesar(ctx, s)
		}
		w, err := d.Subscriber.Subscribe(
			"operacion-emergencia",
			bus.SensibilidadRestringida,
			[]string{consumidores.TipoEmergenciaNotaAgregada},
			handler,
		)
		if err != nil {
			return nil, err
		}
		m.consumidorEmergencia = w
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
	// La proyección de tombstones se consulta en cada ruta protegida (ADR 26 §3):
	// una sesión revocada se rechaza aunque su token siga siendo válido por firma.
	// Si no hay proyección (sin bus), se pasa un RevocationChecker nil de interfaz
	// (no un puntero nil envuelto) para que el middleware omita la consulta en vez
	// de hacer panic: en desarrollo las rutas valen solo por firma.
	var rev auth.RevocationChecker
	if m.revocacion != nil {
		rev = m.revocacion
	}
	protegido := auth.Middleware(m.deps.Verificador, rev)
	mux.Handle("POST /api/v1/viajes/{id}/transiciones", protegido(http.HandlerFunc(m.handleTransicionar)))
	mux.Handle("POST /api/v1/viajes/{id}/asignacion", protegido(http.HandlerFunc(m.handleAsignar)))
	mux.Handle("PATCH /api/v1/viajes/{id}/asignacion", protegido(http.HandlerFunc(m.handleReasignar)))
	mux.Handle("PATCH /api/v1/viajes/{id}/eta", protegido(http.HandlerFunc(m.handleFijarEta)))
	mux.Handle("POST /api/v1/viajes/{id}/bitacora", protegido(http.HandlerFunc(m.handleRegistrarBitacora)))
	mux.Handle("GET /api/v1/viajes/{id}/bitacora", protegido(http.HandlerFunc(m.handleListarBitacora)))
	// Condiciones ortogonales (E1-T6): incidencias y estadías. Abrir/cerrar
	// NINGUNA cambia viaje.estado (§C.2). Los sirve el módulo viajes (dueño de
	// condicion_viaje y de la nueva tabla estadia, mismo rol op_viajes).
	mux.Handle("POST /api/v1/viajes/{id}/incidencias", protegido(http.HandlerFunc(m.handleAbrirIncidencia)))
	mux.Handle("PATCH /api/v1/viajes/{id}/incidencias/{iid}", protegido(http.HandlerFunc(m.handleResolverIncidencia)))
	mux.Handle("POST /api/v1/viajes/{id}/estadias", protegido(http.HandlerFunc(m.handleAbrirEstadia)))
	mux.Handle("PATCH /api/v1/viajes/{id}/estadias/{eid}", protegido(http.HandlerFunc(m.handleCerrarEstadia)))
	mux.Handle("GET /api/v1/viajes/{id}/estadias", protegido(http.HandlerFunc(m.handleListarEstadias)))
	// Descarga del expediente (AI27-80, E1-T7): bff compone la descarga e invoca a
	// operacion para DEJAR el rastro de auditoría (el rastro lo escribe operacion
	// en su transacción, no el bff). Sin cuerpo; responde el Viaje descargado.
	mux.Handle("POST /api/v1/viajes/{id}/descargas", protegido(http.HandlerFunc(m.handleRegistrarDescarga)))
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
	// El rol, la audiencia y el tenant del actor viajan en los claims (DEC-053/054).
	// El rol lo exige la guarda del cierre (DEC-020 #6 / DEC-060 #1). La audiencia y
	// el tenant habilitan la transición de cliente de E9-T7 (DEC-111): un cliente
	// solo cancela/rechaza su propio viaje (tenant = cliente, DEC-112).
	var rol, audiencia, tenantID string
	if claims := auth.ClaimsFromContext(r.Context()); claims != nil {
		rol = claims.Rol
		audiencia = string(claims.Audiencia)
		tenantID = claims.TenantID
	}
	v, err := m.viajes.Transicionar(r.Context(), id, body, rol, audiencia, tenantID)
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

// handleFijarEta atiende PATCH /api/v1/viajes/{id}/eta: lee el ETA capturado a
// mano (EtaEntrada) y delega en viajes. No es un cambio de estado (ADR 20): solo
// rechaza si el viaje está en estado terminal.
func (m *Module) handleFijarEta(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		escribirError(w, platformerrors.NewValidacion("operacion: id de viaje inválido"))
		return
	}
	var body operclient.EtaEntrada
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		escribirError(w, platformerrors.NewValidacion("operacion: cuerpo de la petición inválido: %v", err))
		return
	}
	v, err := m.viajes.FijarEta(r.Context(), id, body)
	if err != nil {
		escribirError(w, err)
		return
	}
	escribirJSON(w, http.StatusOK, v)
}

// handleRegistrarBitacora atiende POST /api/v1/viajes/{id}/bitacora: lee el texto
// de la entrada (EntradaBitacoraEntrada) y delega en el caso de uso de bitácora,
// que inserta la entrada (append-only) y audita en la misma tx. 201 Created.
//
// NO existe handler de edición ni de borrado de entradas: la bitácora es
// append-only (E1-T5), y una corrección es una entrada nueva.
func (m *Module) handleRegistrarBitacora(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		escribirError(w, platformerrors.NewValidacion("operacion: id de viaje inválido"))
		return
	}
	var body operclient.EntradaBitacoraEntrada
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		escribirError(w, platformerrors.NewValidacion("operacion: cuerpo de la petición inválido: %v", err))
		return
	}
	var actor string
	if claims := auth.ClaimsFromContext(r.Context()); claims != nil {
		actor = claims.Subject
	}
	e, err := m.bitacora.RegistrarEntrada(r.Context(), id, body, actor)
	if err != nil {
		escribirError(w, err)
		return
	}
	escribirJSON(w, http.StatusCreated, e)
}

// handleListarBitacora atiende GET /api/v1/viajes/{id}/bitacora: lee el cursor
// opcional de la query y delega en el caso de uso, que devuelve las entradas en
// orden cronológico (las más antiguas primero).
func (m *Module) handleListarBitacora(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		escribirError(w, platformerrors.NewValidacion("operacion: id de viaje inválido"))
		return
	}
	var params operclient.ListarBitacoraParams
	if c := r.URL.Query().Get("cursor"); c != "" {
		cur := operclient.Cursor(c)
		params.Cursor = &cur
	}
	pag, err := m.bitacora.ListarBitacora(r.Context(), id, params)
	if err != nil {
		escribirError(w, err)
		return
	}
	escribirJSON(w, http.StatusOK, pag)
}

// handleAbrirIncidencia atiende POST /api/v1/viajes/{id}/incidencias: lee el tipo
// interno y la descripción (IncidenciaEntrada) y delega en viajes, que abre la
// condición ortogonal INCIDENCIA sin tocar viaje.estado. El rol del actor viaja
// en los claims (operador/supervisor/administrador; reacción NO, DEC-028). 201.
func (m *Module) handleAbrirIncidencia(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		escribirError(w, platformerrors.NewValidacion("operacion: id de viaje inválido"))
		return
	}
	var body operclient.IncidenciaEntrada
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		escribirError(w, platformerrors.NewValidacion("operacion: cuerpo de la petición inválido: %v", err))
		return
	}
	var rol string
	if claims := auth.ClaimsFromContext(r.Context()); claims != nil {
		rol = claims.Rol
	}
	inc, err := m.viajes.AbrirIncidencia(r.Context(), id, body, rol)
	if err != nil {
		escribirError(w, err)
		return
	}
	escribirJSON(w, http.StatusCreated, inc)
}

// handleResolverIncidencia atiende PATCH /api/v1/viajes/{id}/incidencias/{iid}:
// cierra la condición INCIDENCIA (sin tocar viaje.estado) y publica
// viaje.condicion_cerrada. SOLO supervisor o administrador (un operador que abrió
// no puede resolver). Lee id/iid con r.PathValue.
func (m *Module) handleResolverIncidencia(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		escribirError(w, platformerrors.NewValidacion("operacion: id de viaje inválido"))
		return
	}
	iid, err := uuid.Parse(r.PathValue("iid"))
	if err != nil {
		escribirError(w, platformerrors.NewValidacion("operacion: id de incidencia inválido"))
		return
	}
	var body operclient.ResolucionIncidenciaEntrada
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		escribirError(w, platformerrors.NewValidacion("operacion: cuerpo de la petición inválido: %v", err))
		return
	}
	var rol string
	if claims := auth.ClaimsFromContext(r.Context()); claims != nil {
		rol = claims.Rol
	}
	inc, err := m.viajes.ResolverIncidencia(r.Context(), id, iid, body, rol)
	if err != nil {
		escribirError(w, err)
		return
	}
	escribirJSON(w, http.StatusOK, inc)
}

// handleAbrirEstadia atiende POST /api/v1/viajes/{id}/estadias: lee el motivo y el
// eje de autorización (EstadiaEntrada) y delega en viajes, que abre la estadía
// fijando inicio = ahora, sin tocar viaje.estado. 201 Created.
func (m *Module) handleAbrirEstadia(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		escribirError(w, platformerrors.NewValidacion("operacion: id de viaje inválido"))
		return
	}
	var body operclient.EstadiaEntrada
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		escribirError(w, platformerrors.NewValidacion("operacion: cuerpo de la petición inválido: %v", err))
		return
	}
	est, err := m.viajes.AbrirEstadia(r.Context(), id, body)
	if err != nil {
		escribirError(w, err)
		return
	}
	escribirJSON(w, http.StatusCreated, est)
}

// handleCerrarEstadia atiende PATCH /api/v1/viajes/{id}/estadias/{eid}: cierra la
// estadía abierta fijando cierre = ahora (cuerpo vacío, CierreEstadiaEntrada), sin
// tocar viaje.estado. Lee id/eid con r.PathValue.
func (m *Module) handleCerrarEstadia(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		escribirError(w, platformerrors.NewValidacion("operacion: id de viaje inválido"))
		return
	}
	eid, err := uuid.Parse(r.PathValue("eid"))
	if err != nil {
		escribirError(w, platformerrors.NewValidacion("operacion: id de estadía inválido"))
		return
	}
	// El cuerpo es vacío por contrato; no se decodifica nada de él.
	est, err := m.viajes.CerrarEstadia(r.Context(), id, eid)
	if err != nil {
		escribirError(w, err)
		return
	}
	escribirJSON(w, http.StatusOK, est)
}

// handleListarEstadias atiende GET /api/v1/viajes/{id}/estadias: lee el cursor
// opcional de la query y delega en viajes, que devuelve las estadías en orden de
// ocurrencia. La duración NO viene en la respuesta: se deriva de inicio/cierre.
func (m *Module) handleListarEstadias(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		escribirError(w, platformerrors.NewValidacion("operacion: id de viaje inválido"))
		return
	}
	var params operclient.ListarEstadiasParams
	if c := r.URL.Query().Get("cursor"); c != "" {
		cur := operclient.Cursor(c)
		params.Cursor = &cur
	}
	pag, err := m.viajes.ListarEstadias(r.Context(), id, params)
	if err != nil {
		escribirError(w, err)
		return
	}
	escribirJSON(w, http.StatusOK, pag)
}

// handleRegistrarDescarga atiende POST /api/v1/viajes/{id}/descargas
// (registrarDescargaViaje, AI27-80): deja el rastro de auditoría de la descarga
// del expediente y responde el Viaje descargado. No tiene cuerpo. El actor (quién
// descargó) sale de los claims del token; el viaje, de {id}. El registro se
// escribe con el rol dueño op_auditoria vía el interceptor, append-only.
func (m *Module) handleRegistrarDescarga(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		escribirError(w, platformerrors.NewValidacion("operacion: id de viaje inválido"))
		return
	}
	var actor string
	if claims := auth.ClaimsFromContext(r.Context()); claims != nil {
		actor = claims.Subject
	}
	v, err := m.descargas.Registrar(r.Context(), id, actor)
	if err != nil {
		escribirError(w, err)
		return
	}
	escribirJSON(w, http.StatusOK, v)
}

// escribirJSON serializa v como respuesta JSON con el status dado.
func escribirJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// escribirError traduce un error de la taxonomía de la plataforma al Error del
// contrato y a su status HTTP, reusando el mapeo de la plataforma. El código de
// máquina sale de CodigoDe: el Codigo específico del error si lo trae (p. ej.
// cierre_anticipado_requiere_incidencia), si no el genérico de la categoría.
func escribirError(w http.ResponseWriter, err error) {
	cat := platformerrors.AsCategory(err)
	escribirJSON(w, cat.HTTPStatus(), operclient.Error{Codigo: platformerrors.CodigoDe(err), Mensaje: err.Error()})
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
// (bus/memory en desarrollo; broker real en espera de ADR 32), más el consumidor
// durable de catalogo.actualizado cuando la raíz inyectó el Subscriber (E5-T5):
// invalida la caché en proceso de aptitud. El consumidor durable de
// sesion.revocada sigue por el canal de difusión (Broadcaster), no por el
// Subscriber. Sin BusPublisher ni Subscriber ni Broadcaster, la lista va vacía
// (arranque standalone sin bus).
func (m *Module) Workers() []worker.Worker {
	var ws []worker.Worker
	if m.relay != nil {
		ws = append(ws, m.relay)
	}
	if m.consumidor != nil {
		ws = append(ws, m.consumidor)
	}
	if m.consumidorCatalogos != nil {
		ws = append(ws, m.consumidorCatalogos)
	}
	if m.consumidorEmergencia != nil {
		ws = append(ws, m.consumidorEmergencia)
	}
	return ws
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
	// ListarBitacora, ConsultarAuditoria y RegistrarDescargaViaje sostienen la
	// composición del expediente de descarga de UN viaje (E6-T3, ADR 27): bff
	// agota las páginas de bitácora y auditoría y deja el rastro de la descarga.
	ListarBitacora(ctx context.Context, id operclient.Id, params operclient.ListarBitacoraParams) (*operclient.PaginaBitacora, error)
	ConsultarAuditoria(ctx context.Context, id operclient.Id, params operclient.ConsultarAuditoriaParams) (*operclient.PaginaAuditoria, error)
	RegistrarDescargaViaje(ctx context.Context, id operclient.Id) (*operclient.Viaje, error)
}

type consultas struct {
	m *Module
}

func (c *consultas) CrearViaje(ctx context.Context, body operclient.CrearViajeEntrada) (*operclient.Viaje, error) {
	// El estado inicial lo fija el rol del creador (DEC-026). Para un cliente, el
	// cliente_id del viaje se deriva de su tenant (DEC-112: tenant == cliente_id),
	// no del cuerpo. Rol y tenant viajan en los claims del token, que el
	// middleware inyecta en el ctx (en core, vía bff).
	claims := auth.ClaimsFromContext(ctx)
	if claims == nil {
		return nil, platformerrors.NewNoAutorizado("operacion: crearViaje sin claims en el contexto")
	}
	return c.m.viajes.Crear(ctx, body, claims.Rol, claims.TenantID)
}

func (c *consultas) ListarViajes(ctx context.Context, params operclient.ListarViajesParams) (*operclient.PaginaViajes, error) {
	return c.m.viajes.Listar(ctx, params)
}

func (c *consultas) ObtenerViaje(ctx context.Context, id operclient.Id) (*operclient.ViajeDetalle, error) {
	return c.m.viajes.Obtener(ctx, id)
}

// ListarBitacora reenvía al caso de uso de bitácora (mismo que el handler HTTP,
// I3). El cursor ya viene armado en params.
func (c *consultas) ListarBitacora(ctx context.Context, id operclient.Id, params operclient.ListarBitacoraParams) (*operclient.PaginaBitacora, error) {
	return c.m.bitacora.ListarBitacora(ctx, id, params)
}

// ConsultarAuditoria reenvía al servicio de consulta de auditoría (op_auditoria).
func (c *consultas) ConsultarAuditoria(ctx context.Context, id operclient.Id, params operclient.ConsultarAuditoriaParams) (*operclient.PaginaAuditoria, error) {
	return c.m.auditoriaConsulta.Consultar(ctx, id, params)
}

// RegistrarDescargaViaje deja el rastro de la descarga y devuelve el Viaje. El
// actor sale de los claims del ctx (igual que el handler HTTP, I3); nil-safe:
// sin claims el actor queda vacío (autor_id NULL), no es error.
func (c *consultas) RegistrarDescargaViaje(ctx context.Context, id operclient.Id) (*operclient.Viaje, error) {
	var actor string
	if claims := auth.ClaimsFromContext(ctx); claims != nil {
		actor = claims.Subject
	}
	return c.m.descargas.Registrar(ctx, id, actor)
}
