package viajes

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	operclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/operacion"
	platformerrors "github.com/AI27-Tech/pf-pkg-platform-go/errors"
	"github.com/AI27-Tech/pf-pkg-platform-go/outbox"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Incidencia es una CONDICIÓN ORTOGONAL al progreso del viaje (§C.2, P15): abrirla
// o resolverla NO cambia viaje.estado. Un viaje está EN_TRANSITO *y* con
// incidencia a la vez. La condición vive en viajes.condicion_viaje
// (tipo='INCIDENCIA'); el índice único parcial `una_condicion_abierta_por_tipo`
// garantiza a lo más una incidencia abierta por viaje.
//
// UBICACIÓN DEL DETALLE: el detalle de la incidencia (tipo_interno, descripcion) se
// guarda en viajes.condicion_viaje, NO en bitacora.incidencia. abrirIncidencia
// escribe la condición, su detalle y el evento viaje.condicion_abierta (viajes.outbox)
// en UNA sola transacción con el rol op_viajes. Escribir además bitacora.incidencia
// en la misma tx exigiría una segunda excepción de GRANT cruzado (hoy solo la de
// op_asignacion), lo que requiere ADR (operacion.md §(e)). Por eso el caso de uso vive
// íntegro en viajes.

// rolesQuePuedenAbrirIncidencia son los roles facultados para ABRIR una
// incidencia (requisito del ticket E1-T6): operador, supervisor y administrador.
// Reacción NO (DEC-028): su visibilidad es solo del caso alertado, no abre
// condiciones. El cliente tampoco.
var rolesQuePuedenAbrirIncidencia = map[string]bool{
	"operador":      true,
	"supervisor":    true,
	"administrador": true,
}

// rolesQuePuedenResolverIncidencia son los roles facultados para RESOLVER una
// incidencia (requisito del ticket E1-T6): SOLO supervisor o administrador. Un
// operador puede abrir pero no resolver (rechazo por rol).
var rolesQuePuedenResolverIncidencia = map[string]bool{
	"supervisor":    true,
	"administrador": true,
}

// AbrirIncidencia abre una condición INCIDENCIA sobre el viaje, en una sola
// transacción (P17): inserta la condición en viajes.condicion_viaje
// (tipo='INCIDENCIA', cerrada_en NULL) con su detalle (tipo_interno, descripcion)
// y publica viaje.condicion_abierta por el outbox de viajes. NO cambia
// viaje.estado.
//
// rol es el rol del actor (claims.Rol): operador, supervisor o administrador.
// Reacción y cliente se rechazan por rol (DEC-028).
//
// Dos incidencias abiertas del mismo viaje las impide el índice único parcial
// `una_condicion_abierta_por_tipo`; la violación se traduce a Conflicto (409),
// nunca a un 500.
func (s *Servicio) AbrirIncidencia(ctx context.Context, viajeID operclient.Id, in operclient.IncidenciaEntrada, rol string) (*operclient.Incidencia, error) {
	if !rolesQuePuedenAbrirIncidencia[rol] {
		return nil, platformerrors.NewNoAutorizado("operacion: el rol %q no puede abrir una incidencia", rol)
	}
	if strings.TrimSpace(in.TipoInterno) == "" {
		return nil, platformerrors.NewValidacion("operacion: tipo_interno es obligatorio para abrir una incidencia")
	}

	tx, err := s.tx.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	id := uuid.New()

	// INSERT de la condición + detalle. abierta_en lo fija la base (DEFAULT now()),
	// se recupera con RETURNING para la respuesta. cerrada_en queda NULL (abierta).
	var abiertaEn time.Time
	err = tx.QueryRow(ctx,
		`INSERT INTO viajes.condicion_viaje (id, viaje_id, tipo, tipo_interno, descripcion)
		 VALUES ($1, $2, 'INCIDENCIA', $3, $4)
		 RETURNING abierta_en`,
		id, viajeID, in.TipoInterno, in.Descripcion,
	).Scan(&abiertaEn)
	if err != nil {
		if esViolacionUnicidad(err) {
			return nil, platformerrors.NewConflicto("operacion: el viaje %s ya tiene una incidencia abierta", viajeID)
		}
		return nil, fmt.Errorf("operacion: abrir incidencia: %w", err)
	}

	// viaje.condicion_abierta al outbox de viajes, en la misma tx (ADR 35). Payload
	// mínimo: no hay struct de payload generado para los eventos viaje.* (brecha de
	// contrato), así que la forma se fija a mano, acotada a la identidad de la
	// condición. clave de orden = viaje_id.
	if err := s.pub.Publish(ctx, tx, outbox.Event{
		Tipo:       "viaje.condicion_abierta",
		Version:    1,
		AgregadoID: viajeID.String(),
		Payload: map[string]any{
			"viaje_id":     viajeID.String(),
			"condicion_id": id.String(),
			"tipo":         "INCIDENCIA",
			"tipo_interno": in.TipoInterno,
			"abierta_en":   abiertaEn.UTC(),
		},
	}); err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("operacion: commit abrir incidencia: %w", err)
	}

	return &operclient.Incidencia{
		Id:          id,
		ViajeId:     viajeID,
		TipoInterno: in.TipoInterno,
		Estado:      operclient.EnAtencion,
		AbiertaEn:   abiertaEn.UTC(),
	}, nil
}

// ResolverIncidencia cierra la condición INCIDENCIA del viaje, en una sola
// transacción (P17): marca cerrada_en y publica viaje.condicion_cerrada. NO
// cambia viaje.estado —"el viaje regresa a su curso normal al resolverse" (§C.2),
// el progreso nunca se perdió—.
//
// rol es el rol del actor: SOLO supervisor o administrador (un operador que abrió
// no puede resolver, rechazo por rol). resolucionID es el id de la condición
// (el id público de la Incidencia).
//
// La resolución (texto) del contrato (ResolucionIncidenciaEntrada.resolucion) se
// exige no vacía. shortcut: el texto de resolución no tiene columna propia en
// condicion_viaje; se usa como causa narrativa del cierre y no se persiste por
// separado (igual criterio que el detalle: no se abre una segunda tabla). Cuando
// el detalle narrativo se mueva a bitacora.incidencia (requiere ADR por el GRANT
// cruzado), la resolución se guarda allí.
func (s *Servicio) ResolverIncidencia(ctx context.Context, viajeID, incidenciaID operclient.Id, in operclient.ResolucionIncidenciaEntrada, rol string) (*operclient.Incidencia, error) {
	if !rolesQuePuedenResolverIncidencia[rol] {
		return nil, platformerrors.NewNoAutorizado("operacion: el rol %q no puede resolver una incidencia", rol)
	}
	if strings.TrimSpace(in.Resolucion) == "" {
		return nil, platformerrors.NewValidacion("operacion: la resolución de la incidencia es obligatoria")
	}

	tx, err := s.tx.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Cerrar la condición marcando cerrada_en, SOLO si está abierta (cerrada_en
	// NULL): resolver dos veces no reabre ni re-cierra. Se recupera el detalle
	// para la respuesta.
	ahora := time.Now().UTC()
	var (
		tipoInterno *string
		abiertaEn   time.Time
	)
	err = tx.QueryRow(ctx,
		`UPDATE viajes.condicion_viaje
		    SET cerrada_en = $1
		  WHERE id = $2 AND viaje_id = $3 AND tipo = 'INCIDENCIA' AND cerrada_en IS NULL
		 RETURNING tipo_interno, abierta_en`,
		ahora, incidenciaID, viajeID,
	).Scan(&tipoInterno, &abiertaEn)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, platformerrors.NewNoEncontrado("operacion: no hay una incidencia abierta %s en el viaje %s", incidenciaID, viajeID)
	}
	if err != nil {
		return nil, fmt.Errorf("operacion: resolver incidencia: %w", err)
	}

	if err := s.pub.Publish(ctx, tx, outbox.Event{
		Tipo:       "viaje.condicion_cerrada",
		Version:    1,
		AgregadoID: viajeID.String(),
		Payload: map[string]any{
			"viaje_id":     viajeID.String(),
			"condicion_id": incidenciaID.String(),
			"tipo":         "INCIDENCIA",
			"cerrada_en":   ahora,
		},
	}); err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("operacion: commit resolver incidencia: %w", err)
	}

	ti := ""
	if tipoInterno != nil {
		ti = *tipoInterno
	}
	return &operclient.Incidencia{
		Id:          incidenciaID,
		ViajeId:     viajeID,
		TipoInterno: ti,
		Estado:      operclient.Resuelta,
		AbiertaEn:   abiertaEn.UTC(),
		ResueltaEn:  &ahora,
	}, nil
}

// esViolacionUnicidad detecta la violación de una restricción de unicidad
// (SQLSTATE 23505): aquí, el índice único parcial que impide dos condiciones
// abiertas del mismo tipo en un viaje.
func esViolacionUnicidad(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
