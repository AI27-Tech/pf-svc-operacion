# CHANGELOG

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
