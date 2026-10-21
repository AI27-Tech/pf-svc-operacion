package viajes

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	operclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/operacion"
	platformerrors "github.com/AI27-Tech/pf-pkg-platform-go/errors"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Estadía (DEC-038) es una PARADA del viaje (combustible, descanso, retén, …),
// CONDICIÓN ORTOGONAL al progreso: abrirla o cerrarla NO cambia viaje.estado. Un
// viaje puede tener a la vez una incidencia abierta y una estadía abierta.
//
// No hay duración persistida, ni campo/bandera `prolongada`, ni umbral o parámetro.
// La DURACIÓN se DERIVA de (inicio, cierre) en la lectura, por quien la consuma;
// este servicio solo guarda las dos marcas.

// motivosEstadiaValidos es el catálogo cerrado de MotivoEstadia (contrato,
// DEC-038). Un motivo fuera del catálogo se rechaza por validación.
var motivosEstadiaValidos = map[operclient.MotivoEstadia]bool{
	operclient.Combustible:         true,
	operclient.Descanso:            true,
	operclient.Alimentos:           true,
	operclient.Sanitario:           true,
	operclient.Reten:               true,
	operclient.RevisionAutoridades: true,
	operclient.Gastos:              true,
	operclient.EsperaEntrega:       true,
}

// ejesAutorizacionValidos es el catálogo cerrado de EjeAutorizacionEstadia
// (contrato, E1-HU-008).
var ejesAutorizacionValidos = map[operclient.EjeAutorizacionEstadia]bool{
	operclient.Autorizada:   true,
	operclient.NoAutorizada: true,
	operclient.Antes200km:   true,
}

// AbrirEstadia abre una estadía sobre el viaje. Fija inicio = momento de registro
// (DEFAULT now() de la base) y deja cierre NULL (abierta). NO cambia viaje.estado.
func (s *Servicio) AbrirEstadia(ctx context.Context, viajeID operclient.Id, in operclient.EstadiaEntrada) (*operclient.Estadia, error) {
	if !motivosEstadiaValidos[in.Motivo] {
		return nil, platformerrors.NewValidacion("operacion: motivo de estadía inválido %q", in.Motivo)
	}
	if !ejesAutorizacionValidos[in.EjeAutorizacion] {
		return nil, platformerrors.NewValidacion("operacion: eje_autorizacion de estadía inválido %q", in.EjeAutorizacion)
	}

	tx, err := s.tx.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	id := uuid.New()
	var inicio time.Time
	err = tx.QueryRow(ctx,
		`INSERT INTO viajes.estadia (id, viaje_id, motivo, eje_autorizacion)
		 VALUES ($1, $2, $3, $4)
		 RETURNING inicio`,
		id, viajeID, string(in.Motivo), string(in.EjeAutorizacion),
	).Scan(&inicio)
	if err != nil {
		return nil, fmt.Errorf("operacion: abrir estadía: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("operacion: commit abrir estadía: %w", err)
	}

	return &operclient.Estadia{
		Id:              id,
		ViajeId:         viajeID,
		Motivo:          in.Motivo,
		EjeAutorizacion: in.EjeAutorizacion,
		Inicio:          inicio.UTC(),
	}, nil
}

// CerrarEstadia cierra una estadía abierta fijando cierre = momento de registro.
// Cuerpo vacío (CierreEstadiaEntrada): el servidor fija el cierre. NO cambia
// viaje.estado. Cerrar una estadía ya cerrada o inexistente → NoEncontrado.
func (s *Servicio) CerrarEstadia(ctx context.Context, viajeID, estadiaID operclient.Id) (*operclient.Estadia, error) {
	tx, err := s.tx.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	cierre := time.Now().UTC()
	var (
		motivo string
		eje    string
		inicio time.Time
	)
	err = tx.QueryRow(ctx,
		`UPDATE viajes.estadia
		    SET cierre = $1
		  WHERE id = $2 AND viaje_id = $3 AND cierre IS NULL
		 RETURNING motivo, eje_autorizacion, inicio`,
		cierre, estadiaID, viajeID,
	).Scan(&motivo, &eje, &inicio)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, platformerrors.NewNoEncontrado("operacion: no hay una estadía abierta %s en el viaje %s", estadiaID, viajeID)
	}
	if err != nil {
		return nil, fmt.Errorf("operacion: cerrar estadía: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("operacion: commit cerrar estadía: %w", err)
	}

	return &operclient.Estadia{
		Id:              estadiaID,
		ViajeId:         viajeID,
		Motivo:          operclient.MotivoEstadia(motivo),
		EjeAutorizacion: operclient.EjeAutorizacionEstadia(eje),
		Inicio:          inicio.UTC(),
		Cierre:          &cierre,
	}, nil
}

// ListarEstadias devuelve las estadías de un viaje, de las más antiguas a las más
// recientes (orden de ocurrencia), con paginación por cursor keyset sobre
// (inicio, id). La duración de cada estadía NO viene en la respuesta: se deriva de
// (inicio, cierre), que sí vienen, por quien la consuma (DEC-038).
func (s *Servicio) ListarEstadias(ctx context.Context, viajeID operclient.Id, params operclient.ListarEstadiasParams) (*operclient.PaginaEstadias, error) {
	tx, err := s.tx.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	conds := []string{"viaje_id = $1"}
	args := []any{viajeID}
	n := 2
	if params.Cursor != nil && *params.Cursor != "" {
		cInicio, cID, err := decodeCursor(*params.Cursor)
		if err != nil {
			return nil, platformerrors.NewValidacion("operacion: cursor inválido")
		}
		conds = append(conds, fmt.Sprintf("(inicio, id) > ($%d, $%d)", n, n+1))
		args = append(args, cInicio, cID)
		n += 2
	}
	where := "WHERE " + strings.Join(conds, " AND ")
	args = append(args, paginaMax+1) // +1 para saber si hay página siguiente
	q := fmt.Sprintf(
		`SELECT id, motivo, eje_autorizacion, inicio, cierre
		   FROM viajes.estadia %s
		  ORDER BY inicio ASC, id ASC LIMIT $%d`, where, n)

	rows, err := tx.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("operacion: listar estadías: %w", err)
	}
	items := []operclient.Estadia{}
	for rows.Next() {
		var (
			id     uuid.UUID
			motivo string
			eje    string
			inicio time.Time
			cierre *time.Time
		)
		if err := rows.Scan(&id, &motivo, &eje, &inicio, &cierre); err != nil {
			rows.Close()
			return nil, fmt.Errorf("operacion: escanear estadía: %w", err)
		}
		e := operclient.Estadia{
			Id:              id,
			ViajeId:         viajeID,
			Motivo:          operclient.MotivoEstadia(motivo),
			EjeAutorizacion: operclient.EjeAutorizacionEstadia(eje),
			Inicio:          inicio.UTC(),
		}
		if cierre != nil {
			c := cierre.UTC()
			e.Cierre = &c
		}
		items = append(items, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("operacion: iterar estadías: %w", err)
	}

	var cursorSig *string
	if len(items) > paginaMax {
		last := items[paginaMax-1]
		cur := encodeCursor(last.Inicio, last.Id)
		cursorSig = &cur
		items = items[:paginaMax]
	}

	return &operclient.PaginaEstadias{Items: items, CursorSiguiente: cursorSig}, nil
}

// CerrarEstadiaActivaPorReinicio cierra la estadía activa (cierre NULL) de un
// viaje, fijando cierre = ahora. Es el efecto del "reinicio de viaje" (DEC-013)
// sobre la estadía: el reinicio cierra la estadía activa SIN ser una transición
// de estado (no toca viaje.estado). Idempotente: si no hay estadía activa, no
// hace nada y no es error. Devuelve cuántas estadías cerró (0 o 1 por el único
// parcial implícito de "una abierta por viaje" en la práctica).
//
// Hoy no hay un caso de uso de "reinicio" que lo invoque end-to-end; se expone
// como método para que ese flujo, cuando exista, lo llame. Internamente delega en
// cerrarEstadiaActivaEnTx, que un reinicio real usaría dentro de SU propia tx
// (junto con lo demás que el reinicio haga), para no partir la atomicidad.
func (s *Servicio) CerrarEstadiaActivaPorReinicio(ctx context.Context, viajeID operclient.Id) (int64, error) {
	tx, err := s.tx.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	n, err := cerrarEstadiaActivaEnTx(ctx, tx, viajeID, time.Now().UTC())
	if err != nil {
		return 0, err
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("operacion: commit cerrar estadía activa (reinicio): %w", err)
	}
	return n, nil
}

// cerrarEstadiaActivaEnTx cierra la estadía activa (cierre NULL) de un viaje
// dentro de una transacción ya abierta, fijando cierre = `cuando`. Idempotente:
// si no hay estadía activa, no hace nada y no es error. Devuelve el número de
// filas afectadas. Es el núcleo que un reinicio de viaje real invoca dentro de su
// propia tx.
func cerrarEstadiaActivaEnTx(ctx context.Context, tx pgx.Tx, viajeID operclient.Id, cuando time.Time) (int64, error) {
	ct, err := tx.Exec(ctx,
		`UPDATE viajes.estadia SET cierre = $1
		  WHERE viaje_id = $2 AND cierre IS NULL`,
		cuando.UTC(), viajeID,
	)
	if err != nil {
		return 0, fmt.Errorf("operacion: cerrar estadía activa (reinicio): %w", err)
	}
	return ct.RowsAffected(), nil
}
