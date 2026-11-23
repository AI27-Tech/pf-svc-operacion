package solicitudes

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/estados"
	operclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/operacion"
	platformerrors "github.com/AI27-Tech/pf-pkg-platform-go/errors"
	"github.com/AI27-Tech/pf-pkg-platform-go/outbox"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var (
	actorJefe  = Actor{Subject: "jefe-1", Rol: "jefe_operativo"}
	fechaCita  = time.Date(2026, 12, 11, 15, 0, 0, 0, time.UTC)
	fechaCosto = time.Date(2026, 12, 10, 16, 0, 0, 0, time.UTC) // la de cotizacionBase
)

// pagadaCon deja una solicitud PAGADO a partir de una entrada del cliente, con el
// reloj del 21-nov-2026 y la cotización base (servicio el 10-dic-2026 16:00 UTC).
func pagadaCon(t *testing.T, ctx testCtx, svc *Servicio, cliente uuid.UUID, in operclient.CrearSolicitudEntrada) uuid.UUID {
	t.Helper()
	svc.ConReloj(func() time.Time { return time.Date(2026, 11, 21, 5, 30, 0, 0, time.UTC) })
	s, err := svc.Crear(ctx, actorCliente(cliente), in)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Cotizar(ctx, actorCotizacion, s.Id, cotizacionBase()); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ConfirmarPago(ctx, actorCotizacion, s.Id, operclient.PagoEntrada{FechaPago: dia("2026-11-25")}); err != nil {
		t.Fatal(err)
	}
	return s.Id
}

func pagada(t *testing.T, ctx testCtx, svc *Servicio) (uuid.UUID, uuid.UUID) {
	t.Helper()
	cli := uuid.New()
	return pagadaCon(t, ctx, svc, cli, operclient.CrearSolicitudEntrada{Origen: "A", Destinos: []operclient.Destino{{Nombre: "B"}}}), cli
}

func cuentaViajes(t *testing.T, ctx testCtx, id uuid.UUID) int {
	t.Helper()
	return consulta[int](t, ctx, `SELECT count(*) FROM viajes.viaje WHERE id=$1`, id)
}

// TestAprobarCreaElViaje (E1-HU-003, ADR 39): la solicitud pasa a APROBADO y nace
// el viaje con su mismo id, sus destinos en orden, su evento por el outbox único de
// los viaje.* y la auditoría, todo con valores escritos a mano.
func TestAprobarCreaElViaje(t *testing.T) {
	ctx, svc := prepara(t)
	cli := uuid.New()
	id := pagadaCon(t, ctx, svc, cli, operclient.CrearSolicitudEntrada{
		Origen: "Puerto de Manzanillo",
		Destinos: []operclient.Destino{
			{Nombre: "Guadalajara", NumeroEnvio: ptr("ENV-7"), FechaCita: &fechaCita},
			{Nombre: "Monterrey"},
		},
		TipoServicio: ptr("escolta"),
		Tracto:       &operclient.Tracto{Placas: ptr("ABC-123"), Economico: ptr("T-9"), Marca: ptr("Kenworth"), Modelo: ptr("T680"), Color: ptr("Rojo")},
	})
	svc.ConReloj(func() time.Time { return time.Date(2026, 11, 26, 15, 0, 0, 0, time.UTC) })

	s, err := svc.Aprobar(ctx, actorJefe, id)
	if err != nil || s.Estado != operclient.EstadoSolicitud("APROBADO") {
		t.Fatalf("aprobar: %v / %+v", err, s)
	}

	if got := estadoDe(t, ctx, id); got != "APROBADO" {
		t.Errorf("estado de la solicitud = %s", got)
	}
	var estado, origen, tipo, placas, eco, marca, modelo, color string
	var cliente uuid.UUID
	var nacio bool
	var servicio, creado time.Time
	c, cerrar := adminConn(t, ctx)
	defer cerrar()
	if err := c.QueryRow(ctx, `SELECT estado, cliente_id, origen, tipo_servicio, nacio_de_solicitud, fecha_hora_servicio, creado_en,
	    tracto_placas, tracto_economico, tracto_marca, tracto_modelo, tracto_color FROM viajes.viaje WHERE id=$1`, id).
		Scan(&estado, &cliente, &origen, &tipo, &nacio, &servicio, &creado, &placas, &eco, &marca, &modelo, &color); err != nil {
		t.Fatalf("el viaje no nació con el id de la solicitud: %v", err)
	}
	if estado != "APROBADO" || cliente != cli || origen != "Puerto de Manzanillo" || tipo != "escolta" || !nacio {
		t.Errorf("viaje = %s %s %q %q nacio=%v", estado, cliente, origen, tipo, nacio)
	}
	if !servicio.Equal(fechaCosto) || !creado.Equal(time.Date(2026, 11, 26, 15, 0, 0, 0, time.UTC)) {
		t.Errorf("fechas del viaje: servicio %s creado %s", servicio, creado)
	}
	if placas != "ABC-123" || eco != "T-9" || marca != "Kenworth" || modelo != "T680" || color != "Rojo" {
		t.Errorf("tracto del viaje = %s %s %s %s %s", placas, eco, marca, modelo, color)
	}
	filas, err := c.Query(ctx, `SELECT orden, nombre, coalesce(numero_envio,''), fecha_cita FROM viajes.destino WHERE viaje_id=$1 ORDER BY orden`, id)
	if err != nil {
		t.Fatal(err)
	}
	var destinos []string
	for filas.Next() {
		var orden int
		var nombre, envio string
		var cita *time.Time
		if err := filas.Scan(&orden, &nombre, &envio, &cita); err != nil {
			t.Fatal(err)
		}
		destinos = append(destinos, fmt.Sprintf("%d|%s|%s|%v", orden, nombre, envio, cita != nil && cita.Equal(fechaCita)))
	}
	filas.Close()
	if fmt.Sprint(destinos) != "[0|Guadalajara|ENV-7|true 1|Monterrey||false]" {
		t.Errorf("destinos = %v", destinos)
	}

	// Eventos del viaje por viajes.outbox; el de la solicitud por solicitudes.outbox.
	for _, tipo := range []string{"viaje.creado", "viaje.mensajeria_habilitada"} {
		if n := consulta[int](t, ctx, `SELECT count(*) FROM viajes.outbox WHERE tipo=$1 AND agregado_id=$2
		   AND payload->>'viaje_id'=$2 AND payload->>'cliente_id'=$3 AND publicado=false`, tipo, id.String(), cli.String()); n != 1 {
			t.Errorf("viajes.outbox %s = %d, quería 1", tipo, n)
		}
	}
	if n := consulta[int](t, ctx, `SELECT count(*) FROM viajes.outbox WHERE agregado_id=$1 AND payload->>'estado'='APROBADO'`, id.String()); n != 1 {
		t.Errorf("viaje.creado debe decir estado APROBADO (%d)", n)
	}
	if n := consulta[int](t, ctx, `SELECT count(*) FROM solicitudes.outbox WHERE agregado_id=$1 AND tipo LIKE 'viaje.%'`, id.String()); n != 0 {
		t.Errorf("los viaje.* no van al outbox de solicitudes (%d)", n)
	}
	if n := consulta[int](t, ctx, `SELECT count(*) FROM solicitudes.outbox WHERE agregado_id=$1 AND tipo='solicitud.estado_cambiado'
	   AND payload->>'de_estado'='PAGADO' AND payload->>'a_estado'='APROBADO'`, id.String()); n != 1 {
		t.Errorf("solicitud.estado_cambiado PAGADO→APROBADO = %d, quería 1", n)
	}
	// Histórico y auditoría (misma transacción): actor real y viaje_id del viaje (DEC-148 #70).
	if n := consulta[int](t, ctx, `SELECT count(*) FROM solicitudes.transicion_solicitud WHERE solicitud_id=$1 AND accion='aprobada'
	   AND disparador='jefe_operativo' AND actor_id='jefe-1' AND actor_rol='jefe_operativo'`, id); n != 1 {
		t.Errorf("histórico de la aprobación = %d, quería 1", n)
	}
	if n := consulta[int](t, ctx, `SELECT count(*) FROM auditoria.registro_auditoria WHERE modulo='solicitudes' AND accion='aprobada'
	   AND actor_id='jefe-1' AND viaje_id=$1 AND detalle->>'solicitud_id'=$1::text AND detalle->>'de_estado'='PAGADO' AND detalle->>'a_estado'='APROBADO'`, id); n != 1 {
		t.Errorf("auditoría de la aprobación = %d, quería 1", n)
	}
	// DEC-148 #58: no se escribe transicion_viaje.
	if n := consulta[int](t, ctx, `SELECT count(*) FROM viajes.transicion_viaje WHERE viaje_id=$1`, id); n != 0 {
		t.Errorf("no se escribe transicion_viaje (%d filas)", n)
	}
}

// TestAprobarRechazarGuardas: el jefe operativo no ve fuera de PAGADO (404, sin
// cambios); el administrador recibe 409 transicion_no_permitida; sin fecha y hora
// del servicio, 409 fecha_hora_servicio_requerida; el rechazo exige motivo.
func TestAprobarRechazarGuardas(t *testing.T) {
	ctx, svc := prepara(t)
	otros := map[string]func(uuid.UUID){
		"SOLICITADO":            nil,
		"COTIZADO":              nil,
		"VENCIDA":               nil,
		"APROBADO":              nil,
		"RECHAZADO_POR_AI27":    nil,
		"RECHAZADO_POR_CLIENTE": nil,
		"CANCELADO_POR_CLIENTE": nil,
	}
	for estado := range otros {
		t.Run(estado, func(t *testing.T) {
			id, _ := sembrar(t, ctx, estados.EstadoSolicitud(estado))
			ejecuta(t, ctx, `UPDATE solicitudes.solicitud SET fecha_hora_servicio = now() + interval '5 days' WHERE id=$1`, id)
			for nombre, llamar := range map[string]func(Actor) error{
				"aprobar": func(a Actor) error { _, err := svc.Aprobar(ctx, a, id); return err },
				"rechazar": func(a Actor) error {
					_, err := svc.Rechazar(ctx, a, id, operclient.MotivoEntrada{Motivo: "x"})
					return err
				},
			} {
				if err := llamar(actorJefe); categoriaDe(err) != platformerrors.CategoryNoEncontrado {
					t.Errorf("%s del jefe en %s: esperaba 404, obtuve %v", nombre, estado, err)
				}
				if err := llamar(actorAdmin); codigoDe(err) != "transicion_no_permitida" || categoriaDe(err) != platformerrors.CategoryConflicto {
					t.Errorf("%s del administrador en %s: esperaba 409 transicion_no_permitida, obtuve %v", nombre, estado, err)
				}
			}
			if got := estadoDe(t, ctx, id); got != estado || cuentaViajes(t, ctx, id) != 0 || cuenta(t, ctx, "transicion_solicitud", id) != 0 || cuenta(t, ctx, "outbox", id) != 0 {
				t.Errorf("los rechazos dejaron cambios en %s", estado)
			}
		})
	}

	t.Run("sin fecha y hora del servicio no se aprueba", func(t *testing.T) {
		id, _ := pagada(t, ctx, svc)
		ejecuta(t, ctx, `UPDATE solicitudes.solicitud SET fecha_hora_servicio = NULL WHERE id=$1`, id)
		antes := cuenta(t, ctx, "outbox", id)
		for _, a := range []Actor{actorJefe, actorAdmin} {
			_, err := svc.Aprobar(ctx, a, id)
			if codigoDe(err) != "fecha_hora_servicio_requerida" || categoriaDe(err) != platformerrors.CategoryConflicto {
				t.Errorf("%s: esperaba 409 fecha_hora_servicio_requerida, obtuve %v", a.Rol, err)
			}
		}
		if estadoDe(t, ctx, id) != "PAGADO" || cuentaViajes(t, ctx, id) != 0 || cuenta(t, ctx, "outbox", id) != antes {
			t.Error("el rechazo dejó cambios")
		}
	})

	t.Run("rechazar sin motivo o con motivo en blanco", func(t *testing.T) {
		id, _ := pagada(t, ctx, svc)
		for _, m := range []string{"", "   "} {
			if _, err := svc.Rechazar(ctx, actorJefe, id, operclient.MotivoEntrada{Motivo: m}); categoriaDe(err) != platformerrors.CategoryValidacion {
				t.Errorf("motivo %q: esperaba 400, obtuve %v", m, err)
			}
		}
		if estadoDe(t, ctx, id) != "PAGADO" {
			t.Error("el rechazo sin motivo cambió el estado")
		}
	})
}

// TestRechazarSolicitud (E1-HU-003, DEC-148 #19): termina en RECHAZADO_POR_AI27 con
// el motivo visible en la respuesta, sin marca de penalización y sin viaje.
func TestRechazarSolicitud(t *testing.T) {
	ctx, svc := prepara(t)
	id, _ := pagada(t, ctx, svc)
	s, err := svc.Rechazar(ctx, actorJefe, id, operclient.MotivoEntrada{Motivo: " sin escolta disponible "})
	if err != nil || s.Estado != operclient.EstadoSolicitud("RECHAZADO_POR_AI27") {
		t.Fatalf("rechazar: %v / %+v", err, s)
	}
	if s.MotivoRechazo == nil || *s.MotivoRechazo != "sin escolta disponible" || s.MarcaPenalizacion {
		t.Errorf("motivo_rechazo=%v marca=%v", s.MotivoRechazo, s.MarcaPenalizacion)
	}
	if cuentaViajes(t, ctx, id) != 0 || consulta[bool](t, ctx, `SELECT marca_penalizacion FROM solicitudes.solicitud WHERE id=$1`, id) {
		t.Error("el rechazo no crea viaje ni marca")
	}
	if n := consulta[int](t, ctx, `SELECT count(*) FROM solicitudes.transicion_solicitud WHERE solicitud_id=$1 AND accion='rechazada'
	   AND motivo='sin escolta disponible' AND actor_id='jefe-1'`, id); n != 1 {
		t.Errorf("histórico del rechazo = %d", n)
	}
	if n := consulta[int](t, ctx, `SELECT count(*) FROM solicitudes.outbox WHERE agregado_id=$1 AND payload->>'a_estado'='RECHAZADO_POR_AI27'`, id.String()); n != 1 {
		t.Errorf("evento del rechazo = %d", n)
	}
	if n := consulta[int](t, ctx, `SELECT count(*) FROM auditoria.registro_auditoria WHERE modulo='solicitudes' AND accion='rechazada'
	   AND detalle->>'motivo'='sin escolta disponible' AND detalle->>'solicitud_id'=$1`, id.String()); n != 1 {
		t.Errorf("auditoría del rechazo = %d", n)
	}
	// El motivo es visible para quien lee la solicitud después (armar lo relee).
	if n := consulta[int](t, ctx, `SELECT count(*) FROM viajes.outbox WHERE agregado_id=$1`, id.String()); n != 0 {
		t.Errorf("el rechazo no escribe eventos del viaje (%d)", n)
	}
}

// fallaEn hace que todo INSERT en `tabla` falle (falla de infraestructura
// simulada). Se retira al terminar la prueba o con el cierre devuelto.
func fallaEn(t *testing.T, ctx testCtx, tabla string, cuando string) func() {
	t.Helper()
	nombre := "falla_" + fmt.Sprint(time.Now().UnixNano())
	ejecuta(t, ctx, `CREATE OR REPLACE FUNCTION solicitudes.falla_prueba() RETURNS trigger AS $$
	  BEGIN RAISE EXCEPTION 'falla simulada'; END $$ LANGUAGE plpgsql`)
	diferida := cuando == "@commit"
	if cuando != "" && !diferida {
		cuando = " WHEN (" + cuando + ")"
	}
	if diferida {
		// Falla al CONFIRMAR la transacción, cuando ya se escribió todo: ningún
		// camino que confirme una parte antes puede salir bien librado.
		ejecuta(t, ctx, `CREATE CONSTRAINT TRIGGER `+nombre+` AFTER INSERT ON `+tabla+
			` DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION solicitudes.falla_prueba()`)
	} else {
		ejecuta(t, ctx, `CREATE TRIGGER `+nombre+` BEFORE INSERT ON `+tabla+` FOR EACH ROW`+cuando+` EXECUTE FUNCTION solicitudes.falla_prueba()`)
	}
	quitar := func() {
		c, cerrar := adminConn(t, context.Background())
		defer cerrar()
		_, _ = c.Exec(context.Background(), `DROP TRIGGER IF EXISTS `+nombre+` ON `+tabla)
	}
	t.Cleanup(quitar)
	return quitar
}

// TestAprobarEsAtomico (E10-T8): una falla en CUALQUIER punto de la aprobación,
// después de actualizar la solicitud o después de insertar el viaje, deja la
// solicitud en PAGADO y ningún viaje, destino, evento ni auditoría. Reintentar
// sin la falla funciona.
func TestAprobarEsAtomico(t *testing.T) {
	ctx, svc := prepara(t)
	puntos := []struct{ nombre, tabla, cuando string }{
		{"antes del viaje: el evento de la solicitud", "solicitudes.outbox", ""},
		{"al insertar el viaje (la solicitud ya cambió)", "viajes.viaje", ""},
		{"al insertar los destinos (el viaje ya existe)", "viajes.destino", ""},
		{"al escribir viaje.creado", "viajes.outbox", "NEW.tipo = 'viaje.creado'"},
		{"al escribir viaje.mensajeria_habilitada (viaje y primer evento ya escritos)", "viajes.outbox", "NEW.tipo = 'viaje.mensajeria_habilitada'"},
		{"al auditar", "auditoria.registro_auditoria", ""},
		{"al confirmar la transacción (todo escrito, falla el commit)", "solicitudes.transicion_solicitud", "@commit"},
	}
	for _, p := range puntos {
		t.Run(p.nombre, func(t *testing.T) {
			id, _ := pagada(t, ctx, svc)
			quitar := fallaEn(t, ctx, p.tabla, p.cuando) // el setup ya terminó: solo falla la aprobación
			// El rastro previo, para comprobar que no cambia.
			trans, ev, aud := cuenta(t, ctx, "transicion_solicitud", id), cuenta(t, ctx, "outbox", id),
				consulta[int](t, ctx, `SELECT count(*) FROM auditoria.registro_auditoria WHERE detalle->>'solicitud_id'=$1`, id.String())

			if _, err := svc.Aprobar(ctx, actorJefe, id); err == nil {
				t.Fatal("la aprobación debía fallar")
			}
			if got := estadoDe(t, ctx, id); got != "PAGADO" {
				t.Errorf("estado = %s, quería PAGADO", got)
			}
			if cuentaViajes(t, ctx, id) != 0 ||
				consulta[int](t, ctx, `SELECT count(*) FROM viajes.destino WHERE viaje_id=$1`, id) != 0 ||
				consulta[int](t, ctx, `SELECT count(*) FROM viajes.outbox WHERE agregado_id=$1`, id.String()) != 0 {
				t.Error("quedó rastro del viaje")
			}
			if cuenta(t, ctx, "transicion_solicitud", id) != trans || cuenta(t, ctx, "outbox", id) != ev ||
				consulta[int](t, ctx, `SELECT count(*) FROM auditoria.registro_auditoria WHERE detalle->>'solicitud_id'=$1`, id.String()) != aud {
				t.Error("quedó rastro de la solicitud (histórico, evento o auditoría)")
			}

			quitar()
			if s, err := svc.Aprobar(ctx, actorJefe, id); err != nil || s.Estado != operclient.EstadoSolicitud("APROBADO") || cuentaViajes(t, ctx, id) != 1 {
				t.Fatalf("el reintento sin falla debe aprobar: %v", err)
			}
		})
	}
}

// resultado de una acción concurrente.
type resultado struct {
	nombre string
	err    error
}

// carrera lanza las acciones a la vez sobre una solicitud PAGADO.
func carrera(acciones map[string]func() error) []resultado {
	var wg sync.WaitGroup
	inicio := make(chan struct{})
	salida := make([]resultado, 0, len(acciones))
	var mu sync.Mutex
	for nombre, f := range acciones {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-inicio
			err := f()
			mu.Lock()
			salida = append(salida, resultado{nombre, err})
			mu.Unlock()
		}()
	}
	close(inicio)
	wg.Wait()
	return salida
}

// TestAprobarConcurrente: aprobar vs aprobar, aprobar vs rechazar y aprobar vs
// cancelar sobre la misma solicitud producen exactamente UN resultado y, si gana
// la aprobación, UN solo viaje. El perdedor ve lo que dice el contrato: el jefe
// operativo, 404 (ya no ve la solicitud); el administrador, 409.
func TestAprobarConcurrente(t *testing.T) {
	ctx, svc := prepara(t)
	const vueltas = 25
	for i := 0; i < vueltas; i++ {
		t.Run(fmt.Sprintf("aprobar_vs_aprobar_%d", i), func(t *testing.T) {
			id, _ := pagada(t, ctx, svc)
			res := carrera(map[string]func() error{
				"a": func() error { _, err := svc.Aprobar(ctx, actorJefe, id); return err },
				"b": func() error { _, err := svc.Aprobar(ctx, actorJefe, id); return err },
			})
			ok, no := 0, 0
			for _, r := range res {
				switch {
				case r.err == nil:
					ok++
				case categoriaDe(r.err) == platformerrors.CategoryNoEncontrado:
					no++
				default:
					t.Errorf("el perdedor debe recibir 404, obtuvo %v", r.err)
				}
			}
			if ok != 1 || no != 1 || estadoDe(t, ctx, id) != "APROBADO" || cuentaViajes(t, ctx, id) != 1 ||
				nTransiciones(t, ctx, id, "APROBADO") != 1 ||
				consulta[int](t, ctx, `SELECT count(*) FROM viajes.outbox WHERE agregado_id=$1 AND tipo='viaje.creado'`, id.String()) != 1 {
				t.Errorf("ok=%d perdedores=%d estado=%s viajes=%d", ok, no, estadoDe(t, ctx, id), cuentaViajes(t, ctx, id))
			}
		})
		t.Run(fmt.Sprintf("administrador_vs_administrador_%d", i), func(t *testing.T) {
			id, _ := pagada(t, ctx, svc)
			res := carrera(map[string]func() error{
				"a": func() error { _, err := svc.Aprobar(ctx, actorAdmin, id); return err },
				"b": func() error { _, err := svc.Aprobar(ctx, actorAdmin, id); return err },
			})
			ok, conflicto := 0, 0
			for _, r := range res {
				if r.err == nil {
					ok++
				} else if codigoDe(r.err) == "transicion_no_permitida" {
					conflicto++
				} else {
					t.Errorf("el perdedor debe recibir 409, obtuvo %v", r.err)
				}
			}
			if ok != 1 || conflicto != 1 || cuentaViajes(t, ctx, id) != 1 {
				t.Errorf("ok=%d conflictos=%d viajes=%d", ok, conflicto, cuentaViajes(t, ctx, id))
			}
		})
		t.Run(fmt.Sprintf("aprobar_vs_rechazar_%d", i), func(t *testing.T) {
			id, _ := pagada(t, ctx, svc)
			res := carrera(map[string]func() error{
				"aprobar": func() error { _, err := svc.Aprobar(ctx, actorJefe, id); return err },
				"rechazar": func() error {
					_, err := svc.Rechazar(ctx, actorJefe, id, operclient.MotivoEntrada{Motivo: "m"})
					return err
				},
			})
			gano := ""
			for _, r := range res {
				if r.err == nil {
					if gano != "" {
						t.Fatal("ganaron las dos")
					}
					gano = r.nombre
				} else if categoriaDe(r.err) != platformerrors.CategoryNoEncontrado {
					t.Errorf("el perdedor debe recibir 404, obtuvo %v", r.err)
				}
			}
			estado, viajes := estadoDe(t, ctx, id), cuentaViajes(t, ctx, id)
			if (gano == "aprobar" && (estado != "APROBADO" || viajes != 1)) || (gano == "rechazar" && (estado != "RECHAZADO_POR_AI27" || viajes != 0)) || gano == "" {
				t.Errorf("gano=%q estado=%s viajes=%d", gano, estado, viajes)
			}
		})
		t.Run(fmt.Sprintf("aprobar_vs_cancelar_%d", i), func(t *testing.T) {
			id, cli := pagada(t, ctx, svc)
			res := carrera(map[string]func() error{
				"aprobar": func() error { _, err := svc.Aprobar(ctx, actorJefe, id); return err },
				"cancelar": func() error {
					_, err := svc.Cancelar(ctx, actorCliente(cli), id, operclient.AccionClienteEntrada{})
					return err
				},
			})
			gano := ""
			for _, r := range res {
				if r.err == nil {
					if gano != "" {
						t.Fatal("ganaron las dos")
					}
					gano = r.nombre
				}
			}
			estado, viajes := estadoDe(t, ctx, id), cuentaViajes(t, ctx, id)
			marca := consulta[bool](t, ctx, `SELECT marca_penalizacion FROM solicitudes.solicitud WHERE id=$1`, id)
			if (gano == "aprobar" && (estado != "APROBADO" || viajes != 1 || marca)) ||
				(gano == "cancelar" && (estado != "CANCELADO_POR_CLIENTE" || viajes != 0 || !marca)) || gano == "" {
				t.Errorf("gano=%q estado=%s viajes=%d marca=%v", gano, estado, viajes, marca)
			}
		})
	}
}

// TestAprobarPublicaLaFormaDeViajesCrear fija CLAVE POR CLAVE los dos eventos viaje.*
// de una aprobación. Los valores están escritos a mano y son los que emite
// viajes.Crear (internal/viajes/servicio.go): reportes lee viaje_id, cliente_id,
// estado y creado_en de viaje.creado, y mensajeria lee viaje_id y cliente_id de
// viaje.mensajeria_habilitada. Quitar o renombrar una clave, o subir Version,
// rompe a esos consumidores sin ningún error visible.
func TestAprobarPublicaLaFormaDeViajesCrear(t *testing.T) {
	ctx, svc := prepara(t)
	cli := uuid.New()
	id := pagadaCon(t, ctx, svc, cli, operclient.CrearSolicitudEntrada{Origen: "Puerto de Manzanillo", Destinos: []operclient.Destino{{Nombre: "Guadalajara"}}})
	svc.ConReloj(func() time.Time { return time.Date(2026, 11, 26, 15, 0, 0, 0, time.UTC) })
	if _, err := svc.Aprobar(ctx, actorJefe, id); err != nil {
		t.Fatal(err)
	}
	casos := map[string]map[string]any{
		"viaje.creado": {
			"viaje_id": id.String(), "estado": "APROBADO", "cliente_id": cli.String(),
			"origen": "Puerto de Manzanillo", "creado_en": "2026-11-26T15:00:00Z",
		},
		"viaje.mensajeria_habilitada": {
			"viaje_id": id.String(), "cliente_id": cli.String(), "creado_en": "2026-11-26T15:00:00Z",
		},
	}
	c, cerrar := adminConn(t, ctx)
	defer cerrar()
	for tipo, want := range casos {
		var version int
		var agregado string
		var ocurrido time.Time
		var crudo []byte
		if err := c.QueryRow(ctx, `SELECT version, agregado_id, ocurrido_en, payload FROM viajes.outbox WHERE tipo=$1 AND agregado_id=$2`,
			tipo, id.String()).Scan(&version, &agregado, &ocurrido, &crudo); err != nil {
			t.Fatalf("%s no está en viajes.outbox: %v", tipo, err)
		}
		var got map[string]any
		if err := json.Unmarshal(crudo, &got); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: payload = %v, quería exactamente %v", tipo, got, want)
		}
		if version != 1 || agregado != id.String() || !ocurrido.Equal(time.Date(2026, 11, 26, 15, 0, 0, 0, time.UTC)) {
			t.Errorf("%s: version=%d agregado=%s ocurrido_en=%s", tipo, version, agregado, ocurrido)
		}
	}
}

// TestPublicarEnViajesRestauraElSearchPath: el search_path apunta a viajes SOLO
// mientras se publican los viaje.*. Un evento publicado después, en la misma
// transacción, debe ir al outbox de solicitudes y no al de viajes (cuyo relay lo
// publicaría sin error).
func TestPublicarEnViajesRestauraElSearchPath(t *testing.T) {
	ctx, svc := prepara(t)
	id, _ := pagada(t, ctx, svc)
	var antes, despues string
	err := correr(ctx, svc, func(tx pgx.Tx) error {
		f, err := cargar(ctx, tx, id, true)
		if err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT current_setting('search_path')`).Scan(&antes); err != nil {
			return err
		}
		if err := svc.crearViaje(ctx, tx, f); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT current_setting('search_path')`).Scan(&despues); err != nil {
			return err
		}
		return svc.pub.Publish(ctx, tx, outbox.Event{Tipo: "solicitud.prueba_search_path", Version: 1, AgregadoID: id.String(), OcurridoEn: time.Now().UTC(), Payload: map[string]any{}})
	})
	if err != nil {
		t.Fatal(err)
	}
	if antes != despues {
		t.Errorf("search_path = %q tras crearViaje, era %q", despues, antes)
	}
	if n := consulta[int](t, ctx, `SELECT count(*) FROM viajes.outbox WHERE tipo='solicitud.prueba_search_path'`); n != 0 {
		t.Errorf("un evento solicitud.* aterrizó en viajes.outbox (%d)", n)
	}
	if n := consulta[int](t, ctx, `SELECT count(*) FROM solicitudes.outbox WHERE tipo='solicitud.prueba_search_path'`); n != 1 {
		t.Errorf("el evento debía ir a solicitudes.outbox (%d)", n)
	}
}
