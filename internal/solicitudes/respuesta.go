package solicitudes

import (
	"context"

	"github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/estados"
	operclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/operacion"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// RechazarCotizacion es "no acepto este precio o estas condiciones": COTIZADO ->
// RECHAZADO_POR_CLIENTE, sin marca de penalización (DEC-148 #9, #18). Solo el
// cliente dueño; una solicitud de otra organización responde 404. El motivo es
// opcional (el contrato lo deja pendiente).
func (s *Servicio) RechazarCotizacion(ctx context.Context, a Actor, id uuid.UUID, in operclient.AccionClienteEntrada) (*operclient.Solicitud, error) {
	return s.accionCliente(ctx, a, id, in, estados.EstadoSolicitudRechazadoPorCliente)
}

// Cancelar es "ya no necesito el servicio": desde SOLICITADO y COTIZADO termina en
// CANCELADO_POR_CLIENTE sin marca; desde PAGADO, con marca de penalización (la fija
// la arista, DEC-148 #6, #17). No se crea viaje. Distinta de RechazarCotizacion.
func (s *Servicio) Cancelar(ctx context.Context, a Actor, id uuid.UUID, in operclient.AccionClienteEntrada) (*operclient.Solicitud, error) {
	return s.accionCliente(ctx, a, id, in, estados.EstadoSolicitudCanceladoPorCliente)
}

func (s *Servicio) accionCliente(ctx context.Context, a Actor, id uuid.UUID, in operclient.AccionClienteEntrada, destino estados.EstadoSolicitud) (*operclient.Solicitud, error) {
	motivo := ""
	if in.Motivo != nil {
		motivo = *in.Motivo
	}
	return s.enTx(ctx, id, true, a, func(tx pgx.Tx, f *fila) error {
		return s.transicionar(ctx, tx, f, Cambio{A: destino, Disparador: DisparadorCliente, Actor: a, Motivo: motivo})
	})
}
