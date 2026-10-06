// Package bitacora registra las entradas de bitácora del viaje y el detalle de
// incidencias. Append-only: sin edición ni borrado de entradas (E1-T5,
// operacion.md (e)). No publica eventos: no tiene outbox ni worker.
//
// La CONDICIÓN de incidencia (abierta/cerrada) vive en viajes.condicion_viaje
// (P15); aquí va el detalle narrativo/probatorio, que puede denormalizar
// lat/lon pero nunca referencia posicion/telemetria.
//
// Sub-módulo interno de operacion. Scaffolding: sin casos de uso todavía.
package bitacora
