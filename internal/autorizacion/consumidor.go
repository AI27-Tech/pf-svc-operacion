// consumidor.go es el worker de operacion que escucha la difusión del broker
// (ADR 26 §3, ADR 29 §3) y alimenta la proyección local de tombstones. Cada
// réplica corre el suyo: la difusión llega a TODAS las réplicas (no es un
// consumer group), de modo que todas invaliden la sesión.
package autorizacion

import (
	"context"

	"github.com/AI27-Tech/pf-pkg-platform-go/bus"
	"github.com/AI27-Tech/pf-pkg-platform-go/worker"
)

// tipoSesionRevocada es el tipo de evento que marca un tombstone. Cualquier otro
// sobre del broadcast (p. ej. el heartbeat del canal) solo cuenta como señal de
// frescura, no como revocación. Discriminar por INCLUSIÓN —no "todo lo que no es
// heartbeat"— evita acoplar operacion a los tipos de otros emisores.
const tipoSesionRevocada = "sesion.revocada"

// NuevoConsumidorRevocacion crea el worker que suscribe la difusión del canal.
// Cada sobre recibido —sea un sesion.revocada o un heartbeat— refresca la señal
// del canal (senal); solo un sesion.revocada marca el tombstone en la proyección
// (proy). difusor es el puerto de broadcast del bus; todos los inyecta la raíz.
//
// Separar frescura de tombstone cumple ADR 26 §5: la frescura se mide por señal
// del canal, no por la llegada de revocaciones. Marcar es idempotente, así que
// una difusión repetida no cambia nada.
func NuevoConsumidorRevocacion(difusor bus.Broadcaster, proy *Proyeccion, senal *SenalCanal) (worker.Worker, error) {
	manejar := func(ctx context.Context, s bus.Sobre) error {
		if senal != nil {
			senal.Marcar()
		}
		if s.Tipo == tipoSesionRevocada {
			proy.Marcar(s.AgregadoID)
		}
		return nil
	}
	return difusor.SubscribeBroadcast(manejar)
}
