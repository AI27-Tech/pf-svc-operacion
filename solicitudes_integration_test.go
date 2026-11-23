package operacion_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
