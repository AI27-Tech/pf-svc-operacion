-- 0001_init.sql — esquema viajes y rol op_viajes.
-- viajes es la ÚNICA autoridad de transición del viaje (operacion.md (a), P14):
-- no existe escritura directa de `estado`; toda mutación pasa por
-- PuedeTransicionar (del paquete generado estados) más las guardas que el YAML
-- nombra. La máquina de estados NO se declara aquí: se importa de
-- pf-pkg-contracts/packages/api-client-go/estados.

CREATE SCHEMA IF NOT EXISTS viajes;

DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'op_viajes') THEN
    CREATE ROLE op_viajes LOGIN;
  END IF;
END
$$;

GRANT USAGE ON SCHEMA viajes TO op_viajes;
ALTER DEFAULT PRIVILEGES IN SCHEMA viajes
  GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO op_viajes;
-- TODO(E0-T5): privilegios por tabla más allá de USAGE no están fijados en las
-- fuentes; no se inventan aquí.

-- ---------------------------------------------------------------------------
-- viaje — el agregado. `estado` se restringe por constraint a los tokens
-- generados del enum (E1-T1): insertar INCIDENCIA o SIN_SENAL es imposible por
-- la base, porque son CONDICIONES ortogonales, no estados (P15).
-- Los 13 tokens vienen de contracts/estados/viaje.estados.yaml. EN_BASE_CLIENTE
-- y EN_ORIGEN son alias_historico de EN_EMBARQUE y NO se aceptan en filas nuevas.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS viajes.viaje (
  id            UUID        PRIMARY KEY,
  estado        TEXT        NOT NULL,
  cliente_id    UUID        NOT NULL,  -- identificador de catalogos; NO es FK (otro esquema/servicio)
  origen        TEXT        NOT NULL,
  tracto        TEXT,                  -- vehículo del cliente, texto libre (ADR 31 §6); no es recurso
  eta_estimado  TIMESTAMPTZ,           -- ETA manual (ADR 20); operacion NO calcula ni expone `stale`
  creado_en     TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT estado_token_valido CHECK (estado IN (
    'SOLICITADO','APROBADO','RECHAZADO_POR_AI27','RECHAZADO_POR_CLIENTE',
    'ASIGNADO','APTO_PARA_SALIDA','EN_EMBARQUE','PATRULLA_EN_CAMINO',
    'EN_TRANSITO','EN_DESTINO','EN_DESCARGA','CERRADO','CANCELADO'
  ))
  -- TODO(operacion): el CHECK duplica los tokens del enum generado; mantenerlo
  -- en sincronía con estados.EstadosViaje (idealmente verificado en una prueba).
  -- TODO(E1-T5): campos estructurados de la ficha técnica del viaje.
);

-- ---------------------------------------------------------------------------
-- condicion_viaje — condiciones ORTOGONALES al progreso (§C.2, P15). Un viaje
-- está EN_TRANSITO *y* con incidencia a la vez. INCIDENCIA es V1; SIN_SENAL es
-- V2 (inalcanzable). Una sola condición abierta por tipo y viaje.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS viajes.condicion_viaje (
  id          UUID        PRIMARY KEY,
  viaje_id    UUID        NOT NULL,
  tipo        TEXT        NOT NULL CHECK (tipo IN ('INCIDENCIA','SIN_SENAL')),
  abierta_en  TIMESTAMPTZ NOT NULL DEFAULT now(),
  cerrada_en  TIMESTAMPTZ,
  lat         DOUBLE PRECISION,  -- incidencia puede denormalizar posición (operacion.md (e))
  lon         DOUBLE PRECISION
);
CREATE UNIQUE INDEX IF NOT EXISTS una_condicion_abierta_por_tipo
  ON viajes.condicion_viaje (viaje_id, tipo)
  WHERE cerrada_en IS NULL;

-- ---------------------------------------------------------------------------
-- transicion_viaje — historia de transiciones con causa y marca de tiempo
-- (DEC-002 #4). La autoridad de transición es el caso de uso de viajes.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS viajes.transicion_viaje (
  id          BIGSERIAL   PRIMARY KEY,
  viaje_id    UUID        NOT NULL,
  de_estado   TEXT,
  a_estado    TEXT        NOT NULL,
  causa       TEXT,
  cambiado_en TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS transicion_por_viaje_idx
  ON viajes.transicion_viaje (viaje_id, cambiado_en);

-- ---------------------------------------------------------------------------
-- outbox — el ÚNICO outbox de los eventos viaje.* (ADR 35, DEC-068). Lleva
-- tanto los de este esquema (viaje.estado_cambiado, viaje.cerrado, …) como los
-- de asignacion (viaje.asignado, viaje.reasignado), que el caso de uso de
-- asignacion escribe aquí dentro de su propia transacción vía la excepción de
-- GRANT. Escrita en la MISMA tx que el cambio (P17). Clave de orden = viaje_id.
-- Refleja outbox.OutboxDDL de pf-pkg-platform-go.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS viajes.outbox (
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
  ON viajes.outbox (creado_en) WHERE publicado = false;
