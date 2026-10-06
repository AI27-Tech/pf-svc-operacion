// Package viajes es la ÚNICA autoridad de transición del viaje (operacion.md
// (a), P14): crear, transicionar con causa, obtener y listar por alcance. No
// existe escritura directa de `estado`; toda mutación pasa por
// estados.PuedeTransicionar (del paquete generado) más las guardas que el YAML
// nombra (E1-T3). La guarda cierre_anticipado_por_incidencia (EN_TRANSITO →
// CERRADO, DEC-020 #6) vive aquí; el YAML solo declara la arista.
//
// Sub-módulo interno de operacion. Expone su superficie a sus hermanos por su
// port.go (pendiente de implementar); su worker publica viajes.outbox con el
// rol op_viajes. INCIDENCIA y SIN_SENAL son condiciones ortogonales, nunca
// valores de estado (P15).
//
// Scaffolding: sin casos de uso todavía. La máquina de estados se importa de
// pf-pkg-contracts/packages/api-client-go/estados; no se declara aquí.
package viajes
