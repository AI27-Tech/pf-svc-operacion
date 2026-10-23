-- 0004_incidencia_estadia.sql — E1-T6 (AI27-14): incidencias y estadías como
-- CONDICIONES ORTOGONALES al progreso, sin tocar viaje.estado (§C.2, P15).
--
-- Va DESPUÉS de 0002_grants (que revocó los privilegios por defecto de 0001) y
-- de 0003_ficha, así que cada tabla u objeto nuevo declara sus propios GRANT,
-- como anticipa 0002_grants ("una tabla futura (0003+) no hereda privilegios").
--
-- DECISIÓN DE UBICACIÓN (reportada en el CHANGELOG y abajo): el detalle de la
-- incidencia (tipo_interno, descripcion) vive AQUÍ, en viajes.condicion_viaje,
-- NO en bitacora.incidencia. Motivo: abrirIncidencia debe escribir la condición
-- (viajes), su detalle y el evento viaje.condicion_abierta (viajes.outbox) en UNA
-- sola transacción. El detalle narrativo de bitacora.incidencia pertenece al rol
-- op_bitacora; op_viajes no tiene GRANT sobre bitacora y op_bitacora no lo tiene
-- sobre viajes. Escribir ambos esquemas en una tx exigiría una SEGUNDA excepción
-- de GRANT cruzado (hoy solo existe la de op_asignacion), lo que requiere ADR
-- (operacion.md §(e), ADR 18 §4). Por eso el caso de uso vive íntegro en el
-- módulo viajes y guarda el detalle en el mismo esquema. bitacora.incidencia
-- queda para el camino V2 disparado por telemetría (denormaliza lat/lon, §C.5),
-- que es su propósito en el documento de servicio.

-- --- Incidencia: detalle sobre la condición ortogonal existente --------------
-- condicion_viaje ya existe (viajes/0001_init.sql) con el índice único parcial
-- `una_condicion_abierta_por_tipo` (una condición abierta por tipo y viaje) que
-- el cierre anticipado (E1-T8/AI27-16) ya lee vía hayIncidenciaAbierta. E1-T6
-- SOLO agrega las columnas de detalle que el contrato Incidencia expone
-- (tipo_interno, descripcion). El `estado` del contrato (en_atencion|resuelta)
-- se DERIVA de cerrada_en en la lectura (NULL ⇒ en_atencion), no se persiste.
ALTER TABLE viajes.condicion_viaje
  ADD COLUMN IF NOT EXISTS tipo_interno TEXT,
  ADD COLUMN IF NOT EXISTS descripcion  TEXT;
-- Los GRANT de condicion_viaje (SELECT, INSERT, UPDATE para op_viajes) ya están
-- en 0002_grants: abrir (INSERT) y resolver (UPDATE cerrada_en) ya son posibles.
-- Agregar columnas a una tabla ya concedida no cambia sus privilegios.

-- --- Estadía (DEC-038): parada del viaje, condición ortogonal ----------------
-- La estadía es una PARADA del viaje (combustible, descanso, retén, …), NO un
-- estado: abrirla/cerrarla no toca viaje.estado. Vive en el esquema viajes para
-- que el "reinicio de viaje" (DEC-013), que cierra la estadía activa sin ser
-- transición, pueda hacerlo con el mismo rol op_viajes.
--
-- PROHIBIDO (requisito del ticket): NO hay columna de duración ni de `prolongada`
-- ni umbral alguno. La duración se DERIVA de (inicio, cierre) en la lectura. Una
-- estadía abierta tiene cierre NULL.
CREATE TABLE IF NOT EXISTS viajes.estadia (
  id               UUID        PRIMARY KEY,
  viaje_id         UUID        NOT NULL,
  motivo           TEXT        NOT NULL,
  eje_autorizacion TEXT        NOT NULL,
  inicio           TIMESTAMPTZ NOT NULL DEFAULT now(),
  cierre           TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS estadia_por_viaje_idx
  ON viajes.estadia (viaje_id, inicio);

-- op_viajes abre (INSERT), lista (SELECT) y cierra por marca (UPDATE cierre) la
-- estadía; el reinicio de viaje también la cierra por marca. No la borra: la
-- estadía es historia del viaje (append-only salvo la marca de cierre).
-- Privilegio mínimo (I1), declarado explícito porque 0002_grants revocó los
-- defaults y una tabla nueva no hereda.
GRANT SELECT, INSERT, UPDATE ON viajes.estadia TO op_viajes;
