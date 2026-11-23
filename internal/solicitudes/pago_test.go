package solicitudes

import (
	"strings"
	"testing"
	"time"

	"github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/estados"
	operclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/operacion"
	platformerrors "github.com/AI27-Tech/pf-pkg-platform-go/errors"
	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"
)

func dia(s string) openapi_types.Date {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		panic(err)
	}
	return openapi_types.Date{Time: t}
}

// cotizada deja una solicitud COTIZADO entregada el 20-nov-2026 a las 23:30 en
// CDMX (vigente hasta el 5-dic-2026).
func cotizada(t *testing.T, ctx testCtx, svc *Servicio) uuid.UUID {
	t.Helper()
	svc.ConReloj(func() time.Time { return time.Date(2026, 11, 21, 5, 30, 0, 0, time.UTC) })
	cli := uuid.New()
	s, err := svc.Crear(ctx, actorCliente(cli), operclient.CrearSolicitudEntrada{Origen: "A", Destinos: []operclient.Destino{{Nombre: "B"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Cotizar(ctx, actorCotizacion, s.Id, cotizacionBase()); err != nil {
		t.Fatal(err)
	}
	return s.Id
}

var actorAdmin = Actor{Subject: "adm-1", Rol: "administrador"}

func TestConfirmarPago(t *testing.T) {
	ctx, svc := prepara(t)

	t.Run("frontera de vigencia: día de entrega y último día sí; un día antes y un día después no", func(t *testing.T) {
		for _, c := range []struct {
			fecha string
			ok    bool
		}{
			{"2026-11-19", false}, {"2026-11-20", true}, {"2026-12-05", true}, {"2026-12-06", false},
		} {
			id := cotizada(t, ctx, svc)
			s, err := svc.ConfirmarPago(ctx, actorCotizacion, id, operclient.PagoEntrada{FechaPago: dia(c.fecha)})
			if c.ok {
				if err != nil || s.Estado != operclient.EstadoSolicitud("PAGADO") || s.Pago == nil || s.Pago.FechaPago.Format(time.DateOnly) != c.fecha {
					t.Errorf("%s: esperaba pago confirmado, obtuve %v / %+v", c.fecha, err, s)
				}
				continue
			}
			if codigoDe(err) != "fecha_pago_fuera_de_vigencia" || categoriaDe(err) != platformerrors.CategoryConflicto {
				t.Errorf("%s: esperaba 409 fecha_pago_fuera_de_vigencia, obtuve %v", c.fecha, err)
			}
			if estadoDe(t, ctx, id) != "COTIZADO" || cuenta(t, ctx, "pago_solicitud", id) != 0 || cuenta(t, ctx, "transicion_solicitud", id) != 1 {
				t.Errorf("%s: el rechazo dejó cambios", c.fecha)
			}
		}
	})

	t.Run("gana la fecha del pago aunque se confirme después, y guarda la referencia", func(t *testing.T) {
		id := cotizada(t, ctx, svc)
		svc.ConReloj(func() time.Time { return time.Date(2026, 12, 8, 12, 0, 0, 0, time.UTC) }) // confirmación fuera de vigencia
		s, err := svc.ConfirmarPago(ctx, actorCotizacion, id, operclient.PagoEntrada{FechaPago: dia("2026-12-05"), Referencia: ptr(" folio 77 ")})
		if err != nil || s.Estado != operclient.EstadoSolicitud("PAGADO") || *s.Pago.Referencia != "folio 77" {
			t.Fatalf("pago tardío en confirmación pero dentro de la vigencia: %v / %+v", err, s)
		}
		if !s.Pago.ConfirmadoEn.Equal(time.Date(2026, 12, 8, 12, 0, 0, 0, time.UTC)) {
			t.Errorf("confirmado_en = %v", s.Pago.ConfirmadoEn)
		}
		if consulta[string](t, ctx, `SELECT accion FROM solicitudes.transicion_solicitud WHERE solicitud_id=$1 AND a_estado='PAGADO'`, id) != "pago_confirmado" {
			t.Errorf("la acción del histórico no es pago_confirmado")
		}
	})

	t.Run("sin fecha del pago", func(t *testing.T) {
		id := cotizada(t, ctx, svc)
		if _, err := svc.ConfirmarPago(ctx, actorCotizacion, id, operclient.PagoEntrada{}); categoriaDe(err) != platformerrors.CategoryValidacion {
			t.Errorf("esperaba 400, obtuve %v", err)
		}
		if estadoDe(t, ctx, id) != "COTIZADO" {
			t.Errorf("el estado cambió")
		}
	})

	t.Run("en SOLICITADO, PAGADO y terminales se rechaza", func(t *testing.T) {
		for _, e := range []estados.EstadoSolicitud{
			estados.EstadoSolicitudSolicitado, estados.EstadoSolicitudPagado, estados.EstadoSolicitudAprobado,
			estados.EstadoSolicitudRechazadoPorAi27, estados.EstadoSolicitudRechazadoPorCliente, estados.EstadoSolicitudCanceladoPorCliente,
		} {
			id, _ := sembrar(t, ctx, e)
			if _, err := svc.ConfirmarPago(ctx, actorCotizacion, id, operclient.PagoEntrada{FechaPago: dia("2026-11-25")}); codigoDe(err) != "transicion_no_permitida" {
				t.Errorf("%s: esperaba transicion_no_permitida, obtuve %v", e, err)
			}
			if estadoDe(t, ctx, id) != string(e) || cuenta(t, ctx, "pago_solicitud", id) != 0 {
				t.Errorf("%s: la solicitud cambió", e)
			}
		}
	})

	t.Run("VENCIDA solo admite la excepción: pago dentro de la vigencia, registrado como tardío", func(t *testing.T) {
		id := cotizada(t, ctx, svc)
		ejecuta(t, ctx, `UPDATE solicitudes.solicitud SET estado='VENCIDA' WHERE id=$1`, id) // la siembra del vencimiento (E10-T7 aún no existe)
		if _, err := svc.ConfirmarPago(ctx, actorCotizacion, id, operclient.PagoEntrada{FechaPago: dia("2026-12-06")}); codigoDe(err) != "fecha_pago_fuera_de_vigencia" {
			t.Errorf("VENCIDA con pago fuera de vigencia: %v", err)
		}
		if estadoDe(t, ctx, id) != "VENCIDA" {
			t.Fatalf("el rechazo cambió el estado")
		}
		s, err := svc.ConfirmarPago(ctx, actorCotizacion, id, operclient.PagoEntrada{FechaPago: dia("2026-12-05")})
		if err != nil || s.Estado != operclient.EstadoSolicitud("PAGADO") {
			t.Fatalf("la excepción VENCIDA -> PAGADO falló: %v", err)
		}
		if consulta[string](t, ctx, `SELECT accion FROM solicitudes.transicion_solicitud WHERE solicitud_id=$1 AND de_estado='VENCIDA'`, id) != "pago_confirmado_tardio" ||
			!consulta[bool](t, ctx, `SELECT tardio FROM solicitudes.pago_solicitud WHERE solicitud_id=$1`, id) {
			t.Errorf("la excepción no quedó registrada como tardía")
		}
	})

	t.Run("atomicidad: si falla guardar el pago, la solicitud sigue COTIZADO", func(t *testing.T) {
		id := cotizada(t, ctx, svc)
		ejecuta(t, ctx, `CREATE OR REPLACE FUNCTION solicitudes.falla() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'falla inyectada'; END $$;
			CREATE TRIGGER falla_prueba BEFORE INSERT ON solicitudes.pago_solicitud FOR EACH ROW EXECUTE FUNCTION solicitudes.falla()`)
		defer ejecuta(t, ctx, `DROP TRIGGER IF EXISTS falla_prueba ON solicitudes.pago_solicitud; DROP FUNCTION IF EXISTS solicitudes.falla()`)
		if _, err := svc.ConfirmarPago(ctx, actorCotizacion, id, operclient.PagoEntrada{FechaPago: dia("2026-11-25")}); err == nil || !strings.Contains(err.Error(), "falla inyectada") {
			t.Fatalf("esperaba la falla inyectada, obtuve %v", err)
		}
		if estadoDe(t, ctx, id) != "COTIZADO" || cuenta(t, ctx, "transicion_solicitud", id) != 1 || cuenta(t, ctx, "outbox", id) != 2 {
			t.Errorf("la transición sobrevivió a la falla")
		}
	})

	t.Run("un solo pago vigente por solicitud (restricción de la base)", func(t *testing.T) {
		id := cotizada(t, ctx, svc)
		if _, err := svc.ConfirmarPago(ctx, actorCotizacion, id, operclient.PagoEntrada{FechaPago: dia("2026-11-25")}); err != nil {
			t.Fatal(err)
		}
		c, cerrar := adminConn(t, ctx)
		defer cerrar()
		if _, err := c.Exec(ctx, `INSERT INTO solicitudes.pago_solicitud (solicitud_id, fecha_pago, confirmado_en, tardio, cotizacion_version) VALUES ($1,'2026-11-26',now(),false,1)`, id); err == nil ||
			!strings.Contains(err.Error(), "pago_vigente_unico_idx") {
			t.Errorf("un segundo pago vigente debía violar el índice único: %v", err)
		}
	})
}

func TestRevertirPago(t *testing.T) {
	ctx, svc := prepara(t)
	pagada := func(t *testing.T) uuid.UUID {
		id := cotizada(t, ctx, svc)
		if _, err := svc.ConfirmarPago(ctx, actorCotizacion, id, operclient.PagoEntrada{FechaPago: dia("2026-11-25")}); err != nil {
			t.Fatal(err)
		}
		return id
	}

	t.Run("PAGADO -> COTIZADO con motivo; el pago queda revertido y puede confirmarse de nuevo", func(t *testing.T) {
		id := pagada(t)
		s, err := svc.RevertirPago(ctx, actorAdmin, id, operclient.MotivoEntrada{Motivo: " se confirmó por error "})
		if err != nil {
			t.Fatalf("RevertirPago: %v", err)
		}
		if s.Estado != operclient.EstadoSolicitud("COTIZADO") || s.Pago != nil || s.Cotizacion == nil {
			t.Errorf("solicitud tras la reversa = %+v", s)
		}
		if consulta[string](t, ctx, `SELECT accion || '|' || motivo || '|' || actor_rol || '|' || disparador FROM solicitudes.transicion_solicitud WHERE solicitud_id=$1 AND a_estado='COTIZADO' AND de_estado='PAGADO'`, id) !=
			"pago_revertido|se confirmó por error|administrador|administrador" {
			t.Errorf("histórico de la reversa incorrecto")
		}
		if n := consulta[int](t, ctx, `SELECT count(*) FROM solicitudes.pago_solicitud WHERE solicitud_id=$1 AND revertido_en IS NOT NULL`, id); n != 1 {
			t.Errorf("pagos revertidos = %d, el pago no debe borrarse", n)
		}
		if _, err := svc.ConfirmarPago(ctx, actorCotizacion, id, operclient.PagoEntrada{FechaPago: dia("2026-11-26")}); err != nil {
			t.Errorf("confirmar de nuevo tras la reversa: %v", err)
		}
	})
	t.Run("sin motivo", func(t *testing.T) {
		id := pagada(t)
		for _, m := range []string{"", "  "} {
			if _, err := svc.RevertirPago(ctx, actorAdmin, id, operclient.MotivoEntrada{Motivo: m}); categoriaDe(err) != platformerrors.CategoryValidacion {
				t.Errorf("motivo %q: esperaba 400, obtuve %v", m, err)
			}
		}
		if estadoDe(t, ctx, id) != "PAGADO" {
			t.Errorf("el estado cambió")
		}
	})
	t.Run("solo mientras siga en PAGADO", func(t *testing.T) {
		for _, e := range []estados.EstadoSolicitud{
			estados.EstadoSolicitudSolicitado, estados.EstadoSolicitudCotizado, estados.EstadoSolicitudAprobado, estados.EstadoSolicitudVencida,
			estados.EstadoSolicitudRechazadoPorAi27, estados.EstadoSolicitudRechazadoPorCliente, estados.EstadoSolicitudCanceladoPorCliente,
		} {
			id, _ := sembrar(t, ctx, e)
			if _, err := svc.RevertirPago(ctx, actorAdmin, id, operclient.MotivoEntrada{Motivo: "x"}); codigoDe(err) != "transicion_no_permitida" {
				t.Errorf("%s: esperaba transicion_no_permitida, obtuve %v", e, err)
			}
			if estadoDe(t, ctx, id) != string(e) {
				t.Errorf("%s: el estado cambió", e)
			}
		}
	})
}
