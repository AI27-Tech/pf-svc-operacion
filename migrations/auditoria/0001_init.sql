-- 0001_init.sql — esquema auditoria y rol op_auditoria.
-- auditoria NO publica eventos (operacion.md (g)): no tiene outbox ni worker.
-- Es un INTERCEPTOR, no una dependencia: el registro se escribe en la MISMA
-- transacción que el cambio de viajes/asignacion/bitacora, nunca por una llamada
-- que un caso de uso pueda omitir (operacion.md (d), P3). Un registro escrito
-- por red produciría huecos indistinguibles de manipulación.

CREATE SCHEMA IF NOT EXISTS auditoria;

DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'op_auditoria') THEN
    CREATE ROLE op_auditoria LOGIN;
  END IF;
END
$$;

GRANT USAGE ON SCHEMA auditoria TO op_auditoria;
ALTER DEFAULT PRIVILEGES IN SCHEMA auditoria
  GRANT SELECT, INSERT ON TABLES TO op_auditoria;
-- Solo SELECT/INSERT: la auditoría es probatoria, append-only; sin UPDATE ni
-- DELETE para op_auditoria. (El GRANT de excepción de op_asignacion también
-- escribe aquí dentro de la tx de asignación.)

-- ---------------------------------------------------------------------------
-- registro_auditoria — bitácora probatoria append-only de los cambios de
-- dominio. La escribe el interceptor envolviendo la escritura de los tres
-- módulos. Sin edición ni borrado.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS auditoria.registro_auditoria (
  id          BIGSERIAL   PRIMARY KEY,
  viaje_id    UUID,
  modulo      TEXT        NOT NULL,  -- viajes | asignacion | bitacora
  accion      TEXT        NOT NULL,
  actor_id    TEXT,                  -- identificador del usuario de identidad; NO es FK
  detalle     JSONB,
  ocurrido_en TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS registro_por_viaje_idx
  ON auditoria.registro_auditoria (viaje_id, ocurrido_en);
