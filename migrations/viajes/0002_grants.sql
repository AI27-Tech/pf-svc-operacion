-- 0002_grants.sql — privilegios por tabla del esquema viajes (E0-T5).
-- Cierra el TODO(E0-T5) de 0001_init.sql: acota, tabla por tabla, lo que 0001
-- dejó como ALTER DEFAULT PRIVILEGES genérico. El invariante I1 (aislamiento
-- por rol, ADR 17) solo es real cuando el privilegio es mínimo: un rol puede
-- exactamente lo que su caso de uso necesita y nada más.
--
-- Esta migración la aplica el rol dueño del esquema (op_viajes), que es quien
-- puede otorgar sobre sus propias tablas. Incluye el tramo `viajes` de la ÚNICA
-- excepción de GRANT del sistema (op_asignacion), porque esos GRANT se otorgan
-- DESDE el esquema destino, no desde asignacion (operacion.md (e), §7.1).
--
-- Por qué el REVOKE de abajo: 0001 corre un ALTER DEFAULT PRIVILEGES que otorga
-- SELECT, INSERT, UPDATE y DELETE sobre toda tabla, ANTES de crear las tablas;
-- esas tablas nacieron con ese default. Medido sobre Postgres: sin este REVOKE,
-- op_viajes podía DELETE y UPDATE en viajes.transicion_viaje y viajes.outbox
-- (append-only), y op_asignacion heredaba más que su excepción. El invariante I1
-- (ADR 17) solo es real con privilegio mínimo. Patrón de referencia:
-- pf-svc-catalogos/migrations/catalogos/0002_grants.sql.

-- --- Partir de cero: retirar lo que 0001 otorgó de forma genérica --------------
REVOKE ALL ON ALL TABLES    IN SCHEMA viajes FROM op_viajes;
REVOKE ALL ON ALL SEQUENCES IN SCHEMA viajes FROM op_viajes;
-- Una tabla futura (0003+) no hereda privilegios: cada migración que crea una
-- tabla declara sus GRANT explícitos en su propia fase de grants.
ALTER DEFAULT PRIVILEGES IN SCHEMA viajes
  REVOKE ALL ON TABLES FROM op_viajes;

-- --- op_viajes: dueño del agregado --------------------------------------------
-- Muta el estado del viaje y sus condiciones; registra transiciones; escribe y
-- marca su propio outbox. No borra: la historia es append-only (transicion_viaje,
-- outbox), y el viaje/condición se cierra por estado/marca de tiempo, no con DELETE.
GRANT SELECT, INSERT, UPDATE ON viajes.viaje             TO op_viajes;
GRANT SELECT, INSERT, UPDATE ON viajes.condicion_viaje   TO op_viajes;
GRANT SELECT, INSERT          ON viajes.transicion_viaje  TO op_viajes;
GRANT SELECT, INSERT, UPDATE ON viajes.outbox            TO op_viajes;
-- Secuencias de las PK BIGSERIAL: INSERT en una tabla con BIGSERIAL exige USAGE
-- sobre su secuencia. Sin esto el INSERT falla en runtime pese al GRANT de tabla.
GRANT USAGE ON SEQUENCE viajes.transicion_viaje_id_seq TO op_viajes;
GRANT USAGE ON SEQUENCE viajes.outbox_id_seq           TO op_viajes;

-- --- op_asignacion: tramo `viajes` de la excepción (ADR 31/35, §7.1) -----------
-- La asignación transiciona el viaje y escribe el evento viaje.* en el outbox
-- único (ADR 35), todo dentro de la MISMA transacción que la reserva. Necesita:
--   - entrar al esquema (USAGE),
--   - leer y actualizar el estado del viaje (transición),
--   - insertar la transición y el evento del outbox.
-- NUNCA borra, nunca toca condicion_viaje, nunca alcanza catalogos.
GRANT USAGE ON SCHEMA viajes TO op_asignacion;
GRANT SELECT, UPDATE ON viajes.viaje            TO op_asignacion;
GRANT INSERT         ON viajes.transicion_viaje TO op_asignacion;
GRANT INSERT         ON viajes.outbox           TO op_asignacion;
GRANT USAGE ON SEQUENCE viajes.transicion_viaje_id_seq TO op_asignacion;
GRANT USAGE ON SEQUENCE viajes.outbox_id_seq           TO op_asignacion;
