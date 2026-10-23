-- 0003_evento_id.sql — idempotencia durable para las entradas de bitácora
-- proyectadas desde un evento de dominio (E7, tracer.17; DEC-100 punto 1).
--
-- El consumidor durable de emergencia.nota_agregada copia cada nota al histórico
-- del viaje insertando una entrada de bitácora. A diferencia del consumidor de
-- catalogo.actualizado (efecto en memoria, dedup en memoria), aquí el efecto es
-- un INSERT: una reentrega del evento tras un reinicio duplicaría la entrada. La
-- bitácora es un registro narrativo del viaje; un duplicado no es tolerable.
--
-- Solución mínima: una columna evento_id nullable con índice único PARCIAL. Las
-- entradas normales (registrarEntradaBitacora por HTTP) no la llevan y no se ven
-- afectadas; las proyectadas de un evento la llevan, y el índice único garantiza
-- que reentregar el mismo evento no inserta dos veces (INSERT ... ON CONFLICT DO
-- NOTHING en el consumidor). No se usa una tabla inbox: el anti-duplicado vive en
-- la propia fila que se escribiría, que es lo más simple que da la garantía.
--
-- Aditivo: columna nueva nullable, sin reescribir filas existentes. Hereda el
-- grant de tabla de 0002 (SELECT/INSERT sobre entrada_bitacora); no requiere
-- grant nuevo.

ALTER TABLE bitacora.entrada_bitacora
  ADD COLUMN IF NOT EXISTS evento_id TEXT;

-- Idempotencia durable: a lo más una entrada por evento_id (solo donde no es
-- NULL, para no restringir las entradas normales).
CREATE UNIQUE INDEX IF NOT EXISTS entrada_por_evento_idx
  ON bitacora.entrada_bitacora (evento_id)
  WHERE evento_id IS NOT NULL;
