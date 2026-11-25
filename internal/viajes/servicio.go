package viajes

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/estados"
	operclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/operacion"
	"github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/roles"
	platformerrors "github.com/AI27-Tech/pf-pkg-platform-go/errors"
	"github.com/AI27-Tech/pf-pkg-platform-go/outbox"
	"github.com/AI27-Tech/pf-svc-operacion/internal/alcance"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// BeginTx es la frontera transaccional (Unit of Work): el caso de uso controla
// el commit, no el repositorio. La cumple *pgxpool.Pool. Mismo patrón que
// identidad/sesion.
type BeginTx interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Servicio es el caso de uso de viajes para la rebanada de nacimiento y
// lecturas: crear, obtener y listar. La transición (E1-T3) y la asignación
// llegan en sus propias rebanadas; por eso aún no se importa `estados`.
type Servicio struct {
	tx  BeginTx
	pub *outbox.Publisher
	now func() time.Time
}

// NewServicio recibe la frontera transaccional (el pool del rol op_viajes) y el
// publicador de outbox. No lee entorno.
func NewServicio(tx BeginTx, pub *outbox.Publisher) *Servicio {
	return &Servicio{tx: tx, pub: pub, now: time.Now}
}

// ConReloj sustituye el reloj antes de usar el servicio, para probar el límite de la fecha/hora.
func (s *Servicio) ConReloj(now func() time.Time) *Servicio {
	s.now = now
	return s
}

// paginaMax es el tamaño de página de Listar.
// shortcut: tamaño fijo; hacerlo configurable si el panel lo pide.
const paginaMax = 50

// rolesQuePuedenCrear son los roles que crean un viaje (DEC-026, ADR 38): solo
// Operación. El cliente crea una solicitud; su viaje nace al aprobarse.
var rolesQuePuedenCrear = map[string]bool{
	string(roles.RolJefeOperativo): true,
	string(roles.RolAdministrador): true,
}

// Crear registra un viaje nuevo y publica viaje.creado en la MISMA transacción
// (P17). El viaje siempre nace en APROBADO (ADR 38) y solo lo crea Operación; el
// resto de los roles, incluido el cliente, recibe 403. El cliente_id lo manda
// Operación en el cuerpo (selector de cliente, E8-T11). El id lo genera la app.
// Un viaje creado aquí no nació de una solicitud: nacio_de_solicitud queda en
// false (DEC-148 #27), y se escribe explícito: la columna no tiene DEFAULT.
func (s *Servicio) Crear(ctx context.Context, in operclient.CrearViajeEntrada, rol string) (*operclient.Viaje, error) {
	sinPenalizacion := false
	if !rolesQuePuedenCrear[rol] {
		return nil, platformerrors.NewProhibido("operacion: el rol %q no crea viajes", rol)
	}
	if in.ClienteId == nil {
		// El operador debe elegir el cliente.
		return nil, platformerrors.NewValidacion("operacion: cliente_id es obligatorio")
	}
	if len(in.Destinos) == 0 {
		return nil, platformerrors.NewValidacion("operacion: se requiere al menos un destino")
	}
	if strings.TrimSpace(in.Origen) == "" {
		return nil, platformerrors.NewValidacion("operacion: origen es obligatorio")
	}

	id := uuid.New()
	ahora := time.Now().UTC()

	tx, err := s.tx.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op tras commit

	if _, err := tx.Exec(ctx,
		`INSERT INTO viajes.viaje (id, estado, cliente_id, origen, fecha_hora_servicio, tipo_servicio, creado_en, nacio_de_solicitud)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		id, string(operclient.APROBADO), *in.ClienteId, in.Origen, in.FechaHoraServicio, in.TipoServicio, ahora, sinPenalizacion,
	); err != nil {
		return nil, fmt.Errorf("operacion: insertar viaje: %w", err)
	}

	for i, d := range in.Destinos {
		if _, err := tx.Exec(ctx,
			`INSERT INTO viajes.destino (id, viaje_id, orden, nombre, numero_envio, fecha_cita)
			 VALUES ($1,$2,$3,$4,$5,$6)`,
			uuid.New(), id, i, d.Nombre, d.NumeroEnvio, d.FechaCita,
		); err != nil {
			return nil, fmt.Errorf("operacion: insertar destino %d: %w", i, err)
		}
	}

	// viaje.creado al outbox de viajes, en la misma tx (ADR 35). Payload mínimo:
	// no hay struct de payload generado para los eventos viaje.* (brecha de
	// contrato), así que la forma se fija a mano, acotada a la identidad del viaje.
	if err := s.pub.Publish(ctx, tx, outbox.Event{
		Tipo:       "viaje.creado",
		Version:    1,
		AgregadoID: id.String(), // clave de orden = viaje_id
		Payload: map[string]any{
			"viaje_id":   id.String(),
			"estado":     string(operclient.APROBADO),
			"cliente_id": in.ClienteId.String(),
			"origen":     in.Origen,
			"creado_en":  ahora,
		},
	}); err != nil {
		return nil, err
	}

	// viaje.mensajeria_habilitada en la MISMA tx (DEC-016 #1): la conversación del
	// viaje se habilita al crearlo, sin importar el estado inicial, para que el
	// cliente pueda escribir desde que existe el viaje. mensajeria lo consume
	// para crear la conversación idempotentemente (E3-T1); por eso lleva el
	// cliente_id, que acota la conversación (DEC-045 #8). Forma del payload a mano:
	// no hay struct generado para los eventos viaje.* (misma brecha de contrato).
	if err := s.pub.Publish(ctx, tx, outbox.Event{
		Tipo:       "viaje.mensajeria_habilitada",
		Version:    1,
		AgregadoID: id.String(), // clave de orden = viaje_id
		Payload: map[string]any{
			"viaje_id":   id.String(),
			"cliente_id": in.ClienteId.String(),
			"creado_en":  ahora,
		},
	}); err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("operacion: commit crear viaje: %w", err)
	}

	return &operclient.Viaje{
		Id:                id,
		Estado:            operclient.APROBADO,
		ClienteId:         in.ClienteId,
		Origen:            in.Origen,
		Destinos:          in.Destinos,
		FechaHoraServicio: in.FechaHoraServicio,
		CreadoEn:          ahora,
		// Creado por Operación: no nació de una solicitud pagada (DEC-148 #27).
		CancelacionConPenalizacion: &sinPenalizacion,
	}, nil
}

// FijarEta guarda el ETA capturado a mano por el operador (ADR 20 / DEC-007). NO
// es un cambio de estado: no valida contra el grafo de transiciones, solo exige
// que el viaje no esté en un estado terminal (contrato: 409
// viaje_en_estado_terminal). Guarda el valor crudo y devuelve el viaje; la
// obsolescencia (`stale`) la deriva el bff al leer, no operacion. No publica
// evento (los eventos viaje.* no incluyen el ETA).
func (s *Servicio) FijarEta(ctx context.Context, id operclient.Id, in operclient.EtaEntrada) (*operclient.Viaje, error) {
	return s.editarNoTerminal(ctx, id, "fijar ETA", func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE viajes.viaje SET eta_estimado = $2 WHERE id = $1`, id, in.EtaEstimado.UTC())
		return err
	})
}

// CompletarFechaHora reemplaza fecha_hora_servicio en un viaje no terminal.
// La nueva fecha debe ser posterior al momento de guardar (DEC-148 #66).
// No cambia el estado ni publica evento.
func (s *Servicio) CompletarFechaHora(ctx context.Context, id operclient.Id, in operclient.FechaHoraEntrada) (*operclient.Viaje, error) {
	return s.editarNoTerminal(ctx, id, "completar la fecha/hora del servicio", func(tx pgx.Tx) error {
		// Compara el mismo instante que PostgreSQL guardará, a precisión de microsegundos.
		fecha := in.FechaHoraServicio.UTC().Truncate(time.Microsecond)
		if !fecha.After(s.now()) {
			return platformerrors.NewValidacion("operacion: la fecha y hora del servicio debe ser posterior al momento actual")
		}
		_, err := tx.Exec(ctx, `UPDATE viajes.viaje SET fecha_hora_servicio = $2 WHERE id = $1`, id, fecha)
		return err
	})
}

// editarNoTerminal aplica update a un viaje que no está
// en estado terminal y devuelve el viaje resultante. Bloquea la fila (FOR UPDATE)
// para que una transición concurrente a terminal no se cuele entre la guarda y
// la escritura. 404 si no existe; 409 viaje_en_estado_terminal si es terminal.
func (s *Servicio) editarNoTerminal(ctx context.Context, id operclient.Id, accion string, update func(pgx.Tx) error) (*operclient.Viaje, error) {
	tx, err := s.tx.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var estado string
	err = tx.QueryRow(ctx, `SELECT estado FROM viajes.viaje WHERE id = $1 FOR UPDATE`, id).Scan(&estado)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, platformerrors.NewNoEncontrado("operacion: viaje %s no encontrado", id)
	}
	if err != nil {
		return nil, fmt.Errorf("operacion: cargar viaje: %w", err)
	}
	if estados.EstadoViaje(estado).EsTerminal() {
		return nil, platformerrors.NewConflictoCodigo("viaje_en_estado_terminal",
			"operacion: no se puede %s en un viaje en estado terminal (%s)", accion, estado)
	}
	if err := update(tx); err != nil {
		return nil, fmt.Errorf("operacion: %s: %w", accion, err)
	}
	v, err := s.leerViaje(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("operacion: commit %s: %w", accion, err)
	}
	return v, nil
}

// leerViaje lee el agregado y sus destinos dentro de tx. 404 si no existe.
func (s *Servicio) leerViaje(ctx context.Context, tx pgx.Tx, id operclient.Id) (*operclient.Viaje, error) {
	var (
		estado    string
		clienteID uuid.UUID
		origen    string
		fhs       *time.Time
		eta       *time.Time
		creado    time.Time
		penaliza  bool
	)
	err := tx.QueryRow(ctx,
		`SELECT estado, cliente_id, origen, fecha_hora_servicio, eta_estimado, creado_en, nacio_de_solicitud
		   FROM viajes.viaje WHERE id = $1`, id,
	).Scan(&estado, &clienteID, &origen, &fhs, &eta, &creado, &penaliza)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, platformerrors.NewNoEncontrado("operacion: viaje %s no encontrado", id)
	}
	if err != nil {
		return nil, fmt.Errorf("operacion: obtener viaje: %w", err)
	}
	destinos, err := s.destinosDe(ctx, tx, id)
	if err != nil {
		return nil, err
	}
	cid := operclient.Id(clienteID)
	return &operclient.Viaje{
		Id:                id,
		Estado:            operclient.EstadoViaje(estado),
		ClienteId:         &cid,
		Origen:            origen,
		Destinos:          destinos,
		FechaHoraServicio: fhs,
		EtaEstimado:       eta,
		CreadoEn:          creado,
		// DEC-148 #14: la marca de penalización es el origen del viaje.
		CancelacionConPenalizacion: &penaliza,
	}, nil
}

// Obtener lee el detalle de un viaje: el agregado, sus destinos y sus
// condiciones ortogonales abiertas (ADR 27 / §C.2). NoEncontrado si no existe.
func (s *Servicio) Obtener(ctx context.Context, id operclient.Id) (*operclient.ViajeDetalle, error) {
	tx, err := s.tx.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	v, err := s.leerViaje(ctx, tx, id)
	if err != nil {
		return nil, err
	}

	conds, err := s.condicionesAbiertas(ctx, tx, id)
	if err != nil {
		return nil, err
	}

	// Incidencias abiertas con su tipo_interno (AI27-59). Es CONSUMO INTERNO de
	// bff: el curado de la categoría para el cliente lo hace bff (DEC-006), no
	// operacion. Aditivo y ortogonal a CondicionesActivas, que se conserva.
	incs, err := s.incidenciasAbiertas(ctx, tx, id)
	if err != nil {
		return nil, err
	}

	return &operclient.ViajeDetalle{
		Viaje:              *v,
		CondicionesActivas: conds,
		IncidenciasActivas: &incs,
	}, nil
}

// Listar devuelve una página de viajes, más recientes primero, con filtro
// opcional por estado y paginación por cursor keyset sobre (creado_en, id). Sin
// acotar: es la vista de alcance global (jefe_operativo, administrador). El
// alcance del cliente va por ListarDeCliente y el del monitoreo por
// ListarDeClientes (DEC-149).
func (s *Servicio) Listar(ctx context.Context, params operclient.ListarViajesParams) (*operclient.PaginaViajes, error) {
	return s.listar(ctx, params, "", nil)
}

// ListarDeCliente es Listar acotado a los viajes de un cliente (tenant ==
// cliente_id, DEC-112). Un clienteID vacío no devuelve nada.
func (s *Servicio) ListarDeCliente(ctx context.Context, params operclient.ListarViajesParams, clienteID string) (*operclient.PaginaViajes, error) {
	return s.listar(ctx, params, "v.cliente_id::text = $%d", clienteID)
}

// ListarDeClientes es Listar acotado al alcance del monitoreo (DEC-149): viajes
// de esos clientes (todo viaje nace en APROBADO, ADR 38). El filtro va en el WHERE, antes del
// cursor, así que la paginación sigue siendo correcta. Sin clientes no devuelve
// nada.
func (s *Servicio) ListarDeClientes(ctx context.Context, params operclient.ListarViajesParams, clientes []uuid.UUID) (*operclient.PaginaViajes, error) {
	if clientes == nil {
		clientes = []uuid.UUID{}
	}
	return s.listar(ctx, params, "v.cliente_id = ANY($%d::uuid[])", clientes)
}

// HechosAlcance lee lo que la regla de alcance necesita del viaje (DEC-149).
// 404 si no existe.
func (s *Servicio) HechosAlcance(ctx context.Context, id operclient.Id) (alcance.Hechos, error) {
	var h alcance.Hechos
	tx, err := s.tx.Begin(ctx)
	if err != nil {
		return h, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	err = tx.QueryRow(ctx,
		`SELECT cliente_id FROM viajes.viaje WHERE id = $1`, id,
	).Scan(&h.ClienteID)
	if errors.Is(err, pgx.ErrNoRows) {
		return h, platformerrors.NewNoEncontrado("operacion: viaje %s no encontrado", id)
	}
	if err != nil {
		return h, fmt.Errorf("operacion: leer el viaje para el alcance: %w", err)
	}
	return h, nil
}

// listar aplica, si filtro no está vacío, una condición con un solo parámetro
// ($%d) cuyo valor es arg.
func (s *Servicio) listar(ctx context.Context, params operclient.ListarViajesParams, filtro string, arg any) (*operclient.PaginaViajes, error) {
	tx, err := s.tx.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	conds := []string{}
	args := []any{}
	n := 1
	if filtro != "" {
		conds = append(conds, fmt.Sprintf(filtro, n))
		args = append(args, arg)
		n++
	}
	if params.Estado != nil {
		conds = append(conds, fmt.Sprintf("estado = $%d", n))
		args = append(args, string(*params.Estado))
		n++
	}
	if params.Cursor != nil && *params.Cursor != "" {
		cCreado, cID, err := decodeCursor(*params.Cursor)
		if err != nil {
			return nil, platformerrors.NewValidacion("operacion: cursor inválido")
		}
		conds = append(conds, fmt.Sprintf("(creado_en, id) < ($%d, $%d)", n, n+1))
		args = append(args, cCreado, cID)
		n += 2
	}
	where := ""
	if len(conds) > 0 {
		where = "WHERE " + strings.Join(conds, " AND ")
	}
	args = append(args, paginaMax+1) // +1 para saber si hay página siguiente
	q := fmt.Sprintf(
		`SELECT id, estado, cliente_id, origen, fecha_hora_servicio, eta_estimado, creado_en, nacio_de_solicitud
		   FROM viajes.viaje v %s ORDER BY creado_en DESC, id DESC LIMIT $%d`, where, n)

	rows, err := tx.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("operacion: listar viajes: %w", err)
	}
	items := []operclient.Viaje{}
	ids := []string{}
	for rows.Next() {
		var (
			id        uuid.UUID
			estado    string
			clienteID uuid.UUID
			origen    string
			fhs       *time.Time
			eta       *time.Time
			creado    time.Time
			penaliza  bool
		)
		if err := rows.Scan(&id, &estado, &clienteID, &origen, &fhs, &eta, &creado, &penaliza); err != nil {
			rows.Close()
			return nil, fmt.Errorf("operacion: escanear viaje: %w", err)
		}
		cid := operclient.Id(clienteID)
		items = append(items, operclient.Viaje{
			Id: id, Estado: operclient.EstadoViaje(estado), ClienteId: &cid,
			Origen: origen, FechaHoraServicio: fhs, EtaEstimado: eta, CreadoEn: creado,
			CancelacionConPenalizacion: &penaliza,
		})
		ids = append(ids, id.String())
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("operacion: iterar viajes: %w", err)
	}

	var cursorSig *string
	if len(items) > paginaMax {
		last := items[paginaMax-1]
		cur := encodeCursor(last.CreadoEn, last.Id)
		cursorSig = &cur
		items = items[:paginaMax]
		ids = ids[:paginaMax]
	}

	destinosPorViaje, err := s.destinosDeVarios(ctx, tx, ids)
	if err != nil {
		return nil, err
	}
	for i := range items {
		d := destinosPorViaje[items[i].Id.String()]
		if d == nil {
			d = []operclient.Destino{}
		}
		items[i].Destinos = d
	}

	return &operclient.PaginaViajes{Items: items, CursorSiguiente: cursorSig}, nil
}

// destinosDe lee los destinos de un viaje, en orden. Nunca nil: Destinos es
// requerido por el contrato.
func (s *Servicio) destinosDe(ctx context.Context, tx pgx.Tx, viajeID operclient.Id) ([]operclient.Destino, error) {
	rows, err := tx.Query(ctx,
		`SELECT nombre, numero_envio, fecha_cita FROM viajes.destino WHERE viaje_id = $1 ORDER BY orden`, viajeID)
	if err != nil {
		return nil, fmt.Errorf("operacion: leer destinos: %w", err)
	}
	defer rows.Close()
	out := []operclient.Destino{}
	for rows.Next() {
		var (
			nombre string
			ne     *string
			fc     *time.Time
		)
		if err := rows.Scan(&nombre, &ne, &fc); err != nil {
			return nil, fmt.Errorf("operacion: escanear destino: %w", err)
		}
		out = append(out, operclient.Destino{Nombre: nombre, NumeroEnvio: ne, FechaCita: fc})
	}
	return out, rows.Err()
}

// destinosDeVarios lee los destinos de varios viajes en una sola consulta y los
// agrupa por viaje_id (evita N+1 al listar).
func (s *Servicio) destinosDeVarios(ctx context.Context, tx pgx.Tx, ids []string) (map[string][]operclient.Destino, error) {
	out := map[string][]operclient.Destino{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := tx.Query(ctx,
		`SELECT viaje_id, nombre, numero_envio, fecha_cita FROM viajes.destino
		  WHERE viaje_id = ANY($1::uuid[]) ORDER BY viaje_id, orden`, ids)
	if err != nil {
		return nil, fmt.Errorf("operacion: leer destinos (lote): %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			vid    uuid.UUID
			nombre string
			ne     *string
			fc     *time.Time
		)
		if err := rows.Scan(&vid, &nombre, &ne, &fc); err != nil {
			return nil, fmt.Errorf("operacion: escanear destino (lote): %w", err)
		}
		k := vid.String()
		out[k] = append(out[k], operclient.Destino{Nombre: nombre, NumeroEnvio: ne, FechaCita: fc})
	}
	return out, rows.Err()
}

// condicionesAbiertas lee las condiciones ortogonales sin cerrar de un viaje.
// Nunca nil: CondicionesActivas es requerido por el contrato.
func (s *Servicio) condicionesAbiertas(ctx context.Context, tx pgx.Tx, viajeID operclient.Id) ([]operclient.CondicionAbierta, error) {
	rows, err := tx.Query(ctx,
		`SELECT tipo, abierta_en FROM viajes.condicion_viaje
		  WHERE viaje_id = $1 AND cerrada_en IS NULL ORDER BY abierta_en`, viajeID)
	if err != nil {
		return nil, fmt.Errorf("operacion: leer condiciones: %w", err)
	}
	defer rows.Close()
	out := []operclient.CondicionAbierta{}
	for rows.Next() {
		var (
			tipo string
			ab   time.Time
		)
		if err := rows.Scan(&tipo, &ab); err != nil {
			return nil, fmt.Errorf("operacion: escanear condicion: %w", err)
		}
		out = append(out, operclient.CondicionAbierta{Tipo: operclient.CondicionViaje(tipo), AbiertaEn: ab})
	}
	return out, rows.Err()
}

// incidenciasAbiertas lee las incidencias abiertas de un viaje con su
// tipo_interno (AI27-59). Es la misma fila de viajes.condicion_viaje que
// condicionesAbiertas, pero filtrada a tipo='INCIDENCIA' y proyectando el
// tipo_interno (consumo interno de bff, DEC-006) más el id para que bff pueda
// referenciar cada incidencia. Una incidencia que está en la lista está abierta
// por definición, así que su estado es siempre en_atencion (una resuelta tiene
// cerrada_en y queda fuera del filtro). Nunca nil: el campo es opcional en el
// contrato, pero devolvemos una lista vacía (no nil) para una forma estable.
func (s *Servicio) incidenciasAbiertas(ctx context.Context, tx pgx.Tx, viajeID operclient.Id) ([]operclient.IncidenciaActiva, error) {
	rows, err := tx.Query(ctx,
		`SELECT id, tipo_interno, abierta_en FROM viajes.condicion_viaje
		  WHERE viaje_id = $1 AND tipo = 'INCIDENCIA' AND cerrada_en IS NULL
		  ORDER BY abierta_en`, viajeID)
	if err != nil {
		return nil, fmt.Errorf("operacion: leer incidencias abiertas: %w", err)
	}
	defer rows.Close()
	out := []operclient.IncidenciaActiva{}
	for rows.Next() {
		var (
			id          uuid.UUID
			tipoInterno *string
			ab          time.Time
		)
		if err := rows.Scan(&id, &tipoInterno, &ab); err != nil {
			return nil, fmt.Errorf("operacion: escanear incidencia abierta: %w", err)
		}
		ti := ""
		if tipoInterno != nil {
			ti = *tipoInterno
		}
		out = append(out, operclient.IncidenciaActiva{
			Id:          id,
			TipoInterno: ti,
			Estado:      operclient.IncidenciaActivaEstadoEnAtencion,
			AbiertaEn:   ab.UTC(),
		})
	}
	return out, rows.Err()
}

// encodeCursor y decodeCursor codifican el cursor keyset (creado_en, id) como un
// token opaco base64. Su forma interna no es contrato (el cliente lo devuelve tal cual).
func encodeCursor(t time.Time, id operclient.Id) string {
	return base64.RawURLEncoding.EncodeToString([]byte(t.UTC().Format(time.RFC3339Nano) + "|" + id.String()))
}

func decodeCursor(s string) (time.Time, operclient.Id, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return time.Time{}, operclient.Id{}, err
	}
	partes := strings.SplitN(string(raw), "|", 2)
	if len(partes) != 2 {
		return time.Time{}, operclient.Id{}, fmt.Errorf("cursor mal formado")
	}
	t, err := time.Parse(time.RFC3339Nano, partes[0])
	if err != nil {
		return time.Time{}, operclient.Id{}, err
	}
	id, err := uuid.Parse(partes[1])
	if err != nil {
		return time.Time{}, operclient.Id{}, err
	}
	return t, id, nil
}
