package solicitudes

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/estados"
	eventosv1 "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/eventos/ai27/eventos/v1"
	platformerrors "github.com/AI27-Tech/pf-pkg-platform-go/errors"
	"github.com/AI27-Tech/pf-pkg-platform-go/outbox"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Tipos de evento del módulo (registro.tipos.yaml de tracer.45).
const (
	TipoSolicitudCreada         = "solicitud.creada"
	TipoSolicitudEstadoCambiado = "solicitud.estado_cambiado"
)

// Disparador es quién origina una transición (columna `disparador` del ADR 38).
// El administrador hereda las facultades de cotizacion y del jefe operativo
// (DEC-148 #20); esa herencia la decide la facultad del comando, no la arista.
type Disparador string

const (
	DisparadorCliente       Disparador = "cliente"
	DisparadorCotizacion    Disparador = "cotizacion"
	DisparadorJefeOperativo Disparador = "jefe_operativo"
	DisparadorAdministrador Disparador = "administrador"
	DisparadorSistema       Disparador = "sistema"
)

type arista struct{ de, a estados.EstadoSolicitud }

// regla es lo que el ADR 38 dice de cada arista además de su existencia.
type regla struct {
	disparador  Disparador
	accion      string // vocabulario de HistoricoSolicitudItem.accion
	exigeMotivo bool   // guarda motivo_obligatorio
	marca       bool   // la arista deja marca de penalización (DEC-148 #6)
}

// aristas es la tabla de metadatos del ADR 38. La existencia de la arista la
// decide estados.PuedeTransicionarSolicitud (dato generado); una prueba verifica
// que esta tabla y el dato generado tienen exactamente las mismas aristas.
var aristas = map[arista]regla{
	{estados.EstadoSolicitudSolicitado, estados.EstadoSolicitudCotizado}:            {disparador: DisparadorCotizacion, accion: "cotizada"},
	{estados.EstadoSolicitudSolicitado, estados.EstadoSolicitudCanceladoPorCliente}: {disparador: DisparadorCliente, accion: "cancelada"},
	{estados.EstadoSolicitudCotizado, estados.EstadoSolicitudPagado}:                {disparador: DisparadorCotizacion, accion: "pago_confirmado"},
	{estados.EstadoSolicitudCotizado, estados.EstadoSolicitudRechazadoPorCliente}:   {disparador: DisparadorCliente, accion: "cotizacion_rechazada"},
	{estados.EstadoSolicitudCotizado, estados.EstadoSolicitudCanceladoPorCliente}:   {disparador: DisparadorCliente, accion: "cancelada"},
	{estados.EstadoSolicitudCotizado, estados.EstadoSolicitudVencida}:               {disparador: DisparadorSistema, accion: "vencida"},
	{estados.EstadoSolicitudVencida, estados.EstadoSolicitudPagado}:                 {disparador: DisparadorCotizacion, accion: "pago_confirmado_tardio"},
	{estados.EstadoSolicitudPagado, estados.EstadoSolicitudCotizado}:                {disparador: DisparadorAdministrador, accion: "pago_revertido", exigeMotivo: true},
	{estados.EstadoSolicitudPagado, estados.EstadoSolicitudAprobado}:                {disparador: DisparadorJefeOperativo, accion: "aprobada"},
	{estados.EstadoSolicitudPagado, estados.EstadoSolicitudRechazadoPorAi27}:        {disparador: DisparadorJefeOperativo, accion: "rechazada", exigeMotivo: true},
	{estados.EstadoSolicitudPagado, estados.EstadoSolicitudCanceladoPorCliente}:     {disparador: DisparadorCliente, accion: "cancelada", marca: true},
}

// Actor es quien ejecuta un comando, tomado de los claims por la fachada.
type Actor struct {
	Subject  string // `sub` del token; vacío en llamadas del sistema
	Rol      string
	TenantID string // organización del token (cliente_id) cuando el actor es un cliente
}

// Cambio es la transición pedida a transicionar.
type Cambio struct {
	A          estados.EstadoSolicitud
	Disparador Disparador
	Actor      Actor
	Motivo     string
}

// transicionar es la ÚNICA función que cambia `solicitud.estado` (E10-T2). La
// solicitud llega ya cargada y bloqueada (FOR UPDATE) por el comando. Valida la
// arista contra la máquina generada, el disparador y las guardas de la arista;
// y escribe el cambio, su fila en transicion_solicitud y el evento
// solicitud.estado_cambiado en la MISMA transacción: si algo falla, el llamador
// hace rollback y no queda nada. Actualiza sol con el estado nuevo.
func (s *Servicio) transicionar(ctx context.Context, tx pgx.Tx, sol *fila, c Cambio) error {
	de := sol.Estado
	if !estados.PuedeTransicionarSolicitud(de, c.A) {
		return platformerrors.NewConflictoCodigo("transicion_no_permitida",
			"operacion: la solicitud no puede pasar de %s a %s", de, c.A)
	}
	r, ok := aristas[arista{de, c.A}]
	if !ok {
		// El dato generado ganó una arista que esta tabla no conoce: es un defecto,
		// no una entrada del usuario.
		return fmt.Errorf("solicitudes: la arista %s -> %s no tiene reglas en aristas", de, c.A)
	}
	if r.disparador != c.Disparador {
		return platformerrors.NewProhibido("operacion: %s -> %s lo origina %s, no %s", de, c.A, r.disparador, c.Disparador)
	}
	motivo := strings.TrimSpace(c.Motivo)
	if r.exigeMotivo && motivo == "" {
		return platformerrors.NewValidacion("operacion: el motivo es obligatorio para pasar de %s a %s", de, c.A)
	}

	en := s.ahora().UTC()
	// La marca la fija la arista (PAGADO -> CANCELADO_POR_CLIENTE), nunca el llamador.
	if _, err := tx.Exec(ctx,
		`UPDATE solicitudes.solicitud SET estado = $2, marca_penalizacion = $3 WHERE id = $1`,
		sol.ID, string(c.A), r.marca,
	); err != nil {
		return fmt.Errorf("solicitudes: actualizar estado: %w", err)
	}
	var marcaHistorico *bool
	if c.A == estados.EstadoSolicitudCanceladoPorCliente {
		marcaHistorico = &r.marca
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO solicitudes.transicion_solicitud
		   (solicitud_id, de_estado, a_estado, accion, disparador, actor_id, actor_rol, motivo, marca_penalizacion, creado_en)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`,
		sol.ID, string(de), string(c.A), r.accion, string(c.Disparador),
		nullsiVacio(c.Actor.Subject), nullsiVacio(c.Actor.Rol), nullsiVacio(motivo), marcaHistorico, en,
	); err != nil {
		return fmt.Errorf("solicitudes: registrar transición: %w", err)
	}
	// Payload sin datos comerciales ni marca de penalización (solicitud.proto): solo
	// ids, estados y la marca de tiempo.
	if err := s.publicar(ctx, tx, TipoSolicitudEstadoCambiado, sol.ID, en, &eventosv1.SolicitudEstadoCambiado{
		SolicitudId: sol.ID.String(),
		ClienteId:   sol.ClienteID.String(),
		DeEstado:    string(de),
		AEstado:     string(c.A),
		CambiadoEn:  timestamppb.New(en),
	}); err != nil {
		return err
	}
	sol.Estado = c.A
	sol.Marca = r.marca
	return nil
}

// publicar escribe un evento en solicitudes.outbox dentro de tx. El cuerpo es el
// mensaje generado del contrato serializado como lo espera mensajeria: JSON con
// los nombres del .proto y las marcas de tiempo en RFC 3339.
func (s *Servicio) publicar(ctx context.Context, tx pgx.Tx, tipo string, id uuid.UUID, en time.Time, msg proto.Message) error {
	cuerpo, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(msg)
	if err != nil {
		return fmt.Errorf("solicitudes: serializar %s: %w", tipo, err)
	}
	return s.pub.Publish(ctx, tx, outbox.Event{
		Tipo:       tipo,
		Version:    1,
		AgregadoID: id.String(), // clave de orden = solicitud_id
		OcurridoEn: en,
		Payload:    json.RawMessage(cuerpo),
	})
}

func nullsiVacio(s string) any {
	if s == "" {
		return nil
	}
	return s
}
