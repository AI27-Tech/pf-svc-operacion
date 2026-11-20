-- 0001_init.sql — esquema solicitudes y rol op_solicitudes (E10-T1, ADR 38, ADR 39).
-- El módulo `solicitudes` (cotización, pago y aprobación previos al viaje,
-- DEC-148 #8) vive en su propio esquema dentro de operacion, que pasa a SEIS.
--
-- Esta ola solo crea el esquema y el rol. Las tablas de la solicitud (nombres y
-- columnas) las fija E10-T2; aquí NO se inventa el modelo.
--
-- op_solicitudes lleva la SEGUNDA excepción de GRANT cruzado del sistema (ADR
-- 39): al aprobar, crea el viaje en `viajes` y audita en `auditoria` en la misma
-- transacción. Sus privilegios sobre esos esquemas se otorgan desde cada esquema
-- destino, en la fase de grants (no aquí, para que ningún 0001 referencie un rol
-- de otro esquema):
--   viajes/0008_grants_op_solicitudes.sql     — USAGE + INSERT en viaje/transicion_viaje/outbox
--   auditoria/0003_grants_op_solicitudes.sql  — USAGE + INSERT en registro_auditoria
-- op_solicitudes NUNCA recibe nada sobre bitacora, asignacion, roster ni catalogos.

CREATE SCHEMA IF NOT EXISTS solicitudes;

DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'op_solicitudes') THEN
    CREATE ROLE op_solicitudes LOGIN;
  END IF;
END
$$;

GRANT USAGE ON SCHEMA solicitudes TO op_solicitudes;
ALTER DEFAULT PRIVILEGES IN SCHEMA solicitudes
  GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO op_solicitudes;
