// Package alcance es el alcance del monitoreo sobre los viajes (DEC-149,
// tracer.41): qué viajes ve cada rol interno y la proyección local de
// `alcance.cambiado` de identidad (sujeto → clientes efectivos) con la que se
// resuelve en servidor (DEC-053 #3).
//
// La regla (regla.go) es pura: la usan la guarda del detalle y sus subrecursos.
// La lista la aplica en SQL (SQLPasoPorAprobado), para no filtrar páginas en
// memoria. La proyección (proyeccion.go) vive en Postgres, en el esquema viajes:
// el consumidor de alcance.cambiado es durable y comparte grupo entre réplicas,
// así que una proyección en memoria solo la vería actualizada la réplica que
// recibió el evento.
package alcance

import (
	"slices"

	"github.com/google/uuid"
)

// Acceso es la clase de operación sobre viajes que la regla distingue.
type Acceso int

const (
	// Lista es listarViajes.
	Lista Acceso = iota
	// Caso es lo que el bff compone con el token de Reacción para el detalle de
	// su emergencia: obtenerViaje y obtenerAsignacion (operacion.yaml, DEC-121 #2).
	Caso
	// Subrecurso es cualquier otra operación sobre /api/v1/viajes/{id}/...
	Subrecurso
)

// Ambito es lo que un rol ve de los viajes.
type Ambito int

const (
	// Prohibido es 403: el rol no tiene la acción (DEC-149 #8, #16).
	Prohibido Ambito = iota
	// Todos es el alcance global (jefe_operativo, administrador).
	Todos
	// PorClientes son los viajes de los clientes efectivos del sujeto que
	// pasaron por APROBADO (DEC-149 #5, #6, #18).
	PorClientes
)

// Para devuelve el ámbito de un rol INTERNO. La audiencia Cliente no pasa por
// aquí: se acota a su tenant (DEC-112). Un rol desconocido es Prohibido.
//
// Reacción ve sin filtro solo lo que el bff compone para su caso (Caso); el
// resto de los subrecursos le responde 403. Decisión técnica a revisar: el
// contrato fija solo listarViajes (403) y obtenerViaje (lee); se eligió lo
// conservador para lo demás (DEC-028: visibilidad solo del caso alertado).
func Para(rol string, a Acceso) Ambito {
	switch rol {
	case "jefe_operativo", "administrador":
		return Todos
	case "monitorista", "supervisor", "coordinador":
		return PorClientes
	case "reaccion":
		if a == Caso {
			return Todos
		}
	}
	return Prohibido
}

// Hechos es lo que la regla necesita de un viaje.
type Hechos struct {
	ClienteID uuid.UUID
	Estado    string
	// CanceladoDesdeSolicitado: el viaje llegó a CANCELADO sin pasar por
	// APROBADO (arista SOLICITADO → CANCELADO).
	CanceladoDesdeSolicitado bool
}

// PasoPorAprobado: toda salida de SOLICITADO que no es rechazo ni cancelación
// pasa por APROBADO (ADR 19), y un viaje creado por Operación nace en APROBADO
// (DEC-026). Por eso basta excluir SOLICITADO, RECHAZADO_POR_AI27 y la
// cancelación desde SOLICITADO. Gemelo de SQLPasoPorAprobado.
func PasoPorAprobado(h Hechos) bool {
	switch h.Estado {
	case "SOLICITADO", "RECHAZADO_POR_AI27":
		return false
	case "CANCELADO":
		return !h.CanceladoDesdeSolicitado
	}
	return true
}

// Visible es la regla de PorClientes: el cliente del viaje está entre los
// clientes efectivos del sujeto y el viaje pasó por APROBADO. Sin clientes no
// ve nada. Las unidades no intervienen: en un préstamo el viaje lo ve quien
// tiene al cliente del viaje (DEC-149 #5, #18).
func Visible(clientes []uuid.UUID, h Hechos) bool {
	return slices.Contains(clientes, h.ClienteID) && PasoPorAprobado(h)
}

// SQLPasoPorAprobado es PasoPorAprobado sobre `viajes.viaje v`, para el WHERE
// de listarViajes. Mantenerlos iguales; los cubre la misma prueba de
// integración (lista y detalle sobre el mismo juego de viajes).
const SQLPasoPorAprobado = `v.estado NOT IN ('SOLICITADO', 'RECHAZADO_POR_AI27')
	AND NOT (v.estado = 'CANCELADO' AND EXISTS (
	    SELECT 1 FROM viajes.transicion_viaje t
	     WHERE t.viaje_id = v.id AND t.de_estado = 'SOLICITADO' AND t.a_estado = 'CANCELADO'))`
