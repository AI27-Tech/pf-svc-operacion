package operacion_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	operclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/operacion"
	"github.com/AI27-Tech/pf-pkg-platform-go/auth"
	platformerrors "github.com/AI27-Tech/pf-pkg-platform-go/errors"
	platformpgx "github.com/AI27-Tech/pf-pkg-platform-go/pgxpool"
	"github.com/AI27-Tech/pf-svc-operacion"
	"github.com/AI27-Tech/pf-svc-operacion/internal/dbtest"
	"github.com/AI27-Tech/pf-svc-operacion/migrations"
	"github.com/google/uuid"
)

// claimsJefe son los claims de un operador interno que puede aprobar viajes.
var claimsJefe = &auth.Claims{Subject: "jefe-1", Rol: "jefe_operativo", Audiencia: auth.AudienciaInterno}

// autenticadorJefe acepta cualquier token y devuelve claimsJefe, para montar la
// ruta HTTP con el MISMO actor que la fachada y comparar ambos caminos. El token
// "monitorista" devuelve claimsMonitorista, para las rutas que gatean por rol.
type autenticadorJefe struct{}

func (autenticadorJefe) VerifyToken(_ context.Context, token string) (*auth.Claims, error) {
	if token == "monitorista" {
		return claimsMonitorista, nil
	}
	return claimsJefe, nil
}

// claimsMonitorista son los claims de un monitorista: edita la ficha (DEC-060 #2).
var claimsMonitorista = &auth.Claims{Subject: "mon-1", Rol: "monitorista", Audiencia: auth.AudienciaInterno}

// moduloConViajes aplica las migraciones de viajes (mismo setup que
// internal/viajes) y construye el módulo como lo hace core, con el pool op_viajes.
func moduloConViajes(t *testing.T, ctx context.Context) *operacion.Module {
	t.Helper()
	dbtest.Serializar(t, ctx)
	admin, cerrar := dbtest.AdminConn(t, ctx)
	defer cerrar()
	dbtest.Exec(t, ctx, admin,
		`DO $$ BEGIN IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname='op_asignacion') THEN CREATE ROLE op_asignacion LOGIN; END IF; END $$;`)
	archivos, err := migrations.ArchivosDe("viajes")
	if err != nil {
		t.Fatalf("listar migraciones viajes: %v", err)
	}
	for _, f := range archivos {
		sql, err := migrations.FS.ReadFile(f)
		if err != nil {
			t.Fatalf("leer %s: %v", f, err)
		}
		dbtest.Exec(t, ctx, admin, string(sql))
	}
	dbtest.Exec(t, ctx, admin,
		"TRUNCATE viajes.viaje, viajes.destino, viajes.condicion_viaje, viajes.estadia, viajes.transicion_viaje, viajes.outbox")
	pool, cerrarPool := dbtest.PoolAs(t, ctx, "op_viajes", "viajes")
	t.Cleanup(cerrarPool)
	m, err := operacion.New(operacion.Pools{Viajes: &platformpgx.Pool{Pool: pool, Role: "op_viajes"}}, operacion.Deps{Verificador: autenticadorJefe{}})
	if err != nil {
		t.Fatalf("operacion.New: %v", err)
	}
	return m
}

// TestConsultasTransicionarViaje llama la fachada en proceso como core: una
// transición legal avanza el viaje, una ilegal se rechaza con la misma categoría
// que la ruta HTTP (409), y sin claims se rechaza como no autorizado (401).
func TestConsultasTransicionarViaje(t *testing.T) {
	ctx := context.Background()
	m := moduloConViajes(t, ctx)
	api := m.Consultas()

	cli := uuid.New()
	fecha := time.Now().Add(24 * time.Hour).UTC()
	ctxCliente := auth.ContextWithClaims(ctx, &auth.Claims{Subject: "cli-1", Rol: "cliente", Audiencia: auth.AudienciaCliente, TenantID: cli.String()})
	v, err := api.CrearViaje(ctxCliente, operclient.CrearViajeEntrada{
		Origen: "Origen", Destinos: []operclient.Destino{{Nombre: "Destino"}}, FechaHoraServicio: &fecha,
	})
	if err != nil {
		t.Fatalf("crear viaje: %v", err)
	}

	ctxJefe := auth.ContextWithClaims(ctx, claimsJefe)

	// Legal: SOLICITADO -> APROBADO.
	out, err := api.TransicionarViaje(ctxJefe, v.Id, operclient.TransicionEntrada{AEstado: operclient.APROBADO})
	if err != nil {
		t.Fatalf("SOLICITADO->APROBADO: %v", err)
	}
	if out.Estado != "APROBADO" || out.Id != v.Id {
		t.Errorf("tras aprobar: got estado %q id %s, want APROBADO id %s", out.Estado, out.Id, v.Id)
	}

	// Ilegal: APROBADO -> EN_DESTINO no es arista. Fachada y HTTP dan 409.
	_, err = api.TransicionarViaje(ctxJefe, v.Id, operclient.TransicionEntrada{AEstado: operclient.ENDESTINO})
	if got := platformerrors.AsCategory(err).HTTPStatus(); got != 409 {
		t.Errorf("fachada APROBADO->EN_DESTINO: got status %d (err %v), want 409", got, err)
	}
	mux := http.NewServeMux()
	m.Routes(mux)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/viajes/"+v.Id.String()+"/transiciones", strings.NewReader(`{"a_estado":"EN_DESTINO"}`))
	req.Header.Set("Authorization", "Bearer x")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 409 {
		t.Errorf("HTTP APROBADO->EN_DESTINO: got status %d, want 409 (body %s)", rec.Code, rec.Body)
	}

	// Sin claims: no autorizado, y el viaje no se mueve.
	_, err = api.TransicionarViaje(ctx, v.Id, operclient.TransicionEntrada{AEstado: operclient.CANCELADO})
	if got := platformerrors.AsCategory(err); got != platformerrors.CategoryNoAutorizado {
		t.Errorf("sin claims: got categoría %v (err %v), want NoAutorizado", got, err)
	}
	det, err := api.ObtenerViaje(ctxJefe, v.Id)
	if err != nil {
		t.Fatalf("obtener viaje: %v", err)
	}
	if det.Viaje.Estado != "APROBADO" {
		t.Errorf("estado final: got %q, want APROBADO", det.Viaje.Estado)
	}
}

// claimsSupervisor son los claims de un supervisor: abre y resuelve incidencias.
var claimsSupervisor = &auth.Claims{Subject: "sup-1", Rol: "supervisor", Audiencia: auth.AudienciaInterno}

// moduloCompleto aplica TODAS las migraciones en dos fases (todos los 0001
// primero, luego el resto, igual que internal/asignacion) y construye el módulo
// con los pools de viajes, asignacion y bitacora, como core.
func moduloCompleto(t *testing.T, ctx context.Context) *operacion.Module {
	t.Helper()
	dbtest.Serializar(t, ctx)
	admin, cerrar := dbtest.AdminConn(t, ctx)
	defer cerrar()
	aplica := func(f string) {
		sql, err := migrations.FS.ReadFile(f)
		if err != nil {
			t.Fatalf("leer %s: %v", f, err)
		}
		dbtest.Exec(t, ctx, admin, string(sql))
	}
	for _, esquema := range migrations.Orden {
		archivos, err := migrations.ArchivosDe(esquema)
		if err != nil || len(archivos) == 0 {
			t.Fatalf("archivos de %s: %v", esquema, err)
		}
		aplica(archivos[0])
	}
	for _, esquema := range migrations.Orden {
		archivos, _ := migrations.ArchivosDe(esquema)
		for _, f := range archivos[1:] {
			aplica(f)
		}
	}
	dbtest.Exec(t, ctx, admin,
		"TRUNCATE viajes.viaje, viajes.destino, viajes.condicion_viaje, viajes.estadia, viajes.transicion_viaje, viajes.outbox, "+
			"asignacion.asignacion_viaje, asignacion.reserva_recurso, asignacion.historia_asignacion, auditoria.registro_auditoria, bitacora.entrada_bitacora, "+
			"roster.unidad_custodio, roster.unidad")
	pool := func(rol, searchPath string) *platformpgx.Pool {
		p, cerrarPool := dbtest.PoolAs(t, ctx, rol, searchPath)
		t.Cleanup(cerrarPool)
		return &platformpgx.Pool{Pool: p, Role: rol}
	}
	m, err := operacion.New(operacion.Pools{
		Viajes:     pool("op_viajes", "viajes"),
		Asignacion: pool("op_asignacion", "viajes"),
		Bitacora:   pool("op_bitacora", ""),
		Roster:     pool("op_roster", ""),
	}, operacion.Deps{Verificador: autenticadorJefe{}, Atomos: catalogo})
	if err != nil {
		t.Fatalf("operacion.New: %v", err)
	}
	return m
}

// viajeAprobado crea un viaje como cliente y lo aprueba como jefe_operativo.
func viajeAprobado(t *testing.T, ctx context.Context, api operacion.ConsultasAPI) operclient.Id {
	t.Helper()
	fecha := time.Now().Add(24 * time.Hour).UTC()
	ctxCliente := auth.ContextWithClaims(ctx, &auth.Claims{Subject: "cli-1", Rol: "cliente", Audiencia: auth.AudienciaCliente, TenantID: uuid.NewString()})
	v, err := api.CrearViaje(ctxCliente, operclient.CrearViajeEntrada{
		Origen: "Origen", Destinos: []operclient.Destino{{Nombre: "Destino"}}, FechaHoraServicio: &fecha,
	})
	if err != nil {
		t.Fatalf("crear viaje: %v", err)
	}
	if _, err := api.TransicionarViaje(auth.ContextWithClaims(ctx, claimsJefe), v.Id, operclient.TransicionEntrada{AEstado: operclient.APROBADO}); err != nil {
		t.Fatalf("aprobar viaje: %v", err)
	}
	return v.Id
}

// mismoStatus afirma que la fachada y la ruta HTTP (con claimsJefe) rechazan con
// el mismo status, y que es el esperado.
func mismoStatus(t *testing.T, m *operacion.Module, errFachada error, method, path, body string, want int) {
	t.Helper()
	if got := platformerrors.AsCategory(errFachada).HTTPStatus(); got != want {
		t.Errorf("fachada %s %s: got status %d (err %v), want %d", method, path, got, errFachada, want)
	}
	mux := http.NewServeMux()
	m.Routes(mux)
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer x")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != want {
		t.Errorf("HTTP %s %s: got status %d, want %d (body %s)", method, path, rec.Code, want, rec.Body)
	}
}

// TestConsultasFijarEta: fija el ETA de un viaje aprobado; un viaje inexistente
// se rechaza igual por fachada y HTTP (404).
func TestConsultasFijarEta(t *testing.T) {
	ctx := context.Background()
	m := moduloCompleto(t, ctx)
	api := m.Consultas()
	ctxJefe := auth.ContextWithClaims(ctx, claimsJefe)

	id := viajeAprobado(t, ctx, api)
	eta := time.Now().Add(48 * time.Hour).UTC().Truncate(time.Second)
	v, err := api.FijarEta(ctxJefe, id, operclient.EtaEntrada{EtaEstimado: eta})
	if err != nil {
		t.Fatalf("FijarEta: %v", err)
	}
	if v.EtaEstimado == nil || !v.EtaEstimado.Equal(eta) {
		t.Errorf("eta_estimado: got %v, want %v", v.EtaEstimado, eta)
	}

	otro := uuid.New()
	_, err = api.FijarEta(ctxJefe, otro, operclient.EtaEntrada{EtaEstimado: eta})
	mismoStatus(t, m, err, http.MethodPatch, "/api/v1/viajes/"+otro.String()+"/eta", `{"eta_estimado":"`+eta.Format(time.RFC3339)+`"}`, 404)
}

// TestConsultasAsignarRecursos: asigna la escolta a un viaje aprobado (queda
// ASIGNADO); un viaje no aprobado se rechaza igual por fachada y HTTP (409); sin
// claims, no autorizado.
func TestConsultasAsignarRecursos(t *testing.T) {
	ctx := context.Background()
	m := moduloCompleto(t, ctx)
	api := m.Consultas()
	ctxJefe := auth.ContextWithClaims(ctx, claimsJefe)

	id := viajeAprobado(t, ctx, api)
	a, err := api.AsignarRecursos(ctxJefe, id, operclient.AsignacionEntrada{UnidadesIds: []operclient.Id{crear(t, api, moto(), custodio()).Id}})
	if err != nil {
		t.Fatalf("AsignarRecursos: %v", err)
	}
	if a.Estado != operclient.ASIGNADO || a.ViajeId != id || len(a.Unidades) != 1 {
		t.Errorf("asignación: got estado %q viaje %s unidades %d, want ASIGNADO %s 1", a.Estado, a.ViajeId, len(a.Unidades), id)
	}

	// Ya ASIGNADO: no se vuelve a asignar (solo APROBADO).
	u := crear(t, api, moto(), custodio()).Id
	_, err = api.AsignarRecursos(ctxJefe, id, operclient.AsignacionEntrada{UnidadesIds: []operclient.Id{u}})
	mismoStatus(t, m, err, http.MethodPost, "/api/v1/viajes/"+id.String()+"/asignacion", `{"unidades_ids":["`+u.String()+`"]}`, 409)

	_, err = api.AsignarRecursos(ctx, id, operclient.AsignacionEntrada{UnidadesIds: []operclient.Id{u}})
	if got := platformerrors.AsCategory(err); got != platformerrors.CategoryNoAutorizado {
		t.Errorf("sin claims: got categoría %v (err %v), want NoAutorizado", got, err)
	}
}

// TestConsultasBitacora: registra una entrada con el sujeto del token como autor
// y la lee; un texto vacío se rechaza igual por fachada y HTTP (400).
func TestConsultasBitacora(t *testing.T) {
	ctx := context.Background()
	m := moduloCompleto(t, ctx)
	api := m.Consultas()
	ctxJefe := auth.ContextWithClaims(ctx, claimsJefe)

	id := viajeAprobado(t, ctx, api)
	e, err := api.RegistrarEntradaBitacora(ctxJefe, id, operclient.EntradaBitacoraEntrada{Texto: "salida de origen"})
	if err != nil {
		t.Fatalf("RegistrarEntradaBitacora: %v", err)
	}
	if e.Autor != "jefe-1" || e.Texto != "salida de origen" {
		t.Errorf("entrada: got autor %q texto %q, want jefe-1 / salida de origen", e.Autor, e.Texto)
	}
	pag, err := api.ListarBitacora(ctxJefe, id, operclient.ListarBitacoraParams{})
	if err != nil {
		t.Fatalf("ListarBitacora: %v", err)
	}
	if len(pag.Items) != 1 || pag.Items[0].Id != e.Id {
		t.Errorf("listar: got %d entradas, want 1 con id %s", len(pag.Items), e.Id)
	}

	_, err = api.RegistrarEntradaBitacora(ctxJefe, id, operclient.EntradaBitacoraEntrada{Texto: ""})
	mismoStatus(t, m, err, http.MethodPost, "/api/v1/viajes/"+id.String()+"/bitacora", `{"texto":""}`, 400)

	_, err = api.RegistrarEntradaBitacora(ctx, id, operclient.EntradaBitacoraEntrada{Texto: "x"})
	if got := platformerrors.AsCategory(err); got != platformerrors.CategoryNoAutorizado {
		t.Errorf("sin claims: got categoría %v (err %v), want NoAutorizado", got, err)
	}
}

// TestConsultasIncidencias: un supervisor abre y resuelve; jefe_operativo no
// puede abrir y un monitorista no puede resolver: 403 (DEC-132) igual por
// fachada y HTTP.
func TestConsultasIncidencias(t *testing.T) {
	ctx := context.Background()
	m := moduloCompleto(t, ctx)
	api := m.Consultas()
	ctxSup := auth.ContextWithClaims(ctx, claimsSupervisor)

	id := viajeAprobado(t, ctx, api)
	inc, err := api.AbrirIncidencia(ctxSup, id, operclient.IncidenciaEntrada{TipoInterno: "falla_mecanica"})
	if err != nil {
		t.Fatalf("AbrirIncidencia: %v", err)
	}
	res, err := api.ResolverIncidencia(ctxSup, id, inc.Id, operclient.ResolucionIncidenciaEntrada{Resolucion: "reparada"})
	if err != nil {
		t.Fatalf("ResolverIncidencia: %v", err)
	}
	if res.Id != inc.Id || res.ResueltaEn == nil {
		t.Errorf("resuelta: got id %s resuelta_en %v, want id %s con cierre", res.Id, res.ResueltaEn, inc.Id)
	}

	_, err = api.AbrirIncidencia(auth.ContextWithClaims(ctx, claimsJefe), id, operclient.IncidenciaEntrada{TipoInterno: "falla_mecanica"})
	mismoStatus(t, m, err, http.MethodPost, "/api/v1/viajes/"+id.String()+"/incidencias", `{"tipo_interno":"falla_mecanica"}`, 403)

	abierta, err := api.AbrirIncidencia(ctxSup, id, operclient.IncidenciaEntrada{TipoInterno: "falla_mecanica"})
	if err != nil {
		t.Fatalf("AbrirIncidencia (segunda): %v", err)
	}
	_, err = api.ResolverIncidencia(auth.ContextWithClaims(ctx, claimsMonitorista), id, abierta.Id, operclient.ResolucionIncidenciaEntrada{Resolucion: "reparada"})
	if got := platformerrors.AsCategory(err).HTTPStatus(); got != 403 {
		t.Errorf("monitorista resuelve: got status %d (err %v), want 403", got, err)
	}
	mux := http.NewServeMux()
	m.Routes(mux)
	req := httptest.NewRequest(http.MethodPatch, "/api/v1/viajes/"+id.String()+"/incidencias/"+abierta.Id.String(), strings.NewReader(`{"resolucion":"reparada"}`))
	req.Header.Set("Authorization", "Bearer monitorista")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), `"prohibido"`) {
		t.Errorf("HTTP monitorista resuelve: got status %d body %s, want 403 prohibido", rec.Code, rec.Body)
	}
}

// TestConsultasEstadias: abre, lista y cierra una estadía; cerrar una estadía
// inexistente se rechaza igual por fachada y HTTP (404).
func TestConsultasEstadias(t *testing.T) {
	ctx := context.Background()
	m := moduloCompleto(t, ctx)
	api := m.Consultas()
	ctxJefe := auth.ContextWithClaims(ctx, claimsJefe)

	id := viajeAprobado(t, ctx, api)
	est, err := api.AbrirEstadia(ctxJefe, id, operclient.EstadiaEntrada{Motivo: operclient.Combustible, EjeAutorizacion: operclient.Autorizada})
	if err != nil {
		t.Fatalf("AbrirEstadia: %v", err)
	}
	pag, err := api.ListarEstadias(ctxJefe, id, operclient.ListarEstadiasParams{})
	if err != nil {
		t.Fatalf("ListarEstadias: %v", err)
	}
	if len(pag.Items) != 1 || pag.Items[0].Id != est.Id {
		t.Errorf("listar: got %d estadías, want 1 con id %s", len(pag.Items), est.Id)
	}
	cerrada, err := api.CerrarEstadia(ctxJefe, id, est.Id)
	if err != nil {
		t.Fatalf("CerrarEstadia: %v", err)
	}
	if cerrada.Cierre == nil {
		t.Error("estadía cerrada sin cierre")
	}

	otra := uuid.New()
	_, err = api.CerrarEstadia(ctxJefe, id, otra)
	mismoStatus(t, m, err, http.MethodPatch, "/api/v1/viajes/"+id.String()+"/estadias/"+otra.String(), `{}`, 404)
}

// TestConsultasObtenerAsignacion: un interno lee la escolta (vacía sin
// asignación) igual por fachada y HTTP (200); el Cliente recibe 404 (DEC-132) y
// sin claims es no autorizado.
func TestConsultasObtenerAsignacion(t *testing.T) {
	ctx := context.Background()
	m := moduloCompleto(t, ctx)
	api := m.Consultas()

	id := viajeAprobado(t, ctx, api)
	e, err := api.ObtenerAsignacion(auth.ContextWithClaims(ctx, claimsJefe), id)
	if err != nil || e.Unidades == nil || len(e.Unidades) != 0 {
		t.Fatalf("interno sin asignación: %+v err %v; quiere unidades []", e, err)
	}
	mux := http.NewServeMux()
	m.Routes(mux)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/viajes/"+id.String()+"/asignacion", nil)
	req.Header.Set("Authorization", "Bearer x")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"unidades":[]`) {
		t.Errorf("HTTP: status %d cuerpo %s; quiere 200 con unidades []", rec.Code, rec.Body)
	}

	cliente := &auth.Claims{Subject: "cli-1", TenantID: uuid.NewString(), Rol: "cliente", Audiencia: auth.AudienciaCliente}
	if _, err := api.ObtenerAsignacion(auth.ContextWithClaims(ctx, cliente), id); platformerrors.AsCategory(err) != platformerrors.CategoryNoEncontrado {
		t.Errorf("cliente: err %v, quiere no_encontrado", err)
	}
	if _, err := api.ObtenerAsignacion(ctx, id); platformerrors.AsCategory(err) != platformerrors.CategoryNoAutorizado {
		t.Errorf("sin claims: err %v, quiere no_autorizado", err)
	}
}

// TestConsultasListarUnidades: el jefe_operativo lee el roster (activas e
// inactivas, con tipo y custodios) igual por fachada y HTTP; un monitorista
// recibe 403 (DEC-060 #1) y un cursor ilegible, 400.
func TestConsultasListarUnidades(t *testing.T) {
	ctx := context.Background()
	m := moduloCompleto(t, ctx)
	api := m.Consultas()
	admin, cerrar := dbtest.AdminConn(t, ctx)
	defer cerrar()

	patrulla := uuid.MustParse("00000000-0000-0000-0000-0000000000a1")
	moto := uuid.MustParse("00000000-0000-0000-0000-0000000000a2")
	veh1 := uuid.MustParse("00000000-0000-0000-0000-0000000000b1")
	veh2 := uuid.MustParse("00000000-0000-0000-0000-0000000000b2")
	cus1 := uuid.MustParse("00000000-0000-0000-0000-0000000000c1")
	cus2 := uuid.MustParse("00000000-0000-0000-0000-0000000000c2")
	cus3 := uuid.MustParse("00000000-0000-0000-0000-0000000000c3")
	dbtest.Exec(t, ctx, admin, `INSERT INTO roster.unidad (id, tipo, vehiculo_id, activa) VALUES
		('`+patrulla.String()+`', 'patrulla', '`+veh1.String()+`', true),
		('`+moto.String()+`', 'moto', '`+veh2.String()+`', false)`)
	dbtest.Exec(t, ctx, admin, `INSERT INTO roster.unidad_custodio (unidad_id, custodio_id, activa) VALUES
		('`+patrulla.String()+`', '`+cus2.String()+`', true),
		('`+patrulla.String()+`', '`+cus1.String()+`', true),
		('`+moto.String()+`', '`+cus3.String()+`', false)`)

	pag, err := api.ListarUnidades(auth.ContextWithClaims(ctx, claimsJefe), operclient.ListarUnidadesParams{})
	if err != nil {
		t.Fatalf("ListarUnidades: %v", err)
	}
	if len(pag.Items) != 2 || pag.CursorSiguiente != nil {
		t.Fatalf("página: got %d unidades cursor %v, want 2 sin cursor", len(pag.Items), pag.CursorSiguiente)
	}
	p, mo := pag.Items[0], pag.Items[1]
	if p.Id != patrulla || *p.Tipo != operclient.Patrulla || p.VehiculoId != veh1 || !p.Activa ||
		len(p.CustodiosIds) != 2 || p.CustodiosIds[0] != cus1 || p.CustodiosIds[1] != cus2 {
		t.Errorf("patrulla: got %+v", p)
	}
	if mo.Id != moto || *mo.Tipo != operclient.Moto || mo.Activa || len(mo.CustodiosIds) != 1 || mo.CustodiosIds[0] != cus3 {
		t.Errorf("moto: got %+v", mo)
	}

	mux := http.NewServeMux()
	m.Routes(mux)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/unidades", nil)
	req.Header.Set("Authorization", "Bearer x")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"id":"`+moto.String()+`"`) ||
		!strings.Contains(rec.Body.String(), `"cursor_siguiente":null`) {
		t.Errorf("HTTP: status %d cuerpo %s; quiere 200 con la moto y cursor null", rec.Code, rec.Body)
	}

	monitorista := &auth.Claims{Subject: "mon-1", Rol: "monitorista", Audiencia: auth.AudienciaInterno}
	if _, err := api.ListarUnidades(auth.ContextWithClaims(ctx, monitorista), operclient.ListarUnidadesParams{}); platformerrors.AsCategory(err).HTTPStatus() != 403 {
		t.Errorf("monitorista: err %v, quiere 403", err)
	}
	malo := operclient.Cursor("%%%")
	_, err = api.ListarUnidades(auth.ContextWithClaims(ctx, claimsJefe), operclient.ListarUnidadesParams{Cursor: &malo})
	mismoStatus(t, m, err, http.MethodGet, "/api/v1/unidades?cursor=%25%25%25", "", 400)
}

// TestConsultasCompletarFechaHora: una solicitud sin fecha/hora no se aprueba;
// el jefe_operativo la completa y entonces sí. Un monitorista recibe 403 y un
// viaje terminal 409 viaje_en_estado_terminal, igual por fachada y HTTP.
func TestConsultasCompletarFechaHora(t *testing.T) {
	ctx := context.Background()
	m := moduloCompleto(t, ctx)
	api := m.Consultas()
	ctxJefe := auth.ContextWithClaims(ctx, claimsJefe)

	ctxCliente := auth.ContextWithClaims(ctx, &auth.Claims{Subject: "cli-1", Rol: "cliente", Audiencia: auth.AudienciaCliente, TenantID: uuid.NewString()})
	v, err := api.CrearViaje(ctxCliente, operclient.CrearViajeEntrada{Origen: "Origen", Destinos: []operclient.Destino{{Nombre: "Destino"}}})
	if err != nil {
		t.Fatalf("crear viaje: %v", err)
	}
	if _, err := api.TransicionarViaje(ctxJefe, v.Id, operclient.TransicionEntrada{AEstado: operclient.APROBADO}); err == nil {
		t.Fatal("aprobar sin fecha/hora debió fallar")
	}

	fecha := time.Date(2026, 11, 20, 15, 30, 0, 0, time.UTC)
	out, err := api.CompletarFechaHora(ctxJefe, v.Id, operclient.FechaHoraEntrada{FechaHoraServicio: fecha})
	if err != nil {
		t.Fatalf("CompletarFechaHora: %v", err)
	}
	if out.Id != v.Id || out.Estado != operclient.SOLICITADO || out.FechaHoraServicio == nil || !out.FechaHoraServicio.Equal(fecha) {
		t.Errorf("viaje: got id %s estado %q fecha %v, want %s SOLICITADO %v", out.Id, out.Estado, out.FechaHoraServicio, v.Id, fecha)
	}
	if _, err := api.TransicionarViaje(ctxJefe, v.Id, operclient.TransicionEntrada{AEstado: operclient.APROBADO}); err != nil {
		t.Fatalf("aprobar con fecha/hora: %v", err)
	}

	monitorista := &auth.Claims{Subject: "mon-1", Rol: "monitorista", Audiencia: auth.AudienciaInterno}
	if _, err := api.CompletarFechaHora(auth.ContextWithClaims(ctx, monitorista), v.Id, operclient.FechaHoraEntrada{FechaHoraServicio: fecha}); platformerrors.AsCategory(err).HTTPStatus() != 403 {
		t.Errorf("monitorista: err %v, quiere 403", err)
	}

	admin, cerrar := dbtest.AdminConn(t, ctx)
	defer cerrar()
	dbtest.Exec(t, ctx, admin, "UPDATE viajes.viaje SET estado='CERRADO' WHERE id='"+v.Id.String()+"'")
	_, err = api.CompletarFechaHora(ctxJefe, v.Id, operclient.FechaHoraEntrada{FechaHoraServicio: fecha})
	if got := platformerrors.CodigoDe(err); got != "viaje_en_estado_terminal" {
		t.Errorf("terminal: codigo %q (err %v), want viaje_en_estado_terminal", got, err)
	}
	mismoStatus(t, m, err, http.MethodPatch, "/api/v1/viajes/"+v.Id.String()+"/fecha-hora", `{"fecha_hora_servicio":"2026-11-20T15:30:00Z"}`, 409)
}

// TestConsultasListarIncidencias: el histórico trae la resuelta y la abierta, en
// orden de apertura, igual por fachada y HTTP; el Cliente recibe 404 (DEC-132).
func TestConsultasListarIncidencias(t *testing.T) {
	ctx := context.Background()
	m := moduloCompleto(t, ctx)
	api := m.Consultas()
	ctxSup := auth.ContextWithClaims(ctx, claimsSupervisor)

	id := viajeAprobado(t, ctx, api)
	a, err := api.AbrirIncidencia(ctxSup, id, operclient.IncidenciaEntrada{TipoInterno: "falla_mecanica"})
	if err != nil {
		t.Fatalf("abrir A: %v", err)
	}
	if _, err := api.ResolverIncidencia(ctxSup, id, a.Id, operclient.ResolucionIncidenciaEntrada{Resolucion: "reparada"}); err != nil {
		t.Fatalf("resolver A: %v", err)
	}
	b, err := api.AbrirIncidencia(ctxSup, id, operclient.IncidenciaEntrada{TipoInterno: "bloqueo_carretero"})
	if err != nil {
		t.Fatalf("abrir B: %v", err)
	}

	pag, err := api.ListarIncidencias(auth.ContextWithClaims(ctx, claimsJefe), id, operclient.ListarIncidenciasParams{})
	if err != nil {
		t.Fatalf("ListarIncidencias: %v", err)
	}
	if len(pag.Items) != 2 || pag.CursorSiguiente != nil {
		t.Fatalf("página: got %d incidencias cursor %v, want 2 sin cursor", len(pag.Items), pag.CursorSiguiente)
	}
	r, ab := pag.Items[0], pag.Items[1]
	if r.Id != a.Id || r.TipoInterno != "falla_mecanica" || r.Estado != operclient.IncidenciaEstadoResuelta || r.ResueltaEn == nil || r.ViajeId != id {
		t.Errorf("resuelta: got %+v", r)
	}
	if ab.Id != b.Id || ab.TipoInterno != "bloqueo_carretero" || ab.Estado != operclient.IncidenciaEstadoEnAtencion || ab.ResueltaEn != nil {
		t.Errorf("abierta: got %+v", ab)
	}

	mux := http.NewServeMux()
	m.Routes(mux)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/viajes/"+id.String()+"/incidencias", nil)
	req.Header.Set("Authorization", "Bearer x")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"estado":"resuelta"`) || !strings.Contains(rec.Body.String(), `"estado":"en_atencion"`) {
		t.Errorf("HTTP: status %d cuerpo %s; quiere 200 con resuelta y en_atencion", rec.Code, rec.Body)
	}

	cliente := &auth.Claims{Subject: "cli-1", TenantID: uuid.NewString(), Rol: "cliente", Audiencia: auth.AudienciaCliente}
	if _, err := api.ListarIncidencias(auth.ContextWithClaims(ctx, cliente), id, operclient.ListarIncidenciasParams{}); platformerrors.AsCategory(err) != platformerrors.CategoryNoEncontrado {
		t.Errorf("cliente: err %v, quiere no_encontrado", err)
	}
}

// TestConsultasFicha: la ficha nace vacía; el monitorista la guarda por partes
// (contenedor vacío descartado, gps_ids deduplicados, propiedad ausente intacta)
// y por HTTP borra el tracto con `"tracto": null`. El jefe_operativo recibe 403
// igual por fachada y HTTP, un campo desconocido 400 y el Cliente 404 al leer.
func TestConsultasFicha(t *testing.T) {
	ctx := context.Background()
	m := moduloCompleto(t, ctx)
	api := m.Consultas()
	ctxMon := auth.ContextWithClaims(ctx, claimsMonitorista)
	id := viajeAprobado(t, ctx, api)

	f, err := api.ObtenerFicha(auth.ContextWithClaims(ctx, claimsJefe), id)
	if err != nil {
		t.Fatalf("ObtenerFicha: %v", err)
	}
	if f.ViajeId != id || f.Tracto != nil || len(*f.Contenedores) != 0 || len(*f.GpsIds) != 0 {
		t.Fatalf("ficha vacía: got %+v", f)
	}

	g1 := uuid.MustParse("00000000-0000-0000-0000-0000000000d1")
	g2 := uuid.MustParse("00000000-0000-0000-0000-0000000000d2")
	s := func(v string) *string { return &v }
	f, err = api.GuardarFicha(ctxMon, id, operclient.FichaTecnicaEntrada{
		Tracto:       &operclient.Tracto{Placas: s("ABC-123"), Economico: s("E-7"), Marca: s(" ")},
		Contenedores: &[]operclient.Contenedor{{Sello: s("S1")}, {}},
		GpsIds:       &[]operclient.Id{g1, g1, g2},
	})
	if err != nil {
		t.Fatalf("GuardarFicha: %v", err)
	}
	if f.Tracto == nil || *f.Tracto.Placas != "ABC-123" || *f.Tracto.Economico != "E-7" || f.Tracto.Marca != nil {
		t.Errorf("tracto: got %+v", f.Tracto)
	}
	if c := *f.Contenedores; len(c) != 1 || *c[0].Sello != "S1" || c[0].Placas != nil {
		t.Errorf("contenedores: got %+v", c)
	}
	if g := *f.GpsIds; len(g) != 2 || g[0] != g1 || g[1] != g2 {
		t.Errorf("gps_ids: got %v, want [%s %s]", g, g1, g2)
	}

	// Solo gps_ids: [] borra la lista y no toca tracto ni contenedores.
	f, err = api.GuardarFicha(ctxMon, id, operclient.FichaTecnicaEntrada{GpsIds: &[]operclient.Id{}})
	if err != nil {
		t.Fatalf("GuardarFicha parcial: %v", err)
	}
	if len(*f.GpsIds) != 0 || f.Tracto == nil || len(*f.Contenedores) != 1 {
		t.Errorf("parcial: got tracto %+v contenedores %d gps %d", f.Tracto, len(*f.Contenedores), len(*f.GpsIds))
	}

	mux := http.NewServeMux()
	m.Routes(mux)
	patch := func(token, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPatch, "/api/v1/viajes/"+id.String()+"/ficha", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}
	if rec := patch("monitorista", `{"tracto": null}`); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"tracto":null`) ||
		!strings.Contains(rec.Body.String(), `"sello":"S1"`) {
		t.Errorf("HTTP tracto null: status %d cuerpo %s; quiere 200, tracto null y el contenedor intacto", rec.Code, rec.Body)
	}
	if rec := patch("monitorista", `{"tracto":{"vin":"X"}}`); rec.Code != 400 {
		t.Errorf("HTTP campo desconocido: status %d, quiere 400 (body %s)", rec.Code, rec.Body)
	}

	_, err = api.GuardarFicha(auth.ContextWithClaims(ctx, claimsJefe), id, operclient.FichaTecnicaEntrada{})
	mismoStatus(t, m, err, http.MethodPatch, "/api/v1/viajes/"+id.String()+"/ficha", `{}`, 403)

	cliente := &auth.Claims{Subject: "cli-1", TenantID: uuid.NewString(), Rol: "cliente", Audiencia: auth.AudienciaCliente}
	if _, err := api.ObtenerFicha(auth.ContextWithClaims(ctx, cliente), id); platformerrors.AsCategory(err) != platformerrors.CategoryNoEncontrado {
		t.Errorf("cliente: err %v, quiere no_encontrado", err)
	}
	if _, err := api.GuardarFicha(ctxMon, uuid.New(), operclient.FichaTecnicaEntrada{GpsIds: &[]operclient.Id{}}); platformerrors.AsCategory(err) != platformerrors.CategoryNoEncontrado {
		t.Errorf("viaje inexistente: err %v, quiere no_encontrado", err)
	}
}

// TestConsultasAislamientoCliente verifica que un cliente solo lista y lee sus
// propios viajes (tenant == cliente_id, DEC-112): el viaje de otro cliente no
// aparece en la lista y su detalle responde 404 (DEC-132). Un interno ve todos.
func TestConsultasAislamientoCliente(t *testing.T) {
	ctx := context.Background()
	m := moduloConViajes(t, ctx)
	api := m.Consultas()

	ctxDe := func(tenant string) context.Context {
		return auth.ContextWithClaims(ctx, &auth.Claims{Subject: "cli-" + tenant, Rol: "cliente", Audiencia: auth.AudienciaCliente, TenantID: tenant})
	}
	fecha := time.Now().Add(24 * time.Hour).UTC()
	crear := func(c context.Context) operclient.Id {
		v, err := api.CrearViaje(c, operclient.CrearViajeEntrada{
			Origen: "Origen", Destinos: []operclient.Destino{{Nombre: "Destino"}}, FechaHoraServicio: &fecha,
		})
		if err != nil {
			t.Fatalf("crear viaje: %v", err)
		}
		return v.Id
	}
	ctxA, ctxB := ctxDe(uuid.NewString()), ctxDe(uuid.NewString())
	viajeA, viajeB := crear(ctxA), crear(ctxB)

	ids := func(c context.Context) []operclient.Id {
		pag, err := api.ListarViajes(c, operclient.ListarViajesParams{})
		if err != nil {
			t.Fatalf("listar viajes: %v", err)
		}
		out := []operclient.Id{}
		for _, v := range pag.Items {
			out = append(out, v.Id)
		}
		return out
	}
	if got := ids(ctxA); len(got) != 1 || got[0] != viajeA {
		t.Errorf("cliente A lista %v, quiero solo [%s]", got, viajeA)
	}
	if got := ids(ctxDe("")); len(got) != 0 {
		t.Errorf("cliente sin tenant lista %v, quiero ninguno", got)
	}
	if got := ids(auth.ContextWithClaims(ctx, claimsJefe)); len(got) != 2 {
		t.Errorf("interno lista %d viajes, quiero 2", len(got))
	}

	if _, err := api.ObtenerViaje(ctxA, viajeA); err != nil {
		t.Errorf("cliente A lee su viaje: %v", err)
	}
	_, err := api.ObtenerViaje(ctxA, viajeB)
	if got := platformerrors.AsCategory(err); got != platformerrors.CategoryNoEncontrado {
		t.Errorf("cliente A lee el viaje de B: categoría %v (err %v), quiero NoEncontrado", got, err)
	}
}
