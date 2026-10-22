// Consumidor durable de emergencia.nota_agregada (E7, tracer.17; DEC-100 punto
// 1). mensajeria publica una nota de emergencia; operacion la proyecta al
// histórico/bitácora del viaje. A diferencia del consumidor de catálogo (efecto
// en memoria, dedup en memoria), aquí el efecto es un INSERT en bitácora, así que
// la idempotencia es DURABLE: la da el INSERT idempotente por evento_id del
// servicio de bitácora (RegistrarDesdeEvento), no un conjunto en memoria. Por eso
// este consumidor no deduplica por su cuenta: reentregar el mismo evento es un
// no-op en la base.
//
// Evento RESTRINGIDO: viaja por el tema restringido (ADR 28). El consumidor no
// cambia por ello; la sensibilidad es propiedad del transporte y se declara en el
// Subscribe (operacion.go), no aquí.
package consumidores

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/AI27-Tech/pf-pkg-platform-go/bus"
)

// TipoEmergenciaNotaAgregada es el único tipo que este consumidor procesa.
// Coincide con el nombre serializado del registro del contrato.
const TipoEmergenciaNotaAgregada = "emergencia.nota_agregada"

// Historico es lo que el consumidor necesita de la bitácora: proyectar una nota
// al histórico del viaje de forma idempotente por evento. Lo satisface
// *bitacora.Servicio (RegistrarDesdeEvento). Puerto estrecho declarado por el
// consumidor.
type Historico interface {
	RegistrarDesdeEvento(ctx context.Context, viajeID, eventoID, texto, autorID string) (bool, error)
}

// payloadEmergenciaNotaAgregada es la vista del cuerpo JSON del evento (lo que
// mensajeria publica). El agregado del evento es caso_emergencia_id, pero el
// payload trae viaje_id para que operacion, que no conoce el caso, pueda mapear
// la nota al viaje.
type payloadEmergenciaNotaAgregada struct {
	CasoEmergenciaID string `json:"caso_emergencia_id"`
	NotaID           string `json:"nota_id"`
	ViajeID          string `json:"viaje_id"`
	AudienciaAutor   string `json:"audiencia_autor"`
	Cuerpo           string `json:"cuerpo"`
	EsResolucion     bool   `json:"es_resolucion"`
}

// ConsumidorEmergenciaNota proyecta cada nota de emergencia al histórico del
// viaje. La idempotencia es durable (por nota_id como evento_id), así que es
// seguro reprocesar.
type ConsumidorEmergenciaNota struct {
	hist Historico
}

// NuevoConsumidorEmergenciaNota construye el consumidor sobre el servicio de
// histórico/bitácora.
func NuevoConsumidorEmergenciaNota(hist Historico) *ConsumidorEmergenciaNota {
	return &ConsumidorEmergenciaNota{hist: hist}
}

// Procesar maneja un sobre entregado por el bus. Un tipo distinto se ignora
// (acuse sin efecto): el filtro del Subscribe ya restringe, pero el consumidor es
// defensivo. Un payload ilegible o sin viaje_id/cuerpo se acusa sin escribir: no
// es reintentable. La escritura es idempotente por nota_id, así que una reentrega
// no duplica la entrada del histórico.
func (c *ConsumidorEmergenciaNota) Procesar(ctx context.Context, s bus.Sobre) error {
	if s.Tipo != TipoEmergenciaNotaAgregada {
		return nil // no es lo nuestro; acuse sin efecto
	}

	var p payloadEmergenciaNotaAgregada
	if err := json.Unmarshal(s.Payload, &p); err != nil {
		return nil // payload inválido: acuse sin escribir (no reintentable)
	}
	// La clave de idempotencia durable es la nota; sin ella no se puede deduplicar
	// y no se escribe. Sin viaje_id no hay a qué histórico copiar. Sin cuerpo no
	// hay nota que registrar.
	eventoID := p.NotaID
	if eventoID == "" {
		eventoID = s.EventoID // respaldo: el id del sobre, si la nota no viajó
	}
	if eventoID == "" || p.ViajeID == "" || p.Cuerpo == "" {
		return nil // metadato incompleto: nada que proyectar
	}

	// El autor real de la nota no viaja en el evento restringido (queda en la
	// auditoría local de mensajeria, como en conversación); el histórico registra
	// la nota sin actor nominado. Un error de BD sí se propaga: es reintentable y
	// el INSERT idempotente evita el duplicado al reintentar.
	if _, err := c.hist.RegistrarDesdeEvento(ctx, p.ViajeID, eventoID, p.Cuerpo, ""); err != nil {
		return fmt.Errorf("consumidores: proyectar nota de emergencia al histórico: %w", err)
	}
	return nil
}
