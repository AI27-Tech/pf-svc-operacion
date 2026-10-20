package operacion

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/AI27-Tech/pf-pkg-platform-go/auth"
)

// autenticadorSiempreOK es un Authenticator de prueba que acepta cualquier token
// y devuelve claims fijos. Permite montar las rutas protegidas sin un verificador
// real; aquí solo interesa la SUPERFICIE de rutas, no la autenticación.
type autenticadorSiempreOK struct{}

func (autenticadorSiempreOK) VerifyToken(context.Context, string) (*auth.Claims, error) {
	return &auth.Claims{Subject: "tester"}, nil
}

// TestBitacoraSoloExponePostYGet fija la superficie append-only de la bitácora al
// nivel de las rutas: /api/v1/viajes/{id}/bitacora acepta POST (registrar) y GET
// (listar), y NO expone PUT, PATCH ni DELETE —una corrección es una entrada
// nueva, no una edición (E1-T5)—. No toca la base: comprueba el ruteo, que el
// ServeMux resuelve antes de llegar al handler. Un método no registrado sobre una
// ruta que sí existe devuelve 405 con un header Allow que enumera los permitidos.
func TestBitacoraSoloExponePostYGet(t *testing.T) {
	m := &Module{deps: Deps{Verificador: autenticadorSiempreOK{}}}
	mux := http.NewServeMux()
	m.Routes(mux)

	const ruta = "/api/v1/viajes/11111111-1111-1111-1111-111111111111/bitacora"

	// Un método prohibido devuelve 405 y el header Allow lista los permitidos.
	// Se usa DELETE como sonda: si existiera una ruta de borrado, no daría 405.
	req := httptest.NewRequest(http.MethodDelete, ruta, nil)
	req.Header.Set("Authorization", "Bearer x")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE sobre la bitácora: got status %d, want 405 (no debe existir ruta de borrado)", rec.Code)
	}
	allow := rec.Header().Get("Allow")
	if !strings.Contains(allow, http.MethodPost) || !strings.Contains(allow, http.MethodGet) {
		t.Errorf("Allow debe incluir GET y POST: got %q", allow)
	}
	for _, metodo := range []string{http.MethodPut, http.MethodPatch, http.MethodDelete} {
		if strings.Contains(allow, metodo) {
			t.Errorf("Allow no debe incluir %s (la bitácora es append-only): got %q", metodo, allow)
		}
	}
}
