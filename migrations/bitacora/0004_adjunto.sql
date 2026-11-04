-- 0004_adjunto.sql — evidencia del viaje (E6-T4 / E6-HU-027). Metadata del
-- adjunto en el esquema bitacora; los BYTES viven en el almacenamiento de objetos
-- (ADR 32) y NUNCA tocan la base. Cubre el TODO de reserva de 0001_init.sql.
--
-- La aplica el rol dueño op_bitacora. Estructura y grant van juntos en este
-- archivo: la tabla nace con su grant acotado, igual que 0003 no abrió grant
-- nuevo. El grant sigue el patrón de 0002 (mínimo real por tabla).
--
-- estado_escaneo nace 'pendiente' (lo fija la app al insertar; el contrato dice
-- "pendiente tras crear la intención"). En V1 no hay escáner que lo mueva a
-- limpio/infectado (pendiente de Seguridad, brief E8-T17): la columna admite los
-- tres valores del contrato con un CHECK, pero solo se escribe 'pendiente'. Por
-- eso NO se otorga UPDATE todavía: no hay transición de estado implementada; se
-- agrega cuando exista el escáner.

CREATE TABLE IF NOT EXISTS bitacora.adjunto (
  id              UUID        PRIMARY KEY,
  viaje_id        UUID        NOT NULL,
  nombre          TEXT        NOT NULL,
  tipo_medio      TEXT        NOT NULL,
  tamano_bytes    BIGINT      NOT NULL CHECK (tamano_bytes > 0),
  checksum_sha256 TEXT,
  estado_escaneo  TEXT        NOT NULL DEFAULT 'pendiente'
                  CHECK (estado_escaneo IN ('pendiente', 'limpio', 'infectado')),
  creado_en       TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS adjunto_por_viaje_idx
  ON bitacora.adjunto (viaje_id, creado_en);

-- Grant acotado: SELECT para listar y leer; INSERT para crear la intención. Sin
-- UPDATE (no hay transición de estado_escaneo en V1) ni DELETE (la evidencia no
-- se borra desde la app). Mismo criterio append-only que entrada_bitacora.
GRANT SELECT, INSERT ON bitacora.adjunto TO op_bitacora;
