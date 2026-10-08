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

import (
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

// FS contiene los archivos .sql por esquema.
//
//go:embed viajes/*.sql asignacion/*.sql bitacora/*.sql auditoria/*.sql roster/*.sql
var FS embed.FS

// Orden es la secuencia de esquemas en que se aplican las migraciones. El orden
// importa por las dependencias de GRANT entre esquemas (ADR 33, §7.1):
//   - roster antes que asignacion (asignacion recibe SELECT sobre roster),
//   - viajes, bitacora y auditoria antes que asignacion (su excepción de GRANT
//     referencia esos esquemas y sus tablas).
//
// La aplicación es EN DOS FASES por versión de archivo: primero todos los 0001
// (estructura: esquemas, roles, tablas), después todos los 0002 (grants por
// tabla). Motivo: los grants de la excepción de op_asignacion viven en
// viajes/auditoria/roster/0002, pero el rol op_asignacion lo crea
// asignacion/0001, el último de Orden. Aplicar esquema por esquema de principio
// a fin haría que viajes/0002 otorgara a un rol inexistente. El aplicador de
// producción debe respetar las dos fases.
//
// Es la fuente única del orden; la consumen el aplicador de core, cmd/api y las
// pruebas de integración, para no repetir la secuencia en cada llamador.
var Orden = []string{"viajes", "bitacora", "auditoria", "roster", "asignacion"}

// RolPorEsquema mapea cada esquema a su rol dueño. El aplicador de producción
// aplica las migraciones de cada esquema con la conexión de ESE rol (I1); las
// pruebas de integración lo usan para saber qué rol prueba cada esquema.
var RolPorEsquema = map[string]string{
	"viajes":     "op_viajes",
	"bitacora":   "op_bitacora",
	"auditoria":  "op_auditoria",
	"roster":     "op_roster",
	"asignacion": "op_asignacion",
}

// ArchivosDe devuelve los .sql de un esquema, ordenados por nombre (0001 antes
// que 0002). Falla si el esquema no existe en el FS.
func ArchivosDe(esquema string) ([]string, error) {
	entradas, err := fs.ReadDir(FS, esquema)
	if err != nil {
		return nil, fmt.Errorf("leer esquema %q: %w", esquema, err)
	}
	nombres := make([]string, 0, len(entradas))
	for _, e := range entradas {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			nombres = append(nombres, esquema+"/"+e.Name())
		}
	}
	sort.Strings(nombres)
	return nombres, nil
}
