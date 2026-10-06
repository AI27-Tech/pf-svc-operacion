// Package auditoria es el INTERCEPTOR de auditoría probatoria, no una
// dependencia inyectable (operacion.md (d), P3). Se estructura como una función
// que envuelve la escritura de viajes, asignacion y bitacora y recibe la
// transacción ya abierta: escribe el registro en la MISMA tx que el cambio. Así
// no existe el camino en que el cambio se compromete y el registro no, ni una
// ruta de código que pueda omitir la auditoría llamándola condicionalmente.
//
// Ninguna ruta llama a auditoria por HTTP. No publica eventos.
package auditoria

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Registro es lo que el interceptor persiste junto a cada cambio de dominio.
type Registro struct {
	ViajeID string
	Modulo  string // "viajes" | "asignacion" | "bitacora"
	Accion  string
	ActorID string
	Detalle []byte // JSON opcional
}

// Interceptar ejecuta `cambio` y, en la MISMA transacción, escribe el registro
// de auditoría. Si cualquiera de los dos falla, el llamador hace rollback y no
// queda ni el cambio ni el registro (P3). El llamador abre y commitea la tx;
// este interceptor nunca abre conexiones ni decide el commit.
//
// La forma (fn que envuelve, no cliente) es deliberada: un caso de uso no puede
// escribir el cambio sin pasar por aquí, porque la escritura del cambio vive
// DENTRO de `cambio` y el registro se encadena a la misma tx sin punto de salida.
func Interceptar(ctx context.Context, tx pgx.Tx, reg Registro, cambio func(context.Context, pgx.Tx) error) error {
	if err := cambio(ctx, tx); err != nil {
		return err
	}
	if reg.Modulo == "" || reg.Accion == "" {
		return fmt.Errorf("auditoria: modulo y accion son obligatorios para el registro probatorio")
	}
	_, err := tx.Exec(ctx,
		`INSERT INTO auditoria.registro_auditoria (viaje_id, modulo, accion, actor_id, detalle)
		 VALUES ($1, $2, $3, $4, $5)`,
		nullsiVacio(reg.ViajeID), reg.Modulo, reg.Accion, nullsiVacio(reg.ActorID), reg.Detalle,
	)
	if err != nil {
		return fmt.Errorf("auditoria: escribir registro: %w", err)
	}
	return nil
}

func nullsiVacio(s string) any {
	if s == "" {
		return nil
	}
	return s
}
