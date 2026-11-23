package operacion_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	operclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/operacion"
	"github.com/AI27-Tech/pf-pkg-platform-go/auth"
	"github.com/AI27-Tech/pf-svc-operacion"
	"github.com/AI27-Tech/pf-svc-operacion/internal/dbtest"
	"github.com/google/uuid"
)

// autenticadorRoles devuelve claims según el token: el rol (con audiencia Cliente
// y el tenant fijo para "cliente"). Otro token no autentica.
type autenticadorRoles struct{ tenant string }

func (a autenticadorRoles) VerifyToken(_ context.Context, token string) (*auth.Claims, error) {
	if token == "cliente" {
		return &auth.Claims{Subject: "cli-1", Rol: "cliente", Audiencia: auth.AudienciaCliente, TenantID: a.tenant}, nil
	}
	if token == "cliente-otro" {
		return &auth.Claims{Subject: "cli-2", Rol: "cliente", Audiencia: auth.AudienciaCliente, TenantID: uuid.NewString()}, nil
	}
	return &auth.Claims{Subject: token + "-1", Rol: token, Audiencia: auth.AudienciaInterno}, nil
}

func llamar(t *testing.T, mux *http.ServeMux, metodo, ruta, token, cuerpo string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(metodo, ruta, strings.NewReader(cuerpo))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// TestCrearSolicitudHTTP: E10-T3 por la ruta del módulo: solo el cliente crea, la
// organización sale del token y el evento queda en el outbox de solicitudes.
func TestCrearSolicitudHTTP(t *testing.T) {
	ctx := context.Background()
	tenant := uuid.New()
	m := moduloCompletoCon(t, ctx, operacion.Deps{Verificador: autenticadorRoles{tenant: tenant.String()}, Atomos: catalogo})
	mux := http.NewServeMux()
	m.Routes(mux)
	const valido = `{"origen":"CDMX","destinos":[{"nombre":"Guadalajara"}]}`

	rec := llamar(t, mux, http.MethodPost, "/api/v1/solicitudes", "cliente", valido)
	if rec.Code != http.StatusCreated {
		t.Fatalf("crear: status %d, cuerpo %s", rec.Code, rec.Body)
	}
	var s operclient.Solicitud
	if err := json.Unmarshal(rec.Body.Bytes(), &s); err != nil {
		t.Fatal(err)
	}
	if s.Estado != operclient.EstadoSolicitud("SOLICITADO") || s.ClienteId == nil || s.ClienteId.String() != tenant.String() {
		t.Errorf("solicitud = %+v", s)
	}
	admin, cerrar := dbtest.AdminConn(t, ctx)
	defer cerrar()
	var n int
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM solicitudes.outbox WHERE tipo='solicitud.creada' AND agregado_id=$1`, s.Id.String()).Scan(&n); err != nil || n != 1 {
		t.Errorf("solicitud.creada en el outbox: %d (err %v)", n, err)
	}

	casos := []struct {
		nombre, token, cuerpo string
		want                  int
	}{
		{"sin token", "", valido, http.StatusUnauthorized},
		{"cuerpo con cliente_id (additionalProperties false)", "cliente", `{"origen":"A","destinos":[{"nombre":"B"}],"cliente_id":"` + uuid.NewString() + `"}`, http.StatusBadRequest},
		{"sin origen", "cliente", `{"origen":"","destinos":[{"nombre":"B"}]}`, http.StatusBadRequest},
		{"sin destinos", "cliente", `{"origen":"A","destinos":[]}`, http.StatusBadRequest},
	}
	for _, rol := range []string{"jefe_operativo", "administrador", "cotizacion", "monitorista", "supervisor", "coordinador", "ejecutivo", "reaccion"} {
		casos = append(casos, struct {
			nombre, token, cuerpo string
			want                  int
		}{"rol " + rol, rol, valido, http.StatusForbidden})
	}
	var antes int
	_ = admin.QueryRow(ctx, `SELECT count(*) FROM solicitudes.solicitud`).Scan(&antes)
	for _, c := range casos {
		if got := llamar(t, mux, http.MethodPost, "/api/v1/solicitudes", c.token, c.cuerpo).Code; got != c.want {
			t.Errorf("%s: status %d, quería %d", c.nombre, got, c.want)
		}
	}
	var despues int
	_ = admin.QueryRow(ctx, `SELECT count(*) FROM solicitudes.solicitud`).Scan(&despues)
	if despues != antes {
		t.Errorf("los rechazos crearon %d solicitudes", despues-antes)
	}
}

// TestSolicitudesSinPoolNoMontaRutas: sin el pool de op_solicitudes la ruta no existe.
func TestSolicitudesSinPoolNoMontaRutas(t *testing.T) {
	m, err := operacion.New(operacion.Pools{}, operacion.Deps{Verificador: autenticadorRoles{tenant: uuid.NewString()}})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	m.Routes(mux)
	if got := llamar(t, mux, http.MethodPost, "/api/v1/solicitudes", "cliente", `{}`).Code; got != http.StatusNotFound && got != http.StatusMethodNotAllowed {
		t.Errorf("status %d, esperaba 404", got)
	}
}

// TestCotizacionYRespuestaHTTP recorre E10-T4 y E10-T5 por las rutas del módulo:
// la facultad por rol, el 404 entre organizaciones y el flujo feliz.
func TestCotizacionYRespuestaHTTP(t *testing.T) {
	ctx := context.Background()
	tenant := uuid.New()
	m := moduloCompletoCon(t, ctx, operacion.Deps{Verificador: autenticadorRoles{tenant: tenant.String()}, Atomos: catalogo})
	mux := http.NewServeMux()
	m.Routes(mux)
	// Fecha futura relativa: la regla de DEC-148 #66 usa el reloj real aquí y una fecha fija caducaría.
	fut := time.Now().AddDate(1, 0, 0).UTC().Format(time.RFC3339)
	cot := `{"monto":"2500.00","moneda":"MXN","fecha_hora_servicio":"` + fut + `"}`

	crear := func() string {
		rec := llamar(t, mux, http.MethodPost, "/api/v1/solicitudes", "cliente", `{"origen":"CDMX","destinos":[{"nombre":"GDL"}]}`)
		var s operclient.Solicitud
		if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &s) != nil {
			t.Fatalf("crear: %d %s", rec.Code, rec.Body)
		}
		return s.Id.String()
	}
	estado := func(rec *httptest.ResponseRecorder) string {
		var s operclient.Solicitud
		if err := json.Unmarshal(rec.Body.Bytes(), &s); err != nil {
			t.Fatalf("respuesta: %v (%s)", err, rec.Body)
		}
		return string(s.Estado)
	}

	id := crear()
	ruta := "/api/v1/solicitudes/" + id
	// Facultad por rol: solo cotizacion y administrador cotizan y reprograman.
	for _, rol := range []string{"cliente", "jefe_operativo", "monitorista", "supervisor", "coordinador", "ejecutivo", "reaccion"} {
		if got := llamar(t, mux, http.MethodPut, ruta+"/cotizacion", rol, cot).Code; got != http.StatusForbidden {
			t.Errorf("cotizar como %s: %d, quería 403", rol, got)
		}
		if got := llamar(t, mux, http.MethodPut, ruta+"/fecha-hora", rol, `{"fecha_hora_servicio":"`+fut+`","motivo":"x"}`).Code; got != http.StatusForbidden {
			t.Errorf("reprogramar como %s: %d, quería 403", rol, got)
		}
	}
	// Solo el cliente rechaza y cancela.
	for _, rol := range []string{"cotizacion", "administrador", "jefe_operativo", "monitorista", "supervisor", "coordinador", "ejecutivo", "reaccion"} {
		for _, accion := range []string{"/rechazo-cotizacion", "/cancelacion"} {
			if got := llamar(t, mux, http.MethodPost, ruta+accion, rol, "").Code; got != http.StatusForbidden {
				t.Errorf("%s como %s: %d, quería 403", accion, rol, got)
			}
		}
	}
	if got := llamar(t, mux, http.MethodPut, ruta+"/cotizacion", "", cot).Code; got != http.StatusUnauthorized {
		t.Errorf("sin token: %d", got)
	}
	if got := llamar(t, mux, http.MethodPut, "/api/v1/solicitudes/no-es-uuid/cotizacion", "cotizacion", cot).Code; got != http.StatusBadRequest {
		t.Errorf("id inválido: %d", got)
	}
	if got := llamar(t, mux, http.MethodPut, ruta+"/cotizacion", "cotizacion", `{"monto":"1","moneda":"MXN"}`).Code; got != http.StatusBadRequest {
		t.Errorf("cotizar sin fecha: %d, quería 400", got)
	}

	if rec := llamar(t, mux, http.MethodPut, ruta+"/cotizacion", "cotizacion", cot); rec.Code != http.StatusOK || estado(rec) != "COTIZADO" {
		t.Fatalf("cotizar: %d %s", rec.Code, rec.Body)
	}
	// El administrador hereda la facultad: recotiza.
	if rec := llamar(t, mux, http.MethodPut, ruta+"/cotizacion", "administrador", cot); rec.Code != http.StatusOK || estado(rec) != "COTIZADO" {
		t.Fatalf("recotizar como administrador: %d %s", rec.Code, rec.Body)
	}
	// Reprogramar en COTIZADO: 409.
	if got := llamar(t, mux, http.MethodPut, ruta+"/fecha-hora", "cotizacion", `{"fecha_hora_servicio":"`+fut+`","motivo":"x"}`).Code; got != http.StatusConflict {
		t.Errorf("reprogramar en COTIZADO: %d, quería 409", got)
	}
	// Otra organización: 404, no 403.
	if got := llamar(t, mux, http.MethodPost, ruta+"/cancelacion", "cliente-otro", "").Code; got != http.StatusNotFound {
		t.Errorf("cancelar una solicitud ajena: %d, quería 404", got)
	}
	// El cuerpo opcional puede venir vacío o con motivo; un campo ajeno es 400.
	if got := llamar(t, mux, http.MethodPost, ruta+"/rechazo-cotizacion", "cliente", `{"otro":1}`).Code; got != http.StatusBadRequest {
		t.Errorf("cuerpo con campo ajeno: %d, quería 400", got)
	}
	if rec := llamar(t, mux, http.MethodPost, ruta+"/rechazo-cotizacion", "cliente", ""); rec.Code != http.StatusOK || estado(rec) != "RECHAZADO_POR_CLIENTE" {
		t.Fatalf("rechazar la cotización: %d %s", rec.Code, rec.Body)
	}
	if got := llamar(t, mux, http.MethodPost, ruta+"/cancelacion", "cliente", "").Code; got != http.StatusConflict {
		t.Errorf("cancelar tras rechazar: %d, quería 409", got)
	}

	id2 := crear()
	if rec := llamar(t, mux, http.MethodPost, "/api/v1/solicitudes/"+id2+"/cancelacion", "cliente", `{"motivo":"ya no"}`); rec.Code != http.StatusOK || estado(rec) != "CANCELADO_POR_CLIENTE" {
		t.Fatalf("cancelar: %d %s", rec.Code, rec.Body)
	}
}

// TestPagoHTTP recorre E10-T6 por las rutas del módulo: solo cotizacion y
// administrador confirman, solo el administrador revierte, y la frontera de vigencia.
func TestPagoHTTP(t *testing.T) {
	ctx := context.Background()
	tenant := uuid.New()
	m := moduloCompletoCon(t, ctx, operacion.Deps{Verificador: autenticadorRoles{tenant: tenant.String()}, Atomos: catalogo})
	mux := http.NewServeMux()
	m.Routes(mux)

	rec := llamar(t, mux, http.MethodPost, "/api/v1/solicitudes", "cliente", `{"origen":"CDMX","destinos":[{"nombre":"GDL"}]}`)
	var s operclient.Solicitud
	if rec.Code != http.StatusCreated || json.Unmarshal(rec.Body.Bytes(), &s) != nil {
		t.Fatalf("crear: %d %s", rec.Code, rec.Body)
	}
	ruta := "/api/v1/solicitudes/" + s.Id.String()
	// El reloj real fija la entrega: la vigencia es de hoy a hoy+15 en CDMX.
	hoy := time.Now().In(time.FixedZone("mx", -6*3600)).Format(time.DateOnly)
	if rec := llamar(t, mux, http.MethodPut, ruta+"/cotizacion", "cotizacion", `{"monto":"900","moneda":"MXN","fecha_hora_servicio":"2027-01-10T16:00:00Z"}`); rec.Code != http.StatusOK {
		t.Fatalf("cotizar: %d %s", rec.Code, rec.Body)
	}
	pago := `{"fecha_pago":"` + hoy + `","referencia":"folio 1"}`

	for _, rol := range []string{"cliente", "jefe_operativo", "monitorista", "supervisor", "coordinador", "ejecutivo", "reaccion"} {
		if got := llamar(t, mux, http.MethodPut, ruta+"/pago", rol, pago).Code; got != http.StatusForbidden {
			t.Errorf("confirmar pago como %s: %d, quería 403", rol, got)
		}
	}
	if got := llamar(t, mux, http.MethodPut, ruta+"/pago", "cotizacion", `{"fecha_pago":"2000-01-01"}`).Code; got != http.StatusConflict {
		t.Errorf("fecha antes de la entrega: %d, quería 409", got)
	}
	if got := llamar(t, mux, http.MethodPut, ruta+"/pago", "cotizacion", `{}`).Code; got != http.StatusBadRequest {
		t.Errorf("sin fecha_pago: %d, quería 400", got)
	}
	rec = llamar(t, mux, http.MethodPut, ruta+"/pago", "cotizacion", pago)
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &s) != nil || string(s.Estado) != "PAGADO" || s.Pago == nil {
		t.Fatalf("confirmar pago: %d %s", rec.Code, rec.Body)
	}
	if got := llamar(t, mux, http.MethodPut, ruta+"/pago", "cotizacion", pago).Code; got != http.StatusConflict {
		t.Errorf("confirmar dos veces: %d, quería 409", got)
	}

	// La reversa es solo del administrador y exige motivo.
	for _, rol := range []string{"cotizacion", "cliente", "jefe_operativo", "monitorista", "supervisor", "coordinador", "ejecutivo", "reaccion"} {
		if got := llamar(t, mux, http.MethodPost, ruta+"/reversa-pago", rol, `{"motivo":"error"}`).Code; got != http.StatusForbidden {
			t.Errorf("reversa como %s: %d, quería 403", rol, got)
		}
	}
	if got := llamar(t, mux, http.MethodPost, ruta+"/reversa-pago", "administrador", `{"motivo":""}`).Code; got != http.StatusBadRequest {
		t.Errorf("reversa sin motivo: %d, quería 400", got)
	}
	rec = llamar(t, mux, http.MethodPost, ruta+"/reversa-pago", "administrador", `{"motivo":"error de captura"}`)
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &s) != nil || string(s.Estado) != "COTIZADO" || s.Pago != nil {
		t.Fatalf("reversa: %d %s", rec.Code, rec.Body)
	}
	if got := llamar(t, mux, http.MethodPost, ruta+"/reversa-pago", "administrador", `{"motivo":"otra vez"}`).Code; got != http.StatusConflict {
		t.Errorf("reversa fuera de PAGADO: %d, quería 409", got)
	}
}
