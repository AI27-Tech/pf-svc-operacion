# pf-svc-operacion

Módulo `operacion` de AI27: dueño del progreso de un viaje —su creación, su
máquina de estados, su asignación de recursos, su bitácora y su auditoría
probatoria— dentro de **una sola frontera transaccional indivisible**. Además
es dueño del roster de `unidades` (ADR 31 §5). Capa de dominio; vive dentro del
deployable `core`.

Estado: **scaffolding inicial** (prompt 04). Estructura y cableado; sin lógica
de negocio real todavía. Contrato fijado en `pf-pkg-contracts` v0.1.0-tracer.3.

## Cuatro esquemas, cuatro roles, una transacción

`operacion` es el único servicio con cuatro esquemas (`viajes`, `asignacion`,
`bitacora`, `auditoria`) y cuatro roles (`op_viajes`, `op_asignacion`,
`op_bitacora`, `op_auditoria`), cada uno con su pool. Comparten la **transacción**,
no el acoplamiento: la asignación muta la reserva, el estado del viaje y la
auditoría en una sola operación de negocio (ADR 31; P2). Repartirla entre
servicios la convierte en sagas que sobrevenden.

**La única excepción de GRANT cruzado del sistema entero** es `op_asignacion`,
que recibe `USAGE` sobre los cuatro esquemas de `operacion` (nunca sobre
`catalogos`). Está escrita y comentada en `migrations/asignacion/0001_init.sql`.
Ningún otro rol de ningún otro servicio puede introducir otra sin un ADR.

## Piezas clave

- **Máquina de estados importada, no declarada:** se consume
  `pf-pkg-contracts/.../estados` (`EstadoViaje`, `PuedeTransicionar`, …).
  `viajes` es la única autoridad de transición; `viaje.estado` se restringe por
  constraint a los 13 tokens generados. INCIDENCIA y SIN_SENAL son **condiciones
  ortogonales**, nunca estados (P15).
- **Reserva por exclusión:** `asignacion.reserva_recurso` tiene
  `EXCLUDE USING gist (unidad_id WITH =, ventana WITH &&)` (extensión
  `btree_gist`). Se reserva la **unidad completa** (escolta de 1..N unidades);
  nunca custodios por separado ni `patrulla` como recurso aparte (ADR 31).
- **Auditoría como interceptor** (`internal/auditoria`): una función que envuelve
  la escritura y recibe la tx abierta, nunca un cliente omitible. El registro se
  escribe en la misma tx que el cambio (P3).
- **Outbox por esquema productor:** `viajes` y `asignacion` tienen outbox y
  worker (con su rol); `bitacora` y `auditoria` no publican eventos.
- **Aptitud informativa (DEC-055):** `AptitudRecursos` consulta aptitud pero
  **nunca gatea**. El único rechazo de asignación es la reserva fallida.

## Aristas

- `operacion → catalogos` (puerto `AptitudRecursos`, en `ports.go`). Adaptador
  HTTP en `adapters/http/`; el en proceso vive en `pf-svc-core`, no aquí.
- `operacion → identidad` restringida a revocación (`listarRevocadas`,
  `obtenerSesion`; DEC-051, puerto `Revocacion`).
- No llama a `mensajeria`, `reportes`, `realtime` ni `procesamiento`: todo les
  llega por evento. No importa ningún otro `pf-svc-*`.

## Correr standalone (I5)

```bash
SERVICE_NAME=operacion HTTP_ADDR=:8080 \
  DB_VIAJES_URL="postgres://op_viajes@localhost/ai27?sslmode=disable" \
  DB_ASIGNACION_URL="postgres://op_asignacion@localhost/ai27?sslmode=disable" \
  DB_BITACORA_URL="postgres://op_bitacora@localhost/ai27?sslmode=disable" \
  DB_AUDITORIA_URL="postgres://op_auditoria@localhost/ai27?sslmode=disable" \
  CATALOGOS_BASE_URL="http://localhost:8081" \
  go run ./cmd/api
```

## Brechas de contrato (reportadas; bloquean partes del scaffold)

Verificado contra `pf-pkg-contracts` v0.1.0-tracer.3. Lo presente se usa; lo
desalineado o ausente se reporta y **no se escribe a mano** (contrato primero).

| Superficie | Estado en tracer.3 | Efecto |
|---|---|---|
| Enum/grafo de estados (Go) | ✅ presente | se importa y se usa |
| operacion operationIds (crear/transicionar/asignar/bitácora/incidencias/auditoría) | ✅ presentes | implementables conforme se escriban los casos de uso |
| aptitud de unidad y custodio (catalogos) | ✅ presente | el puerto `AptitudRecursos` se tipa con ellos |
| **Átomo `vehículo` (ADR 31)** en lugar de `Unidad` (ADR 24) | ❌ el contrato aún dice `/unidades` | el puerto usa `Unidad`; el rename espera a tracer.4 (C3) |
| **Asignación por escolta (1..N unidades)** | ❌ `asignarRecursos` pide `unidad_id`+`custodios_ids` (ADR 24) | la forma de asignación se alinea en tracer.4 |
| Eventos `viaje.asignado/reasignado` con escolta | ❌ llevan `unidad_id`+`custodios_ids` | ídem |
| `obtenerUnidad`/`obtenerCustodio` por id | ❌ solo hay lista + aptitud | el puerto no inventa el método; se añade cuando el contrato lo tenga |
| `PATCH /viajes/{id}/eta` sin `stale` (ADR 20) | ❌ el contrato trae `eta_cliente.obsoleto` | no se implementa el cálculo de obsolescencia |
| Guarda `ningun_recurso_no_apto` / 409 `recurso_no_apto` | ⚠️ el contrato aún los trae | **NO se implementan** (DEC-055); se van en tracer.4 |
| Rutas del roster de `unidades` (E1-HU-031) | ❌ ausentes | el roster no se scaffoldea (ver pendientes) |

## Pendientes que NO se infieren (requieren decisión)

- **Ubicación del roster de `unidades`:** ADR 31 lo pone en `operacion` pero
  ninguna fuente fija esquema, tablas ni rutas. No se creó un quinto esquema ni
  se eligió uno. **Preguntar antes de crear.**
- **Ubicación de `inbox_evento`** de cada consumidor durable: ADR 29 lo exige
  pero no fija en qué esquema/rol vive. **Preguntar antes de colocar.**
- **Orden por agregado con dos outbox:** `viaje.*` sale de `viajes.outbox` y
  `asignacion.outbox` con el mismo `viaje_id`; ADR 29 promete orden por agregado,
  que con dos publishers independientes no está garantizado por construcción.
  Observación para el Lead.

## Ramas

`pf-op-m` / `pf-op-d` / `pf-op-q`. Se trabaja en `pf-op-d`.
