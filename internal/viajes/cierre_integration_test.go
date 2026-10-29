package viajes_test

import (
	"context"
	"testing"

	operclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/operacion"
	"github.com/AI27-Tech/pf-svc-operacion/internal/dbtest"
	"github.com/google/uuid"
)

// forzarEstado fija el estado de un viaje por la vía de administración, para
// colocar el viaje en el estado de origen del cierre sin recorrer toda la máquina
// (ASIGNADO en adelante se alcanza por asignarRecursos, no por transición). No es
// cómo transiciona producción; es andamiaje de prueba para aislar el cierre.
func forzarEstado(t *testing.T, ctx context.Context, id operclient.Id, estado string) {
	t.Helper()
	admin, cerrar := dbtest.AdminConn(t, ctx)
	defer cerrar()
	dbtest.Exec(t, ctx, admin,
		"UPDATE viajes.viaje SET estado='"+estado+"' WHERE id='"+id.String()+"'")
}

// abrirIncidencia inserta una condición INCIDENCIA abierta (cerrada_en NULL)
// sobre el viaje, como lo hará E1-T6/AI27-14. Andamiaje de prueba.
func abrirIncidencia(t *testing.T, ctx context.Context, id operclient.Id) {
	t.Helper()
	admin, cerrar := dbtest.AdminConn(t, ctx)
	defer cerrar()
	dbtest.Exec(t, ctx, admin,
		"INSERT INTO viajes.condicion_viaje (id, viaje_id, tipo) VALUES ('"+
			uuid.New().String()+"','"+id.String()+"','INCIDENCIA')")
}

// motivoCierreDeOutbox lee el motivo_cierre del payload del evento viaje.cerrado
// del viaje. Falla si no hay exactamente uno. Verifica el requisito de E1-T8: el
// consumidor (mensajeria) obtiene el motivo del payload sin llamar a operacion.
func motivoCierreDeOutbox(t *testing.T, ctx context.Context, viajeID string) string {
	t.Helper()
	admin, cerrar := dbtest.AdminConn(t, ctx)
	defer cerrar()
	var motivo string
	if err := admin.QueryRow(ctx,
		"SELECT payload->>'motivo_cierre' FROM viajes.outbox WHERE tipo='viaje.cerrado' AND agregado_id=$1",
		viajeID,
	).Scan(&motivo); err != nil {
		t.Fatalf("leer motivo_cierre del outbox: %v", err)
	}
	return motivo
}

// crearInterno crea un viaje en estado APROBADO (rol interno), base para forzar
// luego el estado de origen del cierre.
func crearInterno(t *testing.T, ctx context.Context, svc interface {
	Crear(context.Context, operclient.CrearViajeEntrada, string) (*operclient.Viaje, error)
}) *operclient.Viaje {
	t.Helper()
	cli := uuid.New()
	v, err := svc.Crear(ctx, operclient.CrearViajeEntrada{
		ClienteId: &cli, Origen: "Origen", Destinos: []operclient.Destino{{Nombre: "Destino"}},
	}, "jefe_operativo")
	if err != nil {
		t.Fatalf("crear viaje interno: %v", err)
	}
	return v
}

func ptrS(s string) *string { return &s }

// TestCierreNormal — cierre normal tras la última parada (desde EN_DESTINO), por
// el jefe_operativo, con motivo "normal": pasa a CERRADO y publica viaje.cerrado
// con el motivo en el payload.
func TestCierreNormal(t *testing.T) {
	ctx := context.Background()
	svc := prepara(t, ctx)

	v := crearInterno(t, ctx, svc)
	forzarEstado(t, ctx, v.Id, "EN_DESTINO")

	out, err := transicionarInterno(svc, ctx, v.Id,
		operclient.TransicionEntrada{AEstado: operclient.CERRADO, Causa: ptrS("normal")}, "jefe_operativo")
	if err != nil {
		t.Fatalf("cierre normal: %v", err)
	}
	if out.Estado != operclient.CERRADO {
		t.Errorf("estado tras cerrar: got %q, want CERRADO", out.Estado)
	}
	if n := contarEventos(t, ctx, "viaje.cerrado", v.Id.String()); n != 1 {
		t.Errorf("viaje.cerrado en outbox: got %d, want 1", n)
	}
	if m := motivoCierreDeOutbox(t, ctx, v.Id.String()); m != "normal" {
		t.Errorf("motivo_cierre en payload: got %q, want normal", m)
	}
	// La transición genérica también se publica (coherencia con reportes).
	if n := contarEventos(t, ctx, "viaje.estado_cambiado", v.Id.String()); n != 1 {
		t.Errorf("viaje.estado_cambiado en outbox: got %d, want 1", n)
	}
}

// TestCierreSinMotivo — cerrar sin motivo se rechaza (validación → 400).
func TestCierreSinMotivo(t *testing.T) {
	ctx := context.Background()
	svc := prepara(t, ctx)

	v := crearInterno(t, ctx, svc)
	forzarEstado(t, ctx, v.Id, "EN_DESTINO")

	if _, err := transicionarInterno(svc, ctx, v.Id,
		operclient.TransicionEntrada{AEstado: operclient.CERRADO}, "jefe_operativo"); err == nil {
		t.Error("cerrar sin motivo debió fallar")
	}
	if n := contarEventos(t, ctx, "viaje.cerrado", v.Id.String()); n != 0 {
		t.Errorf("no debió publicarse viaje.cerrado: got %d, want 0", n)
	}
}

// TestCierreAnticipadoPorIncidenciaOperador — desde EN_TRANSITO, con motivo
// por_incidencia, una INCIDENCIA abierta y el rol jefe_operativo: se permite.
func TestCierreAnticipadoPorIncidenciaOperador(t *testing.T) {
	ctx := context.Background()
	svc := prepara(t, ctx)

	v := crearInterno(t, ctx, svc)
	forzarEstado(t, ctx, v.Id, "EN_TRANSITO")
	abrirIncidencia(t, ctx, v.Id)

	out, err := transicionarInterno(svc, ctx, v.Id,
		operclient.TransicionEntrada{AEstado: operclient.CERRADO, Causa: ptrS("por_incidencia")}, "jefe_operativo")
	if err != nil {
		t.Fatalf("cierre anticipado por incidencia (operador): %v", err)
	}
	if out.Estado != operclient.CERRADO {
		t.Errorf("estado tras cerrar: got %q, want CERRADO", out.Estado)
	}
	if m := motivoCierreDeOutbox(t, ctx, v.Id.String()); m != "por_incidencia" {
		t.Errorf("motivo_cierre en payload: got %q, want por_incidencia", m)
	}
}

// TestCierreAnticipadoPorClienteRechazado — el MISMO intento (EN_TRANSITO,
// por_incidencia, incidencia abierta) por un cliente se rechaza: cerrar no es
// facultad del cliente. Con E9-T7 (DEC-111) el cliente solo puede CANCELADO o
// RECHAZADO_POR_CLIENTE, así que CERRADO se rechaza antes de la lógica de cierre.
// El tenant del actor coincide con el cliente del viaje (pasa el aislamiento):
// el rechazo es por la facultad, no por alcance.
func TestCierreAnticipadoPorClienteRechazado(t *testing.T) {
	ctx := context.Background()
	svc := prepara(t, ctx)

	v := crearInterno(t, ctx, svc)
	forzarEstado(t, ctx, v.Id, "EN_TRANSITO")
	abrirIncidencia(t, ctx, v.Id)

	if _, err := svc.Transicionar(ctx, v.Id,
		operclient.TransicionEntrada{AEstado: operclient.CERRADO, Causa: ptrS("por_incidencia")},
		"cliente", "Cliente", v.ClienteId.String()); err == nil {
		t.Error("cierre anticipado por un cliente debió rechazarse")
	}
	if n := contarEventos(t, ctx, "viaje.cerrado", v.Id.String()); n != 0 {
		t.Errorf("no debió publicarse viaje.cerrado: got %d, want 0", n)
	}
}

// TestCierreAnticipadoSinIncidenciaRechazado — desde EN_TRANSITO con motivo
// por_incidencia pero SIN incidencia abierta: se rechaza.
func TestCierreAnticipadoSinIncidenciaRechazado(t *testing.T) {
	ctx := context.Background()
	svc := prepara(t, ctx)

	v := crearInterno(t, ctx, svc)
	forzarEstado(t, ctx, v.Id, "EN_TRANSITO")
	// sin abrirIncidencia

	if _, err := transicionarInterno(svc, ctx, v.Id,
		operclient.TransicionEntrada{AEstado: operclient.CERRADO, Causa: ptrS("por_incidencia")}, "jefe_operativo"); err == nil {
		t.Error("cierre anticipado sin incidencia abierta debió rechazarse")
	}
	if n := contarEventos(t, ctx, "viaje.cerrado", v.Id.String()); n != 0 {
		t.Errorf("no debió publicarse viaje.cerrado: got %d, want 0", n)
	}
}

// TestCierreAnticipadoMotivoNormalRechazado — desde EN_TRANSITO con incidencia
// abierta pero motivo "normal": el cierre anticipado solo es válido con
// por_incidencia, así que se rechaza.
func TestCierreAnticipadoMotivoNormalRechazado(t *testing.T) {
	ctx := context.Background()
	svc := prepara(t, ctx)

	v := crearInterno(t, ctx, svc)
	forzarEstado(t, ctx, v.Id, "EN_TRANSITO")
	abrirIncidencia(t, ctx, v.Id)

	if _, err := transicionarInterno(svc, ctx, v.Id,
		operclient.TransicionEntrada{AEstado: operclient.CERRADO, Causa: ptrS("normal")}, "jefe_operativo"); err == nil {
		t.Error("cierre anticipado con motivo normal debió rechazarse")
	}
}
