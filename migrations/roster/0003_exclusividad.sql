-- 0003_exclusividad.sql — un vehículo y un custodio pertenecen a UNA sola unidad
-- activa a la vez (DEC-142 #4, E8-T21). Estar en una unidad desactivada no
-- cuenta. La regla vive en la base (índices únicos parciales), no en un SELECT
-- previo: dos altas concurrentes con el mismo custodio no pueden ganar las dos.
-- Idempotente. Si el roster sembrado ya viola la regla, la creación del índice
-- falla y nombra la clave duplicada: se corrige el dato, no se relaja la regla.

-- --- Vehículo: índice único parcial sobre las unidades activas -----------------
CREATE UNIQUE INDEX IF NOT EXISTS unidad_vehiculo_activa_unico
  ON roster.unidad (vehiculo_id) WHERE activa;

-- --- Custodio: la tabla puente lleva copia de `activa` de su unidad ------------
-- Un índice parcial no puede mirar otra tabla, así que unidad_custodio guarda
-- `activa` y una FK compuesta (unidad_id, activa) → unidad (id, activa) con
-- ON UPDATE CASCADE la mantiene igual a la de la unidad: desactivar o reactivar
-- la unidad actualiza sus filas puente en la misma sentencia, y la reactivación
-- choca con el índice único si el custodio ya está en otra unidad activa.
ALTER TABLE roster.unidad_custodio
  ADD COLUMN IF NOT EXISTS activa BOOLEAN NOT NULL DEFAULT true;

UPDATE roster.unidad_custodio c
   SET activa = u.activa
  FROM roster.unidad u
 WHERE u.id = c.unidad_id AND c.activa IS DISTINCT FROM u.activa;

DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'unidad_id_activa_key') THEN
    ALTER TABLE roster.unidad ADD CONSTRAINT unidad_id_activa_key UNIQUE (id, activa);
  END IF;
  IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'unidad_custodio_activa_fkey') THEN
    ALTER TABLE roster.unidad_custodio ADD CONSTRAINT unidad_custodio_activa_fkey
      FOREIGN KEY (unidad_id, activa) REFERENCES roster.unidad (id, activa) ON UPDATE CASCADE;
  END IF;
END
$$;

CREATE UNIQUE INDEX IF NOT EXISTS unidad_custodio_activa_unico
  ON roster.unidad_custodio (custodio_id) WHERE activa;
