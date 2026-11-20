-- 0007_estados_v2 — el viaje de V2 (ADR 38, DEC-148 #3, #14, #27).
-- La solicitud sale del viaje: el viaje nace siempre en APROBADO y sus estados
-- son los 10 de contracts/estados/viaje.estados.yaml (tracer.45). CANCELADO pasa
-- a CANCELADO_POR_CLIENTE; SOLICITADO, RECHAZADO_POR_AI27, RECHAZADO_POR_CLIENTE
-- y CANCELADO se retiran. No se edita la 0001: cmd/migrate reaplica todo en cada
-- arranque y su CREATE TABLE IF NOT EXISTS no cambiaría el CHECK (DT-25). Esta
-- migración es idempotente: suelta el CHECK por nombre y lo vuelve a crear.
--
-- No migra datos: el módulo `solicitudes` es nuevo en V2 (ADR 39). Una fila con
-- un estado retirado hace fallar el ADD CONSTRAINT a propósito, en vez de
-- esconderla con NOT VALID.
-- TODO(operacion): el CHECK duplica los tokens del enum generado; mantenerlo en
-- sincronía con estados.EstadosViaje (idealmente verificado en una prueba).
-- ---------------------------------------------------------------------------
ALTER TABLE viajes.viaje DROP CONSTRAINT IF EXISTS estado_token_valido;
ALTER TABLE viajes.viaje ADD CONSTRAINT estado_token_valido CHECK (estado IN (
  'APROBADO','ASIGNADO','APTO_PARA_SALIDA','EN_EMBARQUE','PATRULLA_EN_CAMINO',
  'EN_TRANSITO','EN_DESTINO','EN_DESCARGA','CERRADO','CANCELADO_POR_CLIENTE'
));

-- Origen del viaje: true si nació de una solicitud pagada (la pone la aprobación,
-- E10-T8). Es la fuente de Viaje.cancelacion_con_penalizacion (DEC-148 #14, #27).
ALTER TABLE viajes.viaje ADD COLUMN IF NOT EXISTS nacio_de_solicitud boolean NOT NULL DEFAULT false;
