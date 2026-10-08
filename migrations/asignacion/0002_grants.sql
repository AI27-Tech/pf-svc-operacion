-- 0002_grants.sql — privilegios por tabla del esquema asignacion (E0-T5).
-- La aplica el rol dueño (op_asignacion). Es el ÚNICO esquema de operacion con
-- un DELETE legítimo: liberar una reserva de una unidad es un DELETE físico
-- sobre asignacion.reserva_recurso (decisión del Lead E0-T5). Todo lo demás de
-- operacion es append-only o cierre por marca de tiempo, sin DELETE.
--
-- Por qué el REVOKE de abajo: 0001 corre un ALTER DEFAULT PRIVILEGES que otorga
-- SELECT, INSERT, UPDATE y DELETE sobre toda tabla, ANTES de crear las tablas;
-- esas tablas nacieron con ese default. Medido sobre Postgres: sin este REVOKE,
-- op_asignacion podía DELETE y UPDATE en asignacion.historia_asignacion
-- (permanente) y borrar asignacion.asignacion_viaje (que se libera marcando
-- liberada_en, no con DELETE). El invariante I1 (ADR 17) solo es real con
-- privilegio mínimo. Patrón de referencia:
-- pf-svc-catalogos/migrations/catalogos/0002_grants.sql.
--
-- NOTA sobre la excepción cruzada: los GRANT de op_asignacion sobre OTROS
-- esquemas (viajes, auditoria, roster) NO viven aquí; se otorgan desde cada
-- esquema destino en su propio 0002 (viajes/0002, auditoria/0002, roster/0002),
-- porque un GRANT se otorga desde el dueño del objeto. op_asignacion NUNCA
-- recibe nada sobre bitacora ni sobre catalogos.

-- --- Partir de cero: retirar lo que 0001 otorgó de forma genérica --------------
REVOKE ALL ON ALL TABLES    IN SCHEMA asignacion FROM op_asignacion;
REVOKE ALL ON ALL SEQUENCES IN SCHEMA asignacion FROM op_asignacion;
-- Una tabla futura (0003+) no hereda privilegios: cada migración que crea una
-- tabla declara sus GRANT explícitos en su propia fase de grants.
ALTER DEFAULT PRIVILEGES IN SCHEMA asignacion
  REVOKE ALL ON TABLES FROM op_asignacion;

-- --- asignacion_viaje: la asignación vigente del viaje -------------------------
-- Se libera marcando liberada_en (cierre por marca), no con DELETE: la fila
-- queda como historia de qué estuvo asignado. SELECT, INSERT, UPDATE; sin DELETE.
GRANT SELECT, INSERT, UPDATE ON asignacion.asignacion_viaje TO op_asignacion;

-- --- historia_asignacion: cambios con motivo, retención larga ------------------
-- Permanente (operacion.md (e)): append-only. SELECT, INSERT; sin UPDATE ni DELETE.
GRANT SELECT, INSERT ON asignacion.historia_asignacion TO op_asignacion;
-- INSERT en una tabla BIGSERIAL exige USAGE sobre su secuencia.
GRANT USAGE ON SEQUENCE asignacion.historia_asignacion_id_seq TO op_asignacion;

-- --- reserva_recurso: el ÚNICO DELETE de todo operacion ------------------------
-- Reservar una unidad es un INSERT con restricción de exclusión por ventana;
-- liberar esa reserva es un DELETE físico de la fila (decisión del Lead E0-T5).
-- Por eso, y solo aquí, op_asignacion recibe DELETE. SELECT, INSERT, DELETE;
-- sin UPDATE (una reserva no se edita: se crea o se libera).
GRANT SELECT, INSERT, DELETE ON asignacion.reserva_recurso TO op_asignacion;
