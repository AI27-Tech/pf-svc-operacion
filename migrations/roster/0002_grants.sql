-- 0002_grants.sql — privilegios por tabla del esquema roster (E0-T5, ADR 33).
-- La aplica el rol dueño (op_roster).
--
-- Por qué el REVOKE de abajo: 0001 corre un ALTER DEFAULT PRIVILEGES que otorga
-- SELECT, INSERT, UPDATE y DELETE sobre toda tabla, ANTES de crear las tablas.
-- op_roster SÍ necesita los cuatro privilegios sobre sus dos tablas (el Jefe
-- Operativo administra el roster, incluido borrar unidades), así que el efecto
-- neto sobre las tablas de hoy coincide; el REVOKE existe para que una tabla
-- futura (0003+) NO herede privilegios por defecto y para dejar el mismo patrón
-- explícito que los demás esquemas. Referencia:
-- pf-svc-catalogos/migrations/catalogos/0002_grants.sql.

-- --- Partir de cero: retirar lo que 0001 otorgó de forma genérica --------------
REVOKE ALL ON ALL TABLES    IN SCHEMA roster FROM op_roster;
REVOKE ALL ON ALL SEQUENCES IN SCHEMA roster FROM op_roster;
ALTER DEFAULT PRIVILEGES IN SCHEMA roster
  REVOKE ALL ON TABLES FROM op_roster;

-- El Jefe Operativo administra el roster: compone unidades (vehículo + custodios)
-- y su disponibilidad (ADR 31 §5). op_roster muta su propio esquema por completo,
-- incluido DELETE (dar de baja una unidad del roster).
GRANT SELECT, INSERT, UPDATE, DELETE ON roster.unidad          TO op_roster;
GRANT SELECT, INSERT, UPDATE, DELETE ON roster.unidad_custodio TO op_roster;

-- --- op_asignacion: lectura del roster (ADR 33) -------------------------------
-- La asignación LEE la composición de la unidad antes de reservarla: necesita
-- saber qué custodios integran la unidad-paquete. Entra al esquema (USAGE) y
-- hace SOLO LECTURA; nunca muta el roster. El USAGE de esquema se otorga aquí
-- (se movió desde roster/0001 para no referenciar op_asignacion antes de crearlo).
GRANT USAGE ON SCHEMA roster TO op_asignacion;
GRANT SELECT ON roster.unidad          TO op_asignacion;
GRANT SELECT ON roster.unidad_custodio TO op_asignacion;
