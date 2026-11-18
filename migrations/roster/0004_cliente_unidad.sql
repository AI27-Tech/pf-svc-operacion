-- 0004_cliente_unidad.sql — cliente al que sirve la unidad (DEC-149 #2, #13 y
-- #18, tracer.41). Opcional: una unidad sin cliente (de reserva o recién armada)
-- se puede prestar a cualquier viaje. Es un dato del roster para asignar la
-- escolta y reconocer el préstamo; NO define el alcance del monitoreo (ese sale
-- de "mis clientes" en identidad, proyectado en viajes.alcance_sujeto).
-- Identificador de catalogos, no FK (otro servicio). Idempotente. Sin GRANT:
-- op_roster y op_asignacion ya tienen sus privilegios por tabla sobre
-- roster.unidad (0002_grants), que cubren la columna nueva.
ALTER TABLE roster.unidad
  ADD COLUMN IF NOT EXISTS cliente_id UUID NULL;
