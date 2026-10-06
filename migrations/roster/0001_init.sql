-- 0001_init.sql — esquema roster y rol op_roster (ADR 33, DEC-066).
-- El roster de `unidades` (composición vehículo+custodios y disponibilidad,
-- administrado por el Jefe Operativo, ADR 31 §5) vive en su propio esquema
-- dentro de operacion, separado del libro caliente de reservas (asignacion) y
-- del agregado del viaje (viajes). operacion tiene por tanto CINCO esquemas.

CREATE SCHEMA IF NOT EXISTS roster;

DO $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'op_roster') THEN
    CREATE ROLE op_roster LOGIN;
  END IF;
END
$$;

GRANT USAGE ON SCHEMA roster TO op_roster;
ALTER DEFAULT PRIVILEGES IN SCHEMA roster
  GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO op_roster;

-- La asignación LEE el roster para saber qué es una unidad antes de reservarla
-- (ADR 33): se extiende la excepción de GRANT existente de op_asignacion con
-- USAGE de SOLO LECTURA sobre roster. No es una excepción nueva: es la misma de
-- asignacion/0001_init.sql, extendida al quinto esquema. Nunca alcanza catalogos.
GRANT USAGE ON SCHEMA roster TO op_asignacion;
-- TODO(E0-T5): privilegio por tabla acotado a SELECT para op_asignacion sobre
-- las tablas de roster. No se amplía más aquí.

-- ---------------------------------------------------------------------------
-- unidad — paquete pre-armado: un vehículo de AI27 (coche o moto) con sus
-- custodios integrados (ADR 31). patrulla/moto son TIPOS de unidad derivados del
-- tipo de vehículo, nunca columna ni recurso aparte. El vehículo y los custodios
-- son ÁTOMOS de catalogos: aquí se referencian por IDENTIFICADOR, nunca por FK
-- cruzada de esquema/servicio.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS roster.unidad (
  id            UUID        PRIMARY KEY,
  tipo          TEXT        NOT NULL CHECK (tipo IN ('patrulla','moto')),
  vehiculo_id   TEXT        NOT NULL,  -- átomo de catalogos; identificador, NO FK
  activa        BOOLEAN     NOT NULL DEFAULT true,  -- disponibilidad administrada por el Jefe Operativo
  creada_en     TIMESTAMPTZ NOT NULL DEFAULT now()
  -- TODO(ADR 31 "Pendiente real", E1-HU-031): campos reales del vehículo/paquete,
  -- pendientes de Operación. No inventar columnas.
);

-- ---------------------------------------------------------------------------
-- unidad_custodio — composición: qué custodios integran la unidad (coche: 1-2,
-- moto: 1; ADR 31). El custodio es un átomo de catalogos; se referencia por
-- identificador. Reservar la unidad reserva su tripulación: estos custodios NO
-- se reservan por separado (la reserva vive en asignacion, por unidad).
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS roster.unidad_custodio (
  unidad_id     UUID        NOT NULL REFERENCES roster.unidad (id),
  custodio_id   TEXT        NOT NULL,  -- átomo de catalogos; identificador, NO FK
  PRIMARY KEY (unidad_id, custodio_id)
  -- TODO(operacion): la regla coche:1-2 / moto:1 se valida en el caso de uso de
  -- armado de roster; no se fija aquí como constraint hasta confirmar la forma.
);
