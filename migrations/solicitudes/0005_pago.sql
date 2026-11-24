-- 0005_pago.sql — confirmación del pago de la solicitud (E10-T6; ADR 38, DEC-148
-- #34, #35, #37, #43). Va después de 0002_grants: declara sus propios GRANT.
-- Idempotente (DT-25).
--
-- pago_solicitud — una fila por confirmación. El pago ocurre fuera de Argos: esto
-- registra su confirmación. Gana la fecha del PAGO, no la de la confirmación
-- (DEC-148 #34): fecha_pago es el día que declaró cotizacion; confirmado_en es
-- cuándo se capturó. `tardio` marca la excepción VENCIDA -> PAGADO (#35).
-- La reversa del administrador (#37) no borra la fila: marca revertido_en, para
-- conservar la evidencia, y el pago vigente es el que no está revertido. Solo
-- puede haber UN pago vigente por solicitud. Sin archivo adjunto (#43).
CREATE TABLE IF NOT EXISTS solicitudes.pago_solicitud (
  id                BIGSERIAL   PRIMARY KEY,
  solicitud_id      UUID        NOT NULL REFERENCES solicitudes.solicitud (id),
  fecha_pago        DATE        NOT NULL,
  referencia        TEXT,
  confirmado_en     TIMESTAMPTZ NOT NULL,
  tardio            BOOLEAN     NOT NULL,
  cotizacion_version INT        NOT NULL,
  actor_id          TEXT,
  actor_rol         TEXT,
  revertido_en      TIMESTAMPTZ
);
CREATE UNIQUE INDEX IF NOT EXISTS pago_vigente_unico_idx
  ON solicitudes.pago_solicitud (solicitud_id) WHERE revertido_en IS NULL;

-- Solo INSERT, y UPDATE únicamente de la marca de reversa (privilegio por columna).
GRANT SELECT, INSERT ON solicitudes.pago_solicitud TO op_solicitudes;
GRANT UPDATE (revertido_en) ON solicitudes.pago_solicitud TO op_solicitudes;
GRANT USAGE ON SEQUENCE solicitudes.pago_solicitud_id_seq TO op_solicitudes;
