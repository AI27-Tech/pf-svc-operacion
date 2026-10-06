// Package migrations expone las migraciones SQL de operacion como un embed.FS.
// Son CINCO esquemas, cada uno con su rol dueño; pf-svc-core y cmd/api aplican
// cada esquema con su rol (viajes→op_viajes, asignacion→op_asignacion,
// bitacora→op_bitacora, auditoria→op_auditoria, roster→op_roster, ADR 33).
// platform-go no administra schemas: cada módulo trae los suyos.
//
// El orden de aplicación importa: `roster` debe existir antes que `asignacion`
// (asignacion recibe USAGE de lectura sobre roster, ADR 33), y `asignacion`
// después de viajes/bitacora/auditoria (su excepción de GRANT los referencia).
// Orden sugerido: viajes, bitacora, auditoria, roster, asignacion.
package migrations

import "embed"

// FS contiene los archivos .sql por esquema.
//
//go:embed viajes/*.sql asignacion/*.sql bitacora/*.sql auditoria/*.sql roster/*.sql
var FS embed.FS
