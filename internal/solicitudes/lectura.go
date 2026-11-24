package solicitudes

import (
	"context"
	"encoding/base64"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/estados"
	operclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/operacion"
	platformerrors "github.com/AI27-Tech/pf-pkg-platform-go/errors"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Roles que tocan la solicitud (los nombres de roles.Rol; este módulo no importa
// el paquete de roles para no acoplar el dominio a la lista completa).
const (
	rolCliente       = "cliente"
	rolCotizacion    = "cotizacion"
	RolJefeOperativo = "jefe_operativo"
	rolAdministrador = "administrador"
)

// paginaMax es el tamaño de página de Listar (igual que el listado de viajes).
const paginaMax = 50

// diasPorVencer es la anticipación del aviso "por vencer": 3 días antes del
// último día de vigencia (DEC-148 #57).
const diasPorVencer = 3

// Marcas de aviso dentro de Argos (DEC-148 #53 a #57); el vocabulario es el de
// Solicitud.avisos del contrato.
const (
	AvisoNueva                      = "nueva"
	AvisoCotizacionNueva            = "cotizacion_nueva"
	AvisoPorVencer                  = "por_vencer"
	AvisoCancelacionConPenalizacion = "cancelacion_con_penalizacion"
	AvisoRechazoDeAI27              = "rechazo_de_ai27"
)

// tieneAcceso dice si el rol toca solicitudes. Cualquier otro (monitorista,
// supervisor, coordinador, ejecutivo, reaccion) no tiene acceso (DEC-148 #12, #28).
func (a Actor) tieneAcceso() bool {
	switch a.Rol {
	case rolCliente, rolCotizacion, RolJefeOperativo, rolAdministrador:
		return true
	}
	return false
}

// puedeVer es el alcance por rol sobre UNA solicitud (E10-T9): el cliente, las de
// su organización; cotizacion y el administrador, todas en cualquier estado; el
// jefe operativo, solo las que están en PAGADO (DEC-148 #20, #22). Lo que no ve
// responde como si no existiera.
func (a Actor) puedeVer(f *fila) bool {
	switch a.Rol {
	case rolCliente:
		return a.TenantID != "" && a.TenantID == f.ClienteID.String()
	case rolCotizacion, rolAdministrador:
		return true
	case RolJefeOperativo:
		return f.Estado == estados.EstadoSolicitudPagado
	}
	return false
}

// veComercial dice si el rol ve lo comercial de la solicitud: la cotización
// (monto, moneda, notas, versiones) y los datos del pago. El jefe operativo no
// (DEC-148 #67); es lista de permitidos, así un rol nuevo no lo ve por omisión.
func (a Actor) veComercial() bool {
	switch a.Rol {
	case rolCliente, rolCotizacion, rolAdministrador:
		return true
	}
	return false
}

// exigeAcceso es 403 para un rol sin acceso a solicitudes; se decide por el rol y
// antes de tocar la base, así no revela si una solicitud existe.
func (a Actor) exigeAcceso() error {
	if !a.tieneAcceso() {
		return platformerrors.NewProhibido("operacion: el rol %q no tiene acceso a las solicitudes", a.Rol)
	}
	return nil
}

// Listar devuelve las solicitudes que el actor puede ver, de la más nueva a la más
// antigua, en páginas de paginaMax con cursor keyset sobre (creado_en, id). Cada
// fila lleva las marcas de aviso del actor. `estado` filtra; un cursor inválido es 400.
// Cliente: las de su organización. cotizacion y administrador: todas. jefe
// operativo: solo PAGADO (su bandeja). Otro rol: 403.
func (s *Servicio) Listar(ctx context.Context, a Actor, estado *operclient.EstadoSolicitud, cursor *string) (*operclient.PaginaSolicitudes, error) {
	if err := a.exigeAcceso(); err != nil {
		return nil, err
	}
	var conds []string
	var args []any
	arg := func(v any) string { args = append(args, v); return fmt.Sprintf("$%d", len(args)) }

	switch a.Rol {
	case rolCliente:
		cliente, err := uuid.Parse(a.TenantID)
		if err != nil {
			return nil, platformerrors.NewProhibido("operacion: el token no trae una organización válida")
		}
		conds = append(conds, "cliente_id = "+arg(cliente))
	case RolJefeOperativo:
		conds = append(conds, "estado = "+arg(string(estados.EstadoSolicitudPagado)))
	}
	if estado != nil {
		if !slices.Contains(estados.EstadosSolicitud, estados.EstadoSolicitud(*estado)) {
			return nil, platformerrors.NewValidacion("operacion: estado %q desconocido", *estado)
		}
		conds = append(conds, "estado = "+arg(string(*estado)))
	}
	if cursor != nil && *cursor != "" {
		t, id, err := decodeCursor(*cursor)
		if err != nil {
			return nil, platformerrors.NewValidacion("operacion: cursor inválido")
		}
		conds = append(conds, fmt.Sprintf("(creado_en, id) < (%s, %s)", arg(t), arg(id)))
	}
	where := ""
	if len(conds) > 0 {
		where = "WHERE " + strings.Join(conds, " AND ")
	}

	tx, err := s.tx.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }() // solo lectura
	rows, err := tx.Query(ctx, `SELECT `+columnasFila+` FROM solicitudes.solicitud `+where+
		fmt.Sprintf(` ORDER BY creado_en DESC, id DESC LIMIT %d`, paginaMax+1), args...)
	if err != nil {
		return nil, fmt.Errorf("solicitudes: listar: %w", err)
	}
	filas, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (*fila, error) {
		var f fila
		var est string
		if err := r.Scan(f.destino(&est)...); err != nil {
			return nil, err
		}
		f.Estado = estados.EstadoSolicitud(est)
		return &f, nil
	})
	if err != nil {
		return nil, fmt.Errorf("solicitudes: leer el listado: %w", err)
	}
	var sig *string
	if len(filas) > paginaMax {
		filas = filas[:paginaMax]
		c := encodeCursor(filas[paginaMax-1].CreadoEn, filas[paginaMax-1].ID)
		sig = &c
	}
	// shortcut: la cotización y el pago de cada fila se leen una por una (hasta 50
	// por página); pasar a un solo JOIN si el listado se vuelve el camino caliente.
	items := make([]operclient.Solicitud, 0, len(filas))
	for _, f := range filas {
		out, err := s.armar(ctx, tx, f, a)
		if err != nil {
			return nil, err
		}
		items = append(items, *out)
	}
	return &operclient.PaginaSolicitudes{Items: items, CursorSiguiente: sig}, nil
}

// Obtener devuelve la solicitud con su cotización vigente y las marcas de aviso del
// actor. Un rol sin acceso recibe 403; una solicitud que el actor no ve (de otra
// organización, o fuera de PAGADO para el jefe operativo) responde 404, igual que
// una que no existe.
func (s *Servicio) Obtener(ctx context.Context, a Actor, id uuid.UUID) (*operclient.Solicitud, error) {
	if err := a.exigeAcceso(); err != nil {
		return nil, err
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
	return s.armar(ctx, tx, f, a)
}

// avisos deriva las marcas de aviso del estado y la hora, sin almacenarlas
// (DEC-148 #53 a #57). Para el jefe operativo y los demás roles no hay ninguna.
//
//	cotizacion y administrador: nueva (SOLICITADO), por_vencer, cancelacion_con_penalizacion
//	cliente: cotizacion_nueva, por_vencer, rechazo_de_ai27
//
// Sin aviso por pago confirmado ni por aprobación (#56). Cada aviso se apaga solo al
// cambiar el estado (DEC-148 #69 para `cotizacion_nueva`).
func (s *Servicio) avisos(a Actor, f *fila, cot *operclient.Cotizacion) []string {
	out := []string{}
	porVencer := f.Estado == estados.EstadoSolicitudCotizado && cot != nil &&
		!diaMX(s.ahora()).Before(cot.VigenteHasta.Time.AddDate(0, 0, -diasPorVencer))
	switch a.Rol {
	case rolCotizacion, rolAdministrador:
		if f.Estado == estados.EstadoSolicitudSolicitado {
			out = append(out, AvisoNueva)
		}
		if porVencer {
			out = append(out, AvisoPorVencer)
		}
		if f.Estado == estados.EstadoSolicitudCanceladoPorCliente && f.Marca {
			out = append(out, AvisoCancelacionConPenalizacion)
		}
	case rolCliente:
		// "Cotización nueva" dura mientras la solicitud sigue en COTIZADO (DEC-148 #69).
		if f.Estado == estados.EstadoSolicitudCotizado {
			out = append(out, AvisoCotizacionNueva)
		}
		if porVencer {
			out = append(out, AvisoPorVencer)
		}
		if f.Estado == estados.EstadoSolicitudRechazadoPorAi27 {
			out = append(out, AvisoRechazoDeAI27)
		}
	}
	return out
}

// encodeCursor y decodeCursor: cursor keyset opaco (creado_en, id), como el de viajes.
func encodeCursor(t time.Time, id uuid.UUID) string {
	return base64.RawURLEncoding.EncodeToString([]byte(t.UTC().Format(time.RFC3339Nano) + "|" + id.String()))
}

func decodeCursor(s string) (time.Time, uuid.UUID, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return time.Time{}, uuid.Nil, err
	}
	partes := strings.SplitN(string(raw), "|", 2)
	if len(partes) != 2 {
		return time.Time{}, uuid.Nil, fmt.Errorf("cursor mal formado")
	}
	t, err := time.Parse(time.RFC3339Nano, partes[0])
	if err != nil {
		return time.Time{}, uuid.Nil, err
	}
	id, err := uuid.Parse(partes[1])
	return t, id, err
}
