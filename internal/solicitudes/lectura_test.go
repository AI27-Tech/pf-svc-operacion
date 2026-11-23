package solicitudes

import (
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/estados"
	operclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/operacion"
	platformerrors "github.com/AI27-Tech/pf-pkg-platform-go/errors"
	"github.com/google/uuid"
)

// sembrarDe inserta una solicitud de `cliente` en `estado` con la hora de creación
// dada (escrita a mano en cada prueba).
func sembrarDe(t *testing.T, ctx testCtx, cliente uuid.UUID, estado estados.EstadoSolicitud, creado time.Time) uuid.UUID {
	t.Helper()
	id := uuid.New()
	ejecuta(t, ctx,
		`INSERT INTO solicitudes.solicitud (id, cliente_id, estado, marca_penalizacion, origen, destinos, creado_en)
		 VALUES ($1,$2,$3,false,'Origen','[{"nombre":"Destino"}]'::jsonb,$4)`, id, cliente, string(estado), creado)
	return id
}

func ids(p *operclient.PaginaSolicitudes) []string {
	var out []string
	for _, s := range p.Items {
		out = append(out, s.Id.String())
	}
	sort.Strings(out)
	return out
}

func quiero(t *testing.T, nombre string, p *operclient.PaginaSolicitudes, err error, esperados ...uuid.UUID) {
	t.Helper()
	if err != nil {
		t.Errorf("%s: %v", nombre, err)
		return
	}
	var want []string
	for _, id := range esperados {
		want = append(want, id.String())
	}
	sort.Strings(want)
	if fmt.Sprint(ids(p)) != fmt.Sprint(want) {
		t.Errorf("%s: ids = %v, quería %v", nombre, ids(p), want)
	}
}

var (
	actorJefeOp    = Actor{Subject: "jefe-1", Rol: "jefe_operativo"}
	actorCotizador = Actor{Subject: "cot-1", Rol: "cotizacion"}
	actorAdminLee  = Actor{Subject: "adm-1", Rol: "administrador"}
)

// TestAlcanceDeLasBandejas (E10-T9, E10-T10; DEC-148 #20, #22): cada rol ve
// exactamente lo suyo en el listado y por id.
func TestAlcanceDeLasBandejas(t *testing.T) {
	ctx, svc := prepara(t)
	base := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	A, B := uuid.New(), uuid.New()
	aSol := sembrarDe(t, ctx, A, "SOLICITADO", base.Add(1*time.Hour))
	aCot := sembrarDe(t, ctx, A, "COTIZADO", base.Add(2*time.Hour))
	aPag := sembrarDe(t, ctx, A, "PAGADO", base.Add(3*time.Hour))
	aVen := sembrarDe(t, ctx, A, "VENCIDA", base.Add(4*time.Hour))
	aRec := sembrarDe(t, ctx, A, "RECHAZADO_POR_AI27", base.Add(5*time.Hour))
	bPag := sembrarDe(t, ctx, B, "PAGADO", base.Add(6*time.Hour))
	bApr := sembrarDe(t, ctx, B, "APROBADO", base.Add(7*time.Hour))
	bSol := sembrarDe(t, ctx, B, "SOLICITADO", base.Add(8*time.Hour))
	todas := []uuid.UUID{aSol, aCot, aPag, aVen, aRec, bPag, bApr, bSol}

	listar := func(a Actor, e *operclient.EstadoSolicitud) (*operclient.PaginaSolicitudes, error) {
		return svc.Listar(ctx, a, e, nil)
	}
	est := func(s string) *operclient.EstadoSolicitud { e := operclient.EstadoSolicitud(s); return &e }
	cliA := Actor{Subject: "a", Rol: "cliente", TenantID: A.String()}
	cliB := Actor{Subject: "b", Rol: "cliente", TenantID: B.String()}

	p, err := listar(actorCotizador, nil)
	quiero(t, "cotizacion ve todas", p, err, todas...)
	p, err = listar(actorAdminLee, nil)
	quiero(t, "administrador ve todas", p, err, todas...)
	p, err = listar(actorJefeOp, nil)
	quiero(t, "el jefe operativo ve exactamente las PAGADO", p, err, aPag, bPag)
	p, err = listar(cliA, nil)
	quiero(t, "el cliente A ve solo las suyas", p, err, aSol, aCot, aPag, aVen, aRec)
	p, err = listar(cliB, nil)
	quiero(t, "el cliente B ve solo las suyas", p, err, bPag, bApr, bSol)
	p, err = listar(actorCotizador, est("PAGADO"))
	quiero(t, "filtro por estado", p, err, aPag, bPag)
	p, err = listar(actorJefeOp, est("COTIZADO"))
	quiero(t, "el filtro no le abre al jefe lo que no ve", p, err)
	p, err = listar(cliA, est("PAGADO"))
	quiero(t, "filtro del cliente", p, err, aPag)
	if _, err := listar(actorCotizador, est("INVENTADO")); categoriaDe(err) != platformerrors.CategoryValidacion {
		t.Errorf("estado desconocido: esperaba 400, obtuve %v", err)
	}

	// Roles sin acceso: 403 en listado y por id, también con id real y con id inexistente.
	for _, rol := range []string{"monitorista", "supervisor", "coordinador", "ejecutivo", "reaccion", "", "desconocido"} {
		a := Actor{Subject: "x", Rol: rol}
		if _, err := listar(a, nil); categoriaDe(err) != platformerrors.CategoryProhibido {
			t.Errorf("listar como %q: esperaba 403, obtuve %v", rol, err)
		}
		for _, id := range []uuid.UUID{aPag, uuid.New()} {
			if _, err := svc.Obtener(ctx, a, id); categoriaDe(err) != platformerrors.CategoryProhibido {
				t.Errorf("obtener como %q: esperaba 403, obtuve %v", rol, err)
			}
		}
	}
	// Un cliente sin organización válida no lista nada.
	if _, err := listar(Actor{Rol: "cliente"}, nil); categoriaDe(err) != platformerrors.CategoryProhibido {
		t.Errorf("cliente sin organización: esperaba 403, obtuve %v", err)
	}

	// Por id: lo que no se ve responde 404, igual que lo que no existe.
	sinPermiso := []struct {
		quien Actor
		id    uuid.UUID
	}{{cliB, aPag}, {cliA, bPag}, {actorJefeOp, aSol}, {actorJefeOp, aCot}, {actorJefeOp, aVen}, {actorJefeOp, bApr}, {actorJefeOp, aRec}, {cliA, uuid.New()}, {actorCotizador, uuid.New()}}
	for _, c := range sinPermiso {
		_, err := svc.Obtener(ctx, c.quien, c.id)
		if categoriaDe(err) != platformerrors.CategoryNoEncontrado {
			t.Errorf("%s sobre %s: esperaba 404, obtuve %v", c.quien.Rol, c.id, err)
		}
	}
	conPermiso := []struct {
		quien Actor
		id    uuid.UUID
	}{{cliA, aSol}, {cliA, aRec}, {cliB, bApr}, {actorJefeOp, aPag}, {actorJefeOp, bPag}, {actorCotizador, aVen}, {actorAdminLee, bApr}}
	for _, c := range conPermiso {
		if s, err := svc.Obtener(ctx, c.quien, c.id); err != nil || s.Id != c.id {
			t.Errorf("%s sobre %s: esperaba la solicitud, obtuve %v", c.quien.Rol, c.id, err)
		}
	}
}

// TestPaginacionDelListado: páginas de 50, sin repetidos ni huecos, con empates de
// creado_en resueltos por id, de la más nueva a la más antigua.
func TestPaginacionDelListado(t *testing.T) {
	ctx, svc := prepara(t)
	cli := uuid.New()
	base := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	vistos := map[uuid.UUID]bool{}
	for i := 0; i < 120; i++ {
		creado := base.Add(time.Duration(i) * time.Second)
		if i < 10 {
			creado = base // diez empatadas
		}
		vistos[sembrarDe(t, ctx, cli, "SOLICITADO", creado)] = false
	}
	a := Actor{Rol: "cotizacion"}
	var cursor *string
	var tam []int
	var anterior *operclient.Solicitud
	for {
		p, err := svc.Listar(ctx, a, nil, cursor)
		if err != nil {
			t.Fatal(err)
		}
		tam = append(tam, len(p.Items))
		for i := range p.Items {
			s := &p.Items[i]
			if visto, ok := vistos[s.Id]; !ok || visto {
				t.Fatalf("solicitud repetida o ajena: %s", s.Id)
			}
			vistos[s.Id] = true
			if anterior != nil && (s.CreadoEn.After(anterior.CreadoEn) || (s.CreadoEn.Equal(anterior.CreadoEn) && s.Id.String() > anterior.Id.String())) {
				t.Fatalf("orden roto entre %s y %s", anterior.Id, s.Id)
			}
			anterior = s
		}
		if p.CursorSiguiente == nil {
			break
		}
		cursor = p.CursorSiguiente
	}
	if fmt.Sprint(tam) != "[50 50 20]" {
		t.Errorf("tamaños de página = %v, quería [50 50 20]", tam)
	}
	for id, v := range vistos {
		if !v {
			t.Errorf("faltó %s", id)
		}
	}
	mal := "no-es-un-cursor"
	if _, err := svc.Listar(ctx, a, nil, &mal); categoriaDe(err) != platformerrors.CategoryValidacion {
		t.Errorf("cursor inválido: esperaba 400, obtuve %v", err)
	}
}

func clienteDe(t *testing.T, ctx testCtx, id uuid.UUID) uuid.UUID {
	return consulta[uuid.UUID](t, ctx, `SELECT cliente_id FROM solicitudes.solicitud WHERE id=$1`, id)
}

func avisosDe(t *testing.T, ctx testCtx, svc *Servicio, a Actor, id uuid.UUID) string {
	t.Helper()
	s, err := svc.Obtener(ctx, a, id)
	if err != nil {
		t.Fatalf("obtener como %s: %v", a.Rol, err)
	}
	return fmt.Sprint(s.Avisos)
}

// TestAvisos (DEC-148 #53 a #57; CP-035-12, CP-035-13, CP-036-07, CP-036-08):
// marcas derivadas del estado y la hora, por rol. La cotización se entregó el
// 20-nov-2026 a las 23:30 en CDMX: último día de vigencia 5-dic, así que "por
// vencer" empieza el 2-dic a las 00:00 de CDMX (= 06:00 UTC).
func TestAvisos(t *testing.T) {
	ctx, svc := prepara(t)
	id := cotizada(t, ctx, svc)
	cli := Actor{Rol: "cliente", TenantID: clienteDe(t, ctx, id).String()}

	enReloj := func(instante time.Time) { conRelojEn(svc, instante) }
	t.Run("por vencer: una hora antes no, desde las 00:00 del 2-dic sí", func(t *testing.T) {
		for _, c := range []struct {
			ahora  time.Time
			quiero string
		}{
			{time.Date(2026, 12, 1, 18, 0, 0, 0, time.UTC), "[]"},          // V-4
			{time.Date(2026, 12, 2, 5, 59, 59, 0, time.UTC), "[]"},         // 1-dic 23:59:59 CDMX
			{time.Date(2026, 12, 2, 6, 0, 0, 0, time.UTC), "[por_vencer]"}, // V-3, 00:00 CDMX
			{time.Date(2026, 12, 5, 12, 0, 0, 0, time.UTC), "[por_vencer]"},
		} {
			enReloj(c.ahora)
			if got := avisosDe(t, ctx, svc, actorCotizador, id); got != c.quiero {
				t.Errorf("cotizacion en %s: %s, quería %s", c.ahora, got, c.quiero)
			}
		}
	})
	t.Run("el cliente ve cotizacion_nueva y por_vencer; el jefe operativo ni la ve", func(t *testing.T) {
		enReloj(time.Date(2026, 12, 1, 18, 0, 0, 0, time.UTC))
		if got := avisosDe(t, ctx, svc, cli, id); got != "[cotizacion_nueva]" {
			t.Errorf("cliente en V-4: %s", got)
		}
		enReloj(time.Date(2026, 12, 2, 6, 0, 0, 0, time.UTC))
		if got := avisosDe(t, ctx, svc, cli, id); got != "[cotizacion_nueva por_vencer]" {
			t.Errorf("cliente en V-3: %s", got)
		}
	})
	t.Run("recotizar renueva la vigencia y apaga por_vencer", func(t *testing.T) {
		enReloj(time.Date(2026, 12, 3, 18, 0, 0, 0, time.UTC))
		if _, err := svc.Cotizar(ctx, actorCotizacion, id, cotizacionBase()); err != nil { // v2 vence el 18-dic
			t.Fatal(err)
		}
		if got := avisosDe(t, ctx, svc, actorCotizador, id); got != "[]" {
			t.Errorf("tras recotizar: %s", got)
		}
	})
	t.Run("solicitud nueva: solo cotizacion, solo en SOLICITADO", func(t *testing.T) {
		nueva := sembrarDe(t, ctx, uuid.New(), "SOLICITADO", time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC))
		if got := avisosDe(t, ctx, svc, actorCotizador, nueva); got != "[nueva]" {
			t.Errorf("cotizacion: %s", got)
		}
		if got := avisosDe(t, ctx, svc, actorAdminLee, nueva); got != "[nueva]" {
			t.Errorf("administrador: %s", got)
		}
		dueno := Actor{Rol: "cliente", TenantID: clienteDe(t, ctx, nueva).String()}
		if got := avisosDe(t, ctx, svc, dueno, nueva); got != "[]" {
			t.Errorf("cliente: %s", got)
		}
	})
	t.Run("cancelación con penalización: solo a cotizacion y solo con marca", func(t *testing.T) {
		con, sin := uuid.New(), uuid.New()
		cli1 := uuid.New()
		for _, c := range []struct {
			id    uuid.UUID
			marca bool
		}{{con, true}, {sin, false}} {
			ejecuta(t, ctx, `INSERT INTO solicitudes.solicitud (id, cliente_id, estado, marca_penalizacion, origen, destinos, creado_en)
			  VALUES ($1,$2,'CANCELADO_POR_CLIENTE',$3,'O','[{"nombre":"D"}]'::jsonb,'2026-11-01T00:00:00Z')`, c.id, cli1, c.marca)
		}
		dueno := Actor{Rol: "cliente", TenantID: cli1.String()}
		if got := avisosDe(t, ctx, svc, actorCotizador, con); got != "[cancelacion_con_penalizacion]" {
			t.Errorf("con marca: %s", got)
		}
		if got := avisosDe(t, ctx, svc, actorCotizador, sin); got != "[]" {
			t.Errorf("sin marca: %s", got)
		}
		if got := avisosDe(t, ctx, svc, dueno, con); got != "[]" {
			t.Errorf("el cliente no recibe este aviso: %s", got)
		}
	})
	t.Run("rechazo de AI27: al cliente, con su motivo; no a cotizacion", func(t *testing.T) {
		pagada, _ := pagada(t, ctx, svc)
		if _, err := svc.Rechazar(ctx, actorJefe, pagada, operclient.MotivoEntrada{Motivo: "sin escolta"}); err != nil {
			t.Fatal(err)
		}
		dueno := Actor{Rol: "cliente", TenantID: clienteDe(t, ctx, pagada).String()}
		s, err := svc.Obtener(ctx, dueno, pagada)
		if err != nil || fmt.Sprint(s.Avisos) != "[rechazo_de_ai27]" || s.MotivoRechazo == nil || *s.MotivoRechazo != "sin escolta" {
			t.Errorf("cliente: %v avisos=%v motivo=%v", err, s, s.MotivoRechazo)
		}
		if got := avisosDe(t, ctx, svc, actorCotizador, pagada); got != "[]" {
			t.Errorf("cotizacion: %s", got)
		}
	})
	t.Run("sin aviso por pago confirmado ni por aprobación (DEC-148 #56)", func(t *testing.T) {
		pagada, _ := pagada(t, ctx, svc)
		for _, a := range []Actor{actorCotizador, actorJefeOp, {Rol: "cliente", TenantID: clienteDe(t, ctx, pagada).String()}} {
			if got := avisosDe(t, ctx, svc, a, pagada); got != "[]" {
				t.Errorf("%s en PAGADO: %s", a.Rol, got)
			}
		}
		if _, err := svc.Aprobar(ctx, actorJefe, pagada); err != nil {
			t.Fatal(err)
		}
		if got := avisosDe(t, ctx, svc, actorCotizador, pagada); got != "[]" {
			t.Errorf("cotizacion en APROBADO: %s", got)
		}
	})
}

// TestListadoDelClienteNoDuplicaAlAprobar (DEC-148 #32): la misma fila, con el mismo
// id, pasa de PAGADO a APROBADO; el jefe operativo deja de verla.
func TestListadoDelClienteNoDuplicaAlAprobar(t *testing.T) {
	ctx, svc := prepara(t)
	id, cli := pagada(t, ctx, svc)
	dueno := Actor{Rol: "cliente", TenantID: cli.String()}

	antes, err := svc.Listar(ctx, dueno, nil, nil)
	if err != nil || len(antes.Items) != 1 || antes.Items[0].Id != id || antes.Items[0].Estado != operclient.EstadoSolicitud("PAGADO") {
		t.Fatalf("antes: %v %+v", err, antes)
	}
	if bandeja, _ := svc.Listar(ctx, actorJefeOp, nil, nil); len(bandeja.Items) != 1 {
		t.Fatalf("la bandeja del jefe antes: %d", len(bandeja.Items))
	}
	if _, err := svc.Aprobar(ctx, actorJefe, id); err != nil {
		t.Fatal(err)
	}
	despues, err := svc.Listar(ctx, dueno, nil, nil)
	if err != nil || len(despues.Items) != 1 || despues.Items[0].Id != id || despues.Items[0].Estado != operclient.EstadoSolicitud("APROBADO") {
		t.Fatalf("después: %v %+v", err, despues)
	}
	if bandeja, _ := svc.Listar(ctx, actorJefeOp, nil, nil); len(bandeja.Items) != 0 {
		t.Errorf("el jefe ya no debe ver la aprobada: %d", len(bandeja.Items))
	}
	if _, err := svc.Obtener(ctx, actorJefeOp, id); categoriaDe(err) != platformerrors.CategoryNoEncontrado {
		t.Errorf("el jefe por id tras aprobar: esperaba 404, obtuve %v", err)
	}
}

// TestObtenerTraeLaCotizacionVigente: la última versión; las anteriores no salen aquí.
func TestObtenerTraeLaCotizacionVigente(t *testing.T) {
	ctx, svc := prepara(t)
	id := cotizada(t, ctx, svc)
	conRelojEn(svc, time.Date(2026, 11, 22, 18, 0, 0, 0, time.UTC))
	if _, err := svc.Cotizar(ctx, actorCotizacion, id, operclient.CotizacionEntrada{Monto: "2000", Moneda: "MXN", FechaHoraServicio: time.Date(2026, 12, 12, 16, 0, 0, 0, time.UTC)}); err != nil {
		t.Fatal(err)
	}
	s, err := svc.Obtener(ctx, actorCotizador, id)
	if err != nil || s.Cotizacion == nil || s.Cotizacion.Version != 2 || s.Cotizacion.Monto != "2000" {
		t.Fatalf("cotización vigente: %v %+v", err, s.Cotizacion)
	}
}
