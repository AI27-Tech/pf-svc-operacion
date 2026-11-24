package operacion_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/AI27-Tech/pf-svc-operacion"
	"github.com/google/uuid"
)

// TestTablaRolPorAccionDeSolicitudes (E10-T9): cada celda rol × acción, escrita a
// mano. Una celda sin facultad responde 403 y nunca toca la solicitud; una con
// facultad responde cualquier cosa MENOS 401 o 403 (el resultado depende del
// estado y del alcance, que prueban las pruebas del módulo).
func TestTablaRolPorAccionDeSolicitudes(t *testing.T) {
	ctx := context.Background()
	m := moduloCompletoCon(t, ctx, operacion.Deps{Verificador: autenticadorRoles{tenant: uuid.NewString()}, Atomos: catalogo})
	mux := http.NewServeMux()
	m.Routes(mux)
	id := sembrarPagada(t, ctx).String()
	const (
		cot   = `{"monto":"100","moneda":"MXN","fecha_hora_servicio":"2099-01-01T10:00:00Z"}`
		repro = `{"fecha_hora_servicio":"2099-01-01T10:00:00Z","motivo":"m"}`
	)
	acciones := []struct {
		nombre, metodo, ruta, cuerpo string
		permitidos                   []string
	}{
		{"crear", "POST", "/api/v1/solicitudes", `{"origen":"A","destinos":[{"nombre":"B"}]}`, []string{"cliente"}},
		{"listar", "GET", "/api/v1/solicitudes", ``, []string{"cliente", "cotizacion", "jefe_operativo", "administrador"}},
		{"obtener", "GET", "/api/v1/solicitudes/" + id, ``, []string{"cliente", "cotizacion", "jefe_operativo", "administrador"}},
		{"histórico", "GET", "/api/v1/solicitudes/" + id + "/historico", ``, []string{"cotizacion", "jefe_operativo", "administrador"}},
		{"cotizar y recotizar", "PUT", "/api/v1/solicitudes/" + id + "/cotizacion", cot, []string{"cotizacion", "administrador"}},
		{"reprogramar", "PUT", "/api/v1/solicitudes/" + id + "/fecha-hora", repro, []string{"cotizacion", "administrador"}},
		{"confirmar pago", "PUT", "/api/v1/solicitudes/" + id + "/pago", `{"fecha_pago":"2099-01-01"}`, []string{"cotizacion", "administrador"}},
		{"revertir pago", "POST", "/api/v1/solicitudes/" + id + "/reversa-pago", `{"motivo":"m"}`, []string{"administrador"}},
		{"aprobar", "POST", "/api/v1/solicitudes/" + id + "/aprobacion", ``, []string{"jefe_operativo", "administrador"}},
		{"rechazar", "POST", "/api/v1/solicitudes/" + id + "/rechazo", `{"motivo":"m"}`, []string{"jefe_operativo", "administrador"}},
		{"rechazar cotización", "POST", "/api/v1/solicitudes/" + id + "/rechazo-cotizacion", ``, []string{"cliente"}},
		{"cancelar", "POST", "/api/v1/solicitudes/" + id + "/cancelacion", ``, []string{"cliente"}},
	}
	roles := []string{"cliente", "cotizacion", "jefe_operativo", "administrador", "monitorista", "supervisor", "coordinador", "ejecutivo", "reaccion"}
	for _, ac := range acciones {
		permitido := map[string]bool{}
		for _, r := range ac.permitidos {
			permitido[r] = true
		}
		for _, rol := range roles {
			rec := llamar(t, mux, ac.metodo, ac.ruta, rol, ac.cuerpo)
			if permitido[rol] && (rec.Code == http.StatusForbidden || rec.Code == http.StatusUnauthorized) {
				t.Errorf("%s como %s: debía tener la facultad, obtuvo %d (%s)", ac.nombre, rol, rec.Code, rec.Body)
			}
			if !permitido[rol] && rec.Code != http.StatusForbidden {
				t.Errorf("%s como %s: esperaba 403, obtuvo %d", ac.nombre, rol, rec.Code)
			}
		}
		if rec := llamar(t, mux, ac.metodo, ac.ruta, "", ac.cuerpo); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s sin token: esperaba 401, obtuvo %d", ac.nombre, rec.Code)
		}
	}
}

// TestLecturaDeSolicitudesHTTP: el contenido por ruta con los alcances del módulo.
func TestLecturaDeSolicitudesHTTP(t *testing.T) {
	ctx := context.Background()
	m := moduloCompletoCon(t, ctx, operacion.Deps{Verificador: autenticadorRoles{tenant: uuid.NewString()}, Atomos: catalogo})
	mux := http.NewServeMux()
	m.Routes(mux)
	id := sembrarPagada(t, ctx)

	if rec := llamar(t, mux, "GET", "/api/v1/solicitudes/"+id.String(), "jefe_operativo", ""); rec.Code != http.StatusOK {
		t.Errorf("el jefe lee una PAGADO: %d", rec.Code)
	}
	if rec := llamar(t, mux, "GET", "/api/v1/solicitudes/"+id.String(), "cliente-otro", ""); rec.Code != http.StatusForbidden && rec.Code != http.StatusNotFound {
		t.Errorf("otra organización: %d", rec.Code)
	}
	if rec := llamar(t, mux, "GET", "/api/v1/solicitudes/"+id.String(), "cliente", ""); rec.Code != http.StatusNotFound {
		t.Errorf("la solicitud es de otra organización (el tenant del token no es el suyo): esperaba 404, obtuve %d", rec.Code)
	}
	if rec := llamar(t, mux, "GET", "/api/v1/solicitudes?estado=INVENTADO", "cotizacion", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("estado inválido: %d", rec.Code)
	}
	if rec := llamar(t, mux, "GET", "/api/v1/solicitudes?cursor=zzz", "cotizacion", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("cursor inválido: %d", rec.Code)
	}
	if rec := llamar(t, mux, "GET", "/api/v1/solicitudes/no-es-uuid", "cotizacion", ""); rec.Code != http.StatusBadRequest {
		t.Errorf("id inválido: %d", rec.Code)
	}
}
