package viajes_test

import (
	"context"
	"testing"
	"time"

	operclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/operacion"
	platformerrors "github.com/AI27-Tech/pf-pkg-platform-go/errors"
	"github.com/AI27-Tech/pf-svc-operacion/internal/dbtest"
	"github.com/google/uuid"
)

// crearAprobado crea un viaje por Operación: nace en APROBADO (ADR 38).
func crearAprobado(t *testing.T, ctx context.Context, svc interface {
	Crear(context.Context, operclient.CrearViajeEntrada, string) (*operclient.Viaje, error)
}) *operclient.Viaje {
	t.Helper()
	cli := uuid.New()
	f := time.Now().Add(24 * time.Hour).UTC()
	v, err := svc.Crear(ctx, operclient.CrearViajeEntrada{
		ClienteId: &cli, Origen: "Origen", Destinos: []operclient.Destino{{Nombre: "Destino"}}, FechaHoraServicio: &f,
	}, "jefe_operativo")
	if err != nil {
		t.Fatalf("crear APROBADO: %v", err)
	}
	if v.Estado != operclient.APROBADO {
		t.Fatalf("estado tras crear: got %q, want APROBADO", v.Estado)
	}
	return v
}

func TestTransicionar(t *testing.T) {
	ctx := context.Background()
	svc := prepara(t, ctx)
	v := crearAprobado(t, ctx, svc)

	// --- Operación cancela un viaje APROBADO: APROBADO -> CANCELADO_POR_CLIENTE ---
	vc := crearAprobado(t, ctx, svc)
	out, err := transicionarInterno(svc, ctx, vc.Id, operclient.TransicionEntrada{AEstado: operclient.CANCELADOPORCLIENTE}, "jefe_operativo")
	if err != nil {
		t.Fatalf("transición APROBADO->CANCELADO_POR_CLIENTE: %v", err)
	}
	if out.Estado != operclient.CANCELADOPORCLIENTE {
		t.Errorf("estado tras cancelar: got %q, want CANCELADO_POR_CLIENTE", out.Estado)
	}
	if n := contarEventos(t, ctx, "viaje.estado_cambiado", vc.Id.String()); n != 1 {
		t.Errorf("viaje.estado_cambiado en outbox: got %d, want 1", n)
	}

	// --- Arista inexistente: APROBADO -> EN_DESTINO (no hay arista) -> error ---
	if _, err := transicionarInterno(svc, ctx, v.Id, operclient.TransicionEntrada{AEstado: operclient.ENDESTINO}, "jefe_operativo"); err == nil {
		t.Error("transición por arista inexistente debió fallar")
	}

	// --- Guarda diferida: APROBADO -> ASIGNADO (escolta, Slice 3) -> error ---
	if _, err := transicionarInterno(svc, ctx, v.Id, operclient.TransicionEntrada{AEstado: operclient.ASIGNADO}, "jefe_operativo"); err == nil {
		t.Error("transición diferida APROBADO->ASIGNADO debió rechazarse")
	}

	// --- Un estado terminal no admite salida ---
	if _, err := transicionarInterno(svc, ctx, vc.Id, operclient.TransicionEntrada{AEstado: operclient.ASIGNADO}, "jefe_operativo"); err == nil {
		t.Error("salir de CANCELADO_POR_CLIENTE debió fallar")
	}

	// --- Transición a un viaje inexistente -> NoEncontrado ---
	if _, err := transicionarInterno(svc, ctx, uuid.New(), operclient.TransicionEntrada{AEstado: operclient.CANCELADOPORCLIENTE}, "jefe_operativo"); err == nil {
		t.Error("transición de viaje inexistente debió fallar")
	}
}

// --- Transición de audiencia Cliente (E9-T7 / DEC-111/112) ------------------
// El cliente solo puede cancelar SU propio viaje, con motivo
// obligatorio. tenant = cliente (DEC-112): el aislamiento compara el tenant del
// actor contra el cliente_id del viaje.

// TestClienteCancelaSuViaje — un cliente cancela su propio viaje con motivo:
// APROBADO -> CANCELADO_POR_CLIENTE. Caso feliz.
func TestClienteCancelaSuViaje(t *testing.T) {
	ctx := context.Background()
	svc := prepara(t, ctx)

	v := crearAprobado(t, ctx, svc)
	motivo := "ya no necesito el servicio"
	out, err := svc.Transicionar(ctx, v.Id,
		operclient.TransicionEntrada{AEstado: operclient.CANCELADOPORCLIENTE, Causa: &motivo},
		"cliente", "Cliente", v.ClienteId.String())
	if err != nil {
		t.Fatalf("cliente cancela su viaje: %v", err)
	}
	if out.Estado != operclient.CANCELADOPORCLIENTE {
		t.Errorf("estado tras cancelar: got %q, want CANCELADO_POR_CLIENTE", out.Estado)
	}
	if n := contarEventos(t, ctx, "viaje.estado_cambiado", v.Id.String()); n != 1 {
		t.Errorf("viaje.estado_cambiado en outbox: got %d, want 1", n)
	}
}

// TestTransicionarDevuelvePenalizacion — la respuesta de Transicionar lleva
// cancelacion_con_penalizacion tomada de nacio_de_solicitud (DEC-148 #14): un
// viaje de solicitud pagada que el cliente cancela devuelve true.
func TestTransicionarDevuelvePenalizacion(t *testing.T) {
	ctx := context.Background()
	svc := prepara(t, ctx)

	v := crearAprobado(t, ctx, svc)
	admin, cerrar := dbtest.AdminConn(t, ctx)
	dbtest.Exec(t, ctx, admin, "UPDATE viajes.viaje SET nacio_de_solicitud = true WHERE id = '"+v.Id.String()+"'")
	cerrar()

	motivo := "ya no necesito el servicio"
	out, err := svc.Transicionar(ctx, v.Id,
		operclient.TransicionEntrada{AEstado: operclient.CANCELADOPORCLIENTE, Causa: &motivo},
		"cliente", "Cliente", v.ClienteId.String())
	if err != nil {
		t.Fatalf("cliente cancela: %v", err)
	}
	if out.CancelacionConPenalizacion == nil || !*out.CancelacionConPenalizacion {
		t.Errorf("cancelacion_con_penalizacion: got %v, want true", out.CancelacionConPenalizacion)
	}
}

// TestClienteCancelaSinMotivoRechazado — cancelar sin motivo se rechaza (400):
// el motivo es obligatorio para el cliente (DEC-111, supera DEC-020 #7).
func TestClienteCancelaSinMotivoRechazado(t *testing.T) {
	ctx := context.Background()
	svc := prepara(t, ctx)

	v := crearAprobado(t, ctx, svc)
	if _, err := svc.Transicionar(ctx, v.Id,
		operclient.TransicionEntrada{AEstado: operclient.CANCELADOPORCLIENTE},
		"cliente", "Cliente", v.ClienteId.String()); err == nil {
		t.Error("cancelar sin motivo debió rechazarse")
	}
	if n := contarEventos(t, ctx, "viaje.estado_cambiado", v.Id.String()); n != 0 {
		t.Errorf("no debió publicarse transición: got %d, want 0", n)
	}
}

// TestClienteCancelaViajeAjenoNoEncontrado — un cliente intenta cancelar el
// viaje de OTRO cliente (su tenant no coincide con el cliente_id del viaje): se
// trata como inexistente (404), no 403, para no revelar que el viaje existe.
func TestClienteCancelaViajeAjenoNoEncontrado(t *testing.T) {
	ctx := context.Background()
	svc := prepara(t, ctx)

	v := crearAprobado(t, ctx, svc)
	motivo := "intento sobre viaje ajeno"
	otroTenant := uuid.New().String() // distinto del cliente_id del viaje
	_, err := svc.Transicionar(ctx, v.Id,
		operclient.TransicionEntrada{AEstado: operclient.CANCELADOPORCLIENTE, Causa: &motivo},
		"cliente", "Cliente", otroTenant)
	if err == nil {
		t.Fatal("cancelar un viaje ajeno debió fallar")
	}
	if cat := platformerrors.AsCategory(err); cat != platformerrors.CategoryNoEncontrado {
		t.Errorf("categoría de error: got %v, want NoEncontrado (404)", cat)
	}
	if n := contarEventos(t, ctx, "viaje.estado_cambiado", v.Id.String()); n != 0 {
		t.Errorf("no debió publicarse transición: got %d, want 0", n)
	}
}

// TestClienteAristaNoPermitidaRechazada — un cliente intenta una arista que no
// le corresponde (APROBADO, de operador): se rechaza por facultad aunque el
// viaje sea suyo y lleve motivo.
func TestClienteAristaNoPermitidaRechazada(t *testing.T) {
	ctx := context.Background()
	svc := prepara(t, ctx)

	v := crearAprobado(t, ctx, svc)
	motivo := "intento de arista de operador"
	if _, err := svc.Transicionar(ctx, v.Id,
		operclient.TransicionEntrada{AEstado: operclient.APROBADO, Causa: &motivo},
		"cliente", "Cliente", v.ClienteId.String()); platformerrors.AsCategory(err).HTTPStatus() != 403 {
		t.Errorf("un cliente no puede aprobar su viaje: got %v, want 403 (DEC-111 #3)", err)
	}
	if n := contarEventos(t, ctx, "viaje.estado_cambiado", v.Id.String()); n != 0 {
		t.Errorf("no debió publicarse transición: got %d, want 0", n)
	}
}

// TestClienteNoRechazaLaEscolta — el cliente ya no rechaza (DEC-148 #14): ni la
// arista retirada ni cualquier otro destino distinto de CANCELADO_POR_CLIENTE.
func TestClienteNoRechazaLaEscolta(t *testing.T) {
	ctx := context.Background()
	svc := prepara(t, ctx)

	v := crearAprobado(t, ctx, svc)
	motivo := "no me gusta la escolta"
	if _, err := svc.Transicionar(ctx, v.Id,
		operclient.TransicionEntrada{AEstado: operclient.ASIGNADO, Causa: &motivo},
		"cliente", "Cliente", v.ClienteId.String()); platformerrors.AsCategory(err).HTTPStatus() != 403 {
		t.Errorf("el cliente solo cancela: got %v, want 403", err)
	}
}
