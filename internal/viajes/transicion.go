package viajes

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/estados"
	operclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/operacion"
	platformerrors "github.com/AI27-Tech/pf-pkg-platform-go/errors"
	"github.com/AI27-Tech/pf-pkg-platform-go/outbox"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Transicionar mueve un viaje a un estado nuevo. viajes es la ÚNICA autoridad de
// transición (P14): valida la arista contra la máquina de estados generada
// (estados.PuedeTransicionar), aplica la guarda que la arista tenga, y escribe
// el cambio de estado, su fila de historia y el evento viaje.estado_cambiado en
// la MISMA transacción (P17). Devuelve el viaje actualizado.
func (s *Servicio) Transicionar(ctx context.Context, id operclient.Id, in operclient.TransicionEntrada) (*operclient.Viaje, error) {
	destino := estados.EstadoViaje(string(in.AEstado))

	tx, err := s.tx.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		estadoAct string
		clienteID uuid.UUID
		origen    string
		fhs       *time.Time
		eta       *time.Time
		creado    time.Time
	)
	err = tx.QueryRow(ctx,
		`SELECT estado, cliente_id, origen, fecha_hora_servicio, eta_estimado, creado_en
		   FROM viajes.viaje WHERE id = $1`, id,
	).Scan(&estadoAct, &clienteID, &origen, &fhs, &eta, &creado)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, platformerrors.NewNoEncontrado("operacion: viaje %s no encontrado", id)
	}
	if err != nil {
		return nil, fmt.Errorf("operacion: cargar viaje: %w", err)
	}

	de := estados.EstadoViaje(estadoAct)
	if !estados.PuedeTransicionar(de, destino) {
		return nil, platformerrors.NewConflicto("operacion: transición no permitida de %s a %s", de, destino)
	}
	if g, ok := guardas[transicionKey{de, destino}]; ok {
		if err := g(datosGuarda{fechaHoraServicio: fhs}, in); err != nil {
			return nil, err
		}
	}

	if _, err := tx.Exec(ctx, `UPDATE viajes.viaje SET estado = $1 WHERE id = $2`, string(destino), id); err != nil {
		return nil, fmt.Errorf("operacion: actualizar estado: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO viajes.transicion_viaje (viaje_id, de_estado, a_estado, causa) VALUES ($1,$2,$3,$4)`,
		id, estadoAct, string(destino), in.Causa,
	); err != nil {
		return nil, fmt.Errorf("operacion: registrar transición: %w", err)
	}

	payload := map[string]any{
		"viaje_id":    id.String(),
		"de_estado":   estadoAct,
		"a_estado":    string(destino),
		"cambiado_en": time.Now().UTC(),
	}
	if in.Causa != nil {
		payload["causa"] = *in.Causa
	}
	if err := s.pub.Publish(ctx, tx, outbox.Event{
		Tipo:       "viaje.estado_cambiado",
		Version:    1,
		AgregadoID: id.String(), // clave de orden = viaje_id
		Payload:    payload,
	}); err != nil {
		return nil, err
	}

	// Los destinos para el viaje devuelto se leen DENTRO de la tx, antes del commit.
	destinos, err := s.destinosDe(ctx, tx, id)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("operacion: commit transición: %w", err)
	}

	cid := operclient.Id(clienteID)
	return &operclient.Viaje{
		Id:                id,
		Estado:            in.AEstado,
		ClienteId:         &cid,
		Origen:            origen,
		Destinos:          destinos,
		FechaHoraServicio: fhs,
		EtaEstimado:       eta,
		CreadoEn:          creado,
	}, nil
}

// transicionKey identifica una arista de la máquina de estados.
type transicionKey struct{ de, a estados.EstadoViaje }

// datosGuarda son los datos del viaje que una guarda puede necesitar.
type datosGuarda struct {
	fechaHoraServicio *time.Time
}

// guarda valida una arista más allá de que exista. Devuelve nil si pasa.
type guarda func(datosGuarda, operclient.TransicionEntrada) error

// guardas asocia cada arista con guarda a su validación. Las aristas SIN entrada
// no tienen guarda y pasan libres (modela el dominio como tabla, no como ifs
// dispersos). Las guardas que dependen de subsistemas aún no construidos se
// declaran como diferidas: rechazan la arista con un mensaje claro hasta que su
// rebanada las implemente (escolta → asignación; cierre → multi-parada /
// incidencia).
var guardas = map[transicionKey]guarda{
	{estados.EstadoSolicitado, estados.EstadoAprobado}:         guardaFechaHoraServicio,
	{estados.EstadoSolicitado, estados.EstadoRechazadoPorAi27}: guardaMotivo,
	{estados.EstadoAprobado, estados.EstadoAsignado}:           guardaDiferida("la asignación se hace con asignarRecursos (reserva de escolta), no con transiciones"),
	{estados.EstadoEnDestino, estados.EstadoCerrado}:           guardaDiferida("cerrar tras la última parada aún no está soportado (multi-parada pendiente)"),
	{estados.EstadoEnDescarga, estados.EstadoCerrado}:          guardaDiferida("cerrar tras la última parada aún no está soportado (multi-parada pendiente)"),
	{estados.EstadoEnTransito, estados.EstadoCerrado}:          guardaDiferida("el cierre anticipado requiere una incidencia abierta (E1-HU-007, pendiente)"),
}

// guardaFechaHoraServicio exige fecha_hora_servicio antes de aprobar (DEC-010 #2).
func guardaFechaHoraServicio(d datosGuarda, _ operclient.TransicionEntrada) error {
	if d.fechaHoraServicio == nil {
		return platformerrors.NewValidacion("operacion: aprobar un viaje exige fecha_hora_servicio definida")
	}
	return nil
}

// guardaMotivo exige una causa en el rechazo (DEC-020 #2).
func guardaMotivo(_ datosGuarda, in operclient.TransicionEntrada) error {
	if in.Causa == nil || strings.TrimSpace(*in.Causa) == "" {
		return platformerrors.NewValidacion("operacion: el rechazo exige una causa (motivo)")
	}
	return nil
}

// guardaDiferida rechaza una arista cuya guarda real depende de un subsistema que
// esta rebanada no construye todavía. 409 con mensaje claro.
func guardaDiferida(motivo string) guarda {
	return func(datosGuarda, operclient.TransicionEntrada) error {
		return platformerrors.NewConflicto("operacion: transición no soportada todavía: %s", motivo)
	}
}
