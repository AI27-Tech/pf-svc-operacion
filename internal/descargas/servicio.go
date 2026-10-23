// Package descargas registra el rastro de auditoría de la descarga del
// expediente de un viaje (AI27-80 / E1-T7). bff compone la descarga (ficha +
// bitácora + auditoría) y luego invoca a operacion para que DEJE el rastro: "el
// rastro lo escribe operacion en su transacción vía registrarDescargaViaje, no
// el bff" (contrato bff). bff no tiene base; el registro probatorio lo escribe
// operacion.
//
// El registro se escribe con el INTERCEPTOR de auditoría (P3) DENTRO de una
// transacción del rol dueño op_auditoria —que tiene INSERT sobre
// auditoria.registro_auditoria por diseño (migrations/auditoria/0001_init.sql)—.
// No se otorga GRANT de escritura a op_viajes ni se abre una segunda excepción
// de GRANT cruzado (la única es la de op_asignacion, ADR 31/33): el camino es el
// rol dueño, sin ADR.
//
// La lectura del viaje para la respuesta (su Viaje) corre por SEPARADO sobre el
// pool del rol op_viajes (un SELECT sobre su propio esquema, como el resto de
// lecturas de viajes): da el 404 cuando el viaje no existe y arma el cuerpo. No
// es una escritura cruzada —es una lectura de op_viajes a su propia tabla— y el
// registro probatorio sigue escribiéndose solo con op_auditoria.
package descargas

import (
	"context"
	"errors"
	"fmt"
	"time"

	operclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/operacion"
	platformerrors "github.com/AI27-Tech/pf-pkg-platform-go/errors"
	"github.com/AI27-Tech/pf-svc-operacion/internal/auditoria"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// BeginTx es la frontera transaccional (Unit of Work). La cumplen tanto el pool
// del rol op_viajes (lectura) como el del rol op_auditoria (escritura del
// registro). Mismo patrón que el resto de los módulos.
type BeginTx interface {
	Begin(ctx context.Context) (pgx.Tx, error)
}

// accionDescarga es la acción con la que queda el registro probatorio de la
// descarga del expediente (E1-T7): un valor estable del catálogo de acciones de
// auditoría, igual que "asignar"/"reasignar" del módulo asignacion.
const accionDescarga = "descarga_expediente"

// Servicio registra la descarga del expediente de un viaje. Recibe dos fronteras
// transaccionales, una por rol: `viajes` (op_viajes) para leer el viaje de la
// respuesta y dar el 404; `auditoria` (op_auditoria) para escribir el registro
// probatorio vía el interceptor. Nunca comparte un pool entre roles (I1).
type Servicio struct {
	viajes    BeginTx
	auditoria BeginTx
}

// NewServicio recibe el pool del rol op_viajes (lectura del viaje) y el del rol
// op_auditoria (escritura del registro). No lee entorno.
func NewServicio(viajes, auditoria BeginTx) *Servicio {
	return &Servicio{viajes: viajes, auditoria: auditoria}
}

// Registrar deja el rastro de auditoría de la descarga del expediente del viaje
// y devuelve el Viaje descargado. actorID es el sujeto del token (quién
// descargó); viajeID identifica el expediente.
//
// Dos pasos, cada uno con su rol:
//  1. Lee el viaje con op_viajes (SELECT sobre su propio esquema). Si no existe,
//     NoEncontrado (404) y NO se escribe ningún registro: no se audita la
//     descarga de un expediente inexistente.
//  2. Escribe EXACTAMENTE un registro en auditoria.registro_auditoria con el
//     interceptor (P3), en una transacción del rol dueño op_auditoria. El
//     registro es append-only: el esquema no otorga UPDATE ni DELETE a nadie
//     (migrations/auditoria), así que no existe forma de editarlo ni borrarlo.
func (s *Servicio) Registrar(ctx context.Context, viajeID operclient.Id, actorID string) (*operclient.Viaje, error) {
	viaje, err := s.leerViaje(ctx, viajeID)
	if err != nil {
		return nil, err
	}

	// El registro probatorio va por el interceptor, en la tx del rol op_auditoria.
	// El "cambio" que el interceptor envuelve es la descarga en sí (la composición
	// la hizo bff); aquí no hay mutación de dominio que encadenar, solo el acto
	// auditado. El interceptor escribe el único INSERT en la MISMA tx y no hay
	// ruta que lo omita (P3).
	tx, err := s.auditoria.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op tras commit

	reg := auditoria.Registro{
		ViajeID: viajeID.String(),
		Modulo:  "auditoria",
		Accion:  accionDescarga,
		ActorID: actorID,
	}
	err = auditoria.Interceptar(ctx, tx, reg, func(ctx context.Context, tx pgx.Tx) error {
		// El acto auditado es la descarga del expediente; su composición es de bff.
		// No hay escritura de dominio adicional en operacion para este comando.
		return nil
	})
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("operacion: commit registrar descarga: %w", err)
	}

	return viaje, nil
}

// leerViaje carga el viaje para la respuesta con el rol op_viajes. Devuelve
// NoEncontrado (404) si no existe. No lee condiciones ni incidencias: la
// descarga responde el Viaje, no el detalle.
func (s *Servicio) leerViaje(ctx context.Context, viajeID operclient.Id) (*operclient.Viaje, error) {
	tx, err := s.viajes.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var (
		estado    string
		clienteID uuid.UUID
		origen    string
		fhs       *time.Time
		eta       *time.Time
		creado    time.Time
	)
	err = tx.QueryRow(ctx,
		`SELECT estado, cliente_id, origen, fecha_hora_servicio, eta_estimado, creado_en
		   FROM viajes.viaje WHERE id = $1`, viajeID,
	).Scan(&estado, &clienteID, &origen, &fhs, &eta, &creado)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, platformerrors.NewNoEncontrado("operacion: viaje %s no encontrado", viajeID)
	}
	if err != nil {
		return nil, fmt.Errorf("operacion: leer viaje para descarga: %w", err)
	}

	destinos, err := s.destinosDe(ctx, tx, viajeID)
	if err != nil {
		return nil, err
	}

	cid := operclient.Id(clienteID)
	return &operclient.Viaje{
		Id:                viajeID,
		Estado:            operclient.EstadoViaje(estado),
		ClienteId:         &cid,
		Origen:            origen,
		Destinos:          destinos,
		FechaHoraServicio: fhs,
		EtaEstimado:       eta,
		CreadoEn:          creado,
	}, nil
}

// destinosDe lee los destinos del viaje, en orden. Nunca nil: Destinos es
// requerido por el contrato.
func (s *Servicio) destinosDe(ctx context.Context, tx pgx.Tx, viajeID operclient.Id) ([]operclient.Destino, error) {
	rows, err := tx.Query(ctx,
		`SELECT nombre, numero_envio, fecha_cita FROM viajes.destino WHERE viaje_id = $1 ORDER BY orden`, viajeID)
	if err != nil {
		return nil, fmt.Errorf("operacion: leer destinos para descarga: %w", err)
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
			return nil, fmt.Errorf("operacion: escanear destino para descarga: %w", err)
		}
		out = append(out, operclient.Destino{Nombre: nombre, NumeroEnvio: ne, FechaCita: fc})
	}
	return out, rows.Err()
}
