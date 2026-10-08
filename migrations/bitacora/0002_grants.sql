-- 0002_grants.sql — privilegios por tabla del esquema bitacora (E0-T5).
-- Acota el ALTER DEFAULT PRIVILEGES genérico de 0001_init.sql al mínimo real.
-- La aplica el rol dueño (op_bitacora).
--
-- Por qué el REVOKE de abajo: 0001 corre un ALTER DEFAULT PRIVILEGES que otorga
-- SELECT, INSERT, UPDATE y DELETE sobre toda tabla, ANTES de crear las tablas;
-- esas tablas nacieron con ese default. Medido sobre Postgres: sin este REVOKE,
-- op_bitacora podía DELETE y UPDATE en bitacora.entrada_bitacora (append-only).
-- Patrón de referencia: pf-svc-catalogos/migrations/catalogos/0002_grants.sql.

-- --- Partir de cero: retirar lo que 0001 otorgó de forma genérica --------------
REVOKE ALL ON ALL TABLES    IN SCHEMA bitacora FROM op_bitacora;
REVOKE ALL ON ALL SEQUENCES IN SCHEMA bitacora FROM op_bitacora;
ALTER DEFAULT PRIVILEGES IN SCHEMA bitacora
  REVOKE ALL ON TABLES FROM op_bitacora;

-- entrada_bitacora e incidencia son append-only (operacion.md (e), E1-T5): no se
-- editan ni se borran entradas. incidencia se cierra con `resuelta_en`, no con
-- UPDATE del cuerpo ni DELETE. Por eso: SELECT + INSERT, y UPDATE acotado a
-- incidencia solo para marcar el cierre.
GRANT SELECT, INSERT         ON bitacora.entrada_bitacora TO op_bitacora;
GRANT SELECT, INSERT, UPDATE ON bitacora.incidencia       TO op_bitacora;
GRANT USAGE ON SEQUENCE bitacora.entrada_bitacora_id_seq TO op_bitacora;

-- op_asignacion NO recibe nada sobre bitacora: su transacción toca viajes
-- (transición + outbox) y auditoria (registro), nunca bitacora. Por eso ningún
-- grant de bitacora apunta a op_asignacion, y 0001 ya no le concede USAGE de
-- esquema (se retiró al acotar la excepción, ver nota de revisión E0-T5).
