package solicitudes

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/estados"
	operclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/operacion"
	platformerrors "github.com/AI27-Tech/pf-pkg-platform-go/errors"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	openapi_types "github.com/oapi-codegen/runtime/types"
)

// ConfirmarPago registra la confirmación del pago: COTIZADO -> PAGADO, o la
// excepción VENCIDA -> PAGADO (DEC-148 #35), que queda en el histórico como
// `pago_confirmado_tardio`. La fecha del pago es obligatoria y debe caer dentro de
// la vigencia de la ÚLTIMA cotización, desde su día de entrega hasta su último día,
// ambos incluidos (DEC-148 #34, #43): gana la fecha del pago, no la de la
// confirmación; puede ser futura mientras caiga en la vigencia (DEC-148 #65).
// VENCIDA -> PAGADO ocurre una sola vez por solicitud (DEC-148 #64): si el
// histórico ya la registra, otro pago tardío responde 409 transicion_no_permitida
// (el contrato no define un código propio). No genera aviso al cliente (DEC-148 #56).
func (s *Servicio) ConfirmarPago(ctx context.Context, a Actor, id uuid.UUID, in operclient.PagoEntrada) (*operclient.Solicitud, error) {
	if in.FechaPago.IsZero() {
		return nil, platformerrors.NewValidacion("operacion: fecha_pago es obligatoria")
	}
	return s.enTx(ctx, id, false, a, func(tx pgx.Tx, f *fila) error {
		if !estados.PuedeTransicionarSolicitud(f.Estado, estados.EstadoSolicitudPagado) {
			return platformerrors.NewConflictoCodigo("transicion_no_permitida",
				"operacion: no se puede confirmar el pago de una solicitud en %s", f.Estado)
		}
		// El histórico (append-only, escrito solo por transicionar) es la fuente de la
		// regla, no pago_solicitud: no depende de que quede la fila del pago y sobrevive
		// a la reversa. La fila de la solicitud está bloqueada FOR UPDATE por enTx, así
		// que dos pagos tardíos simultáneos se serializan y el segundo ve el primero.
		if f.Estado == estados.EstadoSolicitudVencida {
			var yaHubo bool
			if err := tx.QueryRow(ctx,
				`SELECT EXISTS (SELECT 1 FROM solicitudes.transicion_solicitud
				                 WHERE solicitud_id = $1 AND de_estado = 'VENCIDA' AND a_estado = 'PAGADO')`, id).Scan(&yaHubo); err != nil {
				return fmt.Errorf("solicitudes: leer el histórico del pago tardío: %w", err)
			}
			if yaHubo {
				return platformerrors.NewConflictoCodigo("transicion_no_permitida",
					"operacion: el pago tardío (VENCIDA a PAGADO) solo ocurre una vez por solicitud")
			}
		}
		var version int
		var entrega, vigHasta time.Time
		err := tx.QueryRow(ctx,
			`SELECT version, entregada_en, vigente_hasta FROM solicitudes.cotizacion
			  WHERE solicitud_id = $1 ORDER BY version DESC LIMIT 1`, id).Scan(&version, &entrega, &vigHasta)
		if errors.Is(err, pgx.ErrNoRows) {
			// Un estado que admite pagar siempre tiene cotización: es un dato roto.
			return fmt.Errorf("solicitudes: la solicitud %s en %s no tiene cotización", id, f.Estado)
		}
		if err != nil {
			return fmt.Errorf("solicitudes: leer la última cotización: %w", err)
		}
		y, m, d := in.FechaPago.Date()
		pago := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
		if pago.Before(diaMX(entrega)) || pago.After(vigHasta) {
			return platformerrors.NewConflictoCodigo("fecha_pago_fuera_de_vigencia",
				"operacion: la fecha del pago %s no cae dentro de la vigencia de la cotización (%s a %s)",
				pago.Format(time.DateOnly), diaMX(entrega).Format(time.DateOnly), vigHasta.Format(time.DateOnly))
		}
		tardio := f.Estado == estados.EstadoSolicitudVencida
		if err := s.transicionar(ctx, tx, f, Cambio{A: estados.EstadoSolicitudPagado, Disparador: DisparadorCotizacion, Actor: a}); err != nil {
			return err
		}
		var ref *string
		if in.Referencia != nil && strings.TrimSpace(*in.Referencia) != "" {
			r := strings.TrimSpace(*in.Referencia)
			ref = &r
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO solicitudes.pago_solicitud
			   (solicitud_id, fecha_pago, referencia, confirmado_en, tardio, cotizacion_version, actor_id, actor_rol)
			 VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
			id, pago, ref, s.ahora().UTC(), tardio, version, nullsiVacio(a.Subject), nullsiVacio(a.Rol),
		); err != nil {
			return fmt.Errorf("solicitudes: registrar el pago: %w", err)
		}
		return nil
	})
}

// RevertirPago deshace una confirmación hecha por error: PAGADO -> COTIZADO, con
// motivo obligatorio y solo mientras la solicitud siga en PAGADO (DEC-148 #37). El
// pago queda marcado como revertido, no borrado. La facultad (solo administrador)
// la aplica la fachada.
// TODO(producto): si la vigencia ya terminó al revertir, la solicitud queda en
// COTIZADO con vigencia vencida; si vence de inmediato o se recotiza está
// pendiente (ADR 38). Hoy solo regresa a COTIZADO.
func (s *Servicio) RevertirPago(ctx context.Context, a Actor, id uuid.UUID, in operclient.MotivoEntrada) (*operclient.Solicitud, error) {
	if strings.TrimSpace(in.Motivo) == "" {
		return nil, platformerrors.NewValidacion("operacion: el motivo de la reversa es obligatorio")
	}
	return s.enTx(ctx, id, false, a, func(tx pgx.Tx, f *fila) error {
		// La reversa solo existe desde PAGADO; sin esta guarda, SOLICITADO -> COTIZADO
		// (una arista real de otro comando) se rechazaría por disparador y no por estado.
		if f.Estado != estados.EstadoSolicitudPagado {
			return platformerrors.NewConflictoCodigo("transicion_no_permitida",
				"operacion: solo se revierte el pago de una solicitud pagada, no una en %s", f.Estado)
		}
		if err := s.transicionar(ctx, tx, f, Cambio{A: estados.EstadoSolicitudCotizado, Disparador: DisparadorAdministrador, Actor: a, Motivo: in.Motivo}); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE solicitudes.pago_solicitud SET revertido_en = $2 WHERE solicitud_id = $1 AND revertido_en IS NULL`,
			id, s.ahora().UTC()); err != nil {
			return fmt.Errorf("solicitudes: marcar el pago como revertido: %w", err)
		}
		return nil
	})
}

// pagoVigente lee el pago no revertido; nil si no hay.
func pagoVigente(ctx context.Context, tx pgx.Tx, id uuid.UUID) (*operclient.Pago, error) {
	var p operclient.Pago
	var fecha time.Time
	err := tx.QueryRow(ctx,
		`SELECT fecha_pago, referencia, confirmado_en FROM solicitudes.pago_solicitud
		  WHERE solicitud_id = $1 AND revertido_en IS NULL`, id).Scan(&fecha, &p.Referencia, &p.ConfirmadoEn)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("solicitudes: leer el pago vigente: %w", err)
	}
	p.FechaPago = openapi_types.Date{Time: fecha}
	return &p, nil
}
