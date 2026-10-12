-- 0003_ficha.sql — campos mínimos de la ficha técnica que el CONTRATO exige
-- (CrearViajeEntrada/Viaje: destinos, fecha_hora_servicio, tipo_servicio).
-- Están DERIVADOS DEL CONTRATO, no de un inventario de ficha técnica de Producto
-- (que todavía no existe, E1-T5): cuando ese inventario se defina, esta
-- estructura se extiende. No se infiere más de lo que el contrato fija hoy.
--
-- Va DESPUÉS de 0002_grants (que revoca los privilegios por defecto de 0001),
-- así que declara sus propios GRANT, como anticipa el comentario de 0002_grants
-- ("una tabla futura (0003+) no hereda privilegios").

ALTER TABLE viajes.viaje
  ADD COLUMN IF NOT EXISTS fecha_hora_servicio TIMESTAMPTZ,
  ADD COLUMN IF NOT EXISTS tipo_servicio       TEXT;

-- destino — una o más paradas del viaje (multi-parada, DEC-013/DEC-020). El
-- `orden` preserva la secuencia de la ruta. El id lo genera la app, como el del
-- viaje.
CREATE TABLE IF NOT EXISTS viajes.destino (
  id           UUID        PRIMARY KEY,
  viaje_id     UUID        NOT NULL,
  orden        INT         NOT NULL,
  nombre       TEXT        NOT NULL,
  numero_envio TEXT,
  fecha_cita   TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS destino_por_viaje_idx
  ON viajes.destino (viaje_id, orden);

-- op_viajes crea los destinos al crear el viaje y los lee después. No los
-- actualiza ni borra en V1 (se fijan en la creación). Privilegio mínimo (I1).
GRANT SELECT, INSERT ON viajes.destino TO op_viajes;
