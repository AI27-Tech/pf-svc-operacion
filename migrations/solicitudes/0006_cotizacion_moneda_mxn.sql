-- 0006_cotizacion_moneda_mxn.sql — segunda barrera de la moneda de la cotización
-- (DEC-148 #62: la única moneda es el peso mexicano, MXN). El módulo ya la valida;
-- esto impide que un INSERT directo guarde otra. Aditiva e idempotente (DT-25).
--
-- Nota sobre 0004_cotizacion.sql (ya mergeada, no se edita): su comentario dice
-- que el catálogo de monedas sigue pendiente y que moneda es texto libre. Quedó
-- superado por DEC-148 #62: no hay catálogo, la moneda es MXN exacta. El CHECK de
-- 0004 (btrim(moneda) <> '') se conserva; este lo estrecha.
--
-- solicitudes.cotizacion es la única tabla del esquema con columna moneda
-- (pago_solicitud y reprogramacion_solicitud no guardan monto ni moneda). Los
-- GRANT no cambian. Si ya existiera una fila con otra moneda, el ALTER falla a
-- propósito en vez de aceptar el dato.
DO $$
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM pg_constraint
    WHERE conname = 'cotizacion_moneda_mxn'
      AND conrelid = 'solicitudes.cotizacion'::regclass
  ) THEN
    ALTER TABLE solicitudes.cotizacion
      ADD CONSTRAINT cotizacion_moneda_mxn CHECK (moneda = 'MXN');
  END IF;
END
$$;
