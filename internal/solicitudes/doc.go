// Package solicitudes es el módulo de la `solicitud` previa al viaje: creación,
// cotización, pago y respuesta del cliente (ADR 38, DEC-148). Es el ÚNICO dueño
// de las transiciones de la solicitud: ningún camino cambia `estado` por fuera de
// transicionar, que valida contra la máquina generada de pf-pkg-contracts
// (estados.PuedeTransicionarSolicitud). Usa su propio esquema, rol y pool
// (op_solicitudes). Los eventos solicitud.* salen del outbox de su esquema, en la
// misma transacción que el cambio (P17).
package solicitudes

import _ "time/tzdata" // la zona America/Mexico_City debe resolverse sin tzdata del sistema
