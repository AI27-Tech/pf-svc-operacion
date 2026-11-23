package solicitudes

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	operclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/operacion"
	platformerrors "github.com/AI27-Tech/pf-pkg-platform-go/errors"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Seguimiento de DEC-148 #67 (el jefe operativo no ve nada comercial), #68 (aprobar
// con la fecha pasada) y #70 (la auditoría de `aprobada` lleva el viaje_id).

// Valores comerciales escritos a mano: ninguno debe llegarle al jefe operativo.
const (
	montoV1    = "1500.50"
	montoV2    = "2500.75"
	notasV1    = "nota-primera"
	notasV2    = "nota-secreta"
	referencia = "REF-9"
)

// pagadaComercial deja una solicitud PAGADO con dos versiones de cotización (monto,
// notas) y un pago con referencia, y devuelve su id y el del cliente.
func pagadaComercial(t *testing.T, ctx testCtx, svc *Servicio) (uuid.UUID, uuid.UUID) {
	t.Helper()
	cli := uuid.New()
	conRelojEn(svc, time.Date(2026, 11, 21, 6, 0, 0, 0, time.UTC))
	s, err := svc.Crear(ctx, actorCliente(cli), operclient.CrearSolicitudEntrada{
		Origen: "Manzanillo", Destinos: []operclient.Destino{{Nombre: "Guadalajara"}}, TipoServicio: ptr("escolta"),
		Tracto: &operclient.Tracto{Placas: ptr("ABC-123")},
	})
	if err != nil {
		t.Fatal(err)
	}
	fecha := time.Date(2026, 12, 10, 16, 0, 0, 0, time.UTC)
	conRelojEn(svc, time.Date(2026, 11, 21, 7, 0, 0, 0, time.UTC))
	if _, err := svc.Cotizar(ctx, aCot, s.Id, operclient.CotizacionEntrada{Monto: montoV1, Moneda: "MXN", FechaHoraServicio: fecha, Notas: ptr(notasV1)}); err != nil {
		t.Fatal(err)
	}
	conRelojEn(svc, time.Date(2026, 11, 21, 8, 0, 0, 0, time.UTC))
	if _, err := svc.Cotizar(ctx, aCot, s.Id, operclient.CotizacionEntrada{Monto: montoV2, Moneda: "MXN", FechaHoraServicio: fecha, Notas: ptr(notasV2)}); err != nil {
		t.Fatal(err)
	}
	conRelojEn(svc, time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC))
	if _, err := svc.ConfirmarPago(ctx, aCot, s.Id, operclient.PagoEntrada{FechaPago: dia("2026-11-25"), Referencia: ptr(referencia)}); err != nil {
		t.Fatal(err)
	}
	return s.Id, cli
}

// sinComercial falla si el JSON de v trae algún valor comercial o la clave `monto`.
func sinComercial(t *testing.T, donde string, v any) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	for _, prohibido := range []string{montoV1, montoV2, notasV1, notasV2, referencia, `"monto"`, `"notas"`, `"referencia"`} {
		if strings.Contains(string(raw), prohibido) {
			t.Errorf("%s: el JSON del jefe operativo trae %s: %s", donde, prohibido, raw)
		}
	}
}

// TestVisibilidadComercialPorRol: tabla rol × lo comercial que ve en detalle,
// listado e histórico (DEC-148 #67). Valores escritos a mano.
func TestVisibilidadComercialPorRol(t *testing.T) {
	ctx, svc := prepara(t)
	id, cli := pagadaComercial(t, ctx, svc)
	conRelojEn(svc, time.Date(2026, 11, 22, 6, 0, 0, 0, time.UTC))

	tabla := []struct {
		actor        Actor
		cotizacion   bool // ve la cotización vigente (v2) en detalle y listado
		pago         bool // ve el pago en detalle y listado
		verHistorico bool // el histórico responde (el cliente recibe 403)
	}{
		{actorCliente(cli), true, true, false},
		{aCot, true, true, true},
		{aAdmin, true, true, true},
		{aJefe, false, false, true},
	}
	for _, c := range tabla {
		t.Run(c.actor.Rol, func(t *testing.T) {
			revisa := func(donde string, s operclient.Solicitud) {
				if c.cotizacion {
					if s.Cotizacion == nil || s.Cotizacion.Monto != montoV2 || s.Cotizacion.Version != 2 || s.Cotizacion.Notas == nil || *s.Cotizacion.Notas != notasV2 {
						t.Errorf("%s: cotización = %+v", donde, s.Cotizacion)
					}
				} else if s.Cotizacion != nil {
					t.Errorf("%s: no debe traer cotización, trae %+v", donde, s.Cotizacion)
				}
				if c.pago {
					if s.Pago == nil || s.Pago.Referencia == nil || *s.Pago.Referencia != referencia || s.Pago.FechaPago.Format("2006-01-02") != "2026-11-25" {
						t.Errorf("%s: pago = %+v", donde, s.Pago)
					}
				} else if s.Pago != nil {
					t.Errorf("%s: no debe traer pago, trae %+v", donde, s.Pago)
				}
				// Lo no comercial lo ven todos, también el jefe.
				if s.Estado != "PAGADO" || s.Origen != "Manzanillo" || len(s.Destinos) != 1 || s.Destinos[0].Nombre != "Guadalajara" ||
					s.FechaHoraServicio == nil || !s.FechaHoraServicio.Equal(time.Date(2026, 12, 10, 16, 0, 0, 0, time.UTC)) ||
					s.TipoServicio == nil || *s.TipoServicio != "escolta" || s.Tracto == nil || *s.Tracto.Placas != "ABC-123" || s.MarcaPenalizacion {
					t.Errorf("%s: la solicitud perdió campos no comerciales: %+v", donde, s)
				}
				if !c.cotizacion && !c.pago {
					sinComercial(t, donde, s)
				}
			}
			det, err := svc.Obtener(ctx, c.actor, id)
			if err != nil {
				t.Fatal(err)
			}
			revisa("detalle", *det)
			pag, err := svc.Listar(ctx, c.actor, nil, nil)
			if err != nil || len(pag.Items) != 1 || pag.Items[0].Id != id {
				t.Fatalf("listado: %v %+v", err, pag)
			}
			revisa("listado", pag.Items[0])

			h, err := svc.Historico(ctx, c.actor, id)
			if !c.verHistorico {
				if categoriaDe(err) != platformerrors.CategoryProhibido {
					t.Errorf("histórico: esperaba 403, obtuve %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			// El jefe ve las mismas acciones, en el mismo orden; solo cambia el objeto cotizacion.
			var acciones []string
			versiones := map[string]string{}
			for _, it := range h.Items {
				acciones = append(acciones, it.Accion)
				if it.Cotizacion != nil {
					versiones[it.Accion] = it.Cotizacion.Monto + "/" + *it.Cotizacion.Notas
				}
			}
			if got := strings.Join(acciones, ","); got != "creada,cotizada,recotizada,pago_confirmado" {
				t.Errorf("acciones del histórico = %s", got)
			}
			if c.cotizacion {
				if versiones["cotizada"] != montoV1+"/"+notasV1 || versiones["recotizada"] != montoV2+"/"+notasV2 || len(versiones) != 2 {
					t.Errorf("cotizaciones del histórico = %v", versiones)
				}
			} else {
				if len(versiones) != 0 {
					t.Errorf("el histórico del jefe trae cotizaciones: %v", versiones)
				}
				sinComercial(t, "histórico", h)
				// Conserva instante, estados y actor de cada entrada.
				if h.Items[1].AEstado == nil || *h.Items[1].AEstado != "COTIZADO" || !h.Items[2].En.Equal(time.Date(2026, 11, 21, 8, 0, 0, 0, time.UTC)) ||
					h.Items[2].ActorId == nil || h.Items[2].ActorId.String() != subCotizacion {
					t.Errorf("el histórico del jefe perdió datos no comerciales: %+v", h.Items)
				}
			}
		})
	}
}

// consultasRegistradas deja pasar a la base y anota el SQL de cada consulta.
type consultasRegistradas struct {
	BeginTx
	sqls *[]string
}

func (c consultasRegistradas) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := c.BeginTx.Begin(ctx)
	return txRegistrada{tx, c.sqls}, err
}

type txRegistrada struct {
	pgx.Tx
	sqls *[]string
}

func (x txRegistrada) Query(ctx context.Context, sql string, a ...any) (pgx.Rows, error) {
	*x.sqls = append(*x.sqls, sql)
	return x.Tx.Query(ctx, sql, a...)
}

func (x txRegistrada) QueryRow(ctx context.Context, sql string, a ...any) pgx.Row {
	*x.sqls = append(*x.sqls, sql)
	return x.Tx.QueryRow(ctx, sql, a...)
}

// TestLaConsultaDelJefeNoCargaLoComercial: para el jefe operativo no se lee ni el
// monto, ni las notas, ni la tabla del pago (DEC-148 #67). El control con el rol
// cotizacion demuestra que la prueba sí detecta esas lecturas.
func TestLaConsultaDelJefeNoCargaLoComercial(t *testing.T) {
	ctx, svc := prepara(t)
	id, _ := pagadaComercial(t, ctx, svc)
	leidas := func(a Actor) string {
		var sqls []string
		s := NewServicio(consultasRegistradas{svc.tx, &sqls}, svc.pub).ConReloj(svc.ahora)
		if _, err := s.Obtener(ctx, a, id); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Listar(ctx, a, nil, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Historico(ctx, a, id); err != nil {
			t.Fatal(err)
		}
		return strings.ToLower(strings.Join(sqls, "\n"))
	}
	if sql := leidas(aCot); !strings.Contains(sql, "monto") || !strings.Contains(sql, "pago_solicitud") || !strings.Contains(sql, "notas") {
		t.Fatalf("control: el rol cotizacion debería leer lo comercial:\n%s", sql)
	}
	sql := leidas(aJefe)
	for _, prohibido := range []string{"monto", "moneda", "notas", "pago_solicitud", "referencia"} {
		if strings.Contains(sql, prohibido) {
			t.Errorf("la consulta del jefe lee %q:\n%s", prohibido, sql)
		}
	}
}

// TestLaRespuestaDeAprobarYRechazarNoTraeLoComercial: las acciones del jefe
// devuelven la solicitud armada para él.
func TestLaRespuestaDeAprobarYRechazarNoTraeLoComercial(t *testing.T) {
	ctx, svc := prepara(t)
	apr, _ := pagadaComercial(t, ctx, svc)
	rec, _ := pagadaComercial(t, ctx, svc)
	conRelojEn(svc, time.Date(2026, 11, 22, 6, 0, 0, 0, time.UTC))
	s, err := svc.Aprobar(ctx, aJefe, apr)
	if err != nil || s.Estado != "APROBADO" || s.Cotizacion != nil || s.Pago != nil {
		t.Errorf("aprobar: %v %+v", err, s)
	}
	s, err = svc.Rechazar(ctx, aJefe, rec, operclient.MotivoEntrada{Motivo: "sin escolta"})
	if err != nil || s.Estado != "RECHAZADO_POR_AI27" || s.Cotizacion != nil || s.Pago != nil {
		t.Errorf("rechazar: %v %+v", err, s)
	}
}

// TestAprobarConLaFechaDelServicioPasada (DEC-148 #68): el servicio es el
// 2026-12-10 16:00 UTC (10:00 en Ciudad de México, UTC-6). Se aprueba solo si ese
// instante es posterior al actual; igual al actual ya no.
func TestAprobarConLaFechaDelServicioPasada(t *testing.T) {
	mx, err := time.LoadLocation("America/Mexico_City")
	if err != nil {
		t.Fatal(err)
	}
	casos := []struct {
		nombre string
		ahora  time.Time
		ok     bool
	}{
		{"un segundo antes", time.Date(2026, 12, 10, 15, 59, 59, 0, time.UTC), true},
		{"el mismo instante", time.Date(2026, 12, 10, 16, 0, 0, 0, time.UTC), false},
		{"un segundo después", time.Date(2026, 12, 10, 16, 0, 1, 0, time.UTC), false},
		{"un día después", time.Date(2026, 12, 11, 16, 0, 0, 0, time.UTC), false},
		{"mismo instante medido en Ciudad de México", time.Date(2026, 12, 10, 10, 0, 0, 0, mx), false},
		{"un segundo antes medido en Ciudad de México", time.Date(2026, 12, 10, 9, 59, 59, 0, mx), true},
	}
	for _, c := range casos {
		t.Run(c.nombre, func(t *testing.T) {
			ctx, svc := prepara(t)
			id, _ := pagada(t, ctx, svc)
			conRelojEn(svc, c.ahora)
			s, err := svc.Aprobar(ctx, aJefe, id)
			if c.ok {
				if err != nil || s.Estado != "APROBADO" || cuentaViajes(t, ctx, id) != 1 {
					t.Fatalf("debía aprobar: %v %+v", err, s)
				}
				return
			}
			if codigoDe(err) != "fecha_hora_servicio_pasada" || categoriaDe(err) != platformerrors.CategoryConflicto {
				t.Fatalf("esperaba 409 fecha_hora_servicio_pasada, obtuve %v", err)
			}
			// La solicitud no cambia: sin estado nuevo, viaje, transición, auditoría ni eventos.
			if got := estadoDe(t, ctx, id); got != "PAGADO" {
				t.Errorf("estado = %s, quería PAGADO", got)
			}
			if cuentaViajes(t, ctx, id) != 0 || nTransiciones(t, ctx, id, "APROBADO") != 0 ||
				consulta[int](t, ctx, `SELECT count(*) FROM auditoria.registro_auditoria WHERE accion='aprobada'`) != 0 ||
				consulta[int](t, ctx, `SELECT count(*) FROM viajes.outbox`) != 0 ||
				consulta[int](t, ctx, `SELECT count(*) FROM solicitudes.outbox WHERE payload->>'a_estado'='APROBADO'`) != 0 {
				t.Error("quedó rastro de una aprobación rechazada")
			}
		})
	}
}

// TestAuditoriaDeAprobadaLlevaElViajeId (DEC-148 #70): solo `aprobada` lleva
// viaje_id (el id de la solicitud) y la fila se escribe DESPUÉS de crear el viaje.
func TestAuditoriaDeAprobadaLlevaElViajeId(t *testing.T) {
	ctx, svc := prepara(t)
	// Trigger de prueba: una fila de auditoría con viaje_id exige que el viaje ya exista.
	ejecuta(t, ctx, `
	  CREATE OR REPLACE FUNCTION auditoria.exige_viaje_creado() RETURNS trigger LANGUAGE plpgsql SECURITY DEFINER AS $f$
	  BEGIN
	    IF NEW.viaje_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM viajes.viaje WHERE id = NEW.viaje_id) THEN
	      RAISE EXCEPTION 'la auditoría de % antecede al viaje', NEW.accion;
	    END IF;
	    RETURN NEW;
	  END $f$;
	  CREATE TRIGGER exige_viaje_creado BEFORE INSERT ON auditoria.registro_auditoria
	    FOR EACH ROW EXECUTE FUNCTION auditoria.exige_viaje_creado()`)
	t.Cleanup(func() {
		ejecuta(t, ctx, `DROP TRIGGER IF EXISTS exige_viaje_creado ON auditoria.registro_auditoria; DROP FUNCTION IF EXISTS auditoria.exige_viaje_creado()`)
	})

	apr, _ := recorrido(t, ctx, svc) // crea, cotiza, recotiza, reprograma, paga, revierte, paga y aprueba
	rec, _ := pagada(t, ctx, svc)
	if _, err := svc.Rechazar(ctx, aJefe, rec, operclient.MotivoEntrada{Motivo: "sin escolta"}); err != nil {
		t.Fatal(err)
	}

	if n := consulta[int](t, ctx, `SELECT count(*) FROM auditoria.registro_auditoria WHERE accion='aprobada' AND viaje_id=$1 AND detalle->>'solicitud_id'=$1::text`, apr); n != 1 {
		t.Errorf("auditoría de aprobada con viaje_id = %d, quería 1", n)
	}
	// Las demás acciones, de ésta y de la rechazada, conservan el id en el detalle y no llevan viaje_id.
	if n := consulta[int](t, ctx, `SELECT count(*) FROM auditoria.registro_auditoria WHERE accion<>'aprobada' AND viaje_id IS NOT NULL`); n != 0 {
		t.Errorf("%d filas que no son `aprobada` traen viaje_id", n)
	}
	if n := consulta[int](t, ctx, `SELECT count(*) FROM auditoria.registro_auditoria WHERE accion='rechazada' AND viaje_id IS NULL AND detalle->>'solicitud_id'=$1::text`, rec); n != 1 {
		t.Errorf("auditoría de rechazada sin viaje_id = %d, quería 1", n)
	}
	// Las otras acciones de la solicitud aprobada (creada, cotizada, pagos, ...) siguen en su fila.
	if n := consulta[int](t, ctx, `SELECT count(*) FROM auditoria.registro_auditoria WHERE detalle->>'solicitud_id'=$1::text AND viaje_id IS NULL`, apr); n < 5 {
		t.Errorf("las demás acciones de la solicitud aprobada conservan su fila sin viaje_id (%d)", n)
	}
}
