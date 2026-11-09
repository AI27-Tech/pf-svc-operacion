// Package asignacion arma la escolta de un viaje en UNA transacción indivisible
// (operacion.md (a), ADR 31): reservar las unidades por ventana + transicionar el
// viaje APROBADO→ASIGNADO + publicar viaje.asignado + auditar, todo o nada.
//
// La corrección de "no sobrevender una unidad" NO vive en el código: la delega a
// la base con la restricción de exclusión GiST de asignacion.reserva_recurso. Un
// solape se vuelve un error de la base que el caso de uso traduce a 409.
package asignacion

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/estados"
	operclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/operacion"
	platformerrors "github.com/AI27-Tech/pf-pkg-platform-go/errors"
	"github.com/AI27-Tech/pf-pkg-platform-go/outbox"
	"github.com/AI27-Tech/pf-svc-operacion/internal/auditoria"
	"github.com/AI27-Tech/pf-svc-operacion/internal/roster"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// BeginTx es la frontera transaccional (Unit of Work). La cumple el pool del rol
// op_asignacion, que por la ÚNICA excepción de GRANT del sistema (ADR 31/35)
// puede además escribir en viajes y auditoria dentro de la misma tx.
type BeginTx interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// DuracionReserva es cuánto dura la reserva de una unidad a partir de la fecha de
// servicio del viaje. Regla de negocio (Producto, 2026-10-12): 3 horas. Acota la
// ventana de exclusión; registrarla en business-rules.md.
const DuracionReserva = 3 * time.Hour

// Servicio es el caso de uso de asignación.
type Servicio struct {
	tx  BeginTx
	pub *outbox.Publisher
}

// NewServicio recibe el pool del rol op_asignacion y el publicador de outbox (que
// escribe en viajes.outbox vía la excepción de GRANT; su conexión necesita
// search_path=viajes para resolver la tabla sin calificar).
func NewServicio(tx BeginTx, pub *outbox.Publisher) *Servicio {
	return &Servicio{tx: tx, pub: pub}
}

// Asignar reserva la escolta y mueve el viaje a ASIGNADO, atómicamente. actorID
// es el sujeto del token para la auditoría (vacío si no hay).
func (s *Servicio) Asignar(ctx context.Context, viajeID operclient.Id, in operclient.AsignacionEntrada, actorID string) (*operclient.Asignacion, error) {
	if len(in.UnidadesIds) == 0 {
		return nil, platformerrors.NewValidacion("operacion: la asignación requiere al menos una unidad")
	}

	tx, err := s.tx.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Cargar el viaje: debe existir, estar APROBADO y tener fecha de servicio
	// (la ventana de reserva nace de ahí).
	var estado string
	var fhs *time.Time
	err = tx.QueryRow(ctx,
		`SELECT estado, fecha_hora_servicio FROM viajes.viaje WHERE id = $1`, viajeID,
	).Scan(&estado, &fhs)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, platformerrors.NewNoEncontrado("operacion: viaje %s no encontrado", viajeID)
	}
	if err != nil {
		return nil, fmt.Errorf("operacion: cargar viaje: %w", err)
	}
	if estado != string(operclient.APROBADO) {
		return nil, platformerrors.NewConflicto("operacion: solo se asigna un viaje APROBADO; está en %s", estado)
	}
	if fhs == nil {
		return nil, platformerrors.NewValidacion("operacion: asignar requiere fecha_hora_servicio en el viaje")
	}
	inicio := fhs.UTC()
	fin := inicio.Add(DuracionReserva)

	// Todo el cambio va envuelto por el interceptor de auditoría: el registro
	// probatorio se escribe en la MISMA tx, sin ruta que lo omita (P3).
	reg := auditoria.Registro{ViajeID: viajeID.String(), Modulo: "asignacion", Accion: "asignar", ActorID: actorID}
	err = auditoria.Interceptar(ctx, tx, reg, func(ctx context.Context, tx pgx.Tx) error {
		// Candado compartido con la edición del roster (SinReservaVigente); con
		// él tomado, el roster leído es el confirmado y nadie lo cambia en medio.
		if err := bloquearUnidades(ctx, tx, in.UnidadesIds); err != nil {
			return err
		}
		if err := verificarUnidades(ctx, tx, in.UnidadesIds); err != nil {
			return err
		}
		asigID := uuid.New()
		if _, err := tx.Exec(ctx,
			`INSERT INTO asignacion.asignacion_viaje (id, viaje_id) VALUES ($1, $2)`, asigID, viajeID,
		); err != nil {
			return fmt.Errorf("operacion: crear asignación: %w", err)
		}

		// Reservar cada unidad por la ventana. La exclusión GiST corta el
		// sobrevendido: dos reservas de la misma unidad con ventanas que se
		// solapan no coexisten.
		for _, unidadID := range in.UnidadesIds {
			if _, err := tx.Exec(ctx,
				`INSERT INTO asignacion.reserva_recurso (id, asignacion_id, unidad_id, ventana)
				 VALUES ($1, $2, $3, tstzrange($4, $5, '[)'))`,
				uuid.New(), asigID, unidadID, inicio, fin,
			); err != nil {
				if esViolacionExclusion(err) {
					return platformerrors.NewConflicto("operacion: la unidad %s ya está reservada en esa ventana", unidadID)
				}
				return fmt.Errorf("operacion: reservar unidad %s: %w", unidadID, err)
			}
		}

		if _, err := tx.Exec(ctx,
			`INSERT INTO asignacion.historia_asignacion (viaje_id, accion) VALUES ($1, 'asignar')`, viajeID,
		); err != nil {
			return fmt.Errorf("operacion: registrar historia de asignación: %w", err)
		}

		// Transición del viaje en la misma tx, vía la excepción de GRANT sobre
		// viajes. APROBADO→ASIGNADO es la única arista que la asignación recorre.
		if _, err := tx.Exec(ctx,
			`UPDATE viajes.viaje SET estado = 'ASIGNADO' WHERE id = $1`, viajeID,
		); err != nil {
			return fmt.Errorf("operacion: transicionar viaje a ASIGNADO: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO viajes.transicion_viaje (viaje_id, de_estado, a_estado, causa)
			 VALUES ($1, 'APROBADO', 'ASIGNADO', NULL)`, viajeID,
		); err != nil {
			return fmt.Errorf("operacion: registrar transición: %w", err)
		}

		// viaje.asignado al outbox único de viajes (ADR 35), misma tx.
		idsStr := make([]string, len(in.UnidadesIds))
		for i, u := range in.UnidadesIds {
			idsStr[i] = u.String()
		}
		return s.pub.Publish(ctx, tx, outbox.Event{
			Tipo:       "viaje.asignado",
			Version:    1,
			AgregadoID: viajeID.String(), // clave de orden = viaje_id
			Payload: map[string]any{
				"viaje_id":     viajeID.String(),
				"unidades_ids": idsStr,
				"asignada_en":  time.Now().UTC(),
			},
		})
	})
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("operacion: commit asignación: %w", err)
	}

	// Respuesta: las unidades reservadas. La aptitud es informativa (DEC-055) y
	// se enriquece aparte (consulta al puerto de aptitud); aquí va vacía.
	unidades := make([]operclient.UnidadAsignada, 0, len(in.UnidadesIds))
	for _, u := range in.UnidadesIds {
		unidades = append(unidades, operclient.UnidadAsignada{UnidadId: u, Aptitud: []operclient.AptitudRecurso{}})
	}
	// Estado: la asignación acaba de transicionar el viaje APROBADO→ASIGNADO en
	// la misma tx; se afirma en el dato (tracer.20) en vez de inferirlo del éxito.
	return &operclient.Asignacion{Unidades: unidades, ViajeId: viajeID, Estado: operclient.ASIGNADO}, nil
}

// Reasignar cambia la escolta de un viaje YA asignado, atómicamente: libera la
// asignación vigente, reserva la nueva escolta en la misma ventana de servicio,
// registra el motivo y publica viaje.reasignado. NO transiciona el estado del
// viaje (ya está asignado o más avanzado): solo cambia qué unidades lo escoltan.
//
// Se permite desde ASIGNADO en adelante y hasta antes de un estado terminal; en
// vez de enumerar estados (que la máquina puede cambiar), lo expresa con dos
// condiciones estables: el viaje no es terminal (EsTerminal) y tiene una
// asignación vigente (lo que por construcción descarta SOLICITADO/APROBADO).
func (s *Servicio) Reasignar(ctx context.Context, viajeID operclient.Id, in operclient.ReasignacionEntrada, actorID string) (*operclient.Asignacion, error) {
	if len(in.UnidadesIds) == 0 {
		return nil, platformerrors.NewValidacion("operacion: la reasignación requiere al menos una unidad")
	}
	if strings.TrimSpace(in.Motivo) == "" {
		return nil, platformerrors.NewValidacion("operacion: la reasignación exige un motivo")
	}

	tx, err := s.tx.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// El viaje debe existir, no estar en un estado terminal y tener fecha de
	// servicio (la ventana de la nueva reserva nace de ahí).
	var estado string
	var fhs *time.Time
	err = tx.QueryRow(ctx,
		`SELECT estado, fecha_hora_servicio FROM viajes.viaje WHERE id = $1`, viajeID,
	).Scan(&estado, &fhs)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, platformerrors.NewNoEncontrado("operacion: viaje %s no encontrado", viajeID)
	}
	if err != nil {
		return nil, fmt.Errorf("operacion: cargar viaje: %w", err)
	}
	if estados.EstadoViaje(estado).EsTerminal() {
		return nil, platformerrors.NewConflicto("operacion: no se reasigna un viaje en estado terminal (%s)", estado)
	}
	if fhs == nil {
		return nil, platformerrors.NewValidacion("operacion: reasignar requiere fecha_hora_servicio en el viaje")
	}
	inicio := fhs.UTC()
	fin := inicio.Add(DuracionReserva)

	reg := auditoria.Registro{ViajeID: viajeID.String(), Modulo: "asignacion", Accion: "reasignar", ActorID: actorID}
	err = auditoria.Interceptar(ctx, tx, reg, func(ctx context.Context, tx pgx.Tx) error {
		// Candado compartido con la edición del roster (SinReservaVigente); con
		// él tomado, el roster leído es el confirmado y nadie lo cambia en medio.
		if err := bloquearUnidades(ctx, tx, in.UnidadesIds); err != nil {
			return err
		}
		if err := verificarUnidades(ctx, tx, in.UnidadesIds); err != nil {
			return err
		}
		// Liberar la asignación vigente. Si no hay ninguna, el viaje nunca fue
		// asignado: no hay nada que reasignar (descarta SOLICITADO/APROBADO).
		var vigenteID uuid.UUID
		err := tx.QueryRow(ctx,
			`UPDATE asignacion.asignacion_viaje SET liberada_en = now()
			  WHERE viaje_id = $1 AND liberada_en IS NULL
			  RETURNING id`, viajeID,
		).Scan(&vigenteID)
		if errors.Is(err, pgx.ErrNoRows) {
			return platformerrors.NewConflicto("operacion: el viaje %s no tiene una asignación vigente que reasignar", viajeID)
		}
		if err != nil {
			return fmt.Errorf("operacion: liberar asignación vigente: %w", err)
		}

		// Las reservas de la asignación liberada dejan de solapar: se borran para
		// que la exclusión GiST no choque contra la escolta nueva (una unidad que
		// se mantiene puede volver a reservarse en la misma ventana).
		if _, err := tx.Exec(ctx,
			`DELETE FROM asignacion.reserva_recurso WHERE asignacion_id = $1`, vigenteID,
		); err != nil {
			return fmt.Errorf("operacion: liberar reservas anteriores: %w", err)
		}

		asigID := uuid.New()
		if _, err := tx.Exec(ctx,
			`INSERT INTO asignacion.asignacion_viaje (id, viaje_id) VALUES ($1, $2)`, asigID, viajeID,
		); err != nil {
			return fmt.Errorf("operacion: crear asignación: %w", err)
		}

		for _, unidadID := range in.UnidadesIds {
			if _, err := tx.Exec(ctx,
				`INSERT INTO asignacion.reserva_recurso (id, asignacion_id, unidad_id, ventana)
				 VALUES ($1, $2, $3, tstzrange($4, $5, '[)'))`,
				uuid.New(), asigID, unidadID, inicio, fin,
			); err != nil {
				if esViolacionExclusion(err) {
					return platformerrors.NewConflicto("operacion: la unidad %s ya está reservada en esa ventana", unidadID)
				}
				return fmt.Errorf("operacion: reservar unidad %s: %w", unidadID, err)
			}
		}

		if _, err := tx.Exec(ctx,
			`INSERT INTO asignacion.historia_asignacion (viaje_id, accion, motivo) VALUES ($1, 'reasignar', $2)`,
			viajeID, in.Motivo,
		); err != nil {
			return fmt.Errorf("operacion: registrar historia de reasignación: %w", err)
		}

		// Reasignar NO transiciona el estado del viaje: ya está asignado o más
		// avanzado y solo cambia la escolta.
		idsStr := make([]string, len(in.UnidadesIds))
		for i, u := range in.UnidadesIds {
			idsStr[i] = u.String()
		}
		return s.pub.Publish(ctx, tx, outbox.Event{
			Tipo:       "viaje.reasignado",
			Version:    1,
			AgregadoID: viajeID.String(), // clave de orden = viaje_id
			Payload: map[string]any{
				"viaje_id":      viajeID.String(),
				"unidades_ids":  idsStr,
				"motivo":        in.Motivo,
				"reasignado_en": time.Now().UTC(),
			},
		})
	})
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("operacion: commit reasignación: %w", err)
	}

	unidades := make([]operclient.UnidadAsignada, 0, len(in.UnidadesIds))
	for _, u := range in.UnidadesIds {
		unidades = append(unidades, operclient.UnidadAsignada{UnidadId: u, Aptitud: []operclient.AptitudRecurso{}})
	}
	// Estado: la reasignación NO transiciona el viaje (ya está ASIGNADO o más
	// avanzado); se afirma el estado actual ya cargado, no uno fijo.
	return &operclient.Asignacion{Unidades: unidades, ViajeId: viajeID, Estado: operclient.EstadoViaje(estado)}, nil
}

// esViolacionExclusion detecta la violación de la restricción de exclusión GiST
// (SQLSTATE 23P01): una unidad ya reservada en una ventana que se solapa.
func esViolacionExclusion(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23P01"
}

// bloquearUnidades toma, hasta el fin de la tx, un candado consultivo por unidad
// y en orden de id (dos asignaciones con unidades cruzadas no se interbloquean).
// Es el candado que comparten la reserva (Asignar/Reasignar) y la edición del
// roster (SinReservaVigente): sin él, el roster podría revisar "sin reserva" y
// una asignación reservar la unidad antes de que la edición confirme. Es
// consultivo y no un FOR UPDATE sobre roster.unidad porque op_asignacion solo
// tiene SELECT sobre el roster (FOR UPDATE exige UPDATE).
func bloquearUnidades(ctx context.Context, tx pgx.Tx, ids []uuid.UUID) error {
	for _, id := range slices.SortedFunc(slices.Values(ids), func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) }) {
		if _, err := tx.Exec(ctx,
			`SELECT pg_advisory_xact_lock(hashtextextended('roster.unidad:' || $1::text, 0))`, id.String(),
		); err != nil {
			return fmt.Errorf("operacion: bloquear la unidad %s: %w", id, err)
		}
	}
	return nil
}

// verificarUnidades exige que cada unidad a reservar exista en el roster (400)
// y esté activa (409): una unidad desactivada no está disponible para
// asignarse (contrato, `Unidad.activa`; DEC-142 #2). Lee roster.unidad con el
// SELECT que op_asignacion ya tiene (ADR 33).
func verificarUnidades(ctx context.Context, tx pgx.Tx, ids []uuid.UUID) error {
	rows, err := tx.Query(ctx, `SELECT id, activa FROM roster.unidad WHERE id = ANY($1)`, ids)
	if err != nil {
		return fmt.Errorf("operacion: leer unidades del roster: %w", err)
	}
	activa := map[uuid.UUID]bool{}
	for rows.Next() {
		var id uuid.UUID
		var a bool
		if err := rows.Scan(&id, &a); err != nil {
			rows.Close()
			return fmt.Errorf("operacion: leer unidad del roster: %w", err)
		}
		activa[id] = a
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("operacion: iterar unidades del roster: %w", err)
	}
	for _, id := range ids {
		a, ok := activa[id]
		if !ok {
			return platformerrors.NewValidacion("operacion: la unidad %s no existe en el roster", id)
		}
		if !a {
			return platformerrors.NewConflicto("operacion: la unidad %s está desactivada; no se puede asignar", id)
		}
	}
	return nil
}

// SinReservaVigente es la guarda de edición del roster (DEC-142 #5, puerto
// roster.Reservas). Con la unidad bloqueada, busca sus reservas en viajes no
// terminales; si hay alguna responde 409 unidad_con_reserva_vigente, y si no,
// corre fn (la edición, en su propia tx de op_roster) sin soltar el candado. Lee
// con op_asignacion, que ya ve asignacion y viajes.viaje: op_roster no recibe un
// GRANT cruzado nuevo (I1, ADR 33).
//
// El candado se toma en ESTA tx, antes de que fn pida su conexión de op_roster:
// así el orden de adquisición es siempre op_asignacion -> op_roster y un pool
// agotado por asignaciones que esperan el candado no deja a fn sin conexión.
func (s *Servicio) SinReservaVigente(ctx context.Context, unidadID uuid.UUID, fn func(context.Context) error) error {
	tx, err := s.tx.Begin(ctx)
	if err != nil {
		return fmt.Errorf("operacion: abrir transacción de la guarda de reserva: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // solo lee: el rollback suelta el candado

	if err := bloquearUnidades(ctx, tx, []uuid.UUID{unidadID}); err != nil {
		return err
	}
	rows, err := tx.Query(ctx,
		`SELECT v.id, v.estado
		   FROM asignacion.reserva_recurso r
		   JOIN asignacion.asignacion_viaje a ON a.id = r.asignacion_id
		   JOIN viajes.viaje v ON v.id = a.viaje_id
		  WHERE r.unidad_id = $1 AND a.liberada_en IS NULL`, unidadID)
	if err != nil {
		return fmt.Errorf("operacion: leer reservas de la unidad %s: %w", unidadID, err)
	}
	defer rows.Close()
	for rows.Next() {
		var viajeID uuid.UUID
		var estado string
		if err := rows.Scan(&viajeID, &estado); err != nil {
			return fmt.Errorf("operacion: leer reserva de la unidad %s: %w", unidadID, err)
		}
		if !estados.EstadoViaje(estado).EsTerminal() {
			return platformerrors.NewConflictoCodigo("unidad_con_reserva_vigente",
				"operacion: la unidad %s tiene reserva en el viaje %s (%s); reasigna la escolta primero", unidadID, viajeID, estado)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("operacion: iterar reservas de la unidad %s: %w", unidadID, err)
	}
	rows.Close()
	return fn(ctx)
}

// Escolta devuelve las unidades de la asignación vigente del viaje, cada una con
// su tipo, vehículo y custodios del roster (obtenerAsignacion). Sin asignación
// vigente, unidades va vacío: no es error. Lo usa el bff para la escolta del
// detalle de Reacción (DEC-121 #2).
//
// Asignar y Reasignar solo reservan unidades del roster (verificarUnidades), así
// que una reserva sin su unidad en el roster es un dato roto: es error, no se
// omite en silencio.
func (s *Servicio) Escolta(ctx context.Context, viajeID operclient.Id) (*operclient.EscoltaVigente, error) {
	tx, err := s.tx.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("operacion: abrir transacción de lectura de la escolta: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx,
		`SELECT `+roster.Columnas+`
		   FROM asignacion.asignacion_viaje a
		   JOIN asignacion.reserva_recurso r ON r.asignacion_id = a.id
		   JOIN roster.unidad u ON u.id = r.unidad_id
		   LEFT JOIN roster.unidad_custodio c ON c.unidad_id = u.id
		  WHERE a.viaje_id = $1 AND a.liberada_en IS NULL
		  GROUP BY u.id, u.tipo, u.vehiculo_id, u.activa
		  ORDER BY u.id`,
		viajeID)
	if err != nil {
		return nil, fmt.Errorf("operacion: leer la escolta del viaje: %w", err)
	}
	defer rows.Close()

	unidades := []operclient.Unidad{}
	for rows.Next() {
		u, err := roster.Escanear(rows)
		if err != nil {
			return nil, err
		}
		unidades = append(unidades, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("operacion: iterar la escolta: %w", err)
	}
	var reservadas int
	if err := tx.QueryRow(ctx,
		`SELECT count(DISTINCT r.unidad_id)
		   FROM asignacion.asignacion_viaje a
		   JOIN asignacion.reserva_recurso r ON r.asignacion_id = a.id
		  WHERE a.viaje_id = $1 AND a.liberada_en IS NULL`, viajeID,
	).Scan(&reservadas); err != nil {
		return nil, fmt.Errorf("operacion: contar la escolta reservada: %w", err)
	}
	if reservadas != len(unidades) {
		return nil, fmt.Errorf("operacion: la escolta del viaje %s reserva %d unidades y el roster tiene %d", viajeID, reservadas, len(unidades))
	}
	return &operclient.EscoltaVigente{ViajeId: viajeID, Unidades: unidades}, nil
}
