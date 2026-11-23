package solicitudes

import (
	"context"
	"fmt"
	"strings"

	"github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/estados"
	operclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/operacion"
	platformerrors "github.com/AI27-Tech/pf-pkg-platform-go/errors"
	"github.com/AI27-Tech/pf-pkg-platform-go/outbox"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// RolJefeOperativo es el rol que ve solo las solicitudes en PAGADO (DEC-148 #22).
const RolJefeOperativo = "jefe_operativo"

// resolviendo es la guarda de visibilidad de las acciones del jefe operativo: una
// solicitud que no está en PAGADO no existe para él (404), igual que en las
// lecturas (contrato de aprobarSolicitud: "el 409 lo ve el administrador").
func (a Actor) resolviendo(f *fila) error {
	if a.Rol == RolJefeOperativo && f.Estado != estados.EstadoSolicitudPagado {
		return platformerrors.NewNoEncontrado("operacion: solicitud %s no encontrada", f.ID)
	}
	return nil
}

// Aprobar es PAGADO -> APROBADO y, en la MISMA transacción, el nacimiento del
// viaje en APROBADO con el id de la solicitud (DEC-148 #3, #52; ADR 39): sus
// destinos, su evento viaje.creado y viaje.mensajeria_habilitada por el outbox
// único de los viaje.* (ADR 35), y la auditoría. O existen la solicitud aprobada
// y su viaje, o ninguno de los dos. La aprobación es explícita, nunca automática.
//
// Guardas, en orden: el jefe operativo no ve una solicitud fuera de PAGADO (404);
// el administrador recibe 409 transicion_no_permitida; sin fecha y hora del
// servicio, 409 fecha_hora_servicio_requerida (DEC-010 #2). No se escribe
// transicion_viaje (DEC-148 #58; el viaje que crea Operación tampoco la escribe).
// La facultad (jefe_operativo y administrador) la aplica la fachada.
//
// TODO(arquitectura): enTx toma FOR UPDATE antes de la guarda de visibilidad, así
// que quien no puede ver la fila la bloquea un instante antes de recibir el 404.
func (s *Servicio) Aprobar(ctx context.Context, a Actor, id uuid.UUID) (*operclient.Solicitud, error) {
	return s.enTx(ctx, id, false, a, func(tx pgx.Tx, f *fila) error {
		if err := a.resolviendo(f); err != nil {
			return err
		}
		if f.Estado != estados.EstadoSolicitudPagado {
			return platformerrors.NewConflictoCodigo("transicion_no_permitida",
				"operacion: solo se aprueba una solicitud pagada, no una en %s", f.Estado)
		}
		if f.FechaHoraServicio == nil {
			return platformerrors.NewConflictoCodigo("fecha_hora_servicio_requerida",
				"operacion: la solicitud no tiene fecha y hora del servicio; defínela antes de aprobar")
		}
		// TODO(producto): DEC-148 #66 exige fecha futura al crear, cotizar y reprogramar,
		// no al aprobar; una solicitud pagada cuya fecha pasó esperando al jefe se
		// aprueba y crea un viaje en el pasado. Decidir si se bloquea.
		// El administrador hereda las facultades del jefe operativo (DEC-148 #20): la
		// arista la origina jefe_operativo, y el actor real queda en el histórico.
		if err := s.transicionar(ctx, tx, f, Cambio{A: estados.EstadoSolicitudAprobado, Disparador: DisparadorJefeOperativo, Actor: a}); err != nil {
			return err
		}
		return s.crearViaje(ctx, tx, f)
	})
}

// Rechazar es PAGADO -> RECHAZADO_POR_AI27, con motivo obligatorio (DEC-020 #2),
// sin marca de reembolso (DEC-148 #19) y sin crear viaje. El cliente ve el motivo
// (Solicitud.motivo_rechazo). Mismas guardas de visibilidad que Aprobar.
func (s *Servicio) Rechazar(ctx context.Context, a Actor, id uuid.UUID, in operclient.MotivoEntrada) (*operclient.Solicitud, error) {
	if strings.TrimSpace(in.Motivo) == "" {
		return nil, platformerrors.NewValidacion("operacion: el motivo del rechazo es obligatorio")
	}
	return s.enTx(ctx, id, false, a, func(tx pgx.Tx, f *fila) error {
		if err := a.resolviendo(f); err != nil {
			return err
		}
		if f.Estado != estados.EstadoSolicitudPagado {
			return platformerrors.NewConflictoCodigo("transicion_no_permitida",
				"operacion: solo se rechaza una solicitud pagada, no una en %s", f.Estado)
		}
		return s.transicionar(ctx, tx, f, Cambio{A: estados.EstadoSolicitudRechazadoPorAi27, Disparador: DisparadorJefeOperativo, Actor: a, Motivo: in.Motivo})
	})
}

// crearViaje inserta el viaje de una solicitud aprobada, ya en la transacción de
// la aprobación. Solo INSERT en viajes.viaje, viajes.destino y viajes.outbox (el
// GRANT de op_solicitudes, ADR 39 enmendado por DEC-148 #58); no lee viajes.
func (s *Servicio) crearViaje(ctx context.Context, tx pgx.Tx, f *fila) error {
	en := s.ahora().UTC()
	var placas, economico, marca, modelo, color *string
	if t := f.Tracto; t != nil {
		placas, economico, marca, modelo, color = t.Placas, t.Economico, t.Marca, t.Modelo, t.Color
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO viajes.viaje
		   (id, estado, cliente_id, origen, fecha_hora_servicio, tipo_servicio, creado_en, nacio_de_solicitud,
		    tracto_placas, tracto_economico, tracto_marca, tracto_modelo, tracto_color)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,true,$8,$9,$10,$11,$12)`,
		f.ID, string(operclient.APROBADO), f.ClienteID, f.Origen, f.FechaHoraServicio, f.TipoServicio, en,
		placas, economico, marca, modelo, color,
	); err != nil {
		return fmt.Errorf("solicitudes: crear el viaje: %w", err)
	}
	for i, d := range f.Destinos {
		if _, err := tx.Exec(ctx,
			`INSERT INTO viajes.destino (id, viaje_id, orden, nombre, numero_envio, fecha_cita)
			 VALUES ($1,$2,$3,$4,$5,$6)`,
			uuid.New(), f.ID, i, d.Nombre, d.NumeroEnvio, d.FechaCita,
		); err != nil {
			return fmt.Errorf("solicitudes: crear el destino %d del viaje: %w", i, err)
		}
	}
	// Mismos eventos y misma forma que viajes.Crear: no hay struct generado para
	// los viaje.* (brecha de contrato). viaje.mensajeria_habilitada crea la
	// conversación del viaje, distinta de la de la solicitud (DEC-148 #29).
	return s.publicarEnViajes(ctx, tx,
		outbox.Event{
			Tipo: "viaje.creado", Version: 1, AgregadoID: f.ID.String(), OcurridoEn: en,
			Payload: map[string]any{
				"viaje_id":   f.ID.String(),
				"estado":     string(operclient.APROBADO),
				"cliente_id": f.ClienteID.String(),
				"origen":     f.Origen,
				"creado_en":  en,
			},
		},
		outbox.Event{
			Tipo: "viaje.mensajeria_habilitada", Version: 1, AgregadoID: f.ID.String(), OcurridoEn: en,
			Payload: map[string]any{
				"viaje_id":   f.ID.String(),
				"cliente_id": f.ClienteID.String(),
				"creado_en":  en,
			},
		})
}

// publicarEnViajes escribe en viajes.outbox, el outbox único de los viaje.* (ADR
// 35), con el publicador de la plataforma, que resuelve `outbox` por search_path.
// El pool de este módulo tiene search_path=solicitudes, así que se apunta a
// viajes SOLO dentro de esta transacción (set_config local) y se restaura al
// terminar; si algo falla, el rollback de la transacción lo descarta igual.
//
// Es un truco frágil: lo que se publique en esta transacción DESPUÉS de restaurar
// va a solicitudes.outbox, pero si alguien mueve la restauración o agrega un
// evento solicitud.* antes de ella, iría a viajes.outbox (el rol tiene INSERT ahí)
// y su relay lo publicaría sin error. Lo fija TestPublicarEnViajesRestauraElSearchPath.
// Alternativa si estorba: INSERT directo en viajes.outbox con SQL propio.
func (s *Servicio) publicarEnViajes(ctx context.Context, tx pgx.Tx, eventos ...outbox.Event) error {
	var previo string
	if err := tx.QueryRow(ctx, `SELECT current_setting('search_path')`).Scan(&previo); err != nil {
		return fmt.Errorf("solicitudes: leer search_path: %w", err)
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('search_path', 'viajes', true)`); err != nil {
		return fmt.Errorf("solicitudes: apuntar a viajes.outbox: %w", err)
	}
	if err := s.pub.Publish(ctx, tx, eventos...); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('search_path', $1, true)`, previo); err != nil {
		return fmt.Errorf("solicitudes: restaurar search_path: %w", err)
	}
	return nil
}
