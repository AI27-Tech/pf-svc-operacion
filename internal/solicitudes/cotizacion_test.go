package solicitudes

import (
	"fmt"
	"testing"
	"time"

	"github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/estados"
	operclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/operacion"
	platformerrors "github.com/AI27-Tech/pf-pkg-platform-go/errors"
	"github.com/google/uuid"
)

var actorCotizacion = Actor{Subject: "cot-1", Rol: "cotizacion"}

func estadoDe(t *testing.T, ctx testCtx, id uuid.UUID) string {
	t.Helper()
	return consulta[string](t, ctx, `SELECT estado FROM solicitudes.solicitud WHERE id=$1`, id)
}

func cuenta(t *testing.T, ctx testCtx, tabla string, id uuid.UUID) int {
	t.Helper()
	col := "solicitud_id"
	if tabla == "outbox" {
		return consulta[int](t, ctx, `SELECT count(*) FROM solicitudes.outbox WHERE agregado_id=$1`, id.String())
	}
	return consulta[int](t, ctx, fmt.Sprintf(`SELECT count(*) FROM solicitudes.%s WHERE %s=$1`, tabla, col), id)
}

func cotizacionBase() operclient.CotizacionEntrada {
	return operclient.CotizacionEntrada{
		Monto: "1500.50", Moneda: "MXN", FechaHoraServicio: time.Date(2026, 12, 10, 16, 0, 0, 0, time.UTC),
	}
}

// TestVigenciaEnZonaMexico: la vigencia cuenta días calendario de
// America/Mexico_City, no de UTC (DEC-148 #15, #50). Reloj inyectado, sin base.
func TestVigenciaEnZonaMexico(t *testing.T) {
	casos := []struct {
		entrega time.Time
		want    string
	}{
		// 23:30 del 20 en CDMX = 05:30 UTC del 21: el día de CDMX es el 20.
		{time.Date(2026, 11, 21, 5, 30, 0, 0, time.UTC), "2026-12-05"},
		// 18:30 del 20 en CDMX = 00:30 UTC del 21: sigue siendo el 20 en CDMX.
		{time.Date(2026, 11, 21, 0, 30, 0, 0, time.UTC), "2026-12-05"},
		// 06:00 UTC del 21 = 00:00 del 21 en CDMX: ya es el 21.
		{time.Date(2026, 11, 21, 6, 0, 0, 0, time.UTC), "2026-12-06"},
		{time.Date(2026, 12, 20, 12, 0, 0, 0, time.UTC), "2027-01-04"},
	}
	for _, c := range casos {
		if got := vigenteHasta(c.entrega).Format("2006-01-02"); got != c.want {
			t.Errorf("entrega %s: vigente_hasta = %s, quería %s", c.entrega, got, c.want)
		}
	}
}

func TestCotizar(t *testing.T) {
	ctx, svc := prepara(t)
	entrega := time.Date(2026, 11, 21, 5, 30, 0, 0, time.UTC) // 20-nov 23:30 en CDMX
	svc.ConReloj(func() time.Time { return entrega })

	nueva := func(t *testing.T) (uuid.UUID, Actor) {
		cli := uuid.New()
		s, err := svc.Crear(ctx, actorCliente(cli), operclient.CrearSolicitudEntrada{Origen: "A", Destinos: []operclient.Destino{{Nombre: "B"}}})
		if err != nil {
			t.Fatal(err)
		}
		return s.Id, actorCliente(cli)
	}

	t.Run("cotiza, recotiza y conserva las versiones", func(t *testing.T) {
		id, _ := nueva(t)
		s, err := svc.Cotizar(ctx, actorCotizacion, id, cotizacionBase())
		if err != nil {
			t.Fatalf("Cotizar: %v", err)
		}
		if s.Estado != operclient.EstadoSolicitud("COTIZADO") || s.Cotizacion == nil || s.Cotizacion.Version != 1 ||
			s.Cotizacion.Monto != "1500.50" || s.Cotizacion.VigenteHasta.Format("2006-01-02") != "2026-12-05" {
			t.Fatalf("solicitud tras cotizar = %+v / %+v", s, s.Cotizacion)
		}
		if s.FechaHoraServicio == nil || !s.FechaHoraServicio.Equal(cotizacionBase().FechaHoraServicio) {
			t.Errorf("la fecha del servicio debe ser la de la cotización: %v", s.FechaHoraServicio)
		}
		if n := cuenta(t, ctx, "transicion_solicitud", id); n != 1 {
			t.Errorf("transiciones = %d", n)
		}

		// Recotizar un día después (reloj movido): vigencia desde la última entrega.
		entrega = entrega.Add(24 * time.Hour)
		r := cotizacionBase()
		r.Monto, r.Notas = "1700", ptr("incluye casetas")
		s2, err := svc.Cotizar(ctx, actorCotizacion, id, r)
		if err != nil {
			t.Fatalf("recotizar: %v", err)
		}
		if s2.Estado != operclient.EstadoSolicitud("COTIZADO") || s2.Cotizacion.Version != 2 || s2.Cotizacion.Monto != "1700" ||
			s2.Cotizacion.VigenteHasta.Format("2006-01-02") != "2026-12-06" || *s2.Cotizacion.Notas != "incluye casetas" {
			t.Fatalf("recotización = %+v", s2.Cotizacion)
		}
		if n := consulta[int](t, ctx, `SELECT count(*) FROM solicitudes.cotizacion WHERE solicitud_id=$1`, id); n != 2 {
			t.Errorf("versiones guardadas = %d, quería 2", n)
		}
		if m := consulta[string](t, ctx, `SELECT monto::text FROM solicitudes.cotizacion WHERE solicitud_id=$1 AND version=1`, id); m != "1500.50" {
			t.Errorf("la versión 1 cambió: %s", m)
		}
		if n := cuenta(t, ctx, "transicion_solicitud", id); n != 1 {
			t.Errorf("recotizar no cambia de estado: transiciones = %d", n)
		}
		if n := consulta[int](t, ctx, `SELECT count(*) FROM solicitudes.outbox WHERE agregado_id=$1 AND tipo='solicitud.estado_cambiado'`, id.String()); n != 1 {
			t.Errorf("estado_cambiado = %d, quería 1 (recotizar no publica)", n)
		}
	})

	t.Run("entrada inválida: la solicitud no cambia", func(t *testing.T) {
		id, _ := nueva(t)
		mal := map[string]func(*operclient.CotizacionEntrada){
			"sin monto":   func(c *operclient.CotizacionEntrada) { c.Monto = "" },
			"monto texto": func(c *operclient.CotizacionEntrada) { c.Monto = "mucho" },
			"monto neg":   func(c *operclient.CotizacionEntrada) { c.Monto = "-10" },
			"sin moneda":  func(c *operclient.CotizacionEntrada) { c.Moneda = "  " },
			"sin fecha":   func(c *operclient.CotizacionEntrada) { c.FechaHoraServicio = time.Time{} },
		}
		for nombre, mod := range mal {
			c := cotizacionBase()
			mod(&c)
			if _, err := svc.Cotizar(ctx, actorCotizacion, id, c); categoriaDe(err) != platformerrors.CategoryValidacion {
				t.Errorf("%s: esperaba 400, obtuve %v", nombre, err)
			}
		}
		if estadoDe(t, ctx, id) != "SOLICITADO" || cuenta(t, ctx, "cotizacion", id) != 0 || cuenta(t, ctx, "transicion_solicitud", id) != 0 {
			t.Errorf("la solicitud cambió tras entradas inválidas")
		}
	})

	t.Run("estados que no admiten cotizar", func(t *testing.T) {
		for _, e := range []estados.EstadoSolicitud{
			estados.EstadoSolicitudPagado, estados.EstadoSolicitudVencida, estados.EstadoSolicitudAprobado,
			estados.EstadoSolicitudRechazadoPorAi27, estados.EstadoSolicitudRechazadoPorCliente, estados.EstadoSolicitudCanceladoPorCliente,
		} {
			id, _ := sembrar(t, ctx, e)
			_, err := svc.Cotizar(ctx, actorCotizacion, id, cotizacionBase())
			if codigoDe(err) != "transicion_no_permitida" {
				t.Errorf("%s: esperaba transicion_no_permitida, obtuve %v", e, err)
			}
			if estadoDe(t, ctx, id) != string(e) || cuenta(t, ctx, "cotizacion", id) != 0 {
				t.Errorf("%s: la solicitud cambió", e)
			}
		}
	})

	t.Run("solicitud inexistente", func(t *testing.T) {
		if _, err := svc.Cotizar(ctx, actorCotizacion, uuid.New(), cotizacionBase()); categoriaDe(err) != platformerrors.CategoryNoEncontrado {
			t.Errorf("esperaba 404, obtuve %v", err)
		}
	})
}

func TestReprogramar(t *testing.T) {
	ctx, svc := prepara(t)
	actor := Actor{Subject: "adm-1", Rol: "administrador"}
	nueva := time.Date(2026, 12, 20, 9, 0, 0, 0, time.UTC)

	t.Run("en PAGADO registra actor, motivo y fecha; no cambia el estado", func(t *testing.T) {
		id, _ := sembrar(t, ctx, estados.EstadoSolicitudPagado)
		ejecuta(t, ctx, `UPDATE solicitudes.solicitud SET fecha_hora_servicio = '2026-12-10T16:00:00Z' WHERE id=$1`, id)
		s, err := svc.Reprogramar(ctx, actor, id, operclient.ReprogramacionEntrada{FechaHoraServicio: nueva, Motivo: "  acordado en el chat  "})
		if err != nil {
			t.Fatalf("Reprogramar: %v", err)
		}
		if s.Estado != operclient.EstadoSolicitud("PAGADO") || s.FechaHoraServicio == nil || !s.FechaHoraServicio.Equal(nueva) {
			t.Errorf("solicitud = %+v", s)
		}
		var motivo, rol, sub string
		var anterior, fecha time.Time
		c, cerrar := adminConn(t, ctx)
		defer cerrar()
		if err := c.QueryRow(ctx, `SELECT motivo, actor_rol, actor_id, fecha_hora_anterior, fecha_hora_nueva FROM solicitudes.reprogramacion_solicitud WHERE solicitud_id=$1`, id).
			Scan(&motivo, &rol, &sub, &anterior, &fecha); err != nil {
			t.Fatalf("histórico de reprogramación: %v", err)
		}
		if motivo != "acordado en el chat" || rol != "administrador" || sub != "adm-1" || !fecha.Equal(nueva) ||
			!anterior.Equal(time.Date(2026, 12, 10, 16, 0, 0, 0, time.UTC)) {
			t.Errorf("histórico = %q %s %s %v %v", motivo, rol, sub, anterior, fecha)
		}
		if cuenta(t, ctx, "transicion_solicitud", id) != 0 || cuenta(t, ctx, "outbox", id) != 0 {
			t.Errorf("reprogramar no cambia de estado ni publica")
		}
	})
	t.Run("sin motivo o sin fecha", func(t *testing.T) {
		id, _ := sembrar(t, ctx, estados.EstadoSolicitudPagado)
		for _, in := range []operclient.ReprogramacionEntrada{
			{FechaHoraServicio: nueva, Motivo: ""}, {FechaHoraServicio: nueva, Motivo: "   "}, {Motivo: "x"},
		} {
			if _, err := svc.Reprogramar(ctx, actor, id, in); categoriaDe(err) != platformerrors.CategoryValidacion {
				t.Errorf("%+v: esperaba 400, obtuve %v", in, err)
			}
		}
		if cuenta(t, ctx, "reprogramacion_solicitud", id) != 0 {
			t.Errorf("se registró una reprogramación inválida")
		}
	})
	t.Run("fuera de PAGADO", func(t *testing.T) {
		for _, e := range []estados.EstadoSolicitud{
			estados.EstadoSolicitudSolicitado, estados.EstadoSolicitudCotizado, estados.EstadoSolicitudVencida, estados.EstadoSolicitudAprobado,
			estados.EstadoSolicitudRechazadoPorAi27, estados.EstadoSolicitudRechazadoPorCliente, estados.EstadoSolicitudCanceladoPorCliente,
		} {
			id, _ := sembrar(t, ctx, e)
			if _, err := svc.Reprogramar(ctx, actor, id, operclient.ReprogramacionEntrada{FechaHoraServicio: nueva, Motivo: "x"}); codigoDe(err) != "transicion_no_permitida" {
				t.Errorf("%s: esperaba transicion_no_permitida, obtuve %v", e, err)
			}
			if cuenta(t, ctx, "reprogramacion_solicitud", id) != 0 {
				t.Errorf("%s: se registró una reprogramación", e)
			}
		}
	})
}

// TestCotizarPersisteFechaDelServicio: la fecha de la cotización queda en
// solicitud.fecha_hora_servicio (se lee de la base, no de la respuesta): E10-T8
// la usa para la guarda de DEC-010 #2 y para el viaje. Una recotización la cambia.
func TestCotizarPersisteFechaDelServicio(t *testing.T) {
	ctx, svc := prepara(t)
	id, _ := sembrar(t, ctx, estados.EstadoSolicitudSolicitado) // sin fecha del cliente
	enBase := func() time.Time {
		p := consulta[*time.Time](t, ctx, `SELECT fecha_hora_servicio FROM solicitudes.solicitud WHERE id=$1`, id)
		if p == nil {
			return time.Time{}
		}
		return *p
	}
	f1 := time.Date(2026, 12, 10, 16, 0, 0, 0, time.UTC)
	in := cotizacionBase()
	in.FechaHoraServicio = f1
	if _, err := svc.Cotizar(ctx, actorCotizacion, id, in); err != nil {
		t.Fatalf("cotizar: %v", err)
	}
	if got := enBase(); !got.Equal(f1) {
		t.Errorf("tras cotizar, solicitud.fecha_hora_servicio = %v, quería %v", got, f1)
	}
	f2 := f1.Add(48 * time.Hour)
	in.FechaHoraServicio = f2
	if _, err := svc.Cotizar(ctx, actorCotizacion, id, in); err != nil {
		t.Fatalf("recotizar: %v", err)
	}
	if got := enBase(); !got.Equal(f2) {
		t.Errorf("tras recotizar, solicitud.fecha_hora_servicio = %v, quería %v", got, f2)
	}
}
