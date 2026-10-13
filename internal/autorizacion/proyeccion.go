// Package autorizacion es la proyección local de tombstones de sesión de
// operacion (ADR 26 §3): el conjunto de sesion_id revocados que cada réplica
// mantiene en memoria para rechazar, en sus rutas protegidas, un token cuya
// sesión ya fue revocada. La alimenta el consumidor de la difusión de
// sesion.revocada (consumidor.go) y la reconstruye por snapshot al arrancar
// (reconstructor.go).
//
// Es el gemelo del paquete homónimo de pf-svc-bff: mismo patrón, adaptado al
// puerto Revocacion de operacion (ListarRevocadas/ObtenerSesion), que es más
// estrecho que la arista de bff a identidad. No se reusa el de bff porque es
// internal/ de otro módulo (inalcanzable). Cuando un tercer consumidor lo
// necesite (realtime, ADR 28), extraer el patrón a pf-pkg-platform-go; dos
// copias todavía no justifican la abstracción.
package autorizacion

import (
	"context"
	"sync"
	"time"

	"github.com/AI27-Tech/pf-pkg-platform-go/auth"
)

// Proyeccion es la proyección local de tombstones sesion_id → revocada
// (ADR 26 §3). Toda ruta protegida la consulta antes de aceptar. Implementa
// auth.RevocationChecker. Idempotente por sesion_id (P13).
//
// Regla dura (ADR 26 §5): si el canal de revocación no tiene frescura por más
// del umbral, cada petición cae a un fallback síncrono a identidad; si identidad
// no responde, FALLA CERRADO (trata la sesión como revocada). La frescura y el
// fallback son opcionales (los habilita ConFrescura); sin ellos la proyección se
// queda en la ruta local (solo el set de tombstones), el modo de desarrollo sin
// bus.
type Proyeccion struct {
	// mu protege revocadas: el consumidor escribe (Marcar) y el middleware lee
	// (Revocada) en cada petición, concurrentemente. RWMutex porque las lecturas
	// (una por petición protegida) superan de lejos a las escrituras.
	mu        sync.RWMutex
	revocadas map[string]struct{}

	// Regla de frescura/fallback de ADR 26 §5 (opcional; la habilita ConFrescura).
	reloj     func() time.Time
	frescura  Frescura
	autoridad Autoridad
	umbral    time.Duration
}

// NuevaProyeccion crea una proyección vacía. Zero value útil: sin snapshot
// cargado no conoce ninguna revocación; la raíz reconstruye por snapshot antes
// de servir tráfico protegido (ADR 26 §4).
func NuevaProyeccion() *Proyeccion {
	return &Proyeccion{revocadas: make(map[string]struct{})}
}

// Revocada implementa auth.RevocationChecker: true si la sesión debe rechazarse.
// Codifica ADR 26 §3 y §5 como una decisión en orden:
//
//  1. Si la sesión está en el set local de tombstones, se rechaza (el broadcast
//     ya la marcó). La respuesta local gana y no consulta a nadie.
//  2. Si no, y el canal está FRESCO, se considera viva: la ausencia de tombstone
//     local es confiable mientras el canal esté sano.
//  3. Si el canal está OBSOLETO (>umbral sin señal), se cae al fallback síncrono
//     a identidad: si confirma vigencia, se permite; si confirma revocación o NO
//     RESPONDE, se rechaza (FAIL-CLOSED).
//
// Sin ConFrescura (desarrollo sin bus), la decisión se queda en los pasos 1–2.
func (p *Proyeccion) Revocada(ctx context.Context, sesionID string) bool {
	if p == nil {
		return false
	}
	p.mu.RLock()
	_, enLocal := p.revocadas[sesionID]
	obsoleto := p.canalObsoleto()
	aut := p.autoridad
	p.mu.RUnlock()

	// Paso 1: la respuesta local gana.
	if enLocal {
		return true
	}
	// Paso 2: canal fresco (o sin frescura configurada) → vivo.
	if !obsoleto || aut == nil {
		return false
	}
	// Paso 3: canal obsoleto → fallback a identidad, fail-closed ante error.
	revocada, err := aut.SesionRevocada(ctx, sesionID)
	if err != nil {
		return true // FAIL-CLOSED (ADR 26 §5): identidad no responde → rechazar.
	}
	return revocada
}

// Marcar registra una sesión como revocada. Idempotente por sesion_id (P13). Lo
// invoca el consumidor al procesar un sesion.revocada y la reconstrucción por
// snapshot.
func (p *Proyeccion) Marcar(sesionID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.revocadas == nil {
		p.revocadas = make(map[string]struct{})
	}
	p.revocadas[sesionID] = struct{}{}
}

// ConFrescura habilita la regla de obsolescencia/fallback de ADR 26 §5. Sin
// llamarla, Revocada se queda en la ruta local. reloj permite inyectar el tiempo
// en pruebas; umbral es la ventana de frescura (15 s en producción, ADR 26 §5).
func (p *Proyeccion) ConFrescura(reloj func() time.Time, fr Frescura, aut Autoridad, umbral time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reloj = reloj
	p.frescura = fr
	p.autoridad = aut
	p.umbral = umbral
}

// canalObsoleto indica si el canal lleva más del umbral sin señal verificable.
// Debe llamarse con el lock tomado (lo llama Revocada).
func (p *Proyeccion) canalObsoleto() bool {
	if p.frescura == nil || p.reloj == nil || p.umbral <= 0 {
		return false // sin frescura configurada, nunca se declara obsoleto.
	}
	return p.reloj().Sub(p.frescura.UltimaSenal()) > p.umbral
}

var _ auth.RevocationChecker = (*Proyeccion)(nil)
