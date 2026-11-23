package solicitudes

import (
	"testing"

	"github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/estados"
	operclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/operacion"
	platformerrors "github.com/AI27-Tech/pf-pkg-platform-go/errors"
	"github.com/google/uuid"
)

// TestRespuestaDelCliente cubre E10-T5: rechazar la cotización y cancelar.
func TestRespuestaDelCliente(t *testing.T) {
	ctx, svc := prepara(t)
	cancelar := func(a Actor, id uuid.UUID) (*operclient.Solicitud, error) {
		return svc.Cancelar(ctx, a, id, operclient.AccionClienteEntrada{})
	}
	rechazar := func(a Actor, id uuid.UUID) (*operclient.Solicitud, error) {
		return svc.RechazarCotizacion(ctx, a, id, operclient.AccionClienteEntrada{})
	}

	t.Run("cancelar: la marca la lleva solo la cancelación desde PAGADO", func(t *testing.T) {
		casos := []struct {
			de    estados.EstadoSolicitud
			marca bool
		}{
			{estados.EstadoSolicitudSolicitado, false}, {estados.EstadoSolicitudCotizado, false}, {estados.EstadoSolicitudPagado, true},
		}
		for _, c := range casos {
			id, cli := sembrar(t, ctx, c.de)
			s, err := svc.Cancelar(ctx, actorCliente(cli), id, operclient.AccionClienteEntrada{Motivo: ptr("ya no lo necesito")})
			if err != nil {
				t.Fatalf("%s: %v", c.de, err)
			}
			if s.Estado != operclient.EstadoSolicitud("CANCELADO_POR_CLIENTE") || s.MarcaPenalizacion != c.marca {
				t.Errorf("%s: estado %s marca %v", c.de, s.Estado, s.MarcaPenalizacion)
			}
			var de string
			var marca *bool
			var motivo *string
			cn, cerrar := adminConn(t, ctx)
			if err := cn.QueryRow(ctx, `SELECT de_estado, marca_penalizacion, motivo FROM solicitudes.transicion_solicitud WHERE solicitud_id=$1`, id).Scan(&de, &marca, &motivo); err != nil {
				t.Fatal(err)
			}
			cerrar()
			if de != string(c.de) || marca == nil || *marca != c.marca || motivo == nil || *motivo != "ya no lo necesito" {
				t.Errorf("%s: histórico de la cancelación = %s %v %v", c.de, de, marca, motivo)
			}
			// No se crea viaje: el esquema viajes ni se toca (el pool no tiene acceso), y
			// la solicitud queda terminal.
			if _, err := cancelar(actorCliente(cli), id); codigoDe(err) != "transicion_no_permitida" {
				t.Errorf("cancelar dos veces: esperaba 409, obtuve %v", err)
			}
		}
	})

	t.Run("rechazar la cotización: COTIZADO -> RECHAZADO_POR_CLIENTE sin marca", func(t *testing.T) {
		id, cli := sembrar(t, ctx, estados.EstadoSolicitudCotizado)
		s, err := rechazar(actorCliente(cli), id)
		if err != nil {
			t.Fatal(err)
		}
		if s.Estado != operclient.EstadoSolicitud("RECHAZADO_POR_CLIENTE") || s.MarcaPenalizacion {
			t.Errorf("solicitud = %+v", s)
		}
		// Rechazar y cancelar en COTIZADO son operaciones distintas con estados finales distintos.
		id2, cli2 := sembrar(t, ctx, estados.EstadoSolicitudCotizado)
		s2, err := cancelar(actorCliente(cli2), id2)
		if err != nil || s2.Estado == s.Estado {
			t.Errorf("cancelar en COTIZADO terminó en %v (err %v)", s2, err)
		}
	})

	t.Run("rechazar solo desde COTIZADO; ambas acciones fallan en VENCIDA y terminales", func(t *testing.T) {
		for _, e := range estados.EstadosSolicitud {
			if e != estados.EstadoSolicitudCotizado {
				id, cli := sembrar(t, ctx, e)
				if _, err := rechazar(actorCliente(cli), id); codigoDe(err) != "transicion_no_permitida" {
					t.Errorf("rechazar en %s: esperaba transicion_no_permitida, obtuve %v", e, err)
				}
				if estadoDe(t, ctx, id) != string(e) {
					t.Errorf("rechazar en %s cambió el estado", e)
				}
			}
			switch e {
			case estados.EstadoSolicitudVencida, estados.EstadoSolicitudAprobado, estados.EstadoSolicitudRechazadoPorAi27,
				estados.EstadoSolicitudRechazadoPorCliente, estados.EstadoSolicitudCanceladoPorCliente:
				id, cli := sembrar(t, ctx, e)
				if _, err := cancelar(actorCliente(cli), id); codigoDe(err) != "transicion_no_permitida" {
					t.Errorf("cancelar en %s: esperaba transicion_no_permitida, obtuve %v", e, err)
				}
				if cuenta(t, ctx, "transicion_solicitud", id) != 0 {
					t.Errorf("cancelar en %s dejó histórico", e)
				}
			}
		}
	})

	t.Run("otra organización recibe 404, no 403, y nada cambia", func(t *testing.T) {
		for _, e := range []estados.EstadoSolicitud{estados.EstadoSolicitudCotizado, estados.EstadoSolicitudPagado} {
			id, _ := sembrar(t, ctx, e)
			ajeno := actorCliente(uuid.New())
			for nombre, f := range map[string]func(Actor, uuid.UUID) (*operclient.Solicitud, error){"rechazar": rechazar, "cancelar": cancelar} {
				if _, err := f(ajeno, id); categoriaDe(err) != platformerrors.CategoryNoEncontrado {
					t.Errorf("%s en %s: esperaba 404, obtuve %v", nombre, e, err)
				}
			}
			if estadoDe(t, ctx, id) != string(e) || cuenta(t, ctx, "transicion_solicitud", id) != 0 {
				t.Errorf("la solicitud de otra organización cambió")
			}
		}
		id, _ := sembrar(t, ctx, estados.EstadoSolicitudCotizado)
		if _, err := cancelar(Actor{Rol: "cliente"}, id); categoriaDe(err) != platformerrors.CategoryNoEncontrado {
			t.Errorf("sin tenant: esperaba 404, obtuve %v", err)
		}
	})
}
