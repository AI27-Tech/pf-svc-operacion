-- 0001_init.sql — esquema asignacion y rol op_asignacion.
-- Aquí vive la ÚNICA excepción de GRANT cruzado del sistema entero
-- (operacion.md (e), §7.1): op_asignacion recibe USAGE sobre los cuatro
-- esquemas de operacion, porque la asignación muta la reserva, el estado del
-- viaje y la auditoría en UNA sola transacción (ADR 31, invariantes; P2).
-- Ningún otro rol de ningún otro servicio puede introducir otra excepción sin
-- un ADR. op_asignacion NUNCA recibe USAGE sobre catalogos: el acceso cruzado a
-- catalogos falla en la base; la aptitud se consulta por el puerto, no por SQL.

CREATE SCHEMA IF NOT EXISTS asignacion;

DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'op_asignacion') THEN
    CREATE ROLE op_asignacion LOGIN;
  END IF;
END
$$;

GRANT USAGE ON SCHEMA asignacion TO op_asignacion;
ALTER DEFAULT PRIVILEGES IN SCHEMA asignacion
  GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO op_asignacion;

-- LA EXCEPCIÓN (operacion.md (e), §7.1): acotada a un rol y a la operación de
-- asignación. Permite escribir la transición del viaje y el registro de
-- auditoría en la misma tx que la reserva.
GRANT USAGE ON SCHEMA viajes     TO op_asignacion;
GRANT USAGE ON SCHEMA bitacora   TO op_asignacion;
GRANT USAGE ON SCHEMA auditoria  TO op_asignacion;
-- TODO(E0-T5): privilegios por tabla sobre esos esquemas (INSERT en
-- viajes.transicion_viaje / viajes.outbox y auditoria.registro_auditoria),
-- acotados a lo que el caso de uso de asignación necesita. No se amplían aquí.

-- Extensión para combinar `=` y `&&` en un índice GiST (restricción de exclusión).
CREATE EXTENSION IF NOT EXISTS btree_gist;

-- ---------------------------------------------------------------------------
-- asignacion_viaje — la asignación vigente de un viaje. Una vigente por viaje.
-- La escolta son 1..N unidades; aquí se registra la asignación, el detalle de
-- qué unidades la componen va en reserva_recurso.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS asignacion.asignacion_viaje (
  id             UUID        PRIMARY KEY,
  viaje_id       UUID        NOT NULL,
  asignada_en    TIMESTAMPTZ NOT NULL DEFAULT now(),
  liberada_en    TIMESTAMPTZ
);
CREATE UNIQUE INDEX IF NOT EXISTS una_asignacion_vigente_por_viaje
  ON asignacion.asignacion_viaje (viaje_id) WHERE liberada_en IS NULL;

-- ---------------------------------------------------------------------------
-- reserva_recurso — reserva POR UNIDAD y ventana (ADR 31 §3). La corrección de
-- "no sobrevender una unidad" se delega a la base con una restricción de
-- EXCLUSIÓN: no pueden coexistir dos reservas de la misma unidad con ventanas
-- que se solapen. Los custodios NO se reservan por separado: reservar la unidad
-- reserva su tripulación. patrulla/custodio/tracto NO son recursos reservables.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS asignacion.reserva_recurso (
  id             UUID        PRIMARY KEY,
  asignacion_id  UUID        NOT NULL REFERENCES asignacion.asignacion_viaje (id),
  unidad_id      UUID        NOT NULL,  -- unidad-paquete (roster de operacion); identificador
  ventana        TSTZRANGE   NOT NULL,
  EXCLUDE USING gist (unidad_id WITH =, ventana WITH &&)
);

-- ---------------------------------------------------------------------------
-- historia_asignacion — cambios con motivo, retención larga (operacion.md (e)).
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS asignacion.historia_asignacion (
  id             BIGSERIAL   PRIMARY KEY,
  viaje_id       UUID        NOT NULL,
  accion         TEXT        NOT NULL,  -- asignar | reasignar | liberar
  motivo         TEXT,
  ocurrido_en    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ---------------------------------------------------------------------------
-- outbox — eventos viaje.asignado / viaje.reasignado (ADR 29). Clave de orden =
-- viaje_id. Cuando la asignación transiciona el viaje a ASIGNADO, el caso de uso
-- escribe también la fila de viaje.estado_cambiado en viajes.outbox dentro de
-- esta misma transacción (puede, por la excepción de GRANT).
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS asignacion.outbox (
  id            BIGSERIAL   PRIMARY KEY,
  evento_id     UUID        NOT NULL UNIQUE,
  tipo          TEXT        NOT NULL,
  version       INT         NOT NULL DEFAULT 1,
  agregado_id   TEXT        NOT NULL,
  sensibilidad  TEXT        NOT NULL DEFAULT 'amplia',
  ocurrido_en   TIMESTAMPTZ NOT NULL,
  payload       JSONB       NOT NULL,
  trace_context JSONB       NOT NULL DEFAULT '{}'::jsonb,
  publicado     BOOLEAN     NOT NULL DEFAULT false,
  publicado_en  TIMESTAMPTZ,
  creado_en     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS outbox_pendientes_idx
  ON asignacion.outbox (creado_en) WHERE publicado = false;

-- NOTA (para el Lead, no se resuelve en el scaffold): viaje.* sale de dos outbox
-- (viajes y asignacion) con el mismo viaje_id; ADR 29 promete orden por
-- agregado, que con dos publishers independientes no está garantizado por
-- construcción.
-- NOTA (pendiente, no inferir): el roster de unidades (ADR 31 §5, E1-HU-031) es
-- de operacion pero ninguna fuente fija en qué esquema vive ni sus rutas. No se
-- crea tabla de roster aquí.
