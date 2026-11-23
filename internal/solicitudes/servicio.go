package solicitudes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/estados"
	eventosv1 "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/eventos/ai27/eventos/v1"
	operclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/operacion"
	platformerrors "github.com/AI27-Tech/pf-pkg-platform-go/errors"
	"github.com/AI27-Tech/pf-pkg-platform-go/outbox"
	"github.com/AI27-Tech/pf-svc-operacion/internal/auditoria"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// BeginTx es la frontera transaccional: el caso de uso controla el commit. La
// cumple *pgxpool.Pool (rol op_solicitudes, search_path solicitudes).
type BeginTx interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// Servicio son los casos de uso de la solicitud.
type Servicio struct {
	tx    BeginTx
	pub   *outbox.Publisher
	ahora func() time.Time
}

// NewServicio recibe el pool del rol op_solicitudes y el publicador de outbox.
func NewServicio(tx BeginTx, pub *outbox.Publisher) *Servicio {
	return &Servicio{tx: tx, pub: pub, ahora: time.Now}
}

// ConReloj sustituye el reloj (pruebas de vigencia y de marcas de tiempo).
func (s *Servicio) ConReloj(f func() time.Time) *Servicio {
	s.ahora = f
	return s
}

// exigeFutura rechaza (400) una fecha y hora de servicio que no sea posterior al
// momento actual del reloj del servicio (DEC-148 #66). Compara instantes: medir
// "ahora" en America/Mexico_City y en UTC da el mismo resultado; la zona solo
// cambia cómo se escribe la hora, no cuál es.
func (s *Servicio) exigeFutura(t time.Time) error {
	if !t.After(s.ahora()) {
		return platformerrors.NewValidacion("operacion: la fecha y hora del servicio debe ser posterior al momento actual")
	}
	return nil
}

// fila es la solicitud cargada de la base, con lo que las reglas necesitan.
type fila struct {
	ID                uuid.UUID
	ClienteID         uuid.UUID
	Estado            estados.EstadoSolicitud
	Marca             bool
	Origen            string
	Destinos          []operclient.Destino
	FechaHoraServicio *time.Time
	TipoServicio      *string
	Tracto            *operclient.Tracto
	CreadoEn          time.Time
}

// columnasFila y destino son las columnas de solicitud y dónde escanearlas.
const columnasFila = `id, cliente_id, estado, marca_penalizacion, origen, destinos, fecha_hora_servicio, tipo_servicio, tracto, creado_en`

func (f *fila) destino(estado *string) []any {
	return []any{&f.ID, &f.ClienteID, estado, &f.Marca, &f.Origen, &f.Destinos, &f.FechaHoraServicio, &f.TipoServicio, &f.Tracto, &f.CreadoEn}
}

// cargar lee la solicitud y, si bloquear, toma la fila FOR UPDATE: los comandos
// serializan sobre ella, así dos acciones concurrentes no se cuelan entre la
// guarda y la escritura. 404 si no existe.
func cargar(ctx context.Context, tx pgx.Tx, id uuid.UUID, bloquear bool) (*fila, error) {
	q := `SELECT ` + columnasFila + ` FROM solicitudes.solicitud WHERE id = $1`
	if bloquear {
		q += ` FOR UPDATE`
	}
	var f fila
	var estado string
	err := tx.QueryRow(ctx, q, id).Scan(f.destino(&estado)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, platformerrors.NewNoEncontrado("operacion: solicitud %s no encontrada", id)
	}
	if err != nil {
		return nil, fmt.Errorf("solicitudes: cargar solicitud: %w", err)
	}
	f.Estado = estados.EstadoSolicitud(estado)
	return &f, nil
}

// cargarDeCliente es cargar bloqueando, para los comandos del cliente: una
// solicitud de otra organización responde 404, no 403, para no revelar que existe.
func cargarDeCliente(ctx context.Context, tx pgx.Tx, id uuid.UUID, a Actor) (*fila, error) {
	f, err := cargar(ctx, tx, id, true)
	if err != nil {
		return nil, err
	}
	if a.TenantID == "" || a.TenantID != f.ClienteID.String() {
		return nil, platformerrors.NewNoEncontrado("operacion: solicitud %s no encontrada", id)
	}
	return f, nil
}

// enTx es el esqueleto de un comando sobre una solicitud existente: abre la
// transacción, carga la solicitud bloqueada (de la organización del actor si es
// un comando del cliente, 404 si es ajena), corre fn y devuelve la solicitud ya
// actualizada. Si fn falla, rollback: no queda ningún cambio, histórico ni evento.
func (s *Servicio) enTx(ctx context.Context, id uuid.UUID, delCliente bool, a Actor, fn func(pgx.Tx, *fila) error) (*operclient.Solicitud, error) {
	tx, err := s.tx.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var f *fila
	if delCliente {
		f, err = cargarDeCliente(ctx, tx, id, a)
	} else {
		f, err = cargar(ctx, tx, id, true)
	}
	if err != nil {
		return nil, err
	}
	if err := fn(tx, f); err != nil {
		return nil, err
	}
	out, err := s.armar(ctx, tx, f, a)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("solicitudes: commit: %w", err)
	}
	return out, nil
}

// armar convierte la fila en la Solicitud del contrato, con las marcas de aviso
// que le tocan al actor (avisos, lectura.go).
func (s *Servicio) armar(ctx context.Context, tx pgx.Tx, f *fila, a Actor) (*operclient.Solicitud, error) {
	cid := operclient.Id(f.ClienteID)
	cot, err := cotizacionVigente(ctx, tx, f.ID)
	if err != nil {
		return nil, err
	}
	pago, err := pagoVigente(ctx, tx, f.ID)
	if err != nil {
		return nil, err
	}
	// El cliente ve el motivo del rechazo de AI27 (DEC-148 #55); nil en otro estado.
	var motivoRechazo *string
	if f.Estado == estados.EstadoSolicitudRechazadoPorAi27 {
		if err := tx.QueryRow(ctx,
			`SELECT motivo FROM solicitudes.transicion_solicitud
			  WHERE solicitud_id = $1 AND a_estado = 'RECHAZADO_POR_AI27' ORDER BY id DESC LIMIT 1`, f.ID).Scan(&motivoRechazo); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("solicitudes: leer el motivo del rechazo: %w", err)
		}
	}
	return &operclient.Solicitud{
		MotivoRechazo:     motivoRechazo,
		Id:                f.ID,
		Estado:            operclient.EstadoSolicitud(f.Estado),
		ClienteId:         &cid,
		Origen:            f.Origen,
		Destinos:          f.Destinos,
		FechaHoraServicio: f.FechaHoraServicio,
		TipoServicio:      f.TipoServicio,
		Tracto:            f.Tracto,
		Cotizacion:        cot,
		Pago:              pago,
		MarcaPenalizacion: f.Marca,
		Avisos:            s.avisos(a, f, cot),
		CreadoEn:          f.CreadoEn,
	}, nil
}

// Crear registra la solicitud en SOLICITADO y publica solicitud.creada en la
// MISMA transacción (P17). La organización es la del token (a.TenantID): el
// cuerpo no la lleva. Origen y al menos un destino son obligatorios (E1-HU-001);
// la fecha y hora es opcional, se guarda tal cual (DEC-148 #38) y si viene debe ser
// posterior al momento actual (DEC-148 #66). El id lo genera
// el módulo y es el que heredará el viaje (DEC-148 #3).
func (s *Servicio) Crear(ctx context.Context, a Actor, in operclient.CrearSolicitudEntrada) (*operclient.Solicitud, error) {
	clienteID, err := uuid.Parse(a.TenantID)
	if err != nil {
		return nil, platformerrors.NewProhibido("operacion: el token no trae una organización válida")
	}
	if strings.TrimSpace(in.Origen) == "" {
		return nil, platformerrors.NewValidacion("operacion: origen es obligatorio")
	}
	if len(in.Destinos) == 0 {
		return nil, platformerrors.NewValidacion("operacion: se requiere al menos un destino")
	}
	for i, d := range in.Destinos {
		if strings.TrimSpace(d.Nombre) == "" {
			return nil, platformerrors.NewValidacion("operacion: el destino %d no tiene nombre", i+1)
		}
	}

	id := uuid.New()
	en := s.ahora().UTC()
	var fecha *time.Time
	if in.FechaHoraServicio != nil {
		t := in.FechaHoraServicio.UTC()
		if err := s.exigeFutura(t); err != nil {
			return nil, err
		}
		fecha = &t
	}

	tx, err := s.tx.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op tras commit

	// La creación se audita en la misma transacción (E10-T11). El detalle lleva ids,
	// nunca datos comerciales.
	detalle, err := json.Marshal(map[string]any{"solicitud_id": id.String(), "a_estado": string(estados.EstadoSolicitudSolicitado), "actor_rol": a.Rol})
	if err != nil {
		return nil, err
	}
	err = auditoria.Interceptar(ctx, tx, auditoria.Registro{Modulo: ModuloAuditoria, Accion: "creada", ActorID: a.Subject, Detalle: detalle},
		func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx,
				`INSERT INTO solicitudes.solicitud
				   (id, cliente_id, estado, marca_penalizacion, origen, destinos, fecha_hora_servicio, tipo_servicio, tracto, creado_en, creado_por)
				 VALUES ($1,$2,$3,false,$4,$5,$6,$7,$8,$9,$10)`,
				id, clienteID, string(estados.EstadoSolicitudSolicitado), in.Origen, in.Destinos, fecha, in.TipoServicio, in.Tracto, en, nullsiVacio(a.Subject),
			); err != nil {
				return fmt.Errorf("solicitudes: insertar solicitud: %w", err)
			}
			return s.publicar(ctx, tx, TipoSolicitudCreada, id, en, &eventosv1.SolicitudCreada{
				SolicitudId: id.String(),
				ClienteId:   clienteID.String(),
				CreadaEn:    timestamppb.New(en),
			})
		})
	if err != nil {
		return nil, err
	}
	f, err := cargar(ctx, tx, id, false)
	if err != nil {
		return nil, err
	}
	out, err := s.armar(ctx, tx, f, a)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("solicitudes: commit crear solicitud: %w", err)
	}
	return out, nil
}
