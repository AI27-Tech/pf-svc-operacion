// frescura.go implementa la regla de ADR 26 §5: cuando el canal de difusión
// lleva más del umbral sin señal verificable, cada petición protegida cae a un
// fallback síncrono contra identidad; si identidad no responde, se falla cerrado.
//
// La frescura NO se infiere de la llegada de revocaciones (puede no haberlas en
// un periodo sano): se mide con una señal del canal (el heartbeat del reemisor
// de core, que el consumidor marca con cada sobre).
package autorizacion

import (
	"context"
	"sync"
	"time"
)

// SenalCanal implementa Frescura: registra cuándo llegó la última señal del
// canal de difusión. Cualquier sobre del broadcast (heartbeat o evento real)
// cuenta como señal de vida; lo alimenta el consumidor en cada entrega. Seguro
// para uso concurrente.
type SenalCanal struct {
	reloj  func() time.Time
	mu     sync.RWMutex
	ultima time.Time
}

// NuevaSenalCanal crea la señal con el reloj dado (time.Now en producción,
// inyectable en pruebas). Arranca con la última señal en el instante de
// creación: una réplica recién nacida se considera fresca hasta que pase el
// umbral sin recibir nada, no obsoleta de entrada.
func NuevaSenalCanal(reloj func() time.Time) *SenalCanal {
	if reloj == nil {
		reloj = time.Now
	}
	return &SenalCanal{reloj: reloj, ultima: reloj()}
}

// Marcar registra que acaba de llegar una señal del canal.
func (s *SenalCanal) Marcar() {
	s.mu.Lock()
	s.ultima = s.reloj()
	s.mu.Unlock()
}

// UltimaSenal implementa Frescura.
func (s *SenalCanal) UltimaSenal() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ultima
}

var _ Frescura = (*SenalCanal)(nil)

// Frescura informa cuándo fue la última señal verificable del canal. La
// proyección la usa para decidir si el canal está obsoleto.
type Frescura interface {
	UltimaSenal() time.Time
}

// Autoridad es el fallback durable a identidad (ADR 26 §5): consulta el estado
// de una sesión cuando el canal está obsoleto. Lo implementa un adaptador sobre
// el puerto Revocacion de operacion. Un error (identidad no responde) es, para
// la regla de seguridad, indistinguible de "no puedo afirmar que esté viva".
type Autoridad interface {
	// SesionRevocada devuelve true si identidad confirma la revocación. El error
	// se propaga para que la proyección decida fail-closed.
	SesionRevocada(ctx context.Context, sesionID string) (bool, error)
}

// ConsultaRevocacion es el subconjunto del puerto Revocacion de operacion que el
// fallback necesita: la consulta puntual de una sesión. Se declara aquí, donde
// se usa, para no acoplar este paquete a todo el puerto.
type ConsultaRevocacion interface {
	ObtenerSesion(ctx context.Context, sesionID string) (revocada bool, err error)
}

// autoridadIdentidad adapta la consulta puntual del puerto Revocacion al puerto
// Autoridad que la proyección usa en el fallback.
type autoridadIdentidad struct {
	consulta ConsultaRevocacion
}

// NuevaAutoridad adapta la consulta puntual de revocación al puerto Autoridad.
func NuevaAutoridad(consulta ConsultaRevocacion) Autoridad {
	return &autoridadIdentidad{consulta: consulta}
}

// SesionRevocada delega en ObtenerSesion del puerto. Propaga el error tal cual:
// la proyección lo trata como fail-closed (ADR 26 §5); no es esta capa la que
// decide la postura de seguridad.
func (a *autoridadIdentidad) SesionRevocada(ctx context.Context, sesionID string) (bool, error) {
	return a.consulta.ObtenerSesion(ctx, sesionID)
}
