-- 0005_ficha_tecnica.sql — ficha técnica del viaje (E8-T12, DEC-104): tracto
-- del cliente, contenedores 0..N y referencias de GPS. Todo opcional (edición
-- incremental, DEC-043 #3); ningún campo gatea transiciones en V1 (DEC-104 #3).
--
-- Vive en viajes.viaje porque la ficha es 1:1 con el viaje y la edita el mismo
-- rol op_viajes, que ya tiene SELECT/INSERT/UPDATE sobre la tabla (0002_grants).
-- Agregar columnas no cambia esos privilegios: esta migración NO declara GRANT.
-- Idempotente (ADD COLUMN IF NOT EXISTS): el aplicador reaplica todo en cada corrida.
--
-- Tracto en columnas separadas, no texto libre (DEC-104 #1, supera DEC-011 #2):
-- placas/economico quedan consultables para la búsqueda futura de viajes por
-- tracto (fuera de alcance de E8-T12, por eso sin índice todavía). Sin dato en
-- las cinco columnas ⇒ el contrato lee `tracto: null`. La columna `tracto TEXT`
-- de 0001 (texto libre previo, sin uso en código) se deja intacta: retirarla es
-- una migración destructiva aparte.
--
-- contenedores y gps_ids se guardan por REEMPLAZO de la lista completa (contrato
-- de guardarFicha), así que se modelan como valor de la fila (JSONB / UUID[]) y
-- no como tablas hijas: reemplazar no exige DELETE, y op_viajes no lo tiene.
-- gps_ids son referencias al catálogo de GPS de catalogos (DEC-104 #5), no FK
-- (otro servicio); se conservan aunque el dispositivo se desactive.
ALTER TABLE viajes.viaje
  ADD COLUMN IF NOT EXISTS tracto_placas    TEXT,
  ADD COLUMN IF NOT EXISTS tracto_economico TEXT,
  ADD COLUMN IF NOT EXISTS tracto_marca     TEXT,
  ADD COLUMN IF NOT EXISTS tracto_modelo    TEXT,
  ADD COLUMN IF NOT EXISTS tracto_color     TEXT,
  ADD COLUMN IF NOT EXISTS contenedores     JSONB  NOT NULL DEFAULT '[]'::jsonb,
  ADD COLUMN IF NOT EXISTS gps_ids          UUID[] NOT NULL DEFAULT '{}';
