// Package roster es el roster de `unidades` (ADR 31 §5 / ADR 33): paquetes
// pre-armados de un vehículo con uno o dos custodios, referidos por
// identificador de catalogos. Lee y escribe con el rol dueño op_roster (I1).
//
// Lectura (listarUnidades, obtenerUnidad) y armado (crearUnidad,
// modificarUnidad, E8-T21, DEC-142). La exclusividad de vehículo y custodio
// entre unidades activas la garantiza la base (roster/0003_exclusividad.sql);
// la guarda de reserva vigente la presta asignacion (puerto Reservas).
package roster

import (
	"context"
	"encoding/base64"
	"fmt"

	catalogos "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/catalogos"
	operclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/operacion"
	platformerrors "github.com/AI27-Tech/pf-pkg-platform-go/errors"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// BeginTx es la frontera transaccional; la cumple *pgxpool.Pool.
type BeginTx interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Atomos es la misma forma del puerto operacion.AtomosCatalogo, declarada aquí
// para no importar la raíz (mismo patrón que internal/catalogoscache).
type Atomos interface {
	ObtenerVehiculo(ctx context.Context, vehiculoID catalogos.Id) (*catalogos.Vehiculo, error)
	ObtenerCustodio(ctx context.Context, custodioID catalogos.Id) (*catalogos.Custodio, error)
}

// Reservas es la guarda de reserva vigente (DEC-142 #5). La implementa
// asignacion con su rol dueño op_asignacion: op_roster no lee asignacion ni
// viajes (I1, sin GRANT cruzado nuevo).
type Reservas interface {
	// SinReservaVigente corre fn solo si la unidad no tiene reserva en un viaje
	// no terminal, y la mantiene bloqueada contra reservas nuevas mientras fn
	// corre. Si la tiene, devuelve 409 unidad_con_reserva_vigente.
	SinReservaVigente(ctx context.Context, unidadID uuid.UUID, fn func(context.Context) error) error
}

// Servicio es el caso de uso del roster con el pool de op_roster.
type Servicio struct {
	tx       BeginTx
	atomos   Atomos
	reservas Reservas
}

// NewServicio recibe el pool del rol op_roster, la lectura de átomos de
// catalogos (nil si la raíz no la cableó: el armado responde 503) y la guarda
// de reservas de asignacion.
func NewServicio(tx BeginTx, atomos Atomos, reservas Reservas) *Servicio {
	return &Servicio{tx: tx, atomos: atomos, reservas: reservas}
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
