package solicitudes

import (
	"fmt"
	"strings"
	"testing"
	"time"

	operclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/operacion"
	platformerrors "github.com/AI27-Tech/pf-pkg-platform-go/errors"
	"github.com/google/uuid"
)

// Actores con `sub` UUID (el contrato del histórico los tipa como Id).
var (
	subCliente    = "11111111-1111-4111-8111-111111111111"
	subCotizacion = "22222222-2222-4222-8222-222222222222"
	subAdmin      = "33333333-3333-4333-8333-333333333333"
	subJefe       = "44444444-4444-4444-8444-444444444444"

	aCliente = func(c uuid.UUID) Actor { return Actor{Subject: subCliente, Rol: "cliente", TenantID: c.String()} }
	aCot     = Actor{Subject: subCotizacion, Rol: "cotizacion"}
	aAdmin   = Actor{Subject: subAdmin, Rol: "administrador"}
	aJefe    = Actor{Subject: subJefe, Rol: "jefe_operativo"}
)

// auditoriaDe devuelve "accion/actor" de cada fila de auditoría de la solicitud, en
// orden de escritura. El actor NULL se escribe "-".
func auditoriaDe(t *testing.T, ctx testCtx, id uuid.UUID) []string {
	t.Helper()
	c, cerrar := adminConn(t, ctx)
	defer cerrar()
	rows, err := c.Query(ctx, `SELECT accion, coalesce(actor_id,'-'), modulo FROM auditoria.registro_auditoria
	   WHERE detalle->>'solicitud_id' = $1 ORDER BY id`, id.String())
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var accion, actor, modulo string
		if err := rows.Scan(&accion, &actor, &modulo); err != nil {
			t.Fatal(err)
		}
		if modulo != "solicitudes" {
			t.Errorf("módulo de auditoría = %q", modulo)
		}
		out = append(out, accion+"/"+actor)
	}
	return out
}

// recorrido hace, con relojes de 1 hora de separación, el ciclo completo de una
// solicitud y devuelve su id. Fechas y montos escritos a mano.
func recorrido(t *testing.T, ctx testCtx, svc *Servicio) (uuid.UUID, uuid.UUID) {
	t.Helper()
	cli := uuid.New()
	hora := 0
	paso := func() {
		hora++
		conRelojEn(svc, time.Date(2026, 11, 21, 6+hora, 0, 0, 0, time.UTC))
	}
	paso()
	fechaCliente := time.Date(2026, 12, 9, 10, 0, 0, 0, time.UTC)
	s, err := svc.Crear(ctx, aCliente(cli), operclient.CrearSolicitudEntrada{Origen: "A", Destinos: []operclient.Destino{{Nombre: "B"}}, FechaHoraServicio: &fechaCliente})
	if err != nil {
		t.Fatal(err)
	}
	id := s.Id
	cot := func(monto string, fecha time.Time) operclient.CotizacionEntrada {
		return operclient.CotizacionEntrada{Monto: monto, Moneda: "MXN", FechaHoraServicio: fecha}
	}
	f10 := time.Date(2026, 12, 10, 16, 0, 0, 0, time.UTC)
	f12 := time.Date(2026, 12, 12, 16, 0, 0, 0, time.UTC)
	f15 := time.Date(2026, 12, 15, 16, 0, 0, 0, time.UTC)
	pasos := []func() error{
		func() error { _, err := svc.Cotizar(ctx, aCot, id, cot("1500.50", f10)); return err }, // cotizada + fecha
		func() error { _, err := svc.Cotizar(ctx, aCot, id, cot("1600", f10)); return err },    // recotizada
		func() error { _, err := svc.Cotizar(ctx, aCot, id, cot("1700", f12)); return err },    // recotizada + fecha
		func() error {
			_, err := svc.ConfirmarPago(ctx, aCot, id, operclient.PagoEntrada{FechaPago: dia("2026-11-25")})
			return err
		},
		func() error {
			_, err := svc.Reprogramar(ctx, aCot, id, operclient.ReprogramacionEntrada{FechaHoraServicio: f15, Motivo: "falta de escolta"})
			return err
		},
		func() error {
			_, err := svc.RevertirPago(ctx, aAdmin, id, operclient.MotivoEntrada{Motivo: "pago por error"})
			return err
		},
		func() error {
			_, err := svc.ConfirmarPago(ctx, aCot, id, operclient.PagoEntrada{FechaPago: dia("2026-11-26")})
			return err
		},
		func() error { _, err := svc.Aprobar(ctx, aJefe, id); return err },
	}
	for i, p := range pasos {
		paso()
		if err := p(); err != nil {
			t.Fatalf("paso %d: %v", i+1, err)
		}
	}
	return id, cli
}

// TestCadaAccionDejaSuFilaDeAuditoria (E10-T11): una fila por acción, con su actor,
// en el orden en que ocurrieron; incluye las acciones sin cambio de estado y el
// cambio de fecha al cotizar. El detalle no lleva datos comerciales.
func TestCadaAccionDejaSuFilaDeAuditoria(t *testing.T) {
	ctx, svc := prepara(t)
	id, _ := recorrido(t, ctx, svc)
	quiero := []string{
		"creada/" + subCliente,
		"cotizada/" + subCotizacion, "fecha_cambiada_en_cotizacion/" + subCotizacion,
		"recotizada/" + subCotizacion,
		"recotizada/" + subCotizacion, "fecha_cambiada_en_cotizacion/" + subCotizacion,
		"pago_confirmado/" + subCotizacion,
		"reprogramada/" + subCotizacion,
		"pago_revertido/" + subAdmin,
		"pago_confirmado/" + subCotizacion,
		"aprobada/" + subJefe,
	}
	if got := auditoriaDe(t, ctx, id); fmt.Sprint(got) != fmt.Sprint(quiero) {
		t.Errorf("auditoría:\n got %v\nwant %v", got, quiero)
	}
	for _, comercial := range []string{"1500", "1600", "1700", "monto", "notas"} {
		if n := consulta[int](t, ctx, `SELECT count(*) FROM auditoria.registro_auditoria WHERE detalle->>'solicitud_id'=$1 AND detalle::text LIKE '%'||$2||'%'`, id.String(), comercial); n != 0 {
			t.Errorf("la auditoría no debe llevar datos comerciales (%q en %d filas)", comercial, n)
		}
	}
	// Motivo y fechas del cambio, escritos a mano.
	if n := consulta[int](t, ctx, `SELECT count(*) FROM auditoria.registro_auditoria WHERE accion='reprogramada' AND detalle->>'solicitud_id'=$1
	   AND detalle->>'motivo'='falta de escolta' AND detalle->>'fecha_hora_anterior'='2026-12-12T16:00:00Z' AND detalle->>'fecha_hora_nueva'='2026-12-15T16:00:00Z'`, id.String()); n != 1 {
		t.Errorf("detalle de la reprogramación = %d filas", n)
	}
	if n := consulta[int](t, ctx, `SELECT count(*) FROM auditoria.registro_auditoria WHERE accion='fecha_cambiada_en_cotizacion' AND detalle->>'solicitud_id'=$1
	   AND detalle->>'fecha_hora_anterior'='2026-12-09T10:00:00Z' AND detalle->>'fecha_hora_nueva'='2026-12-10T16:00:00Z'`, id.String()); n != 1 {
		t.Errorf("detalle del cambio de fecha del cliente a la cotización = %d filas", n)
	}
}

// TestAuditoriaDeLasDemasAcciones: las salidas del cliente, el vencimiento y el
// rechazo de AI27 también quedan, con actor NULL cuando lo hizo el sistema.
func TestAuditoriaDeLasDemasAcciones(t *testing.T) {
	ctx, svc := prepara(t)
	casos := []struct {
		nombre string
		quiero []string
		hacer  func(id uuid.UUID, cli uuid.UUID) error
		desde  func() (uuid.UUID, uuid.UUID)
	}{
		{"rechazar la cotización", []string{"cotizacion_rechazada/" + subCliente}, func(id, cli uuid.UUID) error {
			_, err := svc.RechazarCotizacion(ctx, aCliente(cli), id, operclient.AccionClienteEntrada{})
			return err
		}, func() (uuid.UUID, uuid.UUID) { id := cotizada(t, ctx, svc); return id, clienteDe(t, ctx, id) }},
		{"cancelar con pago", []string{"cancelada/" + subCliente}, func(id, cli uuid.UUID) error {
			_, err := svc.Cancelar(ctx, aCliente(cli), id, operclient.AccionClienteEntrada{})
			return err
		}, func() (uuid.UUID, uuid.UUID) { return pagada(t, ctx, svc) }},
		{"rechazo de AI27", []string{"rechazada/" + subJefe}, func(id, _ uuid.UUID) error {
			_, err := svc.Rechazar(ctx, aJefe, id, operclient.MotivoEntrada{Motivo: "m"})
			return err
		}, func() (uuid.UUID, uuid.UUID) { return pagada(t, ctx, svc) }},
		{"vencimiento del sistema", []string{"vencida/-"}, func(id, _ uuid.UUID) error {
			conRelojEn(svc, diaSiguiente)
			_, err := svc.VencerVencidas(ctx)
			return err
		}, func() (uuid.UUID, uuid.UUID) { id := cotizada(t, ctx, svc); return id, uuid.Nil }},
	}
	for _, c := range casos {
		t.Run(c.nombre, func(t *testing.T) {
			id, cli := c.desde()
			antes := len(auditoriaDe(t, ctx, id))
			if err := c.hacer(id, cli); err != nil {
				t.Fatal(err)
			}
			got := auditoriaDe(t, ctx, id)[antes:]
			if fmt.Sprint(got) != fmt.Sprint(c.quiero) {
				t.Errorf("got %v want %v", got, c.quiero)
			}
		})
	}
	t.Run("la marca de penalización queda en el detalle de la cancelación", func(t *testing.T) {
		id, cli := pagada(t, ctx, svc)
		if _, err := svc.Cancelar(ctx, aCliente(cli), id, operclient.AccionClienteEntrada{}); err != nil {
			t.Fatal(err)
		}
		if n := consulta[int](t, ctx, `SELECT count(*) FROM auditoria.registro_auditoria WHERE accion='cancelada' AND detalle->>'solicitud_id'=$1
		   AND detalle->>'marca_penalizacion'='true' AND detalle->>'de_estado'='PAGADO'`, id.String()); n != 1 {
			t.Errorf("detalle de la cancelación = %d", n)
		}
	})
}

// foto cuenta lo que una acción puede escribir, para comparar antes y después.
func foto(t *testing.T, ctx testCtx) string {
	t.Helper()
	var partes []string
	for _, tabla := range []string{"solicitudes.solicitud", "solicitudes.transicion_solicitud", "solicitudes.cotizacion",
		"solicitudes.reprogramacion_solicitud", "solicitudes.pago_solicitud", "solicitudes.outbox", "auditoria.registro_auditoria", "viajes.viaje"} {
		partes = append(partes, fmt.Sprintf("%s=%d", tabla, consulta[int](t, ctx, "SELECT count(*) FROM "+tabla)))
	}
	partes = append(partes, "estados="+consulta[string](t, ctx, `SELECT coalesce(string_agg(estado||coalesce(fecha_hora_servicio::text,''), ',' ORDER BY id),'') FROM solicitudes.solicitud`))
	return strings.Join(partes, " ")
}

// TestUnaFallaDeAuditoriaRevierteLaAccion: si la auditoría no se puede escribir, la
// acción entera revierte (cambio, histórico, evento y auditoría): ninguna acción
// queda sin su fila, ni al revés.
func TestUnaFallaDeAuditoriaRevierteLaAccion(t *testing.T) {
	ctx, svc := prepara(t)
	acciones := []struct {
		nombre string
		listo  func() (hacer func() error)
	}{
		{"crear", func() func() error {
			cli := uuid.New()
			return func() error {
				_, err := svc.Crear(ctx, aCliente(cli), operclient.CrearSolicitudEntrada{Origen: "A", Destinos: []operclient.Destino{{Nombre: "B"}}})
				return err
			}
		}},
		{"cotizar por primera vez", func() func() error {
			id, _ := sembrar(t, ctx, "SOLICITADO")
			return func() error { _, err := svc.Cotizar(ctx, aCot, id, cotizacionBase()); return err }
		}},
		{"recotizar", func() func() error {
			id := cotizada(t, ctx, svc)
			return func() error {
				_, err := svc.Cotizar(ctx, aCot, id, operclient.CotizacionEntrada{Monto: "9", Moneda: "MXN", FechaHoraServicio: time.Date(2026, 12, 20, 16, 0, 0, 0, time.UTC)})
				return err
			}
		}},
		{"reprogramar", func() func() error {
			id, _ := pagada(t, ctx, svc)
			return func() error {
				_, err := svc.Reprogramar(ctx, aCot, id, operclient.ReprogramacionEntrada{FechaHoraServicio: time.Date(2026, 12, 20, 16, 0, 0, 0, time.UTC), Motivo: "m"})
				return err
			}
		}},
		{"confirmar el pago", func() func() error {
			id := cotizada(t, ctx, svc)
			return func() error {
				_, err := svc.ConfirmarPago(ctx, aCot, id, operclient.PagoEntrada{FechaPago: dia("2026-11-25")})
				return err
			}
		}},
		{"cancelar", func() func() error {
			id, cli := pagada(t, ctx, svc)
			return func() error {
				_, err := svc.Cancelar(ctx, aCliente(cli), id, operclient.AccionClienteEntrada{})
				return err
			}
		}},
		{"vencer", func() func() error {
			cotizada(t, ctx, svc)
			conRelojEn(svc, diaSiguiente)
			return func() error { _, err := svc.VencerVencidas(ctx); return err }
		}},
	}
	for _, c := range acciones {
		t.Run(c.nombre, func(t *testing.T) {
			hacer := c.listo()
			antes := foto(t, ctx)
			quitar := fallaEn(t, ctx, "auditoria.registro_auditoria", "")
			err := hacer()
			if err == nil {
				t.Fatal("la acción debía fallar al no poder auditar")
			}
			if despues := foto(t, ctx); despues != antes {
				t.Errorf("la acción dejó rastro:\n antes   %s\n después %s", antes, despues)
			}
			quitar()
			if err := hacer(); err != nil {
				t.Errorf("sin la falla debe funcionar: %v", err)
			}
		})
	}
}

// TestElHistoricoYLaAuditoriaSonInmutables: ni el módulo ni nadie con su rol puede
// editar o borrar lo que ya quedó (append-only); la auditoría ni siquiera se lee.
func TestElHistoricoYLaAuditoriaSonInmutables(t *testing.T) {
	ctx, svc := prepara(t)
	id, _ := recorrido(t, ctx, svc)
	sentencias := []string{
		`UPDATE solicitudes.transicion_solicitud SET motivo = 'x' WHERE solicitud_id = $1`,
		`DELETE FROM solicitudes.transicion_solicitud WHERE solicitud_id = $1`,
		`UPDATE solicitudes.cotizacion SET monto = 1 WHERE solicitud_id = $1`,
		`DELETE FROM solicitudes.cotizacion WHERE solicitud_id = $1`,
		`UPDATE solicitudes.reprogramacion_solicitud SET motivo = 'x' WHERE solicitud_id = $1`,
		`DELETE FROM solicitudes.reprogramacion_solicitud WHERE solicitud_id = $1`,
		`UPDATE solicitudes.solicitud SET creado_por = 'x' WHERE id = $1`,
		`UPDATE solicitudes.solicitud SET cliente_id = gen_random_uuid() WHERE id = $1`,
		`UPDATE solicitudes.solicitud SET creado_en = now() WHERE id = $1`,
		`DELETE FROM solicitudes.solicitud WHERE id = $1`,
		`SELECT * FROM auditoria.registro_auditoria WHERE detalle->>'solicitud_id' = $1::text`,
		`UPDATE auditoria.registro_auditoria SET accion = 'x' WHERE detalle->>'solicitud_id' = $1::text`,
		`DELETE FROM auditoria.registro_auditoria WHERE detalle->>'solicitud_id' = $1::text`,
		`UPDATE viajes.viaje SET estado = 'CERRADO' WHERE id = $1`,
		`DELETE FROM viajes.viaje WHERE id = $1`,
		`SELECT * FROM viajes.viaje WHERE id = $1`,
		`INSERT INTO viajes.transicion_viaje (viaje_id, a_estado) VALUES ($1, 'APROBADO')`,
	}
	for _, q := range sentencias {
		tx, err := svc.tx.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		_, err = tx.Exec(ctx, q, id)
		_ = tx.Rollback(ctx)
		if err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Errorf("%q: esperaba permission denied, obtuve %v", q, err)
		}
	}
}

// TestHistoricoUnificado (E10-T11): el recorrido completo en orden cronológico, con
// actor, motivo, marca de tiempo y la versión completa de cada cotización; sigue
// consultable después de aprobarse y nacer el viaje; y coincide con la auditoría.
func TestHistoricoUnificado(t *testing.T) {
	ctx, svc := prepara(t)
	id, cli := recorrido(t, ctx, svc)
	h, err := svc.Historico(ctx, aCot, id)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, it := range h.Items {
		de, a := "-", "-"
		if it.DeEstado != nil {
			de = string(*it.DeEstado)
		}
		if it.AEstado != nil {
			a = string(*it.AEstado)
		}
		actor, motivo, v := "-", "-", "-"
		if it.ActorId != nil {
			actor = it.ActorId.String()[:2]
		}
		if it.Motivo != nil {
			motivo = *it.Motivo
		}
		if it.Cotizacion != nil {
			v = fmt.Sprintf("v%d:%s", it.Cotizacion.Version, it.Cotizacion.Monto)
		}
		got = append(got, fmt.Sprintf("%s|%s>%s|%s|%s|%s|%s", it.En.UTC().Format("15:04"), de, a, it.Accion, actor, motivo, v))
	}
	quiero := []string{
		"07:00|->SOLICITADO|creada|11|-|-",
		"08:00|SOLICITADO>COTIZADO|cotizada|22|-|v1:1500.50",
		"09:00|->-|recotizada|22|-|v2:1600",
		"10:00|->-|recotizada|22|-|v3:1700",
		"11:00|COTIZADO>PAGADO|pago_confirmado|22|-|-",
		"12:00|->-|reprogramada|22|falta de escolta|-",
		"13:00|PAGADO>COTIZADO|pago_revertido|33|pago por error|-",
		"14:00|COTIZADO>PAGADO|pago_confirmado|22|-|-",
		"15:00|PAGADO>APROBADO|aprobada|44|-|-",
	}
	if fmt.Sprint(got) != fmt.Sprint(quiero) {
		t.Errorf("histórico:\n got %q\nwant %q", got, quiero)
	}
	// Consistencia con la auditoría: mismas acciones, mismo actor, mismo orden (la
	// auditoría suma el cambio de fecha al cotizar, que el histórico lleva dentro de la cotización).
	var deAuditoria []string
	for _, f := range auditoriaDe(t, ctx, id) {
		if !strings.HasPrefix(f, "fecha_cambiada_en_cotizacion") {
			deAuditoria = append(deAuditoria, strings.Split(f, "/")[0]+"/"+strings.Split(f, "/")[1][:2])
		}
	}
	var deHistorico []string
	for _, it := range h.Items {
		deHistorico = append(deHistorico, it.Accion+"/"+it.ActorId.String()[:2])
	}
	if fmt.Sprint(deAuditoria) != fmt.Sprint(deHistorico) {
		t.Errorf("auditoría %v ≠ histórico %v", deAuditoria, deHistorico)
	}

	// Cancelada con pago: la marca va en el histórico, solo en `cancelada`.
	conRelojEn(svc, time.Date(2026, 11, 22, 6, 0, 0, 0, time.UTC))
	otro, cli2 := pagada(t, ctx, svc)
	if _, err := svc.Cancelar(ctx, aCliente(cli2), otro, operclient.AccionClienteEntrada{Motivo: ptr("ya no lo necesito")}); err != nil {
		t.Fatal(err)
	}
	h2, err := svc.Historico(ctx, aAdmin, otro)
	if err != nil {
		t.Fatal(err)
	}
	ult := h2.Items[len(h2.Items)-1]
	if ult.Accion != "cancelada" || ult.MarcaPenalizacion == nil || !*ult.MarcaPenalizacion || ult.Motivo == nil || *ult.Motivo != "ya no lo necesito" {
		t.Errorf("cancelación en el histórico: %+v", ult)
	}
	if h2.Items[0].MarcaPenalizacion != nil {
		t.Error("la marca solo va en `cancelada`")
	}

	// Alcance del histórico.
	if _, err := svc.Historico(ctx, aCliente(cli), id); categoriaDe(err) != platformerrors.CategoryProhibido {
		t.Errorf("el cliente no ve el histórico: %v", err)
	}
	for _, rol := range []string{"monitorista", "supervisor", "coordinador", "ejecutivo", "reaccion", ""} {
		if _, err := svc.Historico(ctx, Actor{Subject: subJefe, Rol: rol}, id); categoriaDe(err) != platformerrors.CategoryProhibido {
			t.Errorf("%q: esperaba 403, obtuve %v", rol, err)
		}
	}
	// El jefe operativo solo lo ve en PAGADO: tras aprobar, 404 (el histórico sigue para los demás).
	if _, err := svc.Historico(ctx, aJefe, id); categoriaDe(err) != platformerrors.CategoryNoEncontrado {
		t.Errorf("el jefe tras aprobar: esperaba 404, obtuve %v", err)
	}
	pag, _ := pagada(t, ctx, svc)
	hj, err := svc.Historico(ctx, aJefe, pag)
	if err != nil || len(hj.Items) != 3 || hj.Items[0].Accion != "creada" || hj.Items[1].Accion != "cotizada" || hj.Items[2].Accion != "pago_confirmado" {
		t.Errorf("histórico del jefe en PAGADO: %v %+v", err, hj)
	}
	if _, err := svc.Historico(ctx, aCot, uuid.New()); categoriaDe(err) != platformerrors.CategoryNoEncontrado {
		t.Errorf("inexistente: esperaba 404, obtuve %v", err)
	}
}
