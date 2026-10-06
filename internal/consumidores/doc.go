// Package consumidores reúne los dos consumidores durables V1 de operacion, que
// se exponen por Module.Workers() con su propia suscripción durable (DEC-057) y
// su DLQ/cuarentena (ADR 29):
//
//   - catalogo.actualizado → invalida la entrada correspondiente de la caché de
//     catalogos (del lado consumidor, detrás del puerto AptitudRecursos).
//   - sesion.revocada → alimenta la proyección local de tombstones; la réplica
//     queda not-ready hasta converger con snapshot + cursor (ADR 26). El
//     snapshot/fallback llama a identidad por la arista restringida (DEC-051,
//     puerto Revocacion con solo listarRevocadas y obtenerSesion).
//
// IDEMPOTENCIA EN MEMORIA (ADR 34, DEC-067): ambos deduplican por evento_id en
// memoria; NO hay tabla inbox_evento. Su efecto vive en memoria (invalidar una
// entrada de caché / actualizar la proyección de revocación, que ADR 26
// reconstruye desde snapshot), así que no hay escritura a la base a la que
// encadenar un inbox. La robustez ante reinicio la da el snapshot + cursor, no
// una tabla durable.
//
// V2 (NO scaffoldeado): sin_senal.detectado/.recuperado y telemetria.alerta no
// están en el inventario del tracer (operacion.md (g), ADR 5).
//
// Scaffolding: sin consumidores cableados todavía. Se prueban contra bus/memory;
// el adaptador del broker real espera a ADR 32.
package consumidores
