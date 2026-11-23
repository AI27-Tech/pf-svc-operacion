package solicitudes

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/estados"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// IntervaloVencimiento es cada cuánto corre el worker. La frontera del
// vencimiento es la medianoche de America/Mexico_City; un minuto basta para
// cerrar la solicitud poco después sin que la consulta pese (solo recorre las
// solicitudes en COTIZADO).
const IntervaloVencimiento = time.Minute

// loteVencimiento acota las solicitudes que una corrida intenta cerrar. Lo que
// sobre lo toma la siguiente corrida.
const loteVencimiento = 500

// vigenciaTerminada es la condición SQL de "ya pasó el último día de vigencia":
// la vigencia de la ÚLTIMA versión de la cotización (DEC-148 #15) terminó antes
// del día de hoy en America/Mexico_City ($n). El último día es inclusivo, así que
// ese día todavía no vence y vence el día siguiente (DEC-148 #15, #50).
func vigenciaTerminada(hoy string) string {
	return `(SELECT c.vigente_hasta FROM solicitudes.cotizacion c
	          WHERE c.solicitud_id = s.id ORDER BY c.version DESC LIMIT 1) < ` + hoy
}

// VencerVencidas cierra en VENCIDA las solicitudes en COTIZADO cuya vigencia
// terminó, con disparador sistema (DEC-148 #15, #50; E10-T7). Devuelve cuántas
// cerró. Sin marca de penalización y sin viaje: solo la arista COTIZADO → VENCIDA.
//
// Cubre también la reversa de un pago con la vigencia ya terminada: regresa a
// COTIZADO y esta función la cierra en su siguiente corrida (DEC-148 #63).
//
// Una sola ejecución con varias réplicas: cada solicitud se cierra en su propia
// transacción tomando su fila con FOR UPDATE SKIP LOCKED y repitiendo ahí la
// condición. La réplica que llega segunda ve la fila bloqueada (la salta) o ya
// cerrada (no cumple la condición) y no hace nada; no hay lock global que
// serialice a las réplicas ni espera entre ellas. Una confirmación de pago en
// vuelo bloquea la misma fila: gana el pago y la solicitud no vence (#15).
//
// Una falla en una solicitud no frena al resto: las ya cerradas quedan cerradas,
// las demás se intentan, y se devuelve el primer error. La siguiente corrida
// reintenta lo que falló. Idempotente: una solicitud ya VENCIDA no cumple la
// condición y no se toca.
func (s *Servicio) VencerVencidas(ctx context.Context) (int, error) {
	hoy := diaMX(s.ahora())
	lectura, err := s.tx.Begin(ctx)
	if err != nil {
		return 0, err
	}
	rows, err := lectura.Query(ctx,
		`SELECT s.id FROM solicitudes.solicitud s
		  WHERE s.estado = $1 AND `+vigenciaTerminada("$2")+`
		  ORDER BY s.creado_en, s.id LIMIT $3`,
		string(estados.EstadoSolicitudCotizado), hoy, loteVencimiento)
	if err != nil {
		_ = lectura.Rollback(ctx)
		return 0, fmt.Errorf("solicitudes: buscar cotizaciones vencidas: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	_ = lectura.Rollback(ctx) // solo lectura: no hay nada que confirmar
	if err != nil {
		return 0, fmt.Errorf("solicitudes: leer cotizaciones vencidas: %w", err)
	}

	var primero error
	n := 0
	for _, id := range ids {
		ok, err := s.vencer(ctx, id, hoy)
		if err != nil {
			if primero == nil {
				primero = fmt.Errorf("solicitudes: vencer %s: %w", id, err)
			}
			continue
		}
		if ok {
			n++
		}
	}
	return n, primero
}

// vencer cierra UNA solicitud en su propia transacción. Devuelve false, sin error,
// si ya no corresponde cerrarla (otra réplica la tiene o la cerró, o se pagó).
func (s *Servicio) vencer(ctx context.Context, id uuid.UUID, hoy time.Time) (bool, error) {
	tx, err := s.tx.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var bloqueada uuid.UUID
	err = tx.QueryRow(ctx,
		`SELECT s.id FROM solicitudes.solicitud s
		  WHERE s.id = $1 AND s.estado = $2 AND `+vigenciaTerminada("$3")+`
		  FOR UPDATE OF s SKIP LOCKED`,
		id, string(estados.EstadoSolicitudCotizado), hoy).Scan(&bloqueada)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("solicitudes: bloquear la solicitud: %w", err)
	}
	f, err := cargar(ctx, tx, id, false) // ya está bloqueada arriba
	if err != nil {
		return false, err
	}
	if err := s.transicionar(ctx, tx, f, Cambio{A: estados.EstadoSolicitudVencida, Disparador: DisparadorSistema}); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("solicitudes: commit vencimiento: %w", err)
	}
	return true, nil
}

// WorkerVencimiento es el worker embebido del módulo (ADR 29 §6) que corre
// VencerVencidas cada intervalo con el pool de op_solicitudes.
type WorkerVencimiento struct {
	s         *Servicio
	intervalo time.Duration

	mu  sync.Mutex
	err error // resultado de la última corrida
}

// NuevoWorkerVencimiento crea el worker. intervalo <= 0 usa IntervaloVencimiento.
func NuevoWorkerVencimiento(s *Servicio, intervalo time.Duration) *WorkerVencimiento {
	if intervalo <= 0 {
		intervalo = IntervaloVencimiento
	}
	return &WorkerVencimiento{s: s, intervalo: intervalo}
}

func (w *WorkerVencimiento) Name() string { return "vencimiento:solicitudes" }

// Ready falla si la última corrida falló; se recupera sola con la siguiente.
func (w *WorkerVencimiento) Ready(context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.err
}

// Run corre una vez al arrancar y luego cada intervalo, hasta que ctx se cancele.
func (w *WorkerVencimiento) Run(ctx context.Context) error {
	t := time.NewTicker(w.intervalo)
	defer t.Stop()
	for {
		_, err := w.s.VencerVencidas(ctx)
		if ctx.Err() != nil {
			return nil
		}
		w.mu.Lock()
		w.err = err
		w.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}
