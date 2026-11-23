-- 0004_cotizacion.sql — versiones de la cotización y reprogramaciones de la
-- solicitud (E10-T4; ADR 38, DEC-148 #15, #24, #39, #41, #42). Va después de
-- 0002_grants: cada tabla declara sus GRANT mínimos. Idempotente (DT-25).

-- ---------------------------------------------------------------------------
-- cotizacion — una fila por versión; se guardan TODAS y el cliente ve solo la
-- vigente, la de mayor versión (DEC-148 #42). Solo INSERT: recotizar agrega una
-- versión, nunca edita la anterior. monto es NUMERIC sin escala fija (conserva
-- los decimales capturados; se lee como texto, igual que el contrato). El
-- catálogo de monedas y los límites del monto siguen pendientes del Lead
-- (E10, "Pendientes"): moneda es texto libre no vacío y el monto admite cero.
-- vigente_hasta es el último día de vigencia: 15 días calendario desde la
-- entrega, en America/Mexico_City (DEC-148 #15, #50); lo calcula el módulo.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS solicitudes.cotizacion (
  solicitud_id        UUID        NOT NULL REFERENCES solicitudes.solicitud (id),
  version             INT         NOT NULL CHECK (version >= 1),
  monto               NUMERIC     NOT NULL CHECK (monto >= 0),
  moneda              TEXT        NOT NULL CHECK (btrim(moneda) <> ''),
  fecha_hora_servicio TIMESTAMPTZ NOT NULL,
  notas               TEXT,
  entregada_en        TIMESTAMPTZ NOT NULL,
  vigente_hasta       DATE        NOT NULL,
  actor_id            TEXT,
  actor_rol           TEXT,
  PRIMARY KEY (solicitud_id, version)
);

-- ---------------------------------------------------------------------------
-- reprogramacion_solicitud — cambio de fecha y hora de una solicitud PAGADO, sin
-- cambio de estado ni pago nuevo (DEC-148 #24). Motivo obligatorio; queda actor y
-- marca de tiempo. Solo INSERT. El histórico unificado lo arma E10-T11.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS solicitudes.reprogramacion_solicitud (
  id                  BIGSERIAL   PRIMARY KEY,
  solicitud_id        UUID        NOT NULL REFERENCES solicitudes.solicitud (id),
  fecha_hora_anterior TIMESTAMPTZ,
  fecha_hora_nueva    TIMESTAMPTZ NOT NULL,
  motivo              TEXT        NOT NULL CHECK (btrim(motivo) <> ''),
  actor_id            TEXT,
  actor_rol           TEXT,
  creado_en           TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS reprogramacion_por_solicitud_idx
  ON solicitudes.reprogramacion_solicitud (solicitud_id, id);

GRANT SELECT, INSERT ON solicitudes.cotizacion TO op_solicitudes;
GRANT SELECT, INSERT ON solicitudes.reprogramacion_solicitud TO op_solicitudes;
GRANT USAGE ON SEQUENCE solicitudes.reprogramacion_solicitud_id_seq TO op_solicitudes;
