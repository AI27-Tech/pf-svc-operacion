package bitacora

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	operclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/operacion"
	platformerrors "github.com/AI27-Tech/pf-pkg-platform-go/errors"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// BeginTx es la frontera transaccional (Unit of Work): el caso de uso controla
// el commit, no el repositorio. La cumple el pool del rol op_bitacora. Mismo
// patrón que viajes y asignacion.
type BeginTx interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// paginaMax es el tamaño de página de ListarBitacora. Mismo criterio que viajes:
// tamaño fijo; hacerlo configurable si el panel lo pide.
const paginaMax = 50

// namespaceEntrada es el espacio de nombres fijo para derivar el id público (UUID)
// de una entrada a partir de su correlativo interno (BIGSERIAL). El contrato
// expone un UUID y "nunca un correlativo" (operacion.yaml), pero la tabla
// append-only usa un BIGSERIAL y el ticket prohíbe agregar columnas; derivar un
// UUIDv5 determinista del correlativo da un id estable y opaco (igual en registrar
// y en listar) sin tocar el contrato ni el esquema. El namespace es arbitrario
// pero fijo: cambiarlo cambiaría todos los ids derivados.
var namespaceEntrada = uuid.MustParse("6f1b2c3d-4e5a-5b6c-8d7e-9f0a1b2c3d4e")

// idPublico deriva el UUID público de una entrada desde su correlativo interno.
// Determinista: la misma fila siempre produce el mismo UUID.
func idPublico(correlativo int64) uuid.UUID {
	return uuid.NewSHA1(namespaceEntrada, []byte(strconv.FormatInt(correlativo, 10)))
}

// Servicio es el caso de uso de bitácora: registrar una entrada y listar las
// entradas de un viaje. Append-only (E1-T5, operacion.md (e)): no hay edición ni
// borrado de entradas; una corrección es una entrada nueva. No publica eventos:
// bitacora no tiene outbox ni worker.
type Servicio struct {
	tx BeginTx
}

// NewServicio recibe la frontera transaccional (el pool del rol op_bitacora). No
// lee entorno.
func NewServicio(tx BeginTx) *Servicio {
	return &Servicio{tx: tx}
}

// RegistrarEntrada agrega una entrada a la bitácora de un viaje. actorID es el
// sujeto del token (vacío si no hay). El texto es obligatorio.
//
// No valida existencia ni estado del viaje: la bitácora es un registro narrativo
// que puede recibir correcciones aun después de cerrado el viaje (append-only),
// y el único GRANT de bitacora es sobre su propio esquema (no puede leer
// viajes.viaje). El viaje_id se guarda tal cual lo da el llamador.
//
// NO audita en su propia transacción. El interceptor de auditoría (P3) corre en
// la MISMA tx que el cambio y escribe en el esquema `auditoria`, pero el único
// GRANT cruzado del sistema es el de op_asignacion (operacion.md §(e), ADR 18
// §4): op_bitacora no puede escribir en `auditoria`, y abrirle ese acceso sería
// una segunda excepción de GRANT, prohibida sin un ADR que enmiende la regla.
// Por eso, igual que viajes hoy, bitacora no audita en V1. Si P3 exige auditar
// la bitácora en su tx, es una decisión de arquitectura (owner de ADR 31/33/35)
// que debe ampliar la excepción por ADR antes de cablear el interceptor aquí.
func (s *Servicio) RegistrarEntrada(ctx context.Context, viajeID operclient.Id, in operclient.EntradaBitacoraEntrada, actorID string) (*operclient.EntradaBitacora, error) {
	if strings.TrimSpace(in.Texto) == "" {
		return nil, platformerrors.NewValidacion("operacion: el texto de la entrada de bitácora es obligatorio")
	}

	tx, err := s.tx.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op tras commit

	// INSERT append-only. El id (BIGSERIAL) y el creado_en (DEFAULT now()) los
	// fija la base; se recuperan con RETURNING para armar la respuesta. autor_id
	// es NULL si no hay actor (token sin sujeto).
	var (
		correlativo int64
		creadoEn    time.Time
	)
	if err := tx.QueryRow(ctx,
		`INSERT INTO bitacora.entrada_bitacora (viaje_id, texto, autor_id)
		 VALUES ($1, $2, $3)
		 RETURNING id, creado_en`,
		viajeID, in.Texto, nullsiVacio(actorID),
	).Scan(&correlativo, &creadoEn); err != nil {
		return nil, fmt.Errorf("operacion: registrar entrada de bitácora: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("operacion: commit registrar entrada de bitácora: %w", err)
	}

	return &operclient.EntradaBitacora{
		Id:      idPublico(correlativo),
		ViajeId: viajeID,
		Texto:   in.Texto,
		Autor:   actorID,
		Ts:      creadoEn.UTC(),
	}, nil
}

// ListarBitacora devuelve las entradas de un viaje en orden cronológico por
// (viaje_id, creado_en) —las más antiguas primero—, con paginación por cursor
// keyset sobre (creado_en, id). El orden cronológico es el natural de una
// bitácora: se lee como se escribió.
func (s *Servicio) ListarBitacora(ctx context.Context, viajeID operclient.Id, params operclient.ListarBitacoraParams) (*operclient.PaginaBitacora, error) {
	tx, err := s.tx.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	conds := []string{"viaje_id = $1"}
	args := []any{viajeID}
	n := 2
	if params.Cursor != nil && *params.Cursor != "" {
		cCreado, cID, err := decodeCursor(*params.Cursor)
		if err != nil {
			return nil, platformerrors.NewValidacion("operacion: cursor inválido")
		}
		conds = append(conds, fmt.Sprintf("(creado_en, id) > ($%d, $%d)", n, n+1))
		args = append(args, cCreado, cID)
		n += 2
	}
	where := "WHERE " + strings.Join(conds, " AND ")
	args = append(args, paginaMax+1) // +1 para saber si hay página siguiente
	q := fmt.Sprintf(
		`SELECT id, texto, autor_id, creado_en
		   FROM bitacora.entrada_bitacora %s
		  ORDER BY creado_en ASC, id ASC LIMIT $%d`, where, n)

	rows, err := tx.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("operacion: listar bitácora: %w", err)
	}
	items := []operclient.EntradaBitacora{}
	correlativos := []int64{}
	creados := []time.Time{}
	for rows.Next() {
		var (
			correlativo int64
			texto       string
			autor       *string
			creado      time.Time
		)
		if err := rows.Scan(&correlativo, &texto, &autor, &creado); err != nil {
			rows.Close()
			return nil, fmt.Errorf("operacion: escanear entrada de bitácora: %w", err)
		}
		a := ""
		if autor != nil {
			a = *autor
		}
		items = append(items, operclient.EntradaBitacora{
			Id:      idPublico(correlativo),
			ViajeId: viajeID,
			Texto:   texto,
			Autor:   a,
			Ts:      creado.UTC(),
		})
		correlativos = append(correlativos, correlativo)
		creados = append(creados, creado)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("operacion: iterar bitácora: %w", err)
	}

	var cursorSig *string
	if len(items) > paginaMax {
		// El cursor keyset se arma con el correlativo interno y el creado_en de la
		// última fila de la página (no con el UUID público, que es derivado).
		cur := encodeCursor(creados[paginaMax-1], correlativos[paginaMax-1])
		cursorSig = &cur
		items = items[:paginaMax]
	}

	return &operclient.PaginaBitacora{Items: items, CursorSiguiente: cursorSig}, nil
}

// nullsiVacio devuelve nil para la cadena vacía, de modo que autor_id quede NULL
// cuando el token no trae sujeto (mismo criterio que auditoria).
func nullsiVacio(s string) any {
	if s == "" {
		return nil
	}
	return s
}
