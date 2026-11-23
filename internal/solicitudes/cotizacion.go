package solicitudes

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/estados"
	operclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/operacion"
	platformerrors "github.com/AI27-Tech/pf-pkg-platform-go/errors"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	openapi_types "github.com/oapi-codegen/runtime/types"
)

// diasVigencia es la vigencia de una cotización desde su última entrega (DEC-148 #15).
const diasVigencia = 15

// zonaMX es la zona IANA con la que se cuentan los días de vigencia (DEC-148 #50),
// no un desplazamiento fijo. time/tzdata (doc.go) garantiza que se resuelva.
var zonaMX = func() *time.Location {
	z, err := time.LoadLocation("America/Mexico_City")
	if err != nil {
		panic("solicitudes: sin la zona America/Mexico_City: " + err.Error())
	}
	return z
}()

// diaMX es el día calendario de t en America/Mexico_City, como fecha a medianoche UTC.
func diaMX(t time.Time) time.Time {
	y, m, d := t.In(zonaMX).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// vigenteHasta es el último día de vigencia de una cotización entregada en
// `entrega`: 15 días calendario después, en America/Mexico_City.
// TODO(producto): el cómputo exacto del "último día" (inclusivo o exclusivo) sigue
// pendiente del Lead (E10, "Pendientes"); se sigue el contrato: entrega + 15 días.
func vigenteHasta(entrega time.Time) time.Time {
	return diaMX(entrega).AddDate(0, 0, diasVigencia)
}

// montoValido es el formato de Monto del contrato: decimal en texto, sin signo.
var montoValido = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?$`)

// Cotizar envía la primera cotización (SOLICITADO -> COTIZADO) o recotiza
// (COTIZADO, sin cambio de estado). Cada llamada agrega una versión y conserva las
// anteriores (DEC-148 #42); la vigencia corre desde esta entrega. Exige monto,
// moneda y fecha y hora del servicio (DEC-148 #39, #41), que puede diferir de la
// que pidió el cliente. No valida disponibilidad de unidades (DEC-148 #23). En
// cualquier otro estado (PAGADO, VENCIDA, APROBADO, terminales) responde 409
// (DEC-148 #36). La validación de entrada va antes de tocar la base.
func (s *Servicio) Cotizar(ctx context.Context, a Actor, id uuid.UUID, in operclient.CotizacionEntrada) (*operclient.Solicitud, error) {
	// TODO(producto): catálogo de monedas aceptadas y si el monto admite cero; hoy
	// moneda es texto no vacío y el monto cumple el formato del contrato.
	if !montoValido.MatchString(in.Monto) {
		return nil, platformerrors.NewValidacion("operacion: monto es obligatorio y debe ser un decimal sin signo")
	}
	if strings.TrimSpace(in.Moneda) == "" {
		return nil, platformerrors.NewValidacion("operacion: moneda es obligatoria")
	}
	if in.FechaHoraServicio.IsZero() {
		return nil, platformerrors.NewValidacion("operacion: fecha_hora_servicio es obligatoria para cotizar")
	}
	return s.enTx(ctx, id, false, a, func(tx pgx.Tx, f *fila) error {
		if f.Estado != estados.EstadoSolicitudSolicitado && f.Estado != estados.EstadoSolicitudCotizado {
			return platformerrors.NewConflictoCodigo("transicion_no_permitida",
				"operacion: no se puede cotizar una solicitud en %s", f.Estado)
		}
		en := s.ahora().UTC()
		fecha := in.FechaHoraServicio.UTC()
		if f.Estado == estados.EstadoSolicitudSolicitado {
			if err := s.transicionar(ctx, tx, f, Cambio{A: estados.EstadoSolicitudCotizado, Disparador: DisparadorCotizacion, Actor: a}); err != nil {
				return err
			}
		}
		// Versión nueva: max+1 bajo el bloqueo de la solicitud, sin carreras.
		if _, err := tx.Exec(ctx,
			`INSERT INTO solicitudes.cotizacion
			   (solicitud_id, version, monto, moneda, fecha_hora_servicio, notas, entregada_en, vigente_hasta, actor_id, actor_rol)
			 SELECT $1, COALESCE(max(version), 0) + 1, $2::numeric, $3, $4, $5, $6, $7, $8, $9
			   FROM solicitudes.cotizacion WHERE solicitud_id = $1`,
			id, in.Monto, strings.TrimSpace(in.Moneda), fecha, in.Notas, en, vigenteHasta(en), nullsiVacio(a.Subject), nullsiVacio(a.Rol),
		); err != nil {
			return fmt.Errorf("solicitudes: insertar cotización: %w", err)
		}
		// La fecha vigente del servicio es la de la última cotización (contrato).
		if _, err := tx.Exec(ctx, `UPDATE solicitudes.solicitud SET fecha_hora_servicio = $2 WHERE id = $1`, id, fecha); err != nil {
			return fmt.Errorf("solicitudes: fijar la fecha del servicio: %w", err)
		}
		f.FechaHoraServicio = &fecha
		return nil
	})
}

// Reprogramar cambia la fecha y hora de una solicitud PAGADO. Motivo obligatorio;
// sin cambio de estado ni pago nuevo; queda en reprogramacion_solicitud con actor,
// motivo y marca de tiempo (DEC-148 #24, #40).
func (s *Servicio) Reprogramar(ctx context.Context, a Actor, id uuid.UUID, in operclient.ReprogramacionEntrada) (*operclient.Solicitud, error) {
	motivo := strings.TrimSpace(in.Motivo)
	if motivo == "" {
		return nil, platformerrors.NewValidacion("operacion: el motivo de la reprogramación es obligatorio")
	}
	if in.FechaHoraServicio.IsZero() {
		return nil, platformerrors.NewValidacion("operacion: fecha_hora_servicio es obligatoria")
	}
	return s.enTx(ctx, id, false, a, func(tx pgx.Tx, f *fila) error {
		if f.Estado != estados.EstadoSolicitudPagado {
			return platformerrors.NewConflictoCodigo("transicion_no_permitida",
				"operacion: solo se reprograma una solicitud pagada, no una en %s", f.Estado)
		}
		fecha := in.FechaHoraServicio.UTC()
		if _, err := tx.Exec(ctx,
			`INSERT INTO solicitudes.reprogramacion_solicitud
			   (solicitud_id, fecha_hora_anterior, fecha_hora_nueva, motivo, actor_id, actor_rol, creado_en)
			 VALUES ($1,$2,$3,$4,$5,$6,$7)`,
			id, f.FechaHoraServicio, fecha, motivo, nullsiVacio(a.Subject), nullsiVacio(a.Rol), s.ahora().UTC(),
		); err != nil {
			return fmt.Errorf("solicitudes: registrar reprogramación: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE solicitudes.solicitud SET fecha_hora_servicio = $2 WHERE id = $1`, id, fecha); err != nil {
			return fmt.Errorf("solicitudes: reprogramar: %w", err)
		}
		f.FechaHoraServicio = &fecha
		return nil
	})
}

// cotizacionVigente lee la última versión de la cotización; nil si no hay.
func cotizacionVigente(ctx context.Context, tx pgx.Tx, id uuid.UUID) (*operclient.Cotizacion, error) {
	var c operclient.Cotizacion
	var vig time.Time
	err := tx.QueryRow(ctx,
		`SELECT version, monto::text, moneda, fecha_hora_servicio, notas, entregada_en, vigente_hasta
		   FROM solicitudes.cotizacion WHERE solicitud_id = $1 ORDER BY version DESC LIMIT 1`, id,
	).Scan(&c.Version, &c.Monto, &c.Moneda, &c.FechaHoraServicio, &c.Notas, &c.EntregadaEn, &vig)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("solicitudes: leer la cotización vigente: %w", err)
	}
	c.VigenteHasta = openapi_types.Date{Time: vig}
	return &c, nil
}
