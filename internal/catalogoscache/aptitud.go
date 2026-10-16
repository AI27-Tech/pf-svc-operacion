// Package catalogoscache es la caché EN PROCESO del lado consumidor que operacion
// mantiene sobre la arista síncrona a catalogos (operacion.md (f), E5-T5). No es
// una caché que administre catalogos: catalogos solo emite catalogo.actualizado
// (identidad + versión, nunca el payload); cada consumidor cachea por su cuenta y
// la invalida con ese evento. Esta es la mitad de la pieza; la otra —el
// consumidor durable que llama Invalidar— vive en internal/consumidores.
//
// La caché decora el puerto operacion.AptitudRecursos: implementa la MISMA
// interfaz envolviendo a otro AptitudRecursos (el adaptador HTTP en standalone,
// o el en proceso en core). Así el caso de uso de asignación no sabe que hay
// caché; solo ve el puerto.
//
// Dos límites a la obsolescencia (criterios de E5-T5):
//   - TTL corto: una entrada vence por tiempo aunque nunca llegue un evento. Con
//     el broker caído, la obsolescencia queda ACOTADA por el TTL (no crece sin
//     límite).
//   - Invalidación por evento: catalogo.actualizado borra la entrada de esa
//     entidad; la próxima lectura va a catalogos y recachea.
//
// Degradación informativa (DEC-055): la aptitud NO gatea. Si el subyacente falla
// (catalogos caído) y no hay entrada viva, la caché PROPAGA el error degradado
// —no inventa un apto ni un no-apto—. El caso de uso traduce ese degradado a
// "aptitud desconocida" y la asignación procede igual. La caché nunca convierte
// un fallo en un rechazo.
package catalogoscache

import (
	"context"
	"sync"
	"time"

	catalogos "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/catalogos"
)

// AptitudRecursos es la MISMA forma del puerto operacion.AptitudRecursos, pero
// declarada aquí para no importar el paquete raíz (evitaría un ciclo: la raíz
// importa este paquete para cablear la caché en New). Es el patrón de los demás
// internal/ (p. ej. autorizacion): el internal no conoce la raíz; la raíz lo
// consume en un solo sentido. Satisfacer esta interfaz satisface la de la raíz
// por estructura (duck typing de Go), así que el adaptador HTTP o el en proceso
// encajan sin conversión.
type AptitudRecursos interface {
	AptitudVehiculo(ctx context.Context, vehiculoID, clienteID catalogos.Id) (*catalogos.Aptitud, error)
	AptitudCustodio(ctx context.Context, custodioID, clienteID catalogos.Id) (*catalogos.Aptitud, error)
}

// Entidades de catálogo cuya aptitud cachea operacion. Coinciden con el campo
// `entidad` del evento catalogo.actualizado (outbox de catalogos). operacion solo
// lee aptitud de vehículo y custodio (los átomos, ADR 31 §5): son las dos únicas
// entidades que esta caché indexa. Un evento de otra entidad (cliente, contrato,
// sla, valor_catalogo) no tiene entrada aquí y su invalidación es un no-op.
const (
	EntidadVehiculo = "vehiculo"
	EntidadCustodio = "custodio"
)

// reloj es la fuente de tiempo, inyectable para pruebas deterministas del TTL.
type reloj func() time.Time

// entrada es una aptitud cacheada con el instante en que se guardó, para medir
// el TTL contra el reloj.
type entrada struct {
	apt       *catalogos.Aptitud
	guardadaA time.Time
}

// clave indexa una aptitud por entidad + recurso + cliente. clienteID forma
// parte de la clave porque la aptitud se evalúa contra el contrato/SLA de ESE
// cliente (tracer.4): el mismo vehículo puede ser apto para un cliente y no para
// otro, así que son entradas distintas.
type clave struct {
	entidad   string
	recursoID catalogos.Id
	clienteID catalogos.Id
}

// CacheAptitud decora operacion.AptitudRecursos con una caché en proceso de TTL
// corto, invalidable por entidad. Es segura para uso concurrente.
type CacheAptitud struct {
	sub AptitudRecursos // el puerto subyacente (HTTP o en proceso)
	ttl time.Duration
	now reloj

	mu    sync.Mutex
	datos map[clave]entrada
}

// Garantía en compilación: la caché ES un AptitudRecursos (decorador transparente).
var _ AptitudRecursos = (*CacheAptitud)(nil)

// Nueva construye la caché sobre un puerto subyacente, con un TTL corto como
// techo de obsolescencia. ttl<=0 toma el default. El reloj es time.Now; usa
// NuevaConReloj en pruebas para controlar el vencimiento.
func Nueva(sub AptitudRecursos, ttl time.Duration) *CacheAptitud {
	return NuevaConReloj(sub, ttl, time.Now)
}

// ttlPorDefecto es el techo de obsolescencia cuando no se configura uno. Corto a
// propósito: acota la ventana en que una entrada puede quedar obsoleta si el
// broker está caído y no llega catalogo.actualizado. No es un número de negocio
// aprobado; si debe configurarse por despliegue, se pasa por el constructor
// conservando este valor como default.
const ttlPorDefecto = 30 * time.Second

// NuevaConReloj es como Nueva pero con reloj inyectable (pruebas del TTL).
func NuevaConReloj(sub AptitudRecursos, ttl time.Duration, now reloj) *CacheAptitud {
	if ttl <= 0 {
		ttl = ttlPorDefecto
	}
	if now == nil {
		now = time.Now
	}
	return &CacheAptitud{
		sub:   sub,
		ttl:   ttl,
		now:   now,
		datos: make(map[clave]entrada),
	}
}

// AptitudVehiculo devuelve la aptitud del vehículo, servida de caché si hay una
// entrada viva (dentro del TTL) y yendo a catalogos si no. Un fallo del
// subyacente NO se cachea y se propaga tal cual (degradado): la aptitud es
// informativa (DEC-055).
func (c *CacheAptitud) AptitudVehiculo(ctx context.Context, vehiculoID, clienteID catalogos.Id) (*catalogos.Aptitud, error) {
	return c.leer(ctx, EntidadVehiculo, vehiculoID, clienteID, c.sub.AptitudVehiculo)
}

// AptitudCustodio devuelve la aptitud del custodio con la misma mecánica de
// caché/TTL/degradación que AptitudVehiculo.
func (c *CacheAptitud) AptitudCustodio(ctx context.Context, custodioID, clienteID catalogos.Id) (*catalogos.Aptitud, error) {
	return c.leer(ctx, EntidadCustodio, custodioID, clienteID, c.sub.AptitudCustodio)
}

// consulta es la forma común de AptitudVehiculo/AptitudCustodio del subyacente.
type consulta func(ctx context.Context, recursoID, clienteID catalogos.Id) (*catalogos.Aptitud, error)

// leer resuelve una aptitud: acierto de caché si la entrada vive dentro del TTL;
// si no, consulta el subyacente, cachea el resultado exitoso y lo devuelve. El
// error del subyacente se propaga sin cachear (no se envenena la caché con un
// fallo, y la aptitud sigue siendo informativa — DEC-055).
func (c *CacheAptitud) leer(ctx context.Context, entidad string, recursoID, clienteID catalogos.Id, fn consulta) (*catalogos.Aptitud, error) {
	k := clave{entidad: entidad, recursoID: recursoID, clienteID: clienteID}

	c.mu.Lock()
	if e, ok := c.datos[k]; ok && c.now().Sub(e.guardadaA) < c.ttl {
		apt := e.apt
		c.mu.Unlock()
		return apt, nil
	}
	c.mu.Unlock()

	// Miss o entrada vencida: va a catalogos. Fuera del lock para no serializar
	// la red; si dos goroutines corren a la vez, ambas consultan y la última
	// escribe (idempotente: el dato es el mismo recurso).
	apt, err := fn(ctx, recursoID, clienteID)
	if err != nil {
		return nil, err // degradado: no se cachea, no gatea (DEC-055)
	}

	c.mu.Lock()
	c.datos[k] = entrada{apt: apt, guardadaA: c.now()}
	c.mu.Unlock()
	return apt, nil
}

// Invalidar borra de la caché todas las entradas de una entidad+recurso, para
// todos los clientes (una actualización del recurso en catalogos puede cambiar
// su aptitud frente a cualquier cliente). La invoca el consumidor de
// catalogo.actualizado. Es idempotente: invalidar algo ausente no hace nada
// —reentregar el evento dos veces solo invalida dos veces, sin efecto extra
// (P13)—. Una entidad que esta caché no indexa (cliente, contrato, sla,
// valor_catalogo) simplemente no tiene entradas y el barrido es un no-op.
func (c *CacheAptitud) Invalidar(entidad string, recursoID catalogos.Id) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.datos {
		if k.entidad == entidad && k.recursoID == recursoID {
			delete(c.datos, k)
		}
	}
}
