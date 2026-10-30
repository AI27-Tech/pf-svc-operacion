package viajes

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/estados"
	operclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/operacion"
	platformerrors "github.com/AI27-Tech/pf-pkg-platform-go/errors"
	"github.com/AI27-Tech/pf-pkg-platform-go/outbox"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// audienciaCliente es el valor de dominio de la Audiencia del actor cliente
// (auth.AudienciaCliente, DEC-054). Se compara como string plano para no acoplar
// este paquete de dominio al paquete auth: el handler traduce claims.Audiencia.
const audienciaCliente = "Cliente"

// Transicionar mueve un viaje a un estado nuevo. viajes es la ÚNICA autoridad de
// transición (P14): valida la arista contra la máquina de estados generada
// (estados.PuedeTransicionar), aplica la guarda que la arista tenga, y escribe
// el cambio de estado, su fila de historia y el evento viaje.estado_cambiado en
// la MISMA transacción (P17). Devuelve el viaje actualizado.
//
// rol es el rol del actor (claims.Rol), necesario para las guardas que verifican
// facultad por rol (el cierre anticipado por incidencia solo lo hace un operador
// interno autorizado, DEC-020 #6 / DEC-060 #1). El resto de las aristas lo ignora.
//
// audiencia y tenantID son la Audiencia de dominio y el tenant del actor
// (claims.Audiencia, claims.TenantID). Habilitan la transición de cliente de
// E9-T7 (DEC-111): un actor de audiencia Cliente solo puede cancelar o rechazar
// SU PROPIO viaje. Para audiencia Interno no cambian el comportamiento previo.
func (s *Servicio) Transicionar(ctx context.Context, id operclient.Id, in operclient.TransicionEntrada, rol, audiencia, tenantID string) (*operclient.Viaje, error) {
	destino := estados.EstadoViaje(string(in.AEstado))

	tx, err := s.tx.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		estadoAct string
		clienteID uuid.UUID
		origen    string
		fhs       *time.Time
		eta       *time.Time
		creado    time.Time
	)
	err = tx.QueryRow(ctx,
		`SELECT estado, cliente_id, origen, fecha_hora_servicio, eta_estimado, creado_en
		   FROM viajes.viaje WHERE id = $1`, id,
	).Scan(&estadoAct, &clienteID, &origen, &fhs, &eta, &creado)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, platformerrors.NewNoEncontrado("operacion: viaje %s no encontrado", id)
	}
	if err != nil {
		return nil, fmt.Errorf("operacion: cargar viaje: %w", err)
	}

	// Facultades del cliente sobre SU viaje (E9-T7, DEC-111). Solo aplica a
	// audiencia Cliente; un actor interno conserva el comportamiento previo.
	if audiencia == audienciaCliente {
		// (a) Aislamiento por tenant. tenant = cliente (DEC-112): un cliente solo
		// toca su propio viaje. Un viaje ajeno devuelve 404, NO 403: un 403
		// confirmaría que el viaje existe (fuga entre clientes). 404 no revela nada.
		//
		// shortcut (DEC-112): la igualdad tenant==cliente es regla de dominio
		// ratificada, pero su garantía end-to-end depende de la semilla de tenants,
		// del alta que iguala los UUID y del vínculo usuario↔cliente (DEC-045), aún
		// pendientes. La comparación es correcta en lógica; su blindaje real llega
		// cuando esas piezas existan.
		if tenantID == "" || tenantID != clienteID.String() {
			return nil, platformerrors.NewNoEncontrado("operacion: viaje %s no encontrado", id)
		}
		// (b) El cliente solo cancela o rechaza (DEC-111). Cualquier otra arista
		// es facultad del operador: 401 (NewProhibido/403 no existe en la taxonomía
		// de plataforma; NewNoAutorizado es el patrón ya usado para negar facultad).
		if destino != estados.EstadoCancelado && destino != estados.EstadoRechazadoPorCliente {
			return nil, platformerrors.NewNoAutorizado("operacion: un cliente solo puede cancelar o rechazar su viaje")
		}
		// (c) Motivo OBLIGATORIO en cancelar y rechazar del cliente (DEC-111,
		// supera DEC-020 #7). Texto libre; vacío o solo espacios no cuenta.
		if in.Causa == nil || strings.TrimSpace(*in.Causa) == "" {
			return nil, platformerrors.NewValidacion("operacion: cancelar o rechazar el viaje exige un motivo")
		}
	}

	de := estados.EstadoViaje(estadoAct)
	if !estados.PuedeTransicionar(de, destino) {
		return nil, platformerrors.NewConflicto("operacion: transición no permitida de %s a %s", de, destino)
	}

	// El cierre (destino CERRADO) tiene su propia validación: necesita leer una
	// condición de la base (incidencia abierta) y verificar el rol del actor, lo
	// que no cabe en la firma de una `guarda` pura. Se resuelve aparte, DENTRO de
	// la misma tx, antes de aplicar el cambio de estado.
	var motivoCierre string
	if destino == estados.EstadoCerrado {
		m, err := s.validarCierre(ctx, tx, id, de, in, rol)
		if err != nil {
			return nil, err
		}
		motivoCierre = m
	} else if g, ok := guardas[transicionKey{de, destino}]; ok {
		if err := g(datosGuarda{fechaHoraServicio: fhs}, in); err != nil {
			return nil, err
		}
	}

	if _, err := tx.Exec(ctx, `UPDATE viajes.viaje SET estado = $1 WHERE id = $2`, string(destino), id); err != nil {
		return nil, fmt.Errorf("operacion: actualizar estado: %w", err)
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO viajes.transicion_viaje (viaje_id, de_estado, a_estado, causa) VALUES ($1,$2,$3,$4)`,
		id, estadoAct, string(destino), in.Causa,
	); err != nil {
		return nil, fmt.Errorf("operacion: registrar transición: %w", err)
	}

	payload := map[string]any{
		"viaje_id":    id.String(),
		"de_estado":   estadoAct,
		"a_estado":    string(destino),
		"cambiado_en": time.Now().UTC(),
	}
	if in.Causa != nil {
		payload["causa"] = *in.Causa
	}
	if err := s.pub.Publish(ctx, tx, outbox.Event{
		Tipo:       "viaje.estado_cambiado",
		Version:    1,
		AgregadoID: id.String(), // clave de orden = viaje_id
		Payload:    payload,
	}); err != nil {
		return nil, err
	}

	// Al cerrar, además del viaje.estado_cambiado genérico (que `reportes`
	// proyecta como cualquier transición), se publica viaje.cerrado con el MOTIVO
	// en el payload, en la MISMA tx (P17). `mensajeria` lo consume para decidir la
	// ventana de escritura de la conversación (3 días sin incidencia / 6 meses con
	// incidencia, DEC-030/041) SIN volver a llamar a operacion (E1-T8).
	if destino == estados.EstadoCerrado {
		if err := s.pub.Publish(ctx, tx, outbox.Event{
			Tipo:       "viaje.cerrado",
			Version:    1,
			AgregadoID: id.String(), // clave de orden = viaje_id
			Payload: map[string]any{
				"viaje_id":      id.String(),
				"motivo_cierre": motivoCierre,
				"de_estado":     estadoAct,
				"cerrado_en":    time.Now().UTC(),
			},
		}); err != nil {
			return nil, err
		}
	}

	// Los destinos para el viaje devuelto se leen DENTRO de la tx, antes del commit.
	destinos, err := s.destinosDe(ctx, tx, id)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("operacion: commit transición: %w", err)
	}

	cid := operclient.Id(clienteID)
	return &operclient.Viaje{
		Id:                id,
		Estado:            in.AEstado,
		ClienteId:         &cid,
		Origen:            origen,
		Destinos:          destinos,
		FechaHoraServicio: fhs,
		EtaEstimado:       eta,
		CreadoEn:          creado,
	}, nil
}

// transicionKey identifica una arista de la máquina de estados.
type transicionKey struct{ de, a estados.EstadoViaje }

// datosGuarda son los datos del viaje que una guarda puede necesitar.
type datosGuarda struct {
	fechaHoraServicio *time.Time
}

// guarda valida una arista más allá de que exista. Devuelve nil si pasa.
type guarda func(datosGuarda, operclient.TransicionEntrada) error

// guardas asocia cada arista con guarda a su validación. Las aristas SIN entrada
// no tienen guarda y pasan libres (modela el dominio como tabla, no como ifs
// dispersos). Las guardas que dependen de subsistemas aún no construidos se
// declaran como diferidas: rechazan la arista con un mensaje claro hasta que su
// rebanada las implemente (escolta → asignación; cierre → multi-parada /
// incidencia).
var guardas = map[transicionKey]guarda{
	{estados.EstadoSolicitado, estados.EstadoAprobado}:         guardaFechaHoraServicio,
	{estados.EstadoSolicitado, estados.EstadoRechazadoPorAi27}: guardaMotivo,
	{estados.EstadoAprobado, estados.EstadoAsignado}:           guardaDiferida("la asignación se hace con asignarRecursos (reserva de escolta), no con transiciones"),
	// Las tres aristas de cierre (EN_DESTINO/EN_DESCARGA/EN_TRANSITO -> CERRADO)
	// NO están aquí: su validación necesita leer la base (incidencia abierta) y el
	// rol del actor, que la firma de `guarda` no admite. La resuelve
	// validarCierre, invocada desde Transicionar cuando el destino es CERRADO.
}

// guardaFechaHoraServicio exige fecha_hora_servicio antes de aprobar (DEC-010 #2).
func guardaFechaHoraServicio(d datosGuarda, _ operclient.TransicionEntrada) error {
	if d.fechaHoraServicio == nil {
		return platformerrors.NewValidacion("operacion: aprobar un viaje exige fecha_hora_servicio definida")
	}
	return nil
}

// guardaMotivo exige una causa en el rechazo (DEC-020 #2).
func guardaMotivo(_ datosGuarda, in operclient.TransicionEntrada) error {
	if in.Causa == nil || strings.TrimSpace(*in.Causa) == "" {
		return platformerrors.NewValidacion("operacion: el rechazo exige una causa (motivo)")
	}
	return nil
}

// guardaDiferida rechaza una arista cuya guarda real depende de un subsistema que
// esta rebanada no construye todavía. 409 con mensaje claro.
func guardaDiferida(motivo string) guarda {
	return func(datosGuarda, operclient.TransicionEntrada) error {
		return platformerrors.NewConflicto("operacion: transición no soportada todavía: %s", motivo)
	}
}

// --- Cierre del servicio (E1-T8 / E1-HU-010) --------------------------------

// Motivos de cierre (catálogo cerrado, DEC-113). NORMAL solo ocurre tras la
// última parada; POR_INCIDENCIA habilita además el cierre anticipado (DEC-020 #6).
// Se retiró `bitacora` (era una cita a DEC-013 sin respaldo, DEC-113). El motivo
// viaja en el payload de viaje.cerrado para que `mensajeria` decida la ventana de
// la conversación (DEC-030/041). Los valores son los del enum MotivoCierre del
// contrato (tracer.24).
const (
	motivoCierreNormal        = string(operclient.Normal)
	motivoCierrePorIncidencia = string(operclient.PorIncidencia)
)

// rolesQuePuedenCerrar son los roles internos facultados para cerrar un viaje
// (DEC-060 #1): el jefe_operativo y, por su alcance global, el administrador.
// Monitorista, supervisor, coordinador, ejecutivo, reacción y cliente NO cierran.
var rolesQuePuedenCerrar = map[string]bool{
	"jefe_operativo": true,
	"administrador":  true,
}

// validarCierre valida la transición a CERRADO y devuelve el motivo de cierre
// normalizado. Reúne las tres aristas de cierre del YAML en una sola regla porque
// comparten validaciones y la distinción normal/anticipado se deriva del estado
// de origen:
//
//   - Motivo OBLIGATORIO del catálogo normal|por_incidencia. Sin motivo o con un
//     valor fuera del catálogo -> 400 (DEC-113).
//   - Cierre NORMAL: válido solo desde EN_DESTINO o EN_DESCARGA, es decir con el
//     convoy en una parada (guarda `ultima_parada` del YAML). Desde EN_TRANSITO
//     NO es un cierre normal.
//   - Cierre ANTICIPADO (desde EN_TRANSITO, antes de llegar a una parada): válido
//     SOLO con motivo por_incidencia, SOLO por un rol operador interno autorizado
//     (DEC-020 #6 / DEC-060 #1) y SOLO con una condición INCIDENCIA abierta.
//
// El motivo se lee de `motivo_cierre` (enum MotivoCierre, tracer.24/DEC-113), que
// reemplazó el puente previo sobre `causa`. El contrato lo marca opcional (aplica
// solo al cerrar); aquí es obligatorio para la arista a CERRADO.
//
// shortcut (ADR/E6-HU-033 pendiente): "última parada" se aproxima por el estado
// de origen (EN_DESTINO/EN_DESCARGA). No se verifica que sea literalmente la
// ÚLTIMA parada de la ruta: no existe un puntero de parada actual en el esquema y
// la condición exacta de "listo para cerrar" no está fijada en las fuentes.
func (s *Servicio) validarCierre(ctx context.Context, tx pgx.Tx, id operclient.Id, de estados.EstadoViaje, in operclient.TransicionEntrada, rol string) (string, error) {
	// Motivo obligatorio, del campo tipado motivo_cierre (DEC-113).
	if in.MotivoCierre == nil {
		return "", platformerrors.NewValidacion("operacion: el cierre exige un motivo (normal o por_incidencia)")
	}
	motivo := string(*in.MotivoCierre)
	switch motivo {
	case motivoCierreNormal, motivoCierrePorIncidencia:
		// válido
	default:
		return "", platformerrors.NewValidacion("operacion: motivo de cierre inválido %q; use normal o por_incidencia", motivo)
	}

	// Cierre anticipado: desde EN_TRANSITO, antes de llegar a una parada.
	if de == estados.EstadoEnTransito {
		if motivo != motivoCierrePorIncidencia {
			return "", platformerrors.NewConflictoCodigo("cierre_anticipado_solo_por_incidencia",
				"operacion: el cierre anticipado (antes de la última parada) solo es válido con motivo por_incidencia")
		}
		if !rolesQuePuedenCerrar[rol] {
			return "", platformerrors.NewProhibido("operacion: solo un operador interno autorizado puede cerrar anticipadamente un viaje")
		}
		abierta, err := s.hayIncidenciaAbierta(ctx, tx, id)
		if err != nil {
			return "", err
		}
		if !abierta {
			return "", platformerrors.NewConflictoCodigo("cierre_anticipado_requiere_incidencia",
				"operacion: el cierre anticipado requiere una condición INCIDENCIA abierta")
		}
		return motivo, nil
	}

	// Cierre normal (desde EN_DESTINO o EN_DESCARGA): tras la última parada.
	// La facultad de cierre es de rol operador interno autorizado en todos los
	// casos (DEC-060 #1): un cliente nunca cierra.
	if !rolesQuePuedenCerrar[rol] {
		return "", platformerrors.NewProhibido("operacion: solo un operador interno autorizado puede cerrar un viaje")
	}
	return motivo, nil
}

// hayIncidenciaAbierta indica si el viaje tiene una condición INCIDENCIA sin
// cerrar (cerrada_en IS NULL). Es la precondición del cierre anticipado (E1-T8);
// coordina con E1-T6/AI27-14, que abre y cierra esa misma condición en
// viajes.condicion_viaje.
func (s *Servicio) hayIncidenciaAbierta(ctx context.Context, tx pgx.Tx, id operclient.Id) (bool, error) {
	var existe bool
	err := tx.QueryRow(ctx,
		`SELECT EXISTS (
		   SELECT 1 FROM viajes.condicion_viaje
		    WHERE viaje_id = $1 AND tipo = 'INCIDENCIA' AND cerrada_en IS NULL
		 )`, id,
	).Scan(&existe)
	if err != nil {
		return false, fmt.Errorf("operacion: verificar incidencia abierta: %w", err)
	}
	return existe, nil
}
