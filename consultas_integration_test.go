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
// ruta HTTP con el MISMO actor que la fachada y comparar ambos caminos.
type autenticadorJefe struct{}

func (autenticadorJefe) VerifyToken(context.Context, string) (*auth.Claims, error) {
	return claimsJefe, nil
}

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
			"asignacion.asignacion_viaje, asignacion.reserva_recurso, asignacion.historia_asignacion, auditoria.registro_auditoria, bitacora.entrada_bitacora")
	pool := func(rol, searchPath string) *platformpgx.Pool {
		p, cerrarPool := dbtest.PoolAs(t, ctx, rol, searchPath)
		t.Cleanup(cerrarPool)
		return &platformpgx.Pool{Pool: p, Role: rol}
	}
	m, err := operacion.New(operacion.Pools{
		Viajes:     pool("op_viajes", "viajes"),
		Asignacion: pool("op_asignacion", "viajes"),
		Bitacora:   pool("op_bitacora", ""),
	}, operacion.Deps{Verificador: autenticadorJefe{}})
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
	a, err := api.AsignarRecursos(ctxJefe, id, operclient.AsignacionEntrada{UnidadesIds: []operclient.Id{uuid.New()}})
	if err != nil {
		t.Fatalf("AsignarRecursos: %v", err)
	}
	if a.Estado != operclient.ASIGNADO || a.ViajeId != id || len(a.Unidades) != 1 {
		t.Errorf("asignación: got estado %q viaje %s unidades %d, want ASIGNADO %s 1", a.Estado, a.ViajeId, len(a.Unidades), id)
	}

	// Ya ASIGNADO: no se vuelve a asignar (solo APROBADO).
	u := uuid.New()
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
// puede abrir, igual por fachada y HTTP.
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
	mismoStatus(t, m, err, http.MethodPost, "/api/v1/viajes/"+id.String()+"/incidencias", `{"tipo_interno":"falla_mecanica"}`, 401)
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
