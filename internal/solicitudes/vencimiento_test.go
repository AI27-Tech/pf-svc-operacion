package solicitudes

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/estados"
	operclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/operacion"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Todas las solicitudes de estas pruebas salen de `cotizada` (pago_test.go):
// entregada el 20-nov-2026 a las 23:30 en CDMX, así que su último día de
// vigencia es el 5-dic-2026 (entrega + 15, inclusive). CDMX es UTC-6 en
// diciembre, sin horario de verano. Las fechas de abajo están escritas a mano,
// no calculadas con el código que se prueba.
var (
	finDelUltimoDia = time.Date(2026, 12, 6, 5, 59, 59, 0, time.UTC) // 5-dic 23:59:59 CDMX
	diaSiguiente    = time.Date(2026, 12, 6, 6, 0, 0, 0, time.UTC)   // 6-dic 00:00:00 CDMX
)

func conRelojEn(svc *Servicio, t time.Time) { svc.ConReloj(func() time.Time { return t }) }

func nTransiciones(t *testing.T, ctx testCtx, id uuid.UUID, a string) int {
	t.Helper()
	return consulta[int](t, ctx, `SELECT count(*) FROM solicitudes.transicion_solicitud WHERE solicitud_id=$1 AND a_estado=$2`, id, a)
}

// TestVencimientoFrontera (CP-036-04, DEC-148 #15, #50): el último día de vigencia
// no vence, ni a las 23:59:59 de CDMX; un día después, sí.
func TestVencimientoFrontera(t *testing.T) {
	ctx, svc := prepara(t)
	id := cotizada(t, ctx, svc)

	for _, c := range []struct {
		nombre string
		ahora  time.Time
	}{
		{"mismo día de la entrega", time.Date(2026, 11, 21, 6, 0, 0, 0, time.UTC)},
		{"último día, mediodía", time.Date(2026, 12, 5, 18, 0, 0, 0, time.UTC)},
		{"último día, 23:59:59 CDMX", finDelUltimoDia},
	} {
		conRelojEn(svc, c.ahora)
		n, err := svc.VencerVencidas(ctx)
		if err != nil || n != 0 || estadoDe(t, ctx, id) != "COTIZADO" {
			t.Fatalf("%s: no debe vencer; n=%d err=%v estado=%s", c.nombre, n, err, estadoDe(t, ctx, id))
		}
	}

	conRelojEn(svc, diaSiguiente)
	n, err := svc.VencerVencidas(ctx)
	if err != nil || n != 1 || estadoDe(t, ctx, id) != "VENCIDA" {
		t.Fatalf("00:00:00 del día siguiente debe vencer; n=%d err=%v estado=%s", n, err, estadoDe(t, ctx, id))
	}
}

// TestVencimientoSinMarcaNiViajeConHistorico: la transición la hace el sistema,
// sin actor, sin marca de penalización, con su evento y su fila de histórico.
func TestVencimientoSinMarcaNiViaje(t *testing.T) {
	ctx, svc := prepara(t)
	id := cotizada(t, ctx, svc)
	conRelojEn(svc, diaSiguiente)
	if _, err := svc.VencerVencidas(ctx); err != nil {
		t.Fatal(err)
	}
	if consulta[bool](t, ctx, `SELECT marca_penalizacion FROM solicitudes.solicitud WHERE id=$1`, id) {
		t.Error("el vencimiento no lleva marca de penalización (DEC-148 #9)")
	}
	var de, a, accion, disp string
	var actor *string
	c, cerrar := adminConn(t, ctx)
	defer cerrar()
	if err := c.QueryRow(ctx, `SELECT de_estado, a_estado, accion, disparador, actor_id FROM solicitudes.transicion_solicitud
	   WHERE solicitud_id=$1 AND a_estado='VENCIDA'`, id).Scan(&de, &a, &accion, &disp, &actor); err != nil {
		t.Fatal(err)
	}
	if de != "COTIZADO" || a != "VENCIDA" || accion != "vencida" || disp != "sistema" || actor != nil {
		t.Errorf("histórico = %s→%s accion=%s disparador=%s actor=%v", de, a, accion, disp, actor)
	}
	if n := consulta[int](t, ctx, `SELECT count(*) FROM solicitudes.outbox WHERE agregado_id=$1 AND tipo='solicitud.estado_cambiado'
	   AND payload->>'a_estado'='VENCIDA' AND payload->>'de_estado'='COTIZADO'`, id.String()); n != 1 {
		t.Errorf("eventos de vencimiento = %d, quería 1", n)
	}
	if n := consulta[int](t, ctx, `SELECT count(*) FROM solicitudes.outbox WHERE agregado_id=$1 AND tipo LIKE 'viaje.%'`, id.String()); n != 0 {
		t.Errorf("el vencimiento no crea viaje: %d eventos viaje.*", n)
	}
}

// TestVencimientoNoTocaLoQueNoCorresponde: una solicitud pagada dentro de la
// vigencia no vence aunque el worker corra después (CP-036-05); la recotizada se
// mide contra su ÚLTIMA versión; y una ya VENCIDA no se reprocesa (idempotencia).
func TestVencimientoNoTocaLoQueNoCorresponde(t *testing.T) {
	ctx, svc := prepara(t)

	pagada := cotizada(t, ctx, svc)
	if _, err := svc.ConfirmarPago(ctx, actorCotizacion, pagada, operclient.PagoEntrada{FechaPago: dia("2026-11-30")}); err != nil {
		t.Fatal(err)
	}
	// Recotizada el 1-dic: la v2 vence el 16-dic, aunque la v1 ya habría terminado.
	recotizada := cotizada(t, ctx, svc)
	conRelojEn(svc, time.Date(2026, 12, 1, 18, 0, 0, 0, time.UTC))
	if _, err := svc.Cotizar(ctx, actorCotizacion, recotizada, cotizacionBase()); err != nil {
		t.Fatal(err)
	}
	vencible := cotizada(t, ctx, svc)

	conRelojEn(svc, diaSiguiente)
	n, err := svc.VencerVencidas(ctx)
	if err != nil || n != 1 {
		t.Fatalf("solo la no pagada y no recotizada vence; n=%d err=%v", n, err)
	}
	for id, quiere := range map[uuid.UUID]string{pagada: "PAGADO", recotizada: "COTIZADO", vencible: "VENCIDA"} {
		if got := estadoDe(t, ctx, id); got != quiere {
			t.Errorf("estado = %s, quería %s", got, quiere)
		}
	}
	// Idempotente: otra corrida no cambia nada ni duplica el histórico.
	if n, err := svc.VencerVencidas(ctx); err != nil || n != 0 {
		t.Errorf("segunda corrida: n=%d err=%v", n, err)
	}
	if got := nTransiciones(t, ctx, vencible, "VENCIDA"); got != 1 {
		t.Errorf("filas de vencimiento = %d, quería 1", got)
	}
}

// TestVencimientoCierraLaReversaConVigenciaTerminada (DEC-148 #63): la reversa
// regresa a COTIZADO aunque la vigencia ya terminó; la siguiente corrida la cierra.
func TestVencimientoCierraLaReversaConVigenciaTerminada(t *testing.T) {
	ctx, svc := prepara(t)
	id := cotizada(t, ctx, svc)
	if _, err := svc.ConfirmarPago(ctx, actorCotizacion, id, operclient.PagoEntrada{FechaPago: dia("2026-11-30")}); err != nil {
		t.Fatal(err)
	}
	conRelojEn(svc, time.Date(2026, 12, 10, 12, 0, 0, 0, time.UTC)) // vigencia terminada el 5-dic
	if _, err := svc.RevertirPago(ctx, actorAdmin, id, operclient.MotivoEntrada{Motivo: "pago por error"}); err != nil {
		t.Fatal(err)
	}
	if got := estadoDe(t, ctx, id); got != "COTIZADO" {
		t.Fatalf("la reversa regresa a COTIZADO, no a %s", got)
	}
	if n, err := svc.VencerVencidas(ctx); err != nil || n != 1 || estadoDe(t, ctx, id) != "VENCIDA" {
		t.Fatalf("la corrida debe cerrarla; n=%d err=%v estado=%s", n, err, estadoDe(t, ctx, id))
	}
}

// TestVencimientoDosReplicas: varias réplicas del worker sobre las mismas
// solicitudes producen exactamente UNA transición y UN evento por solicitud, y
// ninguna corrida falla por la otra.
func TestVencimientoDosReplicas(t *testing.T) {
	ctx, svc := prepara(t)
	const total = 40
	ids := make([]uuid.UUID, total)
	for i := range ids {
		ids[i] = cotizada(t, ctx, svc)
	}
	conRelojEn(svc, diaSiguiente)

	const replicas = 4
	var wg sync.WaitGroup
	inicio := make(chan struct{})
	cierres := make([]int, replicas)
	errs := make([]error, replicas)
	for r := 0; r < replicas; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-inicio
			cierres[r], errs[r] = svc.VencerVencidas(ctx)
		}()
	}
	close(inicio)
	wg.Wait()

	suma := 0
	for r := range cierres {
		if errs[r] != nil {
			t.Errorf("réplica %d falló: %v", r, errs[r])
		}
		suma += cierres[r]
	}
	if suma != total {
		t.Errorf("las réplicas cerraron %d en total, quería %d (%v)", suma, total, cierres)
	}
	for _, id := range ids {
		if estadoDe(t, ctx, id) != "VENCIDA" || nTransiciones(t, ctx, id, "VENCIDA") != 1 {
			t.Fatalf("%s: estado %s, transiciones a VENCIDA %d", id, estadoDe(t, ctx, id), nTransiciones(t, ctx, id, "VENCIDA"))
		}
		if n := consulta[int](t, ctx, `SELECT count(*) FROM solicitudes.outbox WHERE agregado_id=$1 AND payload->>'a_estado'='VENCIDA'`, id.String()); n != 1 {
			t.Fatalf("%s: %d eventos de vencimiento, quería 1", id, n)
		}
	}
}

// TestVencimientoPagoEnVueloGana: si una confirmación de pago tiene la fila
// bloqueada, la corrida la salta y la solicitud no vence (DEC-148 #15).
func TestVencimientoPagoEnVueloGana(t *testing.T) {
	ctx, svc := prepara(t)
	id := cotizada(t, ctx, svc)
	conRelojEn(svc, diaSiguiente)

	tx, err := svc.tx.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := cargar(ctx, tx, id, true); err != nil { // el pago en vuelo tiene la fila
		t.Fatal(err)
	}
	hecho := make(chan struct{})
	var n int
	var errV error
	go func() { n, errV = svc.VencerVencidas(ctx); close(hecho) }()
	select {
	case <-hecho:
	case <-time.After(5 * time.Second):
		t.Fatal("la corrida esperó el bloqueo en vez de saltar la fila")
	}
	if errV != nil || n != 0 || estadoDe(t, ctx, id) != "COTIZADO" {
		t.Errorf("con la fila bloqueada no debe vencer; n=%d err=%v estado=%s", n, errV, estadoDe(t, ctx, id))
	}
}

// TestVencimientoFallaAMitadDeLote: una solicitud que falla no frena al resto; las
// cerradas quedan cerradas; la siguiente corrida cierra la rezagada sin duplicar.
func TestVencimientoFallaAMitadDeLote(t *testing.T) {
	ctx, svc := prepara(t)
	sanas := []uuid.UUID{cotizada(t, ctx, svc), cotizada(t, ctx, svc), cotizada(t, ctx, svc)}
	rota := cotizada(t, ctx, svc)
	conRelojEn(svc, diaSiguiente)

	// Falla de infraestructura simulada: el evento de la solicitud rota no se puede escribir.
	ejecuta(t, ctx, `CREATE OR REPLACE FUNCTION solicitudes.falla_prueba() RETURNS trigger AS $$
	  BEGIN RAISE EXCEPTION 'falla simulada'; END $$ LANGUAGE plpgsql`)
	ejecuta(t, ctx, `CREATE TRIGGER falla_prueba BEFORE INSERT ON solicitudes.outbox
	  FOR EACH ROW WHEN (NEW.agregado_id = '`+rota.String()+`') EXECUTE FUNCTION solicitudes.falla_prueba()`)
	t.Cleanup(func() {
		c, cerrar := adminConn(t, context.Background())
		defer cerrar()
		_, _ = c.Exec(context.Background(), `DROP TRIGGER IF EXISTS falla_prueba ON solicitudes.outbox; DROP FUNCTION IF EXISTS solicitudes.falla_prueba()`)
	})

	n, err := svc.VencerVencidas(ctx)
	if err == nil || n != 3 {
		t.Fatalf("esperaba error y 3 cerradas; n=%d err=%v", n, err)
	}
	for _, id := range sanas {
		if estadoDe(t, ctx, id) != "VENCIDA" {
			t.Errorf("la solicitud sana %s quedó en %s", id, estadoDe(t, ctx, id))
		}
	}
	if estadoDe(t, ctx, rota) != "COTIZADO" || nTransiciones(t, ctx, rota, "VENCIDA") != 0 {
		t.Errorf("la rota no debe dejar rastro: estado %s", estadoDe(t, ctx, rota))
	}

	ejecuta(t, ctx, `DROP TRIGGER falla_prueba ON solicitudes.outbox`)
	n, err = svc.VencerVencidas(ctx)
	if err != nil || n != 1 || estadoDe(t, ctx, rota) != "VENCIDA" {
		t.Fatalf("el reintento debe cerrar solo la rezagada; n=%d err=%v", n, err)
	}
	for _, id := range append(sanas, rota) {
		if got := nTransiciones(t, ctx, id, "VENCIDA"); got != 1 {
			t.Errorf("%s: %d transiciones a VENCIDA, quería 1", id, got)
		}
	}
}

// TestWorkerVencimiento: Run cierra lo vencido solo, con el intervalo corto, y
// Ready queda sano.
func TestWorkerVencimiento(t *testing.T) {
	ctx, svc := prepara(t)
	id := cotizada(t, ctx, svc)
	conRelojEn(svc, diaSiguiente)
	w := NuevoWorkerVencimiento(svc, 10*time.Millisecond)

	rctx, cancelar := context.WithCancel(ctx)
	hecho := make(chan error, 1)
	go func() { hecho <- w.Run(rctx) }()
	for fin := time.Now().Add(5 * time.Second); estadoDe(t, ctx, id) != "VENCIDA"; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(fin) {
			t.Fatal("el worker no cerró la solicitud vencida")
		}
	}
	if err := w.Ready(ctx); err != nil {
		t.Errorf("Ready tras una corrida buena: %v", err)
	}
	cancelar()
	if err := <-hecho; err != nil {
		t.Errorf("Run al cancelar devolvió %v", err)
	}
	if w.Name() != "vencimiento:solicitudes" {
		t.Errorf("nombre = %s", w.Name())
	}
}

// TestTransicionarNoPisaUnEstadoCambiado: defensa en profundidad de transicionar.
// Con la solicitud cargada sin bloquear, si otro camino cambia el estado antes
// del UPDATE, la transición responde 409 y no pisa el estado nuevo.
func TestTransicionarNoPisaUnEstadoCambiado(t *testing.T) {
	ctx, svc := prepara(t)
	id := cotizada(t, ctx, svc)
	err := correr(ctx, svc, func(tx pgx.Tx) error {
		f, err := cargar(ctx, tx, id, false) // sin FOR UPDATE: simula un llamador que olvidó bloquear
		if err != nil {
			return err
		}
		ejecuta(t, ctx, `UPDATE solicitudes.solicitud SET estado='PAGADO' WHERE id=$1`, id) // otro camino, ya confirmado
		return svc.transicionar(ctx, tx, f, Cambio{A: estados.EstadoSolicitudVencida, Disparador: DisparadorSistema})
	})
	if codigoDe(err) != "transicion_no_permitida" {
		t.Fatalf("esperaba 409 transicion_no_permitida, obtuve %v", err)
	}
	if got := estadoDe(t, ctx, id); got != "PAGADO" {
		t.Errorf("el estado quedó en %s; el UPDATE pisó al otro camino", got)
	}
}

// TestVencerRevisaDeNuevoBajoElBloqueo: entre armar la lista de candidatas y
// tomar la fila, una recotización o un pago pueden cambiar la solicitud. vencer
// repite la condición dentro de la transacción y no cierra lo que ya no vence.
func TestVencerRevisaDeNuevoBajoElBloqueo(t *testing.T) {
	ctx, svc := prepara(t)
	id := cotizada(t, ctx, svc)
	conRelojEn(svc, time.Date(2026, 12, 1, 18, 0, 0, 0, time.UTC))
	if _, err := svc.Cotizar(ctx, actorCotizacion, id, cotizacionBase()); err != nil { // la v2 vence el 16-dic
		t.Fatal(err)
	}
	// Se llama con "hoy" = 6-dic: la v1 ya habría vencido, la v2 no.
	ok, err := svc.vencer(ctx, id, time.Date(2026, 12, 6, 0, 0, 0, 0, time.UTC))
	if err != nil || ok || estadoDe(t, ctx, id) != "COTIZADO" {
		t.Errorf("no debe cerrar una solicitud vigente; ok=%v err=%v estado=%s", ok, err, estadoDe(t, ctx, id))
	}
}

// TestVencerNoCierraUnaSolicitudRecotizadaEnCarrera: la recotización que se
// confirma mientras vencer toma la fila no debe dejar la solicitud en VENCIDA con
// una cotización vigente. Postgres evalúa la vigencia con la instantánea del
// inicio de la sentencia; si la repetición bajo el bloqueo va en la MISMA
// sentencia que lo toma, una recotización confirmada en medio (que no cambia la
// fila de solicitud) pasa inadvertida. La ventana es de microsegundos, así que la
// prueba lanza vencer y Cotizar a la vez sobre muchas solicitudes: sin la consulta
// nueva tras el bloqueo, unos 25 de 2400 pares terminan mal (medido). Con la
// corrección no hay ninguno.
func TestVencerNoCierraUnaSolicitudRecotizadaEnCarrera(t *testing.T) {
	ctx, svc := prepara(t)
	conRelojEn(svc, diaSiguiente) // la v1 (vigente hasta el 5-dic) ya terminó; la v2 vence el 21-dic
	hoy := diaMX(diaSiguiente)

	const rondas, porRonda = 4, 600
	var malas []uuid.UUID
	for r := 0; r < rondas; r++ {
		ids := make([]uuid.UUID, porRonda)
		for i := range ids {
			ids[i] = uuid.New()
		}
		ejecuta(t, ctx, `INSERT INTO solicitudes.solicitud (id, cliente_id, estado, marca_penalizacion, origen, destinos, creado_en)
		  SELECT i, gen_random_uuid(), 'COTIZADO', false, 'A', '[{"nombre":"B"}]'::jsonb, now() FROM unnest($1::uuid[]) i`, ids)
		ejecuta(t, ctx, `INSERT INTO solicitudes.cotizacion (solicitud_id, version, monto, moneda, fecha_hora_servicio, entregada_en, vigente_hasta)
		  SELECT i, 1, 100, 'MXN', '2026-12-10T16:00:00Z', '2026-11-21T05:30:00Z', '2026-12-05' FROM unnest($1::uuid[]) i`, ids)
		salida := make(chan struct{})
		var wg sync.WaitGroup
		for _, id := range ids {
			wg.Add(2)
			go func() { defer wg.Done(); <-salida; _, _ = svc.vencer(ctx, id, hoy) }()
			go func() { defer wg.Done(); <-salida; _, _ = svc.Cotizar(ctx, actorCotizacion, id, cotizacionBase()) }()
		}
		close(salida)
		wg.Wait()
		for _, id := range ids {
			if estadoDe(t, ctx, id) == "VENCIDA" &&
				consulta[int](t, ctx, `SELECT count(*) FROM solicitudes.cotizacion WHERE solicitud_id=$1`, id) > 1 {
				malas = append(malas, id)
			}
		}
	}
	if len(malas) > 0 {
		t.Errorf("%d solicitudes quedaron VENCIDA con una cotización vigente recién entregada, p. ej. %s", len(malas), malas[0])
	}
}
