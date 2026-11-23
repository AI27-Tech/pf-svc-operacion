-- 0003_solicitud.sql — la solicitud, su histórico de transiciones y su outbox
-- (E10-T2, E10-T3; ADR 38, DEC-148). Va DESPUÉS de 0002_grants (que revocó los
-- privilegios por defecto de 0001), así que cada tabla declara sus propios GRANT
-- mínimos aquí mismo, como viajes/0004. Todo es idempotente: cmd/migrate reaplica
-- todo en cada arranque (DT-25).
--
-- Nombres de tablas y columnas: los fija quien implementa, con revisión del Lead
-- (E10, "Pendientes y decisiones abiertas").

-- ---------------------------------------------------------------------------
-- solicitud — el agregado. NO existe escritura directa de `estado`: solo la
-- función de transición del módulo lo cambia, validada contra
-- contracts/estados/solicitud.estados.yaml (ADR 38). El CHECK impide por la base
-- un estado fuera del enum, aunque la aplicación fallara.
-- TODO(operacion): el CHECK duplica los tokens del enum generado; la prueba
-- TestCheckEstadoCoincideConElContrato falla si divergen de estados.EstadosSolicitud.
--
-- marca_penalizacion es un atributo, no un estado: Argos solo marca, no guarda el
-- monto (DEC-148 #6). Solo puede ser true en CANCELADO_POR_CLIENTE. Sin DEFAULT,
-- como viaje.nacio_de_solicitud: quien inserte decide el valor.
--
-- destinos y tracto se guardan como JSONB con la forma del contrato (Destino,
-- Tracto): la aprobación (E10-T8) los copiará a viajes.destino. cliente_id es el
-- tenant del token (DEC-112), identificador de catalogos, NO una FK.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS solicitudes.solicitud (
  id                  UUID        PRIMARY KEY,
  cliente_id          UUID        NOT NULL,
  estado              TEXT        NOT NULL,
  marca_penalizacion  BOOLEAN     NOT NULL,
  origen              TEXT        NOT NULL,
  destinos            JSONB       NOT NULL,
  fecha_hora_servicio TIMESTAMPTZ,
  tipo_servicio       TEXT,
  tracto              JSONB,
  creado_en           TIMESTAMPTZ NOT NULL,
  CONSTRAINT estado_token_valido CHECK (estado IN (
    'SOLICITADO','COTIZADO','PAGADO','APROBADO','RECHAZADO_POR_AI27',
    'RECHAZADO_POR_CLIENTE','CANCELADO_POR_CLIENTE','VENCIDA'
  )),
  CONSTRAINT marca_solo_en_cancelacion CHECK (NOT marca_penalizacion OR estado = 'CANCELADO_POR_CLIENTE'),
  CONSTRAINT origen_no_vacio CHECK (btrim(origen) <> ''),
  CONSTRAINT destinos_es_lista CHECK (jsonb_typeof(destinos) = 'array' AND jsonb_array_length(destinos) >= 1)
);
CREATE INDEX IF NOT EXISTS solicitud_por_estado_idx ON solicitudes.solicitud (estado, creado_en);
CREATE INDEX IF NOT EXISTS solicitud_por_cliente_idx ON solicitudes.solicitud (cliente_id, creado_en);

-- ---------------------------------------------------------------------------
-- transicion_solicitud — histórico de cada cambio de estado, solo INSERT. Es la
-- fuente del histórico (E10-T11). `accion` usa el vocabulario del contrato
-- (HistoricoSolicitudItem.accion); `disparador` es el de la arista del ADR 38.
-- actor_id es el `sub` del token (opaco, no es FK ni necesariamente un UUID);
-- NULL cuando lo hizo el sistema. marca_penalizacion solo se llena en las
-- cancelaciones (DEC-148 #6, #17).
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS solicitudes.transicion_solicitud (
  id                 BIGSERIAL   PRIMARY KEY,
  solicitud_id       UUID        NOT NULL REFERENCES solicitudes.solicitud (id),
  de_estado          TEXT        NOT NULL,
  a_estado           TEXT        NOT NULL,
  accion             TEXT        NOT NULL,
  disparador         TEXT        NOT NULL CHECK (disparador IN ('cliente','cotizacion','jefe_operativo','administrador','sistema')),
  actor_id           TEXT,
  actor_rol          TEXT,
  motivo             TEXT,
  marca_penalizacion BOOLEAN,
  creado_en          TIMESTAMPTZ NOT NULL,
  CONSTRAINT de_estado_valido CHECK (de_estado IN (
    'SOLICITADO','COTIZADO','PAGADO','APROBADO','RECHAZADO_POR_AI27',
    'RECHAZADO_POR_CLIENTE','CANCELADO_POR_CLIENTE','VENCIDA'
  )),
  CONSTRAINT a_estado_valido CHECK (a_estado IN (
    'SOLICITADO','COTIZADO','PAGADO','APROBADO','RECHAZADO_POR_AI27',
    'RECHAZADO_POR_CLIENTE','CANCELADO_POR_CLIENTE','VENCIDA'
  ))
);
CREATE INDEX IF NOT EXISTS transicion_solicitud_por_solicitud_idx
  ON solicitudes.transicion_solicitud (solicitud_id, id);

-- ---------------------------------------------------------------------------
-- outbox — el outbox de los eventos solicitud.* (operacion.md (g): todos los
-- eventos salen del outbox del esquema que los origina, en la MISMA tx que el
-- cambio, P17). Distinto del de viajes (ADR 35 solo unifica la familia viaje.*).
-- Clave de orden = solicitud_id. Refleja outbox.OutboxDDL de pf-pkg-platform-go.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS solicitudes.outbox (
  id            BIGSERIAL   PRIMARY KEY,
  evento_id     UUID        NOT NULL UNIQUE,
  tipo          TEXT        NOT NULL,
  version       INT         NOT NULL DEFAULT 1,
  agregado_id   TEXT        NOT NULL,
  sensibilidad  TEXT        NOT NULL DEFAULT 'amplia',
  ocurrido_en   TIMESTAMPTZ NOT NULL,
  payload       JSONB       NOT NULL,
  trace_context JSONB       NOT NULL DEFAULT '{}'::jsonb,
  publicado     BOOLEAN     NOT NULL DEFAULT false,
  publicado_en  TIMESTAMPTZ,
  creado_en     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS outbox_pendientes_idx
  ON solicitudes.outbox (creado_en) WHERE publicado = false;

-- ---------------------------------------------------------------------------
-- GRANT mínimos de op_solicitudes (I1). Nunca DELETE. De `solicitud` solo puede
-- actualizar las tres columnas que el módulo cambia: el privilegio por columna
-- impide que otro camino reescriba cliente_id, origen o creado_en.
-- ---------------------------------------------------------------------------
GRANT SELECT, INSERT ON solicitudes.solicitud TO op_solicitudes;
GRANT UPDATE (estado, marca_penalizacion, fecha_hora_servicio) ON solicitudes.solicitud TO op_solicitudes;
GRANT SELECT, INSERT ON solicitudes.transicion_solicitud TO op_solicitudes;
GRANT USAGE ON SEQUENCE solicitudes.transicion_solicitud_id_seq TO op_solicitudes;
GRANT SELECT, INSERT, UPDATE ON solicitudes.outbox TO op_solicitudes;
GRANT USAGE ON SEQUENCE solicitudes.outbox_id_seq TO op_solicitudes;
