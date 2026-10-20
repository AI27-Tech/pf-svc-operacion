# CHANGELOG

## 2026:10:20

**Resumen:** E1-T5 (AI27-13): caso de uso de bitácora del viaje en `operacion`. Registrar una entrada y listar las entradas de un viaje en orden cronológico `(viaje_id, creado_en)`. La bitácora es append-only: no hay edición ni borrado de entradas; una corrección es una entrada nueva. Consume la superficie de bitácora ya contratada en `pf-pkg-contracts` (disponible desde tracer.10).

**Para quién:** para `bff` y los paneles que registran y leen la bitácora de un viaje. Antes `internal/bitacora` era solo un stub (`doc.go`); ahora expone el caso de uso y sus rutas HTTP.

**Cambios:**
- `internal/bitacora/servicio.go` (nuevo): `Servicio` con `RegistrarEntrada` y `ListarBitacora`, mismo patrón que `viajes`/`asignacion` (`Servicio{tx BeginTx}`, `NewServicio`, `tx.Begin → defer Rollback → validar → exec → Commit`). `RegistrarEntrada` hace un INSERT append-only y exige texto no vacío. `ListarBitacora` lee en orden cronológico (más antiguas primero) con paginación por cursor keyset. El id público del contrato (UUID) se deriva como UUIDv5 determinista del correlativo interno (`BIGSERIAL`): estable entre registrar y listar y opaco, sin tocar el contrato ni agregar columnas.
- `internal/bitacora/cursor.go` (nuevo): cursor opaco base64 sobre `(creado_en, correlativo)`.
- `operacion.go`: handlers `handleRegistrarBitacora` y `handleListarBitacora`, rutas `POST` y `GET /api/v1/viajes/{id}/bitacora` protegidas, y construcción del servicio en `New` con el pool `p.Bitacora`.
- `internal/bitacora/servicio_integration_test.go`, `bitacora_rutas_test.go` (tests): registrar y listar en orden con id estable y filtro por viaje; texto obligatorio; append-only verificado al nivel del GRANT (UPDATE/DELETE con `op_bitacora` fallan); la ruta solo acepta POST y GET (DELETE → 405, `Allow` sin PUT/PATCH/DELETE).

**Decisión reportada (no resuelta por código):** la bitácora NO audita en V1. El interceptor de auditoría (P3) escribe en el esquema `auditoria`, pero la única excepción de GRANT cruzado del sistema es `op_asignacion` (operacion.md §(e), ADR 18 §4); dar a `op_bitacora` acceso a `auditoria` sería una segunda excepción prohibida sin ADR. Queda consistente con `viajes`, que tampoco audita hoy. Si P3 exige auditar la bitácora en su transacción, es una decisión de arquitectura que debe ampliar la excepción por ADR.

**Verificado:** `go build ./...`, `go vet ./...`, `go test ./...` en verde.

**Archivos modificados:**
- `operacion.go`
- `internal/bitacora/servicio.go` (nuevo)
- `internal/bitacora/cursor.go` (nuevo)
- `internal/bitacora/servicio_integration_test.go` (nuevo)
- `bitacora_rutas_test.go` (nuevo)

## 2026:10:19

**Resumen:** Se sube el pin de `pf-pkg-contracts` de `v0.1.0-tracer.4` a `v0.1.0-tracer.10`. El salto es 100% aditivo en los clientes generados que `operacion` consume (sin líneas eliminadas ni cambios de firma entre las dos olas), así que no toca ningún caso de uso, handler ni tipo existente. Pone al día el contrato que `operacion` resuelve, que iba siete olas atrás del vivo; deja el cliente con las superficies de bitácora/auditoría ya contratadas disponibles para cuando E1-T5/E6-T3 (otro dueño) las consuman. No implementa ninguna de esas operaciones: solo actualiza el pin.

**Para quién:** para quien construya bitácora (E1-T5, AI27-13) y la descarga (E6-T3, AI27-80) sobre `operacion`: el cliente generado deja de estar atrás del contrato.

**Cambios:**
- `go.mod`/`go.sum`: `api-client-go` a `v0.1.0-tracer.10`.

**Verificado:** `go build`/`vet` en verde, `gofmt` limpio, `go test ./...` en verde (incl. integración contra Postgres local: asignación atómica y viajes), `arch-test@v0.1.1 OK`.

## 2026:10:15 (hora local)

**Resumen:** E5-T5 (AI27-77), lado consumidor en `operacion`: una caché en proceso de aptitud detrás del puerto `AptitudRecursos`, invalidada por el evento `catalogo.actualizado` que publica `catalogos`. Es la mitad consumidora de E5-T5; el productor ya emitía el evento desde E5-T1..T4. Alcance acotado a `operacion` (único consumidor que nombran el proto `catalogo.proto` y `contracts/events/INVENTARIO.md`); `mensajeria`/`bff`/`reportes` quedan como tickets hermanos.

**Para quién:** para el caso de uso de armado/asignación de `operacion`, que lee aptitud de `catalogos`. Antes cada lectura pegaba HTTP a `catalogos` (adaptador `AptitudRecursosHTTP`), sin caché ni invalidación. Ahora, cuando hay bus, esas lecturas se sirven de una caché en proceso con techo de obsolescencia por TTL e invalidación por evento.

**Cambios:**
- `internal/catalogoscache/aptitud.go` (nuevo): `CacheAptitud` decora `AptitudRecursos` (interfaz local, para no importar la raíz y evitar un ciclo). Clave `{entidad, recurso, cliente}`. TTL corto como techo de obsolescencia (default 30 s), reloj inyectable. Un fallo del subyacente NO se cachea y se propaga como degradado: la aptitud es **informativa** (DEC-055), el caso de uso lo traduce a "aptitud desconocida" y la asignación procede; la caché nunca convierte el fallo en rechazo. `Invalidar(entidad, recurso)` borra la entrada (idempotente, P13).
- `internal/consumidores/catalogo.go` (nuevo): `ConsumidorCatalogo` procesa `catalogo.actualizado`, deduplica por `evento_id` **en memoria** (ADR 34 / DEC-067: sin tabla `inbox_evento`, porque el efecto es en memoria) e invalida la entrada de caché. Reentregar el mismo evento invalida una sola vez (P13). Payload ilegible o metadato incompleto se acusa sin invalidar (no reintentable; tolerable porque la aptitud es informativa).
- `operacion.go`: `Deps` gana `Subscriber bus.Subscriber`. Cuando hay `Subscriber` y un `Aptitud` que envolver, `New` envuelve el puerto con la caché (misma instancia que invalida el consumidor, por construcción) y arranca el consumidor durable `operacion-catalogos`, suscrito a `catalogo.actualizado` sobre la superficie amplia; `Workers()` lo expone. Sin `Subscriber` (standalone sin bus) no se cachea: sin evento que invalide, una caché solo acumularía obsolescencia.
- `internal/catalogoscache/aptitud_test.go`, `internal/consumidores/catalogo_test.go`, `internal/consumidores/catalogo_e2e_test.go`, `workers_test.go` (tests): hit dentro del TTL / TTL como techo con broker caído / error no cacheado y propagado (DEC-055) / invalidación por evento / idempotencia P13; invalidación de **extremo a extremo** con `bus/memory` real; cableado del worker en `Module` con y sin `Subscriber`.

**Brecha / nota de contrato (C10):** el proto `CatalogoActualizado` (tracer.8) lleva `entidad` + `entidad_id` + `actualizado_en`, **sin campo `version`**. La versión viaja en el sobre de dominio (`bus.Sobre.Version`); este consumidor invalida de forma incondicional y no la necesita. Alinear el payload queda diferido a cuando el contrato agregue el campo.

**Contradicción reportada (no decidida por código):** el documento `docs/historias-tecnicas/E5-administracion-catalogos.md §E5-T5` dice que la aptitud **falla cerrado**; el ticket AI27-77 y DEC-055 dicen **informativa**. Se implementó **informativa** por decisión de Product (DEC-055, coherente con E5-T1/T2). El documento de historias técnicas quedó desactualizado y debe reconciliarse.

**Verificado:** `go build ./...`, `go vet ./...`, `go test ./...` (todo verde, incluidos los tests de `Workers()` previos, intactos).

**Archivos modificados:**
- `operacion.go`
- `workers_test.go`
- `internal/catalogoscache/aptitud.go` (nuevo)
- `internal/catalogoscache/aptitud_test.go` (nuevo)
- `internal/consumidores/catalogo.go` (nuevo)
- `internal/consumidores/catalogo_test.go` (nuevo)
- `internal/consumidores/catalogo_e2e_test.go` (nuevo)

## 2026:10:07 17:15

**Resumen:** Se amplía `operacion.Consultas()` a los tres métodos que `bff` consume (`CrearViaje`, `ListarViajes`, `ObtenerViaje`), con la forma exacta de `bff.OperacionPort`. Brecha detectada al cablear core: la fachada entrante solo exponía `ObtenerViaje` y devolvía `*Viaje`, no `*ViajeDetalle`; sin los tres, core no podía construir el adaptador en proceso `bff → operacion`.

**Para quién:** para `core`, que compone bff sobre operacion. Antes `bff.New(...)` no se podía cablear porque su puerto `Operacion` no tenía implementación posible.

**Cambios:**
- `operacion.go`: `ConsultasAPI` pasa de un método a tres (`CrearViaje`, `ListarViajes`, `ObtenerViaje`). `ObtenerViaje` ahora devuelve `*ViajeDetalle` (no `*Viaje`): bff compone los hitos a partir del detalle, que incluye las condiciones activas (E2-T2/E2-T3). Las escrituras internas (transición, asignación) no entran: no son arista de bff (bff.md (f)).
- `consultas_test.go` (nuevo): prueba de conformación de superficie. Replica la firma de `bff.OperacionPort` (operacion no puede importar bff) y afirma que `ConsultasAPI` la satisface.

**Alcance / brecha reportada:** el servicio de dominio sigue siendo scaffold; los tres métodos devuelven "no implementado" en runtime. La fachada fija la forma tipada para que core cablee el adaptador con paridad I3, no entrega la lógica de viajes.

**Verificado:** gofmt limpio, `go vet ./...`, `go build ./...`, `go test ./...`, build `-mod=readonly`, arch-test@v0.1.1 OK.

**Archivos modificados:**
- `operacion.go`
- `consultas_test.go` (nuevo)

## 2026:10:07 16:25

**Resumen:** Se exporta `PresupuestoRevocacion` para la arista restringida a `identidad`, por la misma obligación I3 que `PresupuestoAptitud`. Brecha detectada al cablear el prompt 09: el adaptador EN PROCESO de `pf-svc-core` para la revocación no tenía de dónde leer el presupuesto, porque el consumidor no lo exportaba.

**Cambios:**
- `ports.go`: se agrega `PresupuestoRevocacion` (`2 * time.Second`) exportado, junto al puerto `Revocacion`. `core` lo lee de aquí para el adaptador en proceso de revocación, en vez de duplicar el número (I3, ADR 17). El comentario distingue este presupuesto (una llamada puntual) del umbral de frescura del canal (15 s / 30 s, ADR 26), que es independiente.

**Verificado:** gofmt limpio, `go vet ./...`, `go build ./...`, `go test ./...`, build `-mod=readonly`, arch-test@v0.1.1 OK.

**Archivos modificados:**
- `ports.go`

## 2026:10:05 (hora local)

**Resumen:** Se exporta el presupuesto de tiempo de la arista a `catalogos` para que el adaptador EN PROCESO de `pf-svc-core` pueda leer el mismo valor (I3). Brecha detectada al cablear el prompt 09: el presupuesto estaba como `const` privada dentro de `adapters/http`, inalcanzable para `core`.

**Cambios:**
- `ports.go`: se agrega `PresupuestoAptitud` (`2 * time.Second`) exportado, junto al puerto `AptitudRecursos` que gobierna. La obligación I3 (ADR 17) exige que el adaptador HTTP de este repo y el en proceso de `core` usen EXACTAMENTE el mismo valor; `core` lo lee de aquí en vez de duplicar el número.
- `adapters/http/aptitud_recursos.go`: se elimina la `const presupuestoAptitud` privada y los dos `context.WithTimeout` pasan a usar `operacion.PresupuestoAptitud`. Se retira el import de `time`, ya innecesario en el adaptador HTTP.

**Verificado:** gofmt limpio, `go vet ./...`, `go build ./...`, `go test ./...`, build `-mod=readonly`, arch-test OK.

**Archivos modificados:**
- `ports.go`
- `adapters/http/aptitud_recursos.go`

## 2026:10:06 15:30

**Resumen:** Se aplicaron al scaffold las decisiones de arquitectura ADR 33/34/35 (DEC-066/067/068), que resuelven tres huecos que el esqueleto inicial había dejado abiertos.

**Cambios:**
- **ADR 33 (roster en esquema propio):** se agregó `migrations/roster/0001_init.sql` (esquema `roster` + rol `op_roster`, tablas `unidad` y `unidad_custodio`). `operacion` pasa a **cinco** esquemas/roles. La excepción de GRANT de `op_asignacion` se extiende con `USAGE` de solo lectura sobre `roster`. Se agregó el pool `Roster` a `Pools`, el quinto pool y check de readiness a `cmd/api`, y se amplió el embed de `migrations.go` con el orden de aplicación (roster antes de asignacion).
- **ADR 34 (idempotencia en memoria):** `internal/consumidores/doc.go` ahora declara dedup por `evento_id` en memoria y elimina el pendiente de ubicación de `inbox_evento`; no se crea tabla de inbox para los dos consumidores (efecto en memoria, ADR 26).
- **ADR 35 (outbox único por agregado):** se eliminó `asignacion.outbox`; todos los `viaje.*` salen de `viajes.outbox`, donde `asignacion` escribe `viaje.asignado`/`reasignado` vía la excepción de GRANT. Se actualizaron los comentarios de las migraciones de `viajes` y `asignacion`, el doc de `internal/asignacion` y el `Workers()` de `operacion.go`.
- README: "cuatro esquemas" → cinco; sección de decisiones resueltas (antes "pendientes que no se infieren"); `DB_ROSTER_URL` en el ejemplo standalone.

**Verificado:** build `-mod=readonly`, vet, test (interceptor P3), build de `cmd/api`, arch-test OK, gofmt limpio.

**Nota:** los ADR y las entradas de decision-log (DEC-066/067/068) viven en el workspace de producto (`docs/adr/0033..0035`, `.kiro/steering/decision-log.md`), no en este repo.

**Archivos modificados:**
- `migrations/roster/0001_init.sql` (nuevo)
- `migrations/asignacion/0001_init.sql`
- `migrations/viajes/0001_init.sql`
- `migrations/migrations.go`
- `operacion.go`
- `internal/consumidores/doc.go`
- `internal/asignacion/doc.go`
- `cmd/api/main.go`
- `README.md`

## 2026:10:06 14:01

**Resumen:** Esqueleto inicial del módulo `operacion` (prompt 04, ADR 31, DEC-050). Dominio; cuatro esquemas bajo una transacción. Sin lógica de negocio real todavía. Contrato fijado en `pf-pkg-contracts` v0.1.0-tracer.3.

**Cambios:**
- `go.mod`: requiere solo `pf-pkg-contracts/api-client-go` v0.1.0-tracer.3 y `pf-pkg-platform-go` v0.1.0; ningún otro `pf-svc-*`. Sin `replace` ni `go.work`.
- `operacion.go`: superficie pública `New(Pools, Deps)`, `Routes`, `Workers`, `Consultas()`. `Pools` con los cuatro pools por rol; `Deps` con `AptitudRecursos` y `Revocacion`.
- `ports.go`: puerto `AptitudRecursos` hacia catalogos (solo las operaciones que existen en el contrato: aptitud de unidad y custodio, informativa, DEC-055) y puerto `Revocacion` restringido a identidad (DEC-051).
- Migraciones: cuatro esquemas y roles (`op_viajes`, `op_asignacion`, `op_bitacora`, `op_auditoria`); la **única excepción de GRANT cruzado** (`op_asignacion` sobre los cuatro esquemas, nunca catalogos); `btree_gist` y `reserva_recurso` con `EXCLUDE USING gist (unidad_id, ventana)`; `viaje.estado` restringido por constraint a los 13 tokens del enum; `condicion_viaje` con índice único parcial (INCIDENCIA/SIN_SENAL como condiciones, no estados); outbox en `viajes` y `asignacion` (no en bitacora/auditoria).
- `internal/auditoria/interceptor.go`: auditoría como interceptor en la misma transacción (no cliente omitible), con prueba de que un fallo antes del commit no deja ni cambio ni registro (P3).
- `adapters/http/aptitud_recursos.go`: adaptador HTTP del puerto con el cliente generado de catalogos; el adaptador en proceso vive en `pf-svc-core`.
- Sub-módulos `viajes`, `asignacion`, `bitacora`, `consumidores` (doc + alcance).
- `cmd/api`: arranque standalone (I5) con los cuatro pools y health/ready.
- CI y `tools.go` (fija deps del arch-test, que verifica las aristas `operacion → catalogos` e `operacion → identidad`).
- Brechas de contrato reportadas (ola 1 / tracer.4): C3 (átomo vehículo aún `Unidad` de ADR 24), asignación por escolta, eventos con escolta, `obtenerUnidad/obtenerCustodio` por id, `PATCH /eta` sin `stale`, y la guarda/409 por aptitud que NO se implementan (DEC-055).

**Pendientes que no se infieren:** ubicación del roster de unidades y del `inbox_evento` de cada consumidor (ADR 29/31 no las fijan); orden por agregado con dos outbox (observación para el Lead).

**Archivos modificados:**
- `go.mod`, `go.sum`, `.gitignore`
- `operacion.go`, `ports.go`, `errores.go`, `tools.go`, `README.md`
- `migrations/viajes/0001_init.sql`
- `migrations/asignacion/0001_init.sql`
- `migrations/bitacora/0001_init.sql`
- `migrations/auditoria/0001_init.sql`
- `migrations/migrations.go`
- `internal/auditoria/interceptor.go`, `internal/auditoria/interceptor_test.go`
- `internal/viajes/doc.go`, `internal/asignacion/doc.go`, `internal/bitacora/doc.go`, `internal/consumidores/doc.go`
- `adapters/http/aptitud_recursos.go`
- `cmd/api/main.go`
- `.github/workflows/ci.yml`

## 2026:10:06 12:55

**Resumen:** Se agregó la plantilla de Pull Request del backend, adaptada al stack Go de AI27.

**Cambios:**
- Se agregó `.github/pull_request_template.md` con secciones de invariantes de arquitectura (I1-I5, contrato primero), base de datos, eventos/bus, verificación (go build/vet/test, arch-test, gofmt), CHANGELOG y checklist de convenciones.

**Archivos modificados:**
- `.github/pull_request_template.md`

## 2026:10:06 12:48

**Resumen:** Se agregó la regla de steering de CHANGELOG y este archivo, antes de que el repo tenga código, para que la convención esté vigente desde el inicio.

**Cambios:**
- Se agregó `.kiro/steering/changelog.md` (`inclusion: always`): toda modificación del repo debe registrarse aquí, en español, con marca de tiempo, resumen, cambios y archivos modificados.
- Se creó `CHANGELOG.md`.

**Archivos modificados:**
- `.kiro/steering/changelog.md`
- `CHANGELOG.md`
