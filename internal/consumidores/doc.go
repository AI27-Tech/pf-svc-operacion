// Package consumidores reúne los dos consumidores durables V1 de operacion, que
// se exponen por Module.Workers() con su propia suscripción durable (DEC-057) y
// su DLQ/cuarentena (ADR 29):
//
//   - catalogo.actualizado → invalida la entrada correspondiente de la caché de
//     catalogos (del lado consumidor, detrás del puerto AptitudRecursos).
//     Idempotente por naturaleza; además inbox_evento por evento_id.
//   - sesion.revocada → alimenta la proyección local de tombstones; la réplica
//     queda not-ready hasta converger con snapshot + cursor (ADR 26). El
//     snapshot/fallback llama a identidad por la arista restringida (DEC-051,
//     puerto Revocacion con solo listarRevocadas y obtenerSesion).
//
// V2 (NO scaffoldeado): sin_senal.detectado/.recuperado y telemetria.alerta no
// están en el inventario del tracer (operacion.md (g), ADR 5).
//
// Scaffolding: sin consumidores cableados todavía. Se prueban contra bus/memory;
// el adaptador del broker real espera a ADR 32.
//
// PENDIENTE (no inferir, ver README): ninguna fuente fija en qué esquema/rol de
// operacion vive el inbox_evento de cada consumidor durable (ADR 29 lo exige
// pero no fija la ubicación).
package consumidores
