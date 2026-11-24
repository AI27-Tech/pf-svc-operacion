// Package alcance es el alcance del monitoreo sobre los viajes (DEC-149,
// tracer.41): qué viajes ve cada rol interno y la proyección local de
// `alcance.cambiado` de identidad (sujeto → clientes efectivos) con la que se
// resuelve en servidor (DEC-053 #3).
//
// La regla (regla.go) es pura: la usan la guarda del detalle y sus subrecursos.
// La lista la aplica en SQL (listarViajes), para no filtrar páginas en
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
	// PorClientes son los viajes de los clientes efectivos del sujeto
	// (DEC-149 #5, #6, #18).
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
}

// Visible es la regla de PorClientes: el cliente del viaje está entre los
// clientes efectivos del sujeto. Sin clientes no ve nada. Las unidades no
// intervienen: en un préstamo el viaje lo ve quien tiene al cliente del viaje
// (DEC-149 #5, #18). Todo viaje nace en APROBADO (ADR 38), así que el estado ya
// no acota.
func Visible(clientes []uuid.UUID, h Hechos) bool {
	return slices.Contains(clientes, h.ClienteID)
}
