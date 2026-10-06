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
	"github.com/AI27-Tech/pf-pkg-platform-go/pgxpool"
	"github.com/AI27-Tech/pf-pkg-platform-go/worker"
)

// Pools agrupa los cuatro pools, uno por rol/esquema (operacion.md (e)). Es el
// único servicio con esta forma. Nunca se comparte un pool entre roles, aunque
// los cuatro módulos compartan proceso y transacción (I1).
type Pools struct {
	Viajes     *pgxpool.Pool // rol op_viajes
	Asignacion *pgxpool.Pool // rol op_asignacion (la única excepción de GRANT cruzado)
	Bitacora   *pgxpool.Pool // rol op_bitacora
	Auditoria  *pgxpool.Pool // rol op_auditoria
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
	pools Pools
	deps  Deps
	// Aquí vivirán los casos de uso de cada sub-módulo cuando se implementen.
}

// New recibe los pools por rol y los puertos ya resueltos. No lee entorno
// (00 regla 4): la raíz cablea todo.
func New(p Pools, d Deps) (*Module, error) {
	return &Module{pools: p, deps: d}, nil
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

// Workers devuelve los workers embebidos (ADR 29 §6): un publisher de outbox por
// esquema PRODUCTOR (viajes, asignacion) y los dos consumidores durables V1
// (catalogo.actualizado, sesion.revocada). bitacora y auditoria no publican
// eventos: no tienen outbox ni worker. Cada worker usa el rol y pool de SU
// módulo; ninguno accede a varios esquemas.
//
// Scaffolding: hoy devuelve lista vacía. Los publishers se cablean cuando la
// raíz inyecte el adaptador de bus (bus/memory en desarrollo; broker real en
// espera de ADR 32); los consumidores, con su suscripción durable propia
// (DEC-057) y su inbox_evento (ubicación pendiente, ver README).
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
// regla 6). Hoy expone lecturas de viaje; las escrituras (transición,
// asignación) se agregan con su caso de uso.
type ConsultasAPI interface {
	ObtenerViaje(ctx context.Context, id operclient.Id) (*operclient.Viaje, error)
}

type consultas struct {
	m *Module
}

func (c *consultas) ObtenerViaje(ctx context.Context, id operclient.Id) (*operclient.Viaje, error) {
	return nil, errNoImplementado("ObtenerViaje")
}
