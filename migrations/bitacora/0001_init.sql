-- 0001_init.sql — esquema bitacora y rol op_bitacora.
-- bitacora NO publica eventos (operacion.md (g)): no tiene outbox ni worker.

CREATE SCHEMA IF NOT EXISTS bitacora;

DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'op_bitacora') THEN
    CREATE ROLE op_bitacora LOGIN;
  END IF;
END
$$;

GRANT USAGE ON SCHEMA bitacora TO op_bitacora;
ALTER DEFAULT PRIVILEGES IN SCHEMA bitacora
  GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO op_bitacora;

-- ---------------------------------------------------------------------------
-- entrada_bitacora — registro de bitácora del viaje. Sin edición ni borrado
-- (E1-T5, operacion.md (e)): es append-only. DEC-043 #3 pide ficha/bitácora
-- editable, pero esa edición (de confirmarse) sería de los campos estructurados
-- de la ficha del viaje, no de las entradas; no se expone edición de entradas.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS bitacora.entrada_bitacora (
  id          BIGSERIAL   PRIMARY KEY,
  viaje_id    UUID        NOT NULL,
  texto       TEXT        NOT NULL,
  autor_id    TEXT,       -- identificador del usuario de identidad; NO es FK
  creado_en   TIMESTAMPTZ NOT NULL DEFAULT now()
  -- TODO(E1-T5): campos reales de la entrada / ficha técnica, pendientes de
  -- Operación. No inventar columnas.
);
CREATE INDEX IF NOT EXISTS entrada_por_viaje_idx
  ON bitacora.entrada_bitacora (viaje_id, creado_en);

-- ---------------------------------------------------------------------------
-- incidencia — detalle de una incidencia del viaje. La CONDICIÓN abierta vive en
-- viajes.condicion_viaje (P15); aquí va el detalle narrativo/probatorio. Puede
-- denormalizar lat/lon (operacion.md (e)); nunca referencia posicion/telemetria.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS bitacora.incidencia (
  id          UUID        PRIMARY KEY,
  viaje_id    UUID        NOT NULL,
  descripcion TEXT,
  lat         DOUBLE PRECISION,
  lon         DOUBLE PRECISION,
  abierta_en  TIMESTAMPTZ NOT NULL DEFAULT now(),
  resuelta_en TIMESTAMPTZ
  -- TODO(operacion): adjuntos opcionales y genéricos (DEC-040); forma pendiente.
);
