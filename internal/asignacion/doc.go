// Package asignacion reserva la ESCOLTA: una o más unidades-paquete completas
// (ADR 31 §3). Los custodios no se reservan por separado: reservar la unidad
// reserva su tripulación. La reserva vive aquí con una restricción de EXCLUSIÓN
// sobre (unidad_id, ventana); la carrera entre comprobar aptitud y reservar se
// resuelve DENTRO de esta transacción, nunca con un candado en catalogos.
//
// Es el dueño de la ÚNICA excepción de GRANT cruzado del sistema: op_asignacion
// tiene USAGE sobre viajes/bitacora/auditoria (y lectura sobre roster, ADR 33)
// para escribir la transición a ASIGNADO, el evento viaje.asignado en
// viajes.outbox y el registro de auditoría en la MISMA transacción que la
// reserva (P2). Nunca sobre catalogos.
//
// Sub-módulo interno de operacion. NO tiene outbox propio (ADR 35, DEC-068):
// escribe viaje.asignado/viaje.reasignado en viajes.outbox, el único outbox de
// la familia viaje.*, para que ADR 29 cumpla el orden por agregado.
//
// Scaffolding: sin casos de uso todavía. La aptitud NO gatea (DEC-055): el único
// rechazo de asignación es la reserva fallida (no hay unidad libre en la ventana).
package asignacion
