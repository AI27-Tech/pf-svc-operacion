package viajes_test

import (
	"context"
	"testing"
	"time"

	operclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/operacion"
	platformerrors "github.com/AI27-Tech/pf-pkg-platform-go/errors"
	"github.com/google/uuid"
)

// crearSolicitado crea un viaje en estado SOLICITADO (rol cliente). Si conFecha
// es true, fija fecha_hora_servicio (necesaria para la guarda de aprobación).
func crearSolicitado(t *testing.T, ctx context.Context, svc interface {
	Crear(context.Context, operclient.CrearViajeEntrada, string, string) (*operclient.Viaje, error)
}, conFecha bool) *operclient.Viaje {
	t.Helper()
	cli := uuid.New()
	in := operclient.CrearViajeEntrada{
		ClienteId: &cli, Origen: "Origen", Destinos: []operclient.Destino{{Nombre: "Destino"}},
	}
	if conFecha {
		f := time.Now().Add(24 * time.Hour).UTC()
		in.FechaHoraServicio = &f
	}
	// rol cliente: el cliente_id se deriva del tenant (DEC-112). Se pasa cli como
	// tenant para que el viaje quede con ese cliente_id, que las transiciones de
	// aislamiento comparan.
	v, err := svc.Crear(ctx, in, "cliente", cli.String())
	if err != nil {
		t.Fatalf("crear SOLICITADO: %v", err)
	}
	if v.Estado != operclient.SOLICITADO {
		t.Fatalf("estado tras crear: got %q, want SOLICITADO", v.Estado)
	}
	return v
}

func TestTransicionar(t *testing.T) {
	ctx := context.Background()
	svc := prepara(t, ctx)

	// --- Transición válida con guarda satisfecha: SOLICITADO -> APROBADO ---
	v := crearSolicitado(t, ctx, svc, true)
	out, err := transicionarInterno(svc, ctx, v.Id, operclient.TransicionEntrada{AEstado: operclient.APROBADO}, "jefe_operativo")
	if err != nil {
		t.Fatalf("transición SOLICITADO->APROBADO: %v", err)
	}
	if out.Estado != operclient.APROBADO {
		t.Errorf("estado tras aprobar: got %q, want APROBADO", out.Estado)
	}
	if n := contarEventos(t, ctx, "viaje.estado_cambiado", v.Id.String()); n != 1 {
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

	// --- Guarda de motivo: SOLICITADO -> RECHAZADO_POR_AI27 sin causa -> error ---
	vr := crearSolicitado(t, ctx, svc, true)
	if _, err := transicionarInterno(svc, ctx, vr.Id, operclient.TransicionEntrada{AEstado: operclient.RECHAZADOPORAI27}, "jefe_operativo"); err == nil {
		t.Error("rechazo sin causa debió fallar")
	}
	// con causa -> ok
	causa := "documentación incompleta"
	outR, err := transicionarInterno(svc, ctx, vr.Id, operclient.TransicionEntrada{AEstado: operclient.RECHAZADOPORAI27, Causa: &causa}, "jefe_operativo")
	if err != nil {
		t.Fatalf("rechazo con causa: %v", err)
	}
	if outR.Estado != operclient.RECHAZADOPORAI27 {
		t.Errorf("estado tras rechazar: got %q, want RECHAZADO_POR_AI27", outR.Estado)
	}

	// --- Guarda de fecha: SOLICITADO sin fecha -> APROBADO -> error ---
	vsf := crearSolicitado(t, ctx, svc, false)
	if _, err := transicionarInterno(svc, ctx, vsf.Id, operclient.TransicionEntrada{AEstado: operclient.APROBADO}, "jefe_operativo"); err == nil {
		t.Error("aprobar sin fecha_hora_servicio debió fallar")
	}

	// --- Transición a un viaje inexistente -> NoEncontrado ---
	if _, err := transicionarInterno(svc, ctx, uuid.New(), operclient.TransicionEntrada{AEstado: operclient.APROBADO}, "jefe_operativo"); err == nil {
		t.Error("transición de viaje inexistente debió fallar")
	}
}

// --- Transición de audiencia Cliente (E9-T7 / DEC-111/112) ------------------
// El cliente solo puede cancelar o rechazar SU propio viaje, con motivo
// obligatorio. tenant = cliente (DEC-112): el aislamiento compara el tenant del
// actor contra el cliente_id del viaje.

// TestClienteCancelaSuViaje — un cliente cancela su propio viaje con motivo:
// SOLICITADO -> CANCELADO. Caso feliz.
func TestClienteCancelaSuViaje(t *testing.T) {
	ctx := context.Background()
	svc := prepara(t, ctx)

	v := crearSolicitado(t, ctx, svc, true)
	motivo := "ya no necesito el servicio"
	out, err := svc.Transicionar(ctx, v.Id,
		operclient.TransicionEntrada{AEstado: operclient.CANCELADO, Causa: &motivo},
		"cliente", "Cliente", v.ClienteId.String())
	if err != nil {
		t.Fatalf("cliente cancela su viaje: %v", err)
	}
	if out.Estado != operclient.CANCELADO {
		t.Errorf("estado tras cancelar: got %q, want CANCELADO", out.Estado)
	}
	if n := contarEventos(t, ctx, "viaje.estado_cambiado", v.Id.String()); n != 1 {
		t.Errorf("viaje.estado_cambiado en outbox: got %d, want 1", n)
	}
}

// TestClienteCancelaSinMotivoRechazado — cancelar sin motivo se rechaza (400):
// el motivo es obligatorio para el cliente (DEC-111, supera DEC-020 #7).
func TestClienteCancelaSinMotivoRechazado(t *testing.T) {
	ctx := context.Background()
	svc := prepara(t, ctx)

	v := crearSolicitado(t, ctx, svc, true)
	if _, err := svc.Transicionar(ctx, v.Id,
		operclient.TransicionEntrada{AEstado: operclient.CANCELADO},
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

	v := crearSolicitado(t, ctx, svc, true)
	motivo := "intento sobre viaje ajeno"
	otroTenant := uuid.New().String() // distinto del cliente_id del viaje
	_, err := svc.Transicionar(ctx, v.Id,
		operclient.TransicionEntrada{AEstado: operclient.CANCELADO, Causa: &motivo},
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

	v := crearSolicitado(t, ctx, svc, true)
	motivo := "intento de arista de operador"
	if _, err := svc.Transicionar(ctx, v.Id,
		operclient.TransicionEntrada{AEstado: operclient.APROBADO, Causa: &motivo},
		"cliente", "Cliente", v.ClienteId.String()); err == nil {
		t.Error("un cliente no puede aprobar su viaje; debió rechazarse")
	}
	if n := contarEventos(t, ctx, "viaje.estado_cambiado", v.Id.String()); n != 0 {
		t.Errorf("no debió publicarse transición: got %d, want 0", n)
	}
}
