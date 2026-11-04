// Package roster es la lectura del roster de `unidades` (ADR 31 §5 / ADR 33):
// paquetes pre-armados de un vehículo con uno o dos custodios, referidos por
// identificador de catalogos. Lee con el rol dueño op_roster (I1).
//
// Solo lectura en esta rebanada (listarUnidades, E8-T10). El alta y la edición
// del roster (crearUnidad/modificarUnidad) no están montadas todavía.
package roster

import (
	"context"
	"encoding/base64"
	"fmt"

	operclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/operacion"
	platformerrors "github.com/AI27-Tech/pf-pkg-platform-go/errors"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// BeginTx es la frontera transaccional; la cumple *pgxpool.Pool.
type BeginTx interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Servicio lee el roster con el pool de op_roster.
type Servicio struct {
	tx BeginTx
}

// NewServicio recibe el pool del rol op_roster.
func NewServicio(tx BeginTx) *Servicio {
	return &Servicio{tx: tx}
}

// paginaMax es el tamaño de página de Listar.
// shortcut: tamaño fijo como en viajes; hacerlo configurable si el panel lo pide.
const paginaMax = 50

// Columnas es la proyección de una unidad con sus custodios agregados, en el
// orden que espera Escanear. Requiere `roster.unidad u` y un LEFT JOIN de
// `roster.unidad_custodio c`, agrupando por las columnas de u.
const Columnas = `u.id, u.tipo, u.vehiculo_id, u.activa,
	COALESCE(array_agg(c.custodio_id ORDER BY c.custodio_id)
	         FILTER (WHERE c.custodio_id IS NOT NULL), '{}')`

// Escanear lee una fila con la forma de Columnas. Es la única copia del mapeo
// fila → Unidad; la comparten Listar y la escolta vigente de asignacion.
func Escanear(row pgx.Row) (operclient.Unidad, error) {
	var (
		u         operclient.Unidad
		tipo      string
		vehiculo  string
		custodios []string
	)
	if err := row.Scan(&u.Id, &tipo, &vehiculo, &u.Activa, &custodios); err != nil {
		return u, fmt.Errorf("operacion: escanear unidad: %w", err)
	}
	t := operclient.UnidadTipo(tipo)
	u.Tipo = &t
	var err error
	if u.VehiculoId, err = uuid.Parse(vehiculo); err != nil {
		return u, fmt.Errorf("operacion: vehiculo_id inválido en la unidad %s: %w", u.Id, err)
	}
	u.CustodiosIds = make([]operclient.Id, 0, len(custodios))
	for _, c := range custodios {
		id, err := uuid.Parse(c)
		if err != nil {
			return u, fmt.Errorf("operacion: custodio_id inválido en la unidad %s: %w", u.Id, err)
		}
		u.CustodiosIds = append(u.CustodiosIds, id)
	}
	return u, nil
}

// Listar devuelve una página del roster ordenada por id, activas e inactivas
// (el contrato expone `activa`; filtrar es del consumidor). Sin aptitud en V1.
// El cursor es el id de la última unidad de la página anterior, opaco.
func (s *Servicio) Listar(ctx context.Context, params operclient.ListarUnidadesParams) (*operclient.PaginaUnidades, error) {
	var desde *uuid.UUID
	if params.Cursor != nil && *params.Cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(*params.Cursor)
		if err != nil {
			return nil, platformerrors.NewValidacion("operacion: cursor inválido")
		}
		id, err := uuid.ParseBytes(raw)
		if err != nil {
			return nil, platformerrors.NewValidacion("operacion: cursor inválido")
		}
		desde = &id
	}

	tx, err := s.tx.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("operacion: abrir transacción de lectura del roster: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	rows, err := tx.Query(ctx,
		`SELECT `+Columnas+`
		   FROM roster.unidad u
		   LEFT JOIN roster.unidad_custodio c ON c.unidad_id = u.id
		  WHERE $1::uuid IS NULL OR u.id > $1
		  GROUP BY u.id, u.tipo, u.vehiculo_id, u.activa
		  ORDER BY u.id
		  LIMIT $2`,
		desde, paginaMax+1) // +1 para saber si hay página siguiente
	if err != nil {
		return nil, fmt.Errorf("operacion: listar roster: %w", err)
	}
	defer rows.Close()

	items := []operclient.Unidad{}
	for rows.Next() {
		u, err := Escanear(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("operacion: iterar roster: %w", err)
	}

	var cursorSig *string
	if len(items) > paginaMax {
		items = items[:paginaMax]
		cur := base64.RawURLEncoding.EncodeToString([]byte(items[paginaMax-1].Id.String()))
		cursorSig = &cur
	}
	return &operclient.PaginaUnidades{Items: items, CursorSiguiente: cursorSig}, nil
}
