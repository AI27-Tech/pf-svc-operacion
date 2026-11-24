-- 0007_estados_v2 — el viaje de V2 (ADR 38, DEC-148 #3, #14, #27).
-- La solicitud sale del viaje: el viaje nace siempre en APROBADO y sus estados
-- son los 10 de contracts/estados/viaje.estados.yaml (tracer.45). CANCELADO pasa
-- a CANCELADO_POR_CLIENTE; SOLICITADO, RECHAZADO_POR_AI27, RECHAZADO_POR_CLIENTE
-- y CANCELADO se retiran. No se edita la 0001: cmd/migrate reaplica todo en cada
-- arranque y su CREATE TABLE IF NOT EXISTS no cambiaría el CHECK (DT-25). Esta
-- migración es idempotente: solo toca el CHECK si su definición difiere de la
-- esperada, para no tomar ACCESS EXCLUSIVE ni escanear la tabla en cada arranque.
--
-- No migra datos: el módulo `solicitudes` es nuevo en V2 (ADR 39). Una fila con
-- un estado retirado detiene la migración con un mensaje que dice cuántas hay de
-- cada estado. NO se usa NOT VALID: dejaría visibles, sin el filtro de alcance
-- que ya no existe (PasoPorAprobado), filas previas a la aprobación, y haría
-- fallar cualquier UPDATE sobre ellas.
-- TODO(operacion): el CHECK duplica los tokens del enum generado; la prueba
-- TestCheckEstadoCoincideConElContrato falla si divergen de estados.EstadosViaje.
-- ---------------------------------------------------------------------------
DO $$
DECLARE
  tokens   text := $t$'APROBADO','ASIGNADO','APTO_PARA_SALIDA','EN_EMBARQUE','PATRULLA_EN_CAMINO',
    'EN_TRANSITO','EN_DESTINO','EN_DESCARGA','CERRADO','CANCELADO_POR_CLIENTE'$t$;
  esperada text;
  actual   text;
  retirados text;
BEGIN
  -- Definición esperada, tal como Postgres la normaliza, sacada de una tabla
  -- temporal con el mismo CHECK (evita comparar texto escrito a mano).
  EXECUTE 'CREATE TEMP TABLE _viaje_estado_esperado (estado TEXT NOT NULL, '
       || 'CONSTRAINT estado_token_valido CHECK (estado IN (' || tokens || ')))';
  SELECT pg_get_constraintdef(oid) INTO esperada FROM pg_constraint
   WHERE conrelid = '_viaje_estado_esperado'::regclass AND conname = 'estado_token_valido';
  DROP TABLE _viaje_estado_esperado;

  SELECT pg_get_constraintdef(oid) INTO actual FROM pg_constraint
   WHERE conrelid = 'viajes.viaje'::regclass AND conname = 'estado_token_valido';

  IF actual IS DISTINCT FROM esperada THEN
    EXECUTE 'SELECT string_agg(estado || ' || quote_literal(' = ') || ' || n, '
         || quote_literal(', ' ) || ' ORDER BY estado) FROM (SELECT estado, count(*) AS n '
         || 'FROM viajes.viaje WHERE estado NOT IN (' || tokens || ') GROUP BY estado) r'
      INTO retirados;
    IF retirados IS NOT NULL THEN
      RAISE EXCEPTION '0007_estados_v2: hay viajes en estados retirados por el ADR 38 (%). '
        'Reinicia la base de dev/demo o migra esos viajes a mano antes de aplicar la migración.', retirados;
    END IF;
    ALTER TABLE viajes.viaje DROP CONSTRAINT IF EXISTS estado_token_valido;
    EXECUTE 'ALTER TABLE viajes.viaje ADD CONSTRAINT estado_token_valido CHECK (estado IN (' || tokens || '))';
  END IF;
END $$;

-- Origen del viaje: true si nació de una solicitud pagada (la pone la aprobación,
-- E10-T8). Es la fuente de Viaje.cancelacion_con_penalizacion (DEC-148 #14, #27).
-- El DEFAULT false solo sirve para rellenar las filas existentes: se quita para
-- que quien inserte un viaje sin decidir la columna falle, en vez de nacer sin la
-- penalización (50 % por cancelación) sin que nada avise. Crear escribe false.
ALTER TABLE viajes.viaje ADD COLUMN IF NOT EXISTS nacio_de_solicitud boolean NOT NULL DEFAULT false;
DO $$
BEGIN
  IF EXISTS (SELECT 1 FROM pg_attrdef d JOIN pg_attribute a ON a.attrelid = d.adrelid AND a.attnum = d.adnum
              WHERE d.adrelid = 'viajes.viaje'::regclass AND a.attname = 'nacio_de_solicitud') THEN
    ALTER TABLE viajes.viaje ALTER COLUMN nacio_de_solicitud DROP DEFAULT;
  END IF;
END $$;
