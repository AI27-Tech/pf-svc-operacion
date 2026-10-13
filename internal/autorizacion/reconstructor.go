// reconstructor.go implementa la reconstrucción por snapshot al arrancar
// (ADR 26 §4): una réplica nueva carga las sesiones revocadas vigentes desde
// identidad ANTES de declararse lista para tráfico protegido, para no arrancar
// "ciega" sin conocer las revocaciones previas.
//
// Mientras la precarga no termina, el worker reporta not-ready: la raíz no debe
// enrutar tráfico protegido a una réplica que aún no convergió.
//
// Alcance (sin cursor): NO hace el catch-up por cursor del ADR §4, porque
// identidad todavía no pagina el snapshot (devuelve el conjunto completo, su
// propio TODO). La ventana entre snapshot e inicio del canal se mitiga —no se
// cierra— arrancando la suscripción ANTES de la precarga: una revocación durante
// la carga ya la captura el consumidor. Misma deuda que bff.
package autorizacion

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/AI27-Tech/pf-pkg-platform-go/worker"
)

// Snapshot provee la lista de sesiones revocadas vigentes para la precarga. Lo
// satisface el puerto Revocacion de operacion (ListarRevocadas). Interfaz mínima
// declarada aquí, donde se usa.
type Snapshot interface {
	ListarRevocadas(ctx context.Context) ([]string, error)
}

// reconstructor envuelve el worker del consumidor con la precarga por snapshot y
// la readiness de ADR 26 §4. Implementa worker.Worker.
type reconstructor struct {
	suscripcion worker.Worker
	snapshot    Snapshot
	proy        *Proyeccion
	convergido  atomic.Bool
	// errPrecarga guarda el último error de precarga para exponerlo en Ready.
	// operacion no pasa logger a este worker; surfacear la causa en el mensaje de
	// readiness es más útil que un log perdido. atomic.Pointer para el acceso
	// concurrente entre Run (escribe) y Ready (lee).
	errPrecarga atomic.Pointer[error]
}

// NuevoReconstructor envuelve el worker de suscripción (el consumidor del
// broadcast) con la reconstrucción por snapshot. La raíz lo arranca en su
// RunGroup en lugar del consumidor desnudo.
func NuevoReconstructor(suscripcion worker.Worker, snapshot Snapshot, proy *Proyeccion) worker.Worker {
	return &reconstructor{suscripcion: suscripcion, snapshot: snapshot, proy: proy}
}

func (r *reconstructor) Name() string { return "operacion-revocacion" }

// Ready reporta not-ready hasta que la precarga converge (ADR 26 §4). Si la
// precarga falló, incluye la causa. Una vez convergido, delega en la readiness
// del worker de suscripción, de modo que una réplica que perdió el canal tras
// converger vuelva a not-ready.
func (r *reconstructor) Ready(ctx context.Context) error {
	if !r.convergido.Load() {
		if ep := r.errPrecarga.Load(); ep != nil {
			return fmt.Errorf("reconstrucción de revocación no convergió: %w", *ep)
		}
		return fmt.Errorf("reconstrucción de revocación en curso: not-ready")
	}
	return r.suscripcion.Ready(ctx)
}

// Run arranca la suscripción al canal, precarga el snapshot y, al converger,
// declara ready. El orden importa: la suscripción arranca ANTES de la precarga
// para mitigar la ventana sin cursor (una revocación durante la carga ya la
// captura el canal).
//
// Dos caminos de salida, ambos reciben de errSub (sin fuga de goroutine):
//   - precarga falla: queda not-ready con la causa en Ready; espera a que la
//     suscripción termine (al cancelar ctx) y devuelve su resultado.
//   - la suscripción falla temprano (NATS caído al arrancar): su error se
//     propaga; RunGroup lo trata como fallo de arranque. Fail-fast deliberado:
//     sin canal, la réplica no debe servir tráfico protegido.
func (r *reconstructor) Run(ctx context.Context) error {
	errSub := make(chan error, 1)
	go func() { errSub <- r.suscripcion.Run(ctx) }()

	if err := r.precargar(ctx); err != nil {
		r.errPrecarga.Store(&err)
		return <-errSub
	}
	r.convergido.Store(true)
	return <-errSub
}

// precargar carga las revocadas vigentes del snapshot en la proyección. Marcar
// es idempotente, así que si el consumidor ya marcó alguna por el canal, no hay
// doble efecto.
func (r *reconstructor) precargar(ctx context.Context) error {
	ids, err := r.snapshot.ListarRevocadas(ctx)
	if err != nil {
		return fmt.Errorf("precargar snapshot de revocadas: %w", err)
	}
	for _, id := range ids {
		r.proy.Marcar(id)
	}
	return nil
}
