package operacion_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	catalogos "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/catalogos"
	identidad "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/identidad"
	operclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/operacion"
	"github.com/AI27-Tech/pf-pkg-platform-go/auth"
	"github.com/AI27-Tech/pf-pkg-platform-go/bus"
	platformerrors "github.com/AI27-Tech/pf-pkg-platform-go/errors"
	"github.com/AI27-Tech/pf-pkg-platform-go/worker"
	"github.com/AI27-Tech/pf-svc-operacion"
	"github.com/AI27-Tech/pf-svc-operacion/internal/dbtest"
	"github.com/google/uuid"
)

// Claims de los roles de las pruebas de alcance (DEC-149). Cada sujeto tiene su
// token HTTP en autenticadorTabla.
var (
	monA      = &auth.Claims{Subject: "mon-a", Rol: "monitorista", Audiencia: auth.AudienciaInterno}
	monB      = &auth.Claims{Subject: "mon-b", Rol: "monitorista", Audiencia: auth.AudienciaInterno}
	monSin    = &auth.Claims{Subject: "mon-sin", Rol: "monitorista", Audiencia: auth.AudienciaInterno}
	supAB     = &auth.Claims{Subject: "sup-ab", Rol: "supervisor", Audiencia: auth.AudienciaInterno}
	ejecutivo = &auth.Claims{Subject: "eje-1", Rol: "ejecutivo", Audiencia: auth.AudienciaInterno}
	reaccion  = &auth.Claims{Subject: "rea-1", Rol: "reaccion", Audiencia: auth.AudienciaInterno}
)

// autenticadorTabla devuelve los claims cuyo Subject es el token; si no, jefe.
type autenticadorTabla struct{}

func (autenticadorTabla) VerifyToken(_ context.Context, token string) (*auth.Claims, error) {
	for _, c := range []*auth.Claims{monA, monB, monSin, supAB, ejecutivo, reaccion} {
		if c.Subject == token {
			return c, nil
		}
	}
	return claimsJefe, nil
}

func moduloAlcance(t *testing.T, ctx context.Context) *operacion.Module {
	t.Helper()
	return moduloCompletoCon(t, ctx, operacion.Deps{Verificador: autenticadorTabla{}, Atomos: catalogo})
}

// fijarAlcance deja en la proyección los clientes efectivos del sujeto, como lo
// haría alcance.cambiado.
func fijarAlcance(t *testing.T, ctx context.Context, c *auth.Claims, clientes ...uuid.UUID) {
	t.Helper()
	admin, cerrar := dbtest.AdminConn(t, ctx)
	defer cerrar()
	if clientes == nil {
		clientes = []uuid.UUID{}
	}
	if _, err := admin.Exec(ctx, `INSERT INTO viajes.alcance_sujeto (idp_subject, clientes_ids, cambiado_en) VALUES ($1, $2, now())
		ON CONFLICT (idp_subject) DO UPDATE SET clientes_ids = EXCLUDED.clientes_ids, cambiado_en = EXCLUDED.cambiado_en`,
		c.Subject, clientes); err != nil {
		t.Fatalf("fijar alcance: %v", err)
	}
}

func como(c *auth.Claims) context.Context { return auth.ContextWithClaims(context.Background(), c) }

// viajeDe crea un viaje del cliente como Operación: nace en APROBADO (ADR 38).
func viajeDe(t *testing.T, api operacion.ConsultasAPI, cliente uuid.UUID) operclient.Id {
	t.Helper()
	fecha := time.Now().Add(24 * time.Hour).UTC()
	v, err := api.CrearViaje(como(claimsJefe), operclient.CrearViajeEntrada{
		ClienteId: &cliente, Origen: "Origen", Destinos: []operclient.Destino{{Nombre: "Destino"}}, FechaHoraServicio: &fecha,
	})
	if err != nil {
		t.Fatalf("crear viaje: %v", err)
	}
	return v.Id
}

func transicionar(t *testing.T, api operacion.ConsultasAPI, ctx context.Context, id operclient.Id, a operclient.EstadoViaje) {
	t.Helper()
	causa := "motivo"
	if _, err := api.TransicionarViaje(ctx, id, operclient.TransicionEntrada{AEstado: a, Causa: &causa}); err != nil {
		t.Fatalf("transicionar a %s: %v", a, err)
	}
}

// todasLasPaginas recorre listarViajes por cursor y devuelve los ids en orden.
func todasLasPaginas(t *testing.T, api operacion.ConsultasAPI, ctx context.Context, estado *operclient.EstadoViaje) ([]operclient.Id, int) {
	t.Helper()
	var ids []operclient.Id
	params := operclient.ListarViajesParams{Estado: estado}
	paginas := 0
	for {
		p, err := api.ListarViajes(ctx, params)
		if err != nil {
			t.Fatalf("listar: %v", err)
		}
		paginas++
		for _, v := range p.Items {
			ids = append(ids, v.Id)
		}
		if p.CursorSiguiente == nil {
			return ids, paginas
		}
		params.Cursor = p.CursorSiguiente
	}
}

func esStatus(t *testing.T, caso string, err error, quiere int) {
	t.Helper()
	if got := platformerrors.AsCategory(err).HTTPStatus(); err == nil || got != quiere {
		t.Errorf("%s: err %v (status %d); quiere %d", caso, err, got, quiere)
	}
}

// TestAlcanceListarViajes: el filtro va en la consulta y pagina bien. El
// monitorista ve los viajes de sus clientes (52, en dos páginas, sin repetir ni
// perder; todo viaje nace en APROBADO, ADR 38), el supervisor la unión, uno sin
// alcance nada, el jefe todo; ejecutivo y reaccion 403. Para cada viaje, el
// detalle responde 200 si y solo si la lista lo trae.
func TestAlcanceListarViajes(t *testing.T) {
	ctx := context.Background()
	m := moduloAlcance(t, ctx)
	api := m.Consultas()
	A, B := uuid.New(), uuid.New()

	visiblesA := map[operclient.Id]bool{}
	for range 52 {
		visiblesA[viajeDe(t, api, A)] = true
	}
	cancelado := viajeDe(t, api, A)
	transicionar(t, api, como(claimsJefe), cancelado, operclient.CANCELADOPORCLIENTE)
	visiblesA[cancelado] = true
	var deB []operclient.Id
	for range 3 {
		deB = append(deB, viajeDe(t, api, B))
	}

	fijarAlcance(t, ctx, monA, A)
	fijarAlcance(t, ctx, supAB, A, B)

	ids, paginas := todasLasPaginas(t, api, como(monA), nil)
	vistos := map[operclient.Id]bool{}
	for _, id := range ids {
		if vistos[id] {
			t.Errorf("monitorista: viaje %s repetido entre páginas", id)
		}
		vistos[id] = true
	}
	if paginas < 2 || len(vistos) != len(visiblesA) {
		t.Errorf("monitorista: %d viajes en %d páginas; quiere %d en 2 o más", len(vistos), paginas, len(visiblesA))
	}
	for id := range visiblesA {
		if !vistos[id] {
			t.Errorf("monitorista: falta el viaje visible %s", id)
		}
	}

	estadoCancelado := operclient.CANCELADOPORCLIENTE
	if ids, _ := todasLasPaginas(t, api, como(monA), &estadoCancelado); len(ids) != 1 || ids[0] != cancelado {
		t.Errorf("monitorista con estado=CANCELADO_POR_CLIENTE: %v; quiere solo el cancelado", ids)
	}
	if ids, _ := todasLasPaginas(t, api, como(supAB), nil); len(ids) != len(visiblesA)+len(deB) {
		t.Errorf("supervisor A+B: %d viajes; quiere %d", len(ids), len(visiblesA)+len(deB))
	}
	if ids, _ := todasLasPaginas(t, api, como(monSin), nil); len(ids) != 0 {
		t.Errorf("monitorista sin alcance: %d viajes; quiere 0", len(ids))
	}
	if ids, _ := todasLasPaginas(t, api, como(claimsJefe), nil); len(ids) != len(visiblesA)+len(deB) {
		t.Errorf("jefe_operativo: %d viajes; quiere todos (%d)", len(ids), len(visiblesA)+len(deB))
	}
	for _, c := range []*auth.Claims{ejecutivo, reaccion} {
		_, err := api.ListarViajes(como(c), operclient.ListarViajesParams{})
		esStatus(t, "listar como "+c.Rol, err, http.StatusForbidden)
	}

	// Paridad lista ↔ detalle sobre todo el juego.
	todos := append([]operclient.Id{}, deB...)
	for id := range visiblesA {
		todos = append(todos, id)
	}
	for _, id := range todos {
		_, err := api.ObtenerViaje(como(monA), id)
		if vistos[id] != (err == nil) {
			t.Errorf("viaje %s: listado=%v, detalle err=%v", id, vistos[id], err)
		}
		if err != nil {
			esStatus(t, "detalle fuera de alcance", err, http.StatusNotFound)
		}
	}
}

// TestAlcanceDetalleYSubrecursos: fuera de alcance, el detalle y los
// subrecursos responden 404 antes de cualquier efecto, por fachada y HTTP. En un
// préstamo decide el cliente del viaje, no el de la unidad (DEC-149 #5). Dentro
// de alcance el monitorista pasa a APTO_PARA_SALIDA, igual que el jefe (DEC-149
// #7). Ejecutivo 403; Reacción lee el viaje y la escolta de su caso y nada más.
func TestAlcanceDetalleYSubrecursos(t *testing.T) {
	ctx := context.Background()
	m := moduloAlcance(t, ctx)
	api := m.Consultas()
	A, B := catalogo.cliente(catalogos.ClienteEstadoActivo), catalogo.cliente(catalogos.ClienteEstadoActivo)
	fijarAlcance(t, ctx, monA, A)
	fijarAlcance(t, ctx, monB, B)

	// Préstamo: la unidad sirve a B y escolta un viaje de A.
	unidadB, err := api.CrearUnidad(como(claimsJefe), operclient.UnidadEntrada{VehiculoId: coche(), CustodiosIds: ids(custodio()), ClienteId: &B})
	if err != nil {
		t.Fatalf("crear unidad de B: %v", err)
	}
	vA := viajeDe(t, api, A)
	if _, err := api.AsignarRecursos(como(claimsJefe), vA, operclient.AsignacionEntrada{UnidadesIds: ids(unidadB.Id)}); err != nil {
		t.Fatalf("asignar unidad prestada: %v", err)
	}
	vid := vA.String()
	subida := operclient.IntencionSubidaAdjuntoEntrada{Nombre: "foto.jpg", TipoMedio: "image/jpeg", TamanoBytes: 10, ChecksumSha256: strings.Repeat("a", 64)}

	// Monitorista de B (el cliente de la unidad): 404 en todo, sin efectos.
	ctxB := como(monB)
	_, err = api.ObtenerViaje(ctxB, vA)
	esStatus(t, "fachada obtenerViaje", err, 404)
	_, err = api.ObtenerFicha(ctxB, vA)
	esStatus(t, "fachada obtenerFicha", err, 404)
	_, err = api.ObtenerAsignacion(ctxB, vA)
	esStatus(t, "fachada obtenerAsignacion", err, 404)
	_, err = api.TransicionarViaje(ctxB, vA, operclient.TransicionEntrada{AEstado: operclient.APTOPARASALIDA})
	esStatus(t, "fachada transicionar", err, 404)
	_, err = api.ListarAdjuntos(ctxB, vA)
	esStatus(t, "fachada listarAdjuntos", err, 404)
	_, err = api.CrearIntencionSubidaAdjunto(ctxB, vA, subida)
	esStatus(t, "fachada crearIntencionSubida", err, 404)
	_, err = api.RegistrarDescargaViaje(ctxB, vA)
	esStatus(t, "fachada registrarDescarga", err, 404)
	_, err = api.FijarEta(ctxB, vA, operclient.EtaEntrada{EtaEstimado: time.Now()})
	esStatus(t, "fachada fijarEta", err, 404)
	cuerpoSubida, _ := json.Marshal(subida)
	for _, r := range []struct{ method, path, body string }{
		{http.MethodGet, "/api/v1/viajes/" + vid + "/ficha", ""},
		{http.MethodPatch, "/api/v1/viajes/" + vid + "/ficha", `{"gps_ids":[]}`},
		{http.MethodPost, "/api/v1/viajes/" + vid + "/transiciones", `{"a_estado":"APTO_PARA_SALIDA"}`},
		{http.MethodGet, "/api/v1/viajes/" + vid + "/adjuntos", ""},
		{http.MethodPost, "/api/v1/viajes/" + vid + "/adjuntos/intenciones-subida", string(cuerpoSubida)},
		{http.MethodGet, "/api/v1/viajes/" + vid + "/adjuntos/" + uuid.NewString() + "/descarga", ""},
		{http.MethodPost, "/api/v1/viajes/" + vid + "/bitacora", `{"texto":"x"}`},
		{http.MethodPost, "/api/v1/viajes/" + vid + "/incidencias", `{"tipo_interno":"falla_mecanica"}`},
		{http.MethodGet, "/api/v1/viajes/" + vid + "/asignacion", ""},
	} {
		if rec := pedir(m, r.method, r.path, r.body, monB.Subject); rec.Code != http.StatusNotFound {
			t.Errorf("HTTP %s %s como monitorista de B: %d %s; quiere 404", r.method, r.path, rec.Code, rec.Body)
		}
	}
	// Ningún efecto: el viaje sigue ASIGNADO y sin adjuntos ni entradas.
	ctxA := como(monA)
	if det, err := api.ObtenerViaje(ctxA, vA); err != nil || det.Viaje.Estado != operclient.ASIGNADO {
		t.Fatalf("monitorista de A lee su viaje: %+v err %v; quiere ASIGNADO", det, err)
	}
	if pag, err := api.ListarAdjuntos(ctxA, vA); err != nil || len(pag.Items) != 0 {
		t.Errorf("adjuntos tras los 404: %+v err %v; quiere ninguno", pag, err)
	}
	if pag, err := api.ListarBitacora(ctxA, vA, operclient.ListarBitacoraParams{}); err != nil || len(pag.Items) != 0 {
		t.Errorf("bitácora tras los 404: %+v err %v; quiere vacía", pag, err)
	}

	// Monitorista de A (el cliente del viaje): opera.
	if _, err := api.ObtenerFicha(ctxA, vA); err != nil {
		t.Errorf("monitorista de A lee la ficha: %v", err)
	}
	if _, err := api.CrearIntencionSubidaAdjunto(ctxA, vA, subida); err != nil {
		t.Errorf("monitorista de A sube evidencia: %v", err)
	}
	if rec := pedir(m, http.MethodPost, "/api/v1/viajes/"+vid+"/transiciones", `{"a_estado":"APTO_PARA_SALIDA"}`, monA.Subject); rec.Code != http.StatusOK {
		t.Errorf("monitorista de A: ASIGNADO → APTO_PARA_SALIDA: %d %s; quiere 200", rec.Code, rec.Body)
	}
	// El jefe_operativo también lo hace (DEC-149 #7), sobre otro viaje.
	otra, err := api.CrearUnidad(como(claimsJefe), operclient.UnidadEntrada{VehiculoId: coche(), CustodiosIds: ids(custodio())})
	if err != nil {
		t.Fatalf("crear unidad: %v", err)
	}
	vJ := viajeDe(t, api, A)
	if _, err := api.AsignarRecursos(como(claimsJefe), vJ, operclient.AsignacionEntrada{UnidadesIds: ids(otra.Id)}); err != nil {
		t.Fatalf("asignar: %v", err)
	}
	transicionar(t, api, como(claimsJefe), vJ, operclient.APTOPARASALIDA)

	// Ejecutivo: 403 en el detalle y en los subrecursos.
	_, err = api.ObtenerViaje(como(ejecutivo), vA)
	esStatus(t, "ejecutivo obtenerViaje", err, 403)
	if rec := pedir(m, http.MethodGet, "/api/v1/viajes/"+vid+"/ficha", "", ejecutivo.Subject); rec.Code != http.StatusForbidden {
		t.Errorf("HTTP ejecutivo ficha: %d; quiere 403", rec.Code)
	}
	// Reacción, sin alcance en la proyección: lee su caso, nada más.
	if _, err := api.ObtenerViaje(como(reaccion), vA); err != nil {
		t.Errorf("reaccion obtenerViaje: %v; quiere 200", err)
	}
	if e, err := api.ObtenerAsignacion(como(reaccion), vA); err != nil || len(e.Unidades) != 1 {
		t.Errorf("reaccion obtenerAsignacion: %+v err %v; quiere la escolta", e, err)
	}
	_, err = api.ObtenerFicha(como(reaccion), vA)
	esStatus(t, "reaccion obtenerFicha", err, 403)
}

// alcancesFake es identidad.listarAlcances: sin cursor, el snapshot; con
// cursor, la continuación.
type alcancesFake struct {
	snapshot, continuacion []identidad.AlcanceEfectivo
	cursorPedido           *string
}

func (f *alcancesFake) ListarAlcances(_ context.Context, p identidad.ListarAlcancesParams) (*identidad.PaginaAlcances, error) {
	cur := "cursor-1"
	if p.Cursor == nil {
		return &identidad.PaginaAlcances{Items: f.snapshot, CursorSiguiente: &cur}, nil
	}
	f.cursorPedido = p.Cursor
	return &identidad.PaginaAlcances{Items: f.continuacion, CursorSiguiente: &cur}, nil
}

func buscarWorker(t *testing.T, ws []worker.Worker, nombre string) worker.Worker {
	t.Helper()
	for _, w := range ws {
		if w.Name() == nombre {
			return w
		}
	}
	t.Fatalf("falta el worker %s", nombre)
	return nil
}

// TestAlcanceProyeccion: la proyección se arma desde el snapshot y su
// continuación al arrancar (not-ready hasta converger; un item vacío en la
// continuación quita al sujeto; un sujeto que ya no está en el snapshot se
// vacía) y después la mueven los eventos alcance.cambiado (un evento más viejo
// que lo guardado no gana; uno vacío quita). Se observa por listarViajes.
func TestAlcanceProyeccion(t *testing.T) {
	ctx := context.Background()
	sub := &suscriptorFake{}
	A, B := uuid.New(), uuid.New()
	uA, uB, uViejo := uuid.New(), uuid.New(), uuid.New()
	t0 := time.Now().UTC().Add(-time.Hour)
	fake := &alcancesFake{
		snapshot: []identidad.AlcanceEfectivo{
			{SujetoId: uuid.New(), IdpSubject: uA.String(), ClientesEfectivosIds: []uuid.UUID{A}, CambiadoEn: t0},
			{SujetoId: uuid.New(), IdpSubject: uB.String(), ClientesEfectivosIds: []uuid.UUID{B}, CambiadoEn: t0},
		},
		continuacion: []identidad.AlcanceEfectivo{
			{SujetoId: uuid.New(), IdpSubject: uB.String(), ClientesEfectivosIds: []uuid.UUID{}, CambiadoEn: t0.Add(time.Minute)},
		},
	}
	m := moduloCompletoCon(t, ctx, operacion.Deps{Verificador: autenticadorTabla{}, Atomos: catalogo, Subscriber: sub, Alcances: fake})
	api := m.Consultas()
	vA, vB := viajeDe(t, api, A), viajeDe(t, api, B)
	// Un sujeto que quedó de una corrida anterior y ya no viene en el snapshot.
	fijarAlcance(t, ctx, &auth.Claims{Subject: uViejo.String()}, A)

	mon := func(id uuid.UUID) context.Context {
		return como(&auth.Claims{Subject: id.String(), Rol: "monitorista", Audiencia: auth.AudienciaInterno})
	}
	ve := func(id uuid.UUID) map[operclient.Id]bool {
		t.Helper()
		ids, _ := todasLasPaginas(t, api, mon(id), nil)
		out := map[operclient.Id]bool{}
		for _, v := range ids {
			out[v] = true
		}
		return out
	}

	w := buscarWorker(t, m.Workers(), "operacion-alcance")
	if err := w.Ready(ctx); err == nil {
		t.Error("antes de converger: Ready nil; quiere not-ready")
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() { _ = w.Run(runCtx) }()
	limite := time.Now().Add(5 * time.Second)
	for w.Ready(ctx) != nil {
		if time.Now().After(limite) {
			t.Fatalf("no convergió: %v", w.Ready(ctx))
		}
		time.Sleep(10 * time.Millisecond)
	}
	if fake.cursorPedido == nil || *fake.cursorPedido != "cursor-1" {
		t.Errorf("continuación: cursor %v; quiere el del snapshot", fake.cursorPedido)
	}
	if v := ve(uA); len(v) != 1 || !v[vA] {
		t.Errorf("snapshot: uA ve %v; quiere solo %s", v, vA)
	}
	if v := ve(uB); len(v) != 0 {
		t.Errorf("continuación con item vacío: uB ve %v; quiere nada", v)
	}
	if v := ve(uViejo); len(v) != 0 {
		t.Errorf("sujeto fuera del snapshot: ve %v; quiere nada", v)
	}

	// Eventos alcance.cambiado por el consumidor durable.
	s := sub.buscar("operacion-alcance")
	if s == nil || len(s.filtro) != 1 || s.filtro[0] != "alcance.cambiado" || s.superficie != bus.SensibilidadAmplia {
		t.Fatalf("suscripción: %+v; quiere operacion-alcance a alcance.cambiado, amplia", s)
	}
	evento := func(sujeto uuid.UUID, cuando time.Time, clientes ...uuid.UUID) {
		t.Helper()
		ids := []string{}
		for _, c := range clientes {
			ids = append(ids, c.String())
		}
		p, _ := json.Marshal(map[string]any{"sujeto_id": uuid.NewString(), "idp_subject": sujeto.String(), "clientes_efectivos_ids": ids, "cambiado_en": cuando.Format(time.RFC3339Nano)})
		if err := s.h(ctx, bus.Sobre{EventoID: uuid.NewString(), Tipo: "alcance.cambiado", AgregadoID: sujeto.String(), Payload: p}); err != nil {
			t.Fatalf("procesar evento: %v", err)
		}
	}
	t1 := t0.Add(2 * time.Minute)
	evento(uB, t1, A, B)
	if v := ve(uB); len(v) != 2 {
		t.Errorf("evento: uB ve %d viajes; quiere 2", len(v))
	}
	evento(uB, t1, A, B) // reentrega: mismo estado
	evento(uB, t0, B)    // atrasado: no gana
	if v := ve(uB); len(v) != 2 {
		t.Errorf("reentrega y evento atrasado: uB ve %d viajes; quiere 2", len(v))
	}
	evento(uB, t1.Add(time.Minute)) // vacío: quita
	if v := ve(uB); len(v) != 0 {
		t.Errorf("evento vacío: uB ve %v; quiere nada", v)
	}
	if v := ve(uA); !v[vA] || v[vB] {
		t.Errorf("uA no cambió: ve %v", v)
	}
}
