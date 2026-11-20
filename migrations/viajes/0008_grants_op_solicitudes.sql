-- 0008_grants_op_solicitudes.sql — tramo `viajes` de la segunda excepción de
-- GRANT cruzado (ADR 39, E10-T1). op_solicitudes crea el viaje en `APROBADO` con
-- el mismo id de la solicitud, escribe sus destinos y publica su evento por el
-- outbox único de los viaje.* (ADR 35), todo en la MISMA transacción que aprueba
-- la solicitud (lo que escribe `Crear`; enmienda DEC-148 #58). Se otorga desde aquí, el esquema destino, y va en la fase
-- de grants porque el rol lo crea solicitudes/0001.
--
-- Lo que NO permite (ADR 39): UPDATE ni DELETE en viajes, y SELECT en ninguna
-- tabla (INSERT sin RETURNING ni ON CONFLICT no lo necesita); si E10-T8 demuestra
-- que lo necesita, se concede acotado a viajes.viaje y se anota en E10-T1. Nunca
-- bitacora, asignacion, roster ni catalogos. Tampoco
-- transicion_viaje: se concede solo si E10-T8 demuestra que la aprobación la necesita. Solo el caso de uso de aprobación
-- usa este rol para escribir en viajes.
GRANT USAGE ON SCHEMA viajes TO op_solicitudes;
GRANT INSERT ON viajes.viaje            TO op_solicitudes;
GRANT INSERT ON viajes.destino          TO op_solicitudes;
GRANT INSERT ON viajes.outbox           TO op_solicitudes;
GRANT USAGE ON SEQUENCE viajes.outbox_id_seq TO op_solicitudes;
