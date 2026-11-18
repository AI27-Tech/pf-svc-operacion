-- 0006_alcance.sql — proyección local de `alcance.cambiado` (DEC-149 #20,
-- tracer.41): los clientes efectivos de cada sujeto según identidad ("mis
-- clientes" más los de sus subordinados). Con ella operacion acota los viajes
-- de monitorista, supervisor y coordinador por viaje.cliente_id (DEC-053 #3).
--
-- Vive en Postgres y no en memoria porque el consumidor es durable y comparte
-- grupo entre réplicas: cada evento llega a UNA réplica. En el esquema viajes
-- para que op_viajes la lea en la misma conexión que lista los viajes (I1, sin
-- pool ni GRANT cruzado nuevos).
--
-- Cada fila es el conjunto COMPLETO (snapshot, no delta); vacío = el sujeto no
-- ve viajes por alcance de monitoreo. cambiado_en ordena: un alcance más viejo
-- que el guardado no gana. Sin DELETE: quitar a un sujeto es dejar su conjunto
-- vacío, conservando cambiado_en para que un evento atrasado no lo reviva.
-- La llave es idp_subject, el `sub` del token del sujeto (DEC-149 #22,
-- tracer.42): la guarda busca el alcance de quien hace la petición con su
-- claims.Subject, porque el `sub` es opaco y distinto del sujeto_id (DEC-072).
-- TEXT porque el subject del IdP no es un UUID.

CREATE TABLE IF NOT EXISTS viajes.alcance_sujeto (
  idp_subject  TEXT        PRIMARY KEY,
  clientes_ids UUID[]      NOT NULL DEFAULT '{}',
  cambiado_en  TIMESTAMPTZ NOT NULL
);

-- 0002_grants revocó los privilegios por defecto: la tabla declara los suyos.
GRANT SELECT, INSERT, UPDATE ON viajes.alcance_sujeto TO op_viajes;
