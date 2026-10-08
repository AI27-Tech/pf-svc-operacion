-- 0002_grants.sql — privilegios por tabla del esquema auditoria (E0-T5).
-- La aplica el rol dueño (op_auditoria).
--
-- registro_auditoria es probatorio y append-only (operacion.md (g), P3): sin
-- UPDATE ni DELETE para nadie. Un registro editable o borrable produciría huecos
-- indistinguibles de manipulación. 0001_init.sql ya restringe el DEFAULT a
-- SELECT/INSERT (no otorga UPDATE ni DELETE), así que aquí el defecto es más
-- estrecho que en los otros esquemas; aun así, por el patrón de la decisión del
-- Lead se parte de cero y se fija el mínimo explícito por tabla. Patrón de
-- referencia: pf-svc-catalogos/migrations/catalogos/0002_grants.sql.

-- --- Partir de cero: retirar lo que 0001 otorgó de forma genérica --------------
REVOKE ALL ON ALL TABLES    IN SCHEMA auditoria FROM op_auditoria;
REVOKE ALL ON ALL SEQUENCES IN SCHEMA auditoria FROM op_auditoria;
ALTER DEFAULT PRIVILEGES IN SCHEMA auditoria
  REVOKE ALL ON TABLES FROM op_auditoria;

GRANT SELECT, INSERT ON auditoria.registro_auditoria TO op_auditoria;
GRANT USAGE ON SEQUENCE auditoria.registro_auditoria_id_seq TO op_auditoria;

-- --- op_asignacion: tramo `auditoria` de la excepción (ADR 31, §7.1) ----------
-- El interceptor de auditoría escribe el registro en la MISMA transacción que la
-- asignación (P3). op_asignacion entra al esquema (USAGE) e inserta en
-- registro_auditoria; nunca lee, nunca actualiza, nunca borra.
GRANT USAGE ON SCHEMA auditoria TO op_asignacion;
GRANT INSERT ON auditoria.registro_auditoria TO op_asignacion;
GRANT USAGE ON SEQUENCE auditoria.registro_auditoria_id_seq TO op_asignacion;
