package operacion

import (
	"context"
	"time"

	catalogos "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/catalogos"
)

// PresupuestoAptitud es el presupuesto de tiempo de la arista síncrona a
// catalogos. Se exporta JUNTO AL PUERTO AptitudRecursos porque la obligación I3
// (ADR 17) exige que todos sus adaptadores —el HTTP de este repo y el EN PROCESO
// de pf-svc-core— usen EXACTAMENTE el mismo valor. core lo lee de aquí; no
// duplica el número (prompt 09, I3 obligación #1). Si algún día debe
// configurarse por despliegue, pasa a Deps/config conservando este valor como
// default común.
const PresupuestoAptitud = 2 * time.Second

// Puertos de salida que declara operacion COMO CONSUMIDOR (regla 2 de 00-indice,
// ADR 17). La interfaz la declara el consumidor para que pf-svc-core la
// implemente con el adaptador en proceso, y este repo con el adaptador HTTP.
// Los tipos de entrada/salida son los generados de pf-pkg-contracts, nunca
// escritos a mano (regla 6).

// AptitudRecursos es la arista síncrona hacia catalogos. Su alcance es "lectura
// y verificación de aptitud; nada más" (operacion.md (f)), leído a través de
// ADR 31: catalogos es dueño de los ÁTOMOS (vehículos de AI27 y custodios), así
// que el puerto consulta átomos. NO tiene operación de reserva ni de
// disponibilidad: la reserva vive en asignacion, resuelta dentro de su
// transacción con la restricción de exclusión.
//
// Solo se declaran operaciones que EXISTEN en catalogos.yaml de la versión
// fijada (regla 6: 1:1 con operationId). Hoy esas son aptitudVehiculo y
// aptitudCustodio. Las listas (listarUnidades/listarCustodios) existen pero son
// paginadas; el caso de uso de armado/asignación necesita lectura por átomo, y
// esa superficie se añade cuando el contrato la exponga.
//
// C3 (resuelta en tracer.4): el átomo es `Vehiculo` (ADR 31 §5), no `Unidad`;
// el puerto ya usa ese nombre. La aptitud se evalúa contra el contrato/SLA de un
// cliente, así que ambas operaciones reciben `clienteID` (lo exige el contrato).
//
// BRECHA aún abierta: falta un obtenerVehiculo / obtenerCustodio por id (solo hay
// lista + aptitud). El puerto no inventa ese método; se agrega cuando el contrato
// lo tenga.
type AptitudRecursos interface {
	// AptitudVehiculo y AptitudCustodio devuelven aptitud INFORMATIVA (DEC-055):
	// nunca gatean armar la unidad ni asignarla. Si catalogos no responde o el
	// dato está obsoleto, el caso de uso muestra "aptitud desconocida" y la
	// asignación procede. El único rechazo de asignación es la reserva fallida.
	// clienteID acota la evaluación al contrato/SLA de ese cliente (tracer.4).
	AptitudVehiculo(ctx context.Context, vehiculoID, clienteID catalogos.Id) (*catalogos.Aptitud, error)
	AptitudCustodio(ctx context.Context, custodioID, clienteID catalogos.Id) (*catalogos.Aptitud, error)
}

// Revocacion es la arista RESTRINGIDA hacia identidad (DEC-051): solo dos
// operaciones, para el snapshot al arrancar (ADR 26 §4) y el fallback en modo
// degradado (ADR 26 §5). operacion verifica tokens como todo módulo protegido;
// esta no es una dependencia de dominio sino de infraestructura de autorización.
// El adaptador HTTP vive en adapters/http/; el en proceso, en pf-svc-core.
type Revocacion interface {
	// ListarRevocadas devuelve las sesiones revocadas para el snapshot inicial.
	ListarRevocadas(ctx context.Context) ([]string, error)
	// ObtenerSesion consulta una sesión puntual tras 15 s sin frescura.
	ObtenerSesion(ctx context.Context, sesionID string) (revocada bool, err error)
}

// PresupuestoRevocacion es el presupuesto de tiempo de la arista restringida a
// identidad (puerto Revocacion). Se exporta JUNTO AL PUERTO por la obligación I3
// (ADR 17): el adaptador HTTP de este repo y el EN PROCESO de pf-svc-core deben
// usar EXACTAMENTE el mismo valor; core lo lee de aquí y no duplica el número.
//
// Es el presupuesto de UNA llamada puntual (snapshot al arrancar o fallback
// ObtenerSesion), no el umbral de frescura del canal de revocación: ese umbral
// (15 s sin frescura antes del fallback, 30 s de corte máximo) lo fija ADR 26 y
// es independiente de cuánto espera una sola llamada HTTP. El valor concreto no
// está en las fuentes; 2 s es el default común, igual que PresupuestoAptitud, y
// no es un número de negocio aprobado. Si debe configurarse por despliegue, pasa
// a Deps/config conservando este valor como default.
const PresupuestoRevocacion = 2 * time.Second
