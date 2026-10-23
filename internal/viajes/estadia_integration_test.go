package viajes_test

import (
	"context"
	"testing"

	operclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/operacion"
	"github.com/AI27-Tech/pf-svc-operacion/internal/dbtest"
)

// TestAbrirEstadiaNoCambiaEstado — abrir una estadía fija inicio y NO cambia
// viaje.estado (se lee antes y después). Una estadía recién abierta no tiene
// cierre.
func TestAbrirEstadiaNoCambiaEstado(t *testing.T) {
	ctx := context.Background()
	svc := prepara(t, ctx)

	v := crearInterno(t, ctx, svc)
	estadoAntes := estadoDe(t, ctx, v.Id)

	est, err := svc.AbrirEstadia(ctx, v.Id,
		operclient.EstadiaEntrada{Motivo: operclient.Combustible, EjeAutorizacion: operclient.Autorizada})
	if err != nil {
		t.Fatalf("AbrirEstadia: %v", err)
	}
	if est.Inicio.IsZero() {
		t.Error("abrir estadía debió fijar inicio")
	}
	if est.Cierre != nil {
		t.Error("una estadía recién abierta no debe tener cierre")
	}
	if est.Motivo != operclient.Combustible {
		t.Errorf("motivo: got %q, want combustible", est.Motivo)
	}
	if est.EjeAutorizacion != operclient.Autorizada {
		t.Errorf("eje_autorizacion: got %q, want autorizada", est.EjeAutorizacion)
	}
	if estadoDespues := estadoDe(t, ctx, v.Id); estadoAntes != estadoDespues {
		t.Errorf("abrir estadía cambió el estado: antes %q, después %q", estadoAntes, estadoDespues)
	}
}

// TestCerrarEstadiaDuracionDerivable — cerrar fija cierre; la duración se DERIVA
// de (inicio, cierre) SIN capturarla (DEC-038). El test comprueba que ambas
// marcas vienen y que cierre >= inicio, de modo que la duración es calculable por
// quien lea, sin que exista columna de duración.
func TestCerrarEstadiaDuracionDerivable(t *testing.T) {
	ctx := context.Background()
	svc := prepara(t, ctx)

	v := crearInterno(t, ctx, svc)
	est, err := svc.AbrirEstadia(ctx, v.Id,
		operclient.EstadiaEntrada{Motivo: operclient.Descanso, EjeAutorizacion: operclient.NoAutorizada})
	if err != nil {
		t.Fatalf("AbrirEstadia: %v", err)
	}

	estadoAntes := estadoDe(t, ctx, v.Id)
	cerrada, err := svc.CerrarEstadia(ctx, v.Id, est.Id)
	if err != nil {
		t.Fatalf("CerrarEstadia: %v", err)
	}
	if cerrada.Cierre == nil {
		t.Fatal("cerrar estadía debió fijar cierre")
	}
	if cerrada.Cierre.Before(cerrada.Inicio) {
		t.Errorf("cierre (%v) no puede ser anterior a inicio (%v)", cerrada.Cierre, cerrada.Inicio)
	}
	// La duración es derivable: existe y es no negativa, sin haberla persistido.
	duracion := cerrada.Cierre.Sub(cerrada.Inicio)
	if duracion < 0 {
		t.Errorf("duración derivada negativa: %v", duracion)
	}
	if estadoDespues := estadoDe(t, ctx, v.Id); estadoAntes != estadoDespues {
		t.Errorf("cerrar estadía cambió el estado: antes %q, después %q", estadoAntes, estadoDespues)
	}

	// Cerrar de nuevo la misma estadía → NoEncontrado (ya no está abierta).
	if _, err := svc.CerrarEstadia(ctx, v.Id, est.Id); err == nil {
		t.Error("cerrar una estadía ya cerrada debió fallar")
	}
}

// TestEstadiaSinCampoProlongada — requisito duro del ticket: NO existe ningún
// campo/umbral `prolongada` ni columna de duración en la tabla. Se verifica
// contra el esquema real (information_schema): la ausencia de la columna es el
// invariante, no la ausencia de un método.
func TestEstadiaSinCampoProlongada(t *testing.T) {
	ctx := context.Background()
	_ = prepara(t, ctx) // aplica migraciones y crea viajes.estadia

	admin, cerrar := dbtest.AdminConn(t, ctx)
	defer cerrar()

	rows, err := admin.Query(ctx,
		`SELECT column_name FROM information_schema.columns
		  WHERE table_schema = 'viajes' AND table_name = 'estadia'`)
	if err != nil {
		t.Fatalf("leer columnas de viajes.estadia: %v", err)
	}
	defer rows.Close()
	columnas := map[string]bool{}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			t.Fatalf("escanear columna: %v", err)
		}
		columnas[c] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterar columnas: %v", err)
	}

	// Columnas prohibidas: cualquier forma de duración persistida o bandera de
	// "prolongada"/umbral. La duración se deriva; no se guarda.
	prohibidas := []string{"prolongada", "duracion", "duration", "umbral", "es_prolongada", "duracion_minutos"}
	for _, p := range prohibidas {
		if columnas[p] {
			t.Errorf("viajes.estadia no debe tener la columna %q (la duración se deriva, DEC-038)", p)
		}
	}
	// Las columnas que SÍ deben existir (solo marcas y datos, sin duración).
	for _, c := range []string{"id", "viaje_id", "motivo", "eje_autorizacion", "inicio", "cierre"} {
		if !columnas[c] {
			t.Errorf("viajes.estadia debe tener la columna %q", c)
		}
	}
}

// TestReinicioCierraEstadiaActiva — el "reinicio de viaje" (DEC-013) cierra la
// estadía activa sin ser transición de estado: viaje.estado no cambia y la
// estadía queda cerrada. Idempotente: un segundo reinicio no cierra nada.
func TestReinicioCierraEstadiaActiva(t *testing.T) {
	ctx := context.Background()
	svc := prepara(t, ctx)

	v := crearInterno(t, ctx, svc)
	if _, err := svc.AbrirEstadia(ctx, v.Id,
		operclient.EstadiaEntrada{Motivo: operclient.Reten, EjeAutorizacion: operclient.Antes200km}); err != nil {
		t.Fatalf("AbrirEstadia: %v", err)
	}

	estadoAntes := estadoDe(t, ctx, v.Id)
	n, err := svc.CerrarEstadiaActivaPorReinicio(ctx, v.Id)
	if err != nil {
		t.Fatalf("CerrarEstadiaActivaPorReinicio: %v", err)
	}
	if n != 1 {
		t.Errorf("reinicio debió cerrar 1 estadía activa: got %d", n)
	}
	if estadoDespues := estadoDe(t, ctx, v.Id); estadoAntes != estadoDespues {
		t.Errorf("el reinicio cambió el estado (no debe ser transición): antes %q, después %q", estadoAntes, estadoDespues)
	}

	// La estadía quedó cerrada: ya no aparece abierta en el listado.
	pag, err := svc.ListarEstadias(ctx, v.Id, operclient.ListarEstadiasParams{})
	if err != nil {
		t.Fatalf("ListarEstadias: %v", err)
	}
	if len(pag.Items) != 1 {
		t.Fatalf("estadías listadas: got %d, want 1", len(pag.Items))
	}
	if pag.Items[0].Cierre == nil {
		t.Error("la estadía debió quedar cerrada tras el reinicio")
	}

	// Idempotente: un segundo reinicio no encuentra estadía activa y cierra 0.
	n2, err := svc.CerrarEstadiaActivaPorReinicio(ctx, v.Id)
	if err != nil {
		t.Fatalf("CerrarEstadiaActivaPorReinicio (segunda): %v", err)
	}
	if n2 != 0 {
		t.Errorf("un segundo reinicio no debió cerrar ninguna estadía: got %d", n2)
	}
}

// TestAbrirEstadiaMotivoInvalido — un motivo fuera del catálogo cerrado se
// rechaza por validación, sin tocar la base.
func TestAbrirEstadiaMotivoInvalido(t *testing.T) {
	ctx := context.Background()
	svc := prepara(t, ctx)

	v := crearInterno(t, ctx, svc)
	if _, err := svc.AbrirEstadia(ctx, v.Id,
		operclient.EstadiaEntrada{Motivo: operclient.MotivoEstadia("inventado"), EjeAutorizacion: operclient.Autorizada}); err == nil {
		t.Error("abrir estadía con motivo inválido debió fallar")
	}
	if _, err := svc.AbrirEstadia(ctx, v.Id,
		operclient.EstadiaEntrada{Motivo: operclient.Combustible, EjeAutorizacion: operclient.EjeAutorizacionEstadia("x")}); err == nil {
		t.Error("abrir estadía con eje_autorizacion inválido debió fallar")
	}
}
