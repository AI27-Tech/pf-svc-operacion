package roster

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"

	catalogos "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/catalogos"
	operclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/operacion"
	platformerrors "github.com/AI27-Tech/pf-pkg-platform-go/errors"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// tipoPorVehiculo deriva el tipo de la unidad del tipo del vehículo de
// catalogos (ADR 31, DEC-142): coche -> patrulla, moto -> moto.
var tipoPorVehiculo = map[catalogos.VehiculoTipo]operclient.UnidadTipo{
	catalogos.VehiculoTipoCoche: operclient.Patrulla,
	catalogos.VehiculoTipoMoto:  operclient.Moto,
}

// maxCustodios es el tope de custodios por tipo de unidad; el mínimo es uno
// para los dos (ADR 31): patrulla uno o dos, moto exactamente uno.
var maxCustodios = map[operclient.UnidadTipo]int{
	operclient.Patrulla: 2,
	operclient.Moto:     1,
}

// Índices de 0003_exclusividad.sql, para traducir su violación al 409 del contrato.
const (
	indiceVehiculoActivo = "unidad_vehiculo_activa_unico"
	indiceCustodioActivo = "unidad_custodio_activa_unico"
)

// Obtener lee una unidad del roster; 404 si no existe.
func (s *Servicio) Obtener(ctx context.Context, id uuid.UUID) (*operclient.Unidad, error) {
	tx, err := s.tx.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("operacion: abrir transacción de lectura del roster: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	return leer(ctx, tx, id)
}

// Crear arma una unidad: el tipo sale del vehículo, vehículo y custodios deben
// existir y estar activos en catalogos, y ninguno puede estar en otra unidad
// activa (lo corta el índice único, también entre altas concurrentes).
// `activa` omitida vale true, el default de la tabla.
func (s *Servicio) Crear(ctx context.Context, in operclient.UnidadEntrada) (*operclient.Unidad, error) {
	if err := validarCustodios(in.CustodiosIds); err != nil {
		return nil, err
	}
	tipo, err := s.tipoDeVehiculo(ctx, in.VehiculoId)
	if err != nil {
		return nil, err
	}
	if err := validarCantidad(tipo, len(in.CustodiosIds)); err != nil {
		return nil, err
	}
	if err := s.verificarCustodios(ctx, in.CustodiosIds); err != nil {
		return nil, err
	}
	activa := in.Activa == nil || *in.Activa

	tx, err := s.tx.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("operacion: abrir transacción del roster: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	id := uuid.New()
	if _, err := tx.Exec(ctx,
		`INSERT INTO roster.unidad (id, tipo, vehiculo_id, activa) VALUES ($1, $2, $3, $4)`,
		id, string(tipo), in.VehiculoId.String(), activa,
	); err != nil {
		return nil, traducir(err, "crear unidad")
	}
	if err := insertarCustodios(ctx, tx, id, activa, in.CustodiosIds); err != nil {
		return nil, err
	}
	u, err := leer(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, traducir(err, "commit de la unidad")
	}
	return u, nil
}

// Modificar cambia vehículo, custodios o `activa` de una unidad. Una unidad con
// reserva en un viaje no terminal no se toca (409, guarda de asignacion). Las
// referencias nuevas se validan contra catalogos antes de tomar la guarda;
// reactivar no revalida contra catalogos (DEC-142 no lo pide).
func (s *Servicio) Modificar(ctx context.Context, id uuid.UUID, in operclient.UnidadModificacion) (*operclient.Unidad, error) {
	var tipoNuevo *operclient.UnidadTipo
	if in.VehiculoId != nil {
		t, err := s.tipoDeVehiculo(ctx, *in.VehiculoId)
		if err != nil {
			return nil, err
		}
		tipoNuevo = &t
	}
	if in.CustodiosIds != nil {
		if err := validarCustodios(*in.CustodiosIds); err != nil {
			return nil, err
		}
		if err := s.verificarCustodios(ctx, *in.CustodiosIds); err != nil {
			return nil, err
		}
	}

	var out *operclient.Unidad
	err := s.reservas.SinReservaVigente(ctx, id, func(ctx context.Context) error {
		tx, err := s.tx.Begin(ctx)
		if err != nil {
			return fmt.Errorf("operacion: abrir transacción del roster: %w", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()

		var (
			tipo   operclient.UnidadTipo
			activa bool
			n      int
		)
		err = tx.QueryRow(ctx,
			`SELECT u.tipo, u.activa, (SELECT count(*) FROM roster.unidad_custodio c WHERE c.unidad_id = u.id)
			   FROM roster.unidad u WHERE u.id = $1 FOR UPDATE`, id,
		).Scan(&tipo, &activa, &n)
		if errors.Is(err, pgx.ErrNoRows) {
			return platformerrors.NewNoEncontrado("operacion: unidad %s no encontrada", id)
		}
		if err != nil {
			return fmt.Errorf("operacion: cargar unidad %s: %w", id, err)
		}
		if tipoNuevo != nil {
			tipo = *tipoNuevo
		}
		if in.CustodiosIds != nil {
			n = len(*in.CustodiosIds)
		}
		if err := validarCantidad(tipo, n); err != nil {
			return err
		}

		// Custodios primero, con el `activa` vigente de la unidad (lo exige la FK
		// compuesta); el UPDATE de abajo propaga el `activa` nuevo a las filas ya
		// definitivas, así una reactivación solo choca con los custodios que quedan.
		if in.CustodiosIds != nil {
			if _, err := tx.Exec(ctx, `DELETE FROM roster.unidad_custodio WHERE unidad_id = $1`, id); err != nil {
				return fmt.Errorf("operacion: retirar custodios de la unidad %s: %w", id, err)
			}
			if err := insertarCustodios(ctx, tx, id, activa, *in.CustodiosIds); err != nil {
				return err
			}
		}
		var vehiculo *string
		if in.VehiculoId != nil {
			v := in.VehiculoId.String()
			vehiculo = &v
		}
		if in.Activa != nil {
			activa = *in.Activa
		}
		if _, err := tx.Exec(ctx,
			`UPDATE roster.unidad SET tipo = $2, vehiculo_id = COALESCE($3, vehiculo_id), activa = $4 WHERE id = $1`,
			id, string(tipo), vehiculo, activa,
		); err != nil {
			return traducir(err, "modificar unidad")
		}
		if out, err = leer(ctx, tx, id); err != nil {
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return traducir(err, "commit de la unidad")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// validarCustodios es la forma de la lista, sin mirar el tipo: uno o dos
// custodios (el contrato), sin repetidos.
func validarCustodios(ids []uuid.UUID) error {
	if len(ids) < 1 || len(ids) > 2 {
		return platformerrors.NewValidacion("operacion: una unidad lleva uno o dos custodios; llegaron %d", len(ids))
	}
	if len(ids) == 2 && ids[0] == ids[1] {
		return platformerrors.NewValidacion("operacion: el custodio %s viene repetido", ids[0])
	}
	return nil
}

// validarCantidad aplica el tope de custodios del tipo de unidad.
func validarCantidad(tipo operclient.UnidadTipo, n int) error {
	if n > maxCustodios[tipo] {
		return platformerrors.NewValidacion("operacion: una unidad %s lleva a lo más %d custodio(s); llegaron %d", tipo, maxCustodios[tipo], n)
	}
	return nil
}

// tipoDeVehiculo lee el vehículo en catalogos y deriva el tipo de la unidad.
// Inexistente o inactivo es una referencia inválida (400); catalogos caído se
// propaga como degradado (503): sin el tipo no se puede validar la unidad.
func (s *Servicio) tipoDeVehiculo(ctx context.Context, id uuid.UUID) (operclient.UnidadTipo, error) {
	if s.atomos == nil {
		return "", platformerrors.NewDegradado("operacion: la lectura de vehículos de catalogos no está cableada")
	}
	v, err := s.atomos.ObtenerVehiculo(ctx, id)
	if err != nil {
		if platformerrors.AsCategory(err) == platformerrors.CategoryNoEncontrado {
			return "", platformerrors.NewValidacion("operacion: el vehículo %s no existe en catalogos", id)
		}
		return "", err
	}
	if v.Estado != catalogos.VehiculoEstadoActiva {
		return "", platformerrors.NewValidacion("operacion: el vehículo %s está inactivo en catalogos", id)
	}
	tipo, ok := tipoPorVehiculo[v.Tipo]
	if !ok {
		return "", fmt.Errorf("operacion: catalogos devolvió un tipo de vehículo desconocido %q", v.Tipo)
	}
	return tipo, nil
}

// verificarCustodios exige que cada custodio exista y esté activo en catalogos.
func (s *Servicio) verificarCustodios(ctx context.Context, ids []uuid.UUID) error {
	if s.atomos == nil {
		return platformerrors.NewDegradado("operacion: la lectura de custodios de catalogos no está cableada")
	}
	for _, id := range ids {
		c, err := s.atomos.ObtenerCustodio(ctx, id)
		if err != nil {
			if platformerrors.AsCategory(err) == platformerrors.CategoryNoEncontrado {
				return platformerrors.NewValidacion("operacion: el custodio %s no existe en catalogos", id)
			}
			return err
		}
		if c.Estado != catalogos.CustodioEstadoActivo {
			return platformerrors.NewValidacion("operacion: el custodio %s está inactivo en catalogos", id)
		}
	}
	return nil
}

// insertarCustodios escribe la composición en orden de id, para que dos
// escrituras concurrentes tomen los candados del índice único en el mismo orden.
func insertarCustodios(ctx context.Context, tx pgx.Tx, unidadID uuid.UUID, activa bool, ids []uuid.UUID) error {
	ordenados := slices.SortedFunc(slices.Values(ids), func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
	for _, c := range ordenados {
		if _, err := tx.Exec(ctx,
			`INSERT INTO roster.unidad_custodio (unidad_id, custodio_id, activa) VALUES ($1, $2, $3)`,
			unidadID, c.String(), activa,
		); err != nil {
			return traducir(err, "asignar custodio a la unidad")
		}
	}
	return nil
}

// leer proyecta una unidad con la forma de Columnas; 404 si no existe.
func leer(ctx context.Context, tx pgx.Tx, id uuid.UUID) (*operclient.Unidad, error) {
	u, err := Escanear(tx.QueryRow(ctx,
		`SELECT `+Columnas+`
		   FROM roster.unidad u
		   LEFT JOIN roster.unidad_custodio c ON c.unidad_id = u.id
		  WHERE u.id = $1
		  GROUP BY u.id, u.tipo, u.vehiculo_id, u.activa`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, platformerrors.NewNoEncontrado("operacion: unidad %s no encontrada", id)
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// traducir convierte la violación de un índice de exclusividad en el 409 con
// código de máquina (DEC-142 #4); cualquier otro error se envuelve tal cual.
func traducir(err error, que string) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		switch pgErr.ConstraintName {
		case indiceVehiculoActivo:
			return platformerrors.NewConflictoCodigo("vehiculo_en_otra_unidad", "operacion: el vehículo ya está en otra unidad activa")
		case indiceCustodioActivo:
			return platformerrors.NewConflictoCodigo("custodio_en_otra_unidad", "operacion: un custodio ya está en otra unidad activa")
		}
	}
	return fmt.Errorf("operacion: %s: %w", que, err)
}
