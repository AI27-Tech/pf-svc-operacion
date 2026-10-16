// Package consumidores aloja los consumidores durables de operacion (ADR 29,
// DEC-057). Aquí vive el de catalogo.actualizado: escucha las mutaciones de
// catálogo que publica catalogos e invalida la entrada correspondiente de la
// caché en proceso de aptitud (internal/catalogoscache), para que la próxima
// lectura vaya a catalogos y recachee.
//
// Idempotencia EN MEMORIA (ADR 34, DEC-067): el efecto del consumidor es en
// memoria (invalidar una entrada de caché), así que la deduplicación por
// evento_id también es en memoria —un conjunto de evento_id vistos—, sin tabla
// inbox_evento. No hace falta durabilidad transaccional: si el proceso reinicia,
// la caché arranca vacía y no hay nada que invalidar; reprocesar un evento tras
// el reinicio solo volvería a invalidar algo ya ausente (no-op). Reentregar el
// mismo evento dos veces invalida como mucho una vez, y una invalidación
// repetida no tiene efecto adicional (Property P13).
package consumidores

import (
	"context"
	"encoding/json"
	"sync"

	catalogos "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/catalogos"
	"github.com/AI27-Tech/pf-pkg-platform-go/bus"
	"github.com/google/uuid"
)

// TipoCatalogoActualizado es el único tipo que este consumidor procesa. Coincide
// con el tipo que catalogos registra y publica (contrato: registro.tipos.yaml).
const TipoCatalogoActualizado = "catalogo.actualizado"

// Invalidador es lo que el consumidor necesita de la caché de aptitud: borrar la
// entrada de una entidad+recurso. Lo satisface *catalogoscache.CacheAptitud. Se
// declara aquí (en el consumidor) para que el consumidor dependa de un puerto
// estrecho y no del tipo concreto de la caché.
type Invalidador interface {
	Invalidar(entidad string, recursoID catalogos.Id)
}

// payloadCatalogoActualizado es la vista del cuerpo JSON del evento. catalogos
// publica solo metadato —tipo + identidad, nunca el registro completo—: `entidad`
// (p. ej. "vehiculo", "custodio") y `entidad_id`. El campo `tipo` (subtipo de
// valor_catalogo) existe en el payload pero operacion no lo usa: solo cachea
// aptitud de vehículo y custodio, que son entidades de una sola tabla. La versión
// NO viaja en el payload (brecha C10 del contrato): llega en bus.Sobre.Version;
// este consumidor no la necesita, porque invalidar es incondicional (no compara
// versiones), pero se documenta de dónde saldría si hiciera falta.
type payloadCatalogoActualizado struct {
	Entidad   string `json:"entidad"`
	EntidadID string `json:"entidad_id"`
}

// ConsumidorCatalogo invalida la caché de aptitud ante catalogo.actualizado, con
// deduplicación en memoria por evento_id. Es seguro para uso concurrente.
type ConsumidorCatalogo struct {
	cache Invalidador

	mu     sync.Mutex
	vistos map[string]struct{} // evento_id ya procesados (dedup en memoria)
}

// NuevoConsumidorCatalogo construye el consumidor sobre la caché a invalidar.
func NuevoConsumidorCatalogo(cache Invalidador) *ConsumidorCatalogo {
	return &ConsumidorCatalogo{
		cache:  cache,
		vistos: make(map[string]struct{}),
	}
}

// Procesar maneja un sobre entregado por el bus. Deduplica por evento_id: un
// evento ya visto es un no-op (P13). Un sobre de otro tipo se ignora (acuse sin
// efecto): el filtro del Subscribe ya restringe a catalogo.actualizado, pero el
// consumidor es defensivo y no asume el filtro. Un payload ilegible se acusa sin
// invalidar: no es reintentable (reintentarlo daría el mismo JSON inválido) y no
// debe bloquear la cola; el peor efecto de NO invalidar es servir aptitud algo
// obsoleta hasta que venza el TTL, lo cual es tolerable porque la aptitud es
// informativa (DEC-055).
func (c *ConsumidorCatalogo) Procesar(ctx context.Context, s bus.Sobre) error {
	if s.Tipo != TipoCatalogoActualizado {
		return nil // no es lo nuestro; acuse sin efecto
	}

	// Dedup en memoria por evento_id (ADR 34/DEC-067). Si ya se vio, no se
	// reprocesa: invalidar de nuevo no cambiaría nada, pero cortamos aquí para
	// que el conteo de invalidaciones observable sea exacto (P13).
	if s.EventoID != "" {
		c.mu.Lock()
		_, ya := c.vistos[s.EventoID]
		if !ya {
			c.vistos[s.EventoID] = struct{}{}
		}
		c.mu.Unlock()
		if ya {
			return nil
		}
	}

	var p payloadCatalogoActualizado
	if err := json.Unmarshal(s.Payload, &p); err != nil {
		// Payload inválido: acuse sin invalidar (no reintentable, no bloquea).
		return nil
	}
	if p.Entidad == "" || p.EntidadID == "" {
		return nil // metadato incompleto: nada que invalidar
	}

	// El id del recurso es un UUID (catalogos.Id). Si no parsea, el evento está
	// mal formado: acuse sin invalidar, mismo criterio que un payload inválido.
	id, err := uuid.Parse(p.EntidadID)
	if err != nil {
		return nil
	}

	// Invalidación incondicional: borra la entrada de esa entidad+recurso. Para
	// entidades que operacion no cachea (cliente, contrato, sla, valor_catalogo)
	// es un no-op dentro de la caché. No se compara versión: una invalidación de
	// más solo provoca una relectura, nunca un dato incorrecto.
	c.cache.Invalidar(p.Entidad, id)
	return nil
}
