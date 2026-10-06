// Package migrations expone las migraciones SQL de operacion como un embed.FS.
// Son CUATRO esquemas, cada uno con su rol dueño; pf-svc-core y cmd/api aplican
// cada esquema con su rol (viajes→op_viajes, asignacion→op_asignacion,
// bitacora→op_bitacora, auditoria→op_auditoria). platform-go no administra
// schemas: cada módulo trae los suyos.
//
// El orden de aplicación importa: `asignacion` depende de que los esquemas
// viajes, bitacora y auditoria ya existan (el GRANT de excepción referencia
// esos esquemas), así que `asignacion` se aplica después de los otros tres.
package migrations

import "embed"

// FS contiene los archivos .sql por esquema.
//
//go:embed viajes/*.sql asignacion/*.sql bitacora/*.sql auditoria/*.sql
var FS embed.FS
