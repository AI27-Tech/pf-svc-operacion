-- 0002_grants.sql — privilegios por tabla del esquema solicitudes (E10-T1).
-- La aplica el rol dueño (op_solicitudes). Mismo patrón que viajes/0002_grants.sql:
-- 0001 corre un ALTER DEFAULT PRIVILEGES genérico que otorga SELECT, INSERT,
-- UPDATE y DELETE sobre toda tabla futura; aquí se parte de cero para que cada
-- tabla que cree E10-T2 declare en su propia migración los GRANT mínimos que
-- necesita (p. ej. transicion_solicitud, append-only: solo SELECT e INSERT).
--
-- Ningún otro rol recibe nada sobre este esquema: solo op_solicitudes lo escribe.
-- Todavía no hay tablas, así que no hay GRANT por tabla.

-- --- Partir de cero: retirar lo que 0001 otorgó de forma genérica --------------
REVOKE ALL ON ALL TABLES    IN SCHEMA solicitudes FROM op_solicitudes;
REVOKE ALL ON ALL SEQUENCES IN SCHEMA solicitudes FROM op_solicitudes;
ALTER DEFAULT PRIVILEGES IN SCHEMA solicitudes
  REVOKE ALL ON TABLES FROM op_solicitudes;
