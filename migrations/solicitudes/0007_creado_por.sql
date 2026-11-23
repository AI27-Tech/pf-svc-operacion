-- 0007_creado_por.sql — quién creó la solicitud (E10-T11, histórico unificado).
-- El histórico (HistoricoSolicitudItem) lleva el actor de cada acción; la creación
-- era la única sin actor guardado en `solicitudes` (op_solicitudes no lee
-- `auditoria`). Es el `sub` del token del cliente, opaco como actor_id de las demás
-- tablas. NULL en las solicitudes anteriores a esta migración (ninguna en V2: no hay
-- datos de producción, DEC-148 #7). Idempotente (DT-25). No necesita GRANT nuevo:
-- op_solicitudes ya inserta en la tabla y SOLO puede actualizar tres columnas
-- (estado, marca_penalizacion, fecha_hora_servicio), así que el valor no se reescribe.
ALTER TABLE solicitudes.solicitud ADD COLUMN IF NOT EXISTS creado_por TEXT;
