package viajes_test

import (
	"context"
	"testing"
	"time"

	operclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/operacion"
	"github.com/google/uuid"
)

// crearSolicitado crea un viaje en estado SOLICITADO (rol cliente). Si conFecha
// es true, fija fecha_hora_servicio (necesaria para la guarda de aprobación).
func crearSolicitado(t *testing.T, ctx context.Context, svc interface {
	Crear(context.Context, operclient.CrearViajeEntrada, string) (*operclient.Viaje, error)
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
	v, err := svc.Crear(ctx, in, "cliente")
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
	out, err := svc.Transicionar(ctx, v.Id, operclient.TransicionEntrada{AEstado: operclient.APROBADO}, "jefe_operativo")
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
	if _, err := svc.Transicionar(ctx, v.Id, operclient.TransicionEntrada{AEstado: operclient.ENDESTINO}, "jefe_operativo"); err == nil {
		t.Error("transición por arista inexistente debió fallar")
	}

	// --- Guarda diferida: APROBADO -> ASIGNADO (escolta, Slice 3) -> error ---
	if _, err := svc.Transicionar(ctx, v.Id, operclient.TransicionEntrada{AEstado: operclient.ASIGNADO}, "jefe_operativo"); err == nil {
		t.Error("transición diferida APROBADO->ASIGNADO debió rechazarse")
	}

	// --- Guarda de motivo: SOLICITADO -> RECHAZADO_POR_AI27 sin causa -> error ---
	vr := crearSolicitado(t, ctx, svc, true)
	if _, err := svc.Transicionar(ctx, vr.Id, operclient.TransicionEntrada{AEstado: operclient.RECHAZADOPORAI27}, "jefe_operativo"); err == nil {
		t.Error("rechazo sin causa debió fallar")
	}
	// con causa -> ok
	causa := "documentación incompleta"
	outR, err := svc.Transicionar(ctx, vr.Id, operclient.TransicionEntrada{AEstado: operclient.RECHAZADOPORAI27, Causa: &causa}, "jefe_operativo")
	if err != nil {
		t.Fatalf("rechazo con causa: %v", err)
	}
	if outR.Estado != operclient.RECHAZADOPORAI27 {
		t.Errorf("estado tras rechazar: got %q, want RECHAZADO_POR_AI27", outR.Estado)
	}

	// --- Guarda de fecha: SOLICITADO sin fecha -> APROBADO -> error ---
	vsf := crearSolicitado(t, ctx, svc, false)
	if _, err := svc.Transicionar(ctx, vsf.Id, operclient.TransicionEntrada{AEstado: operclient.APROBADO}, "jefe_operativo"); err == nil {
		t.Error("aprobar sin fecha_hora_servicio debió fallar")
	}

	// --- Transición a un viaje inexistente -> NoEncontrado ---
	if _, err := svc.Transicionar(ctx, uuid.New(), operclient.TransicionEntrada{AEstado: operclient.APROBADO}, "jefe_operativo"); err == nil {
		t.Error("transición de viaje inexistente debió fallar")
	}
}
