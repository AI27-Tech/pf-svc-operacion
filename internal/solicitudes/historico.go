package solicitudes

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/estados"
	operclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/operacion"
	platformerrors "github.com/AI27-Tech/pf-pkg-platform-go/errors"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	openapi_types "github.com/oapi-codegen/runtime/types"
)

// Historico es la vista unificada del histórico de la solicitud (E10-T11): cada
// transición y cada acción sin cambio de estado, en orden cronológico, con actor,
// motivo y marca de tiempo; las entradas de cotización llevan la versión completa
// (DEC-148 #42). Une las tablas que escribe el módulo (la solicitud, su histórico
// de transiciones, las versiones de la cotización y las reprogramaciones) y se
// conserva después de aprobarse y nacer el viaje: la solicitud no se borra.
//
// Solo cotizacion, administrador y el jefe operativo (mientras la solicitud está en
// PAGADO; fuera de ahí le responde 404). El cliente y cualquier otro rol, 403: lo
// decide el rol antes de tocar la base. El actor del contrato es un Id (UUID); un
// `sub` que no lo sea se omite (null) en lugar de inventarse (DEC-148 #71).
//
// El jefe operativo recibe las acciones y transiciones sin el objeto `cotizacion`
// (DEC-148 #67): la consulta de versiones no lee monto, moneda ni notas para él.
// Tampoco recibe el `motivo` de `pago_revertido`, texto libre del administrador que
// puede nombrar montos o referencias; el de `reprogramada` es operativo y sí lo
// ve (DEC-148 #73). Para ese rol un `cotizacion` nulo significa oculto.
func (s *Servicio) Historico(ctx context.Context, a Actor, id uuid.UUID) (*operclient.HistoricoSolicitud, error) {
	if a.Rol != rolCotizacion && a.Rol != rolAdministrador && a.Rol != RolJefeOperativo {
		return nil, platformerrors.NewProhibido("operacion: el rol %q no ve el histórico de la solicitud", a.Rol)
	}
	tx, err := s.tx.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }() // solo lectura
	f, err := cargar(ctx, tx, id, false)
	if err != nil {
		return nil, err
	}
	if err := a.resolviendo(f); err != nil {
		return nil, err
	}

	type entrada struct {
		item  operclient.HistoricoSolicitudItem
		rank  int   // desempata el mismo instante: creada, transición, recotizada, reprogramada
		orden int64 // y luego el orden de inserción de su tabla
	}
	var items []entrada
	actor := func(s *string) *openapi_types.UUID {
		if s == nil {
			return nil
		}
		u, err := uuid.Parse(*s)
		if err != nil {
			return nil
		}
		return &u
	}
	est := func(e estados.EstadoSolicitud) *operclient.EstadoSolicitud {
		x := operclient.EstadoSolicitud(e)
		return &x
	}

	var creadoPor *string
	if err := tx.QueryRow(ctx, `SELECT creado_por FROM solicitudes.solicitud WHERE id = $1`, id).Scan(&creadoPor); err != nil {
		return nil, fmt.Errorf("solicitudes: leer quién creó la solicitud: %w", err)
	}
	items = append(items, entrada{item: operclient.HistoricoSolicitudItem{
		Accion: "creada", AEstado: est(estados.EstadoSolicitudSolicitado), ActorId: actor(creadoPor), En: f.CreadoEn,
	}})

	comercial := a.veComercial()
	versiones, err := cotizaciones(ctx, tx, id, comercial)
	if err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx,
		`SELECT id, de_estado, a_estado, accion, actor_id, motivo, marca_penalizacion, creado_en
		   FROM solicitudes.transicion_solicitud WHERE solicitud_id = $1`, id)
	if err != nil {
		return nil, fmt.Errorf("solicitudes: leer las transiciones: %w", err)
	}
	for rows.Next() {
		var n int64
		var de, a, accion string
		var actorID, motivo *string
		var marca *bool
		var en time.Time
		if err := rows.Scan(&n, &de, &a, &accion, &actorID, &motivo, &marca, &en); err != nil {
			rows.Close()
			return nil, fmt.Errorf("solicitudes: leer una transición: %w", err)
		}
		if !comercial && accion == "pago_revertido" { // DEC-148 #73
			motivo = nil
		}
		it := operclient.HistoricoSolicitudItem{
			Accion: accion, DeEstado: est(estados.EstadoSolicitud(de)), AEstado: est(estados.EstadoSolicitud(a)),
			ActorId: actor(actorID), Motivo: motivo, MarcaPenalizacion: marca, En: en,
		}
		if accion == "cotizada" { // la primera cotización nace con la transición: versión 1
			if c, ok := versiones[1]; ok && comercial {
				cc := c.cot
				it.Cotizacion = &cc
			}
		}
		items = append(items, entrada{item: it, rank: 1, orden: n})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("solicitudes: leer las transiciones: %w", err)
	}

	for v, c := range versiones {
		if v == 1 {
			continue // ya va en `cotizada`
		}
		cc := c.cot
		it := operclient.HistoricoSolicitudItem{Accion: "recotizada", ActorId: actor(c.actor), En: cc.EntregadaEn}
		if comercial {
			it.Cotizacion = &cc
		}
		items = append(items, entrada{item: it, rank: 2, orden: int64(v)})
	}

	rows, err = tx.Query(ctx,
		`SELECT id, motivo, actor_id, creado_en FROM solicitudes.reprogramacion_solicitud WHERE solicitud_id = $1`, id)
	if err != nil {
		return nil, fmt.Errorf("solicitudes: leer las reprogramaciones: %w", err)
	}
	for rows.Next() {
		var n int64
		var motivo string
		var actorID *string
		var en time.Time
		if err := rows.Scan(&n, &motivo, &actorID, &en); err != nil {
			rows.Close()
			return nil, fmt.Errorf("solicitudes: leer una reprogramación: %w", err)
		}
		items = append(items, entrada{item: operclient.HistoricoSolicitudItem{
			Accion: "reprogramada", ActorId: actor(actorID), Motivo: &motivo, En: en,
		}, rank: 3, orden: n})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("solicitudes: leer las reprogramaciones: %w", err)
	}

	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		if !a.item.En.Equal(b.item.En) {
			return a.item.En.Before(b.item.En)
		}
		if a.rank != b.rank {
			return a.rank < b.rank
		}
		return a.orden < b.orden
	})
	out := &operclient.HistoricoSolicitud{Items: make([]operclient.HistoricoSolicitudItem, len(items))}
	for i, e := range items {
		out.Items[i] = e.item
	}
	return out, nil
}

type versionCotizacion struct {
	cot   operclient.Cotizacion
	actor *string
}

// cotizaciones lee las versiones de la cotización, por número de versión. Con
// comercial=false solo trae lo que ordena el histórico (versión, instante de
// entrega, vigencia y actor): ni monto, ni moneda, ni notas, ni la fecha cotizada.
func cotizaciones(ctx context.Context, tx pgx.Tx, id uuid.UUID, comercial bool) (map[int]versionCotizacion, error) {
	cols := `version, monto::text, moneda, fecha_hora_servicio, notas, entregada_en, vigente_hasta, actor_id`
	if !comercial {
		cols = `version, entregada_en, vigente_hasta, actor_id`
	}
	rows, err := tx.Query(ctx, `SELECT `+cols+` FROM solicitudes.cotizacion WHERE solicitud_id = $1`, id)
	if err != nil {
		return nil, fmt.Errorf("solicitudes: leer las cotizaciones: %w", err)
	}
	defer rows.Close()
	out := map[int]versionCotizacion{}
	for rows.Next() {
		var c operclient.Cotizacion
		var vig time.Time
		var actor *string
		dest := []any{&c.Version, &c.EntregadaEn, &vig, &actor}
		if comercial {
			dest = []any{&c.Version, &c.Monto, &c.Moneda, &c.FechaHoraServicio, &c.Notas, &c.EntregadaEn, &vig, &actor}
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, fmt.Errorf("solicitudes: leer una cotización: %w", err)
		}
		c.VigenteHasta = openapi_types.Date{Time: vig}
		out[c.Version] = versionCotizacion{cot: c, actor: actor}
	}
	return out, rows.Err()
}
