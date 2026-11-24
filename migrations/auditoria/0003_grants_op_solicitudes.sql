-- 0003_grants_op_solicitudes.sql — tramo `auditoria` de la segunda excepción de
-- GRANT cruzado (ADR 39, E10-T1). El interceptor de auditoría escribe el registro
-- en la MISMA transacción que la aprobación de la solicitud (P3). op_solicitudes
-- entra al esquema (USAGE) e inserta en registro_auditoria; nunca lee, nunca
-- actualiza, nunca borra. Va en la fase de grants: el rol lo crea solicitudes/0001.
GRANT USAGE ON SCHEMA auditoria TO op_solicitudes;
GRANT INSERT ON auditoria.registro_auditoria TO op_solicitudes;
GRANT USAGE ON SEQUENCE auditoria.registro_auditoria_id_seq TO op_solicitudes;
