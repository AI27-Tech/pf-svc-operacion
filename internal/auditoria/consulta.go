package auditoria

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	operclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/operacion"
	platformerrors "github.com/AI27-Tech/pf-pkg-platform-go/errors"
	"github.com/jackc/pgx/v5"
)

// paginaMax es el tamaño de página de la consulta de auditoría. Mismo criterio
// que la bitácora: se pide una fila extra para saber si hay página siguiente.
const paginaMax = 50

// Consultas es la superficie mínima de LECTURA que la consulta de auditoría
// necesita del pool op_auditoria: solo Query. Se declara como interfaz para
// poder sustituirla en pruebas.
type Consultas interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// ServicioConsulta lee la bitácora probatoria (append-only) de un viaje para la
// composición del expediente de descarga (E6-T3, ADR 27: operacion es la fuente
// exacta, nunca reportes). Solo lectura, con el rol op_auditoria (que tiene
// SELECT sobre registro_auditoria).
type ServicioConsulta struct {
	db Consultas
}

// NewServicioConsulta construye el servicio de consulta sobre el pool dado
// (op_auditoria).
func NewServicioConsulta(db Consultas) *ServicioConsulta {
	return &ServicioConsulta{db: db}
}

// Consultar devuelve una página de la auditoría de un viaje en orden cronológico
// (las más antiguas primero), paginada por cursor keyset sobre (ocurrido_en, id).
// El `modulo` de la tabla mapea a `entidad` del contrato; el `viaje_id` a
// `entidad_id`; `de`/`a` salen del `detalle` JSONB si la acción fue una
// transición de estado (opcionales, nil si no aplica).
func (s *ServicioConsulta) Consultar(ctx context.Context, viajeID operclient.Id, params operclient.ConsultarAuditoriaParams) (*operclient.PaginaAuditoria, error) {
	conds := []string{"viaje_id = $1"}
	args := []any{viajeID}
	n := 2
	if params.Cursor != nil && *params.Cursor != "" {
		cOcurrido, cID, err := decodeCursor(*params.Cursor)
		if err != nil {
			return nil, platformerrors.NewValidacion("operacion: cursor inválido")
		}
		conds = append(conds, fmt.Sprintf("(ocurrido_en, id) > ($%d, $%d)", n, n+1))
		args = append(args, cOcurrido, cID)
		n += 2
	}
	where := "WHERE " + strings.Join(conds, " AND ")
	args = append(args, paginaMax+1)
	q := fmt.Sprintf(
		`SELECT id, modulo, actor_id, detalle, ocurrido_en
		   FROM auditoria.registro_auditoria %s
		  ORDER BY ocurrido_en ASC, id ASC LIMIT $%d`, where, n)

	rows, err := s.db.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("operacion: consultar auditoría: %w", err)
	}
	defer rows.Close()

	items := []operclient.RegistroAuditoria{}
	ids := []int64{}
	ocurridos := []time.Time{}
	for rows.Next() {
		var (
			id       int64
			modulo   string
			actor    *string
			detalle  []byte
			ocurrido time.Time
		)
		if err := rows.Scan(&id, &modulo, &actor, &detalle, &ocurrido); err != nil {
			return nil, fmt.Errorf("operacion: escanear registro de auditoría: %w", err)
		}
		a := ""
		if actor != nil {
			a = *actor
		}
		de, hacia := estadosDe(detalle)
		items = append(items, operclient.RegistroAuditoria{
			Entidad:   modulo,
			EntidadId: viajeID,
			Actor:     a,
			Ts:        ocurrido.UTC(),
			De:        de,
			A:         hacia,
		})
		ids = append(ids, id)
		ocurridos = append(ocurridos, ocurrido)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("operacion: iterar auditoría: %w", err)
	}

	var cursorSig *string
	if len(items) > paginaMax {
		cur := encodeCursor(ocurridos[paginaMax-1], ids[paginaMax-1])
		cursorSig = &cur
		items = items[:paginaMax]
	}
	return &operclient.PaginaAuditoria{Items: items, CursorSiguiente: cursorSig}, nil
}

// estadosDe extrae `de`/`a` del detalle JSONB cuando el registro fue una
// transición de estado (claves "de"/"a"). Si no están, devuelve nil: el contrato
// los declara opcionales.
func estadosDe(detalle []byte) (de, a *string) {
	if len(detalle) == 0 {
		return nil, nil
	}
	var m map[string]any
	if err := json.Unmarshal(detalle, &m); err != nil {
		return nil, nil
	}
	if v, ok := m["de"].(string); ok && v != "" {
		de = &v
	}
	if v, ok := m["a"].(string); ok && v != "" {
		a = &v
	}
	return de, a
}

// encodeCursor/decodeCursor codifican el cursor keyset (ocurrido_en, id) como un
// token opaco. Mismo esquema que la bitácora: base64 de "unixnano:id".
func encodeCursor(ocurrido time.Time, id int64) string {
	raw := fmt.Sprintf("%d:%d", ocurrido.UTC().UnixNano(), id)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeCursor(cur string) (time.Time, int64, error) {
	raw, err := base64.RawURLEncoding.DecodeString(cur)
	if err != nil {
		return time.Time{}, 0, err
	}
	partes := strings.SplitN(string(raw), ":", 2)
	if len(partes) != 2 {
		return time.Time{}, 0, fmt.Errorf("cursor malformado")
	}
	nanos, err := strconv.ParseInt(partes[0], 10, 64)
	if err != nil {
		return time.Time{}, 0, err
	}
	id, err := strconv.ParseInt(partes[1], 10, 64)
	if err != nil {
		return time.Time{}, 0, err
	}
	return time.Unix(0, nanos).UTC(), id, nil
}
