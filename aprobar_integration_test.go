package operacion_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	operclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/operacion"
	"github.com/AI27-Tech/pf-svc-operacion"
	"github.com/AI27-Tech/pf-svc-operacion/internal/dbtest"
	"github.com/google/uuid"
)

// sembrarPagada inserta una solicitud PAGADO con fecha de servicio, como admin.
func sembrarPagada(t *testing.T, ctx context.Context) uuid.UUID {
	t.Helper()
	id := uuid.New()
	admin, cerrar := dbtest.AdminConn(t, ctx)
	defer cerrar()
	dbtest.Exec(t, ctx, admin, `INSERT INTO solicitudes.solicitud (id, cliente_id, estado, marca_penalizacion, origen, destinos, fecha_hora_servicio, creado_en)
	  VALUES ('`+id.String()+`', gen_random_uuid(), 'PAGADO', false, 'CDMX', '[{"nombre":"Guadalajara"}]', '2030-01-10T15:00:00Z', '2030-01-01T00:00:00Z')`)
	return id
}

func viajesDe(t *testing.T, ctx context.Context, id uuid.UUID) int {
	t.Helper()
	admin, cerrar := dbtest.AdminConn(t, ctx)
	defer cerrar()
	var n int
	if err := admin.QueryRow(ctx, `SELECT count(*) FROM viajes.viaje WHERE id=$1`, id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestAprobarYRechazarHTTP: facultad por rol y códigos de la aprobación y el
// rechazo por las rutas del módulo (E10-T8).
func TestAprobarYRechazarHTTP(t *testing.T) {
	ctx := context.Background()
	m := moduloCompletoCon(t, ctx, operacion.Deps{Verificador: autenticadorRoles{tenant: uuid.NewString()}, Atomos: catalogo})
	mux := http.NewServeMux()
	m.Routes(mux)
	id := sembrarPagada(t, ctx)
	ruta := func(sufijo string) string { return "/api/v1/solicitudes/" + id.String() + sufijo }

	// Sin la facultad: 403 en ambas rutas y la solicitud no cambia.
	for _, rol := range []string{"cotizacion", "monitorista", "supervisor", "coordinador", "ejecutivo", "reaccion", "cliente"} {
		if rec := llamar(t, mux, http.MethodPost, ruta("/aprobacion"), rol, ``); rec.Code != http.StatusForbidden {
			t.Errorf("aprobar como %s: status %d, quería 403", rol, rec.Code)
		}
		if rec := llamar(t, mux, http.MethodPost, ruta("/rechazo"), rol, `{"motivo":"x"}`); rec.Code != http.StatusForbidden {
			t.Errorf("rechazar como %s: status %d, quería 403", rol, rec.Code)
		}
	}
	if viajesDe(t, ctx, id) != 0 {
		t.Fatal("un rol sin facultad creó el viaje")
	}
	if rec := llamar(t, mux, http.MethodPost, ruta("/aprobacion"), "", ``); rec.Code != http.StatusUnauthorized {
		t.Errorf("sin token: status %d, quería 401", rec.Code)
	}

	// Rechazo con cuerpo inválido: 400 y nada cambia.
	for _, cuerpo := range []string{`{}`, `{"motivo":"  "}`, `{"motivo":"x","otro":1}`} {
		if rec := llamar(t, mux, http.MethodPost, ruta("/rechazo"), "jefe_operativo", cuerpo); rec.Code != http.StatusBadRequest {
			t.Errorf("rechazo %s: status %d, quería 400", cuerpo, rec.Code)
		}
	}

	rec := llamar(t, mux, http.MethodPost, ruta("/aprobacion"), "jefe_operativo", ``)
	if rec.Code != http.StatusOK {
		t.Fatalf("aprobar: status %d, cuerpo %s", rec.Code, rec.Body)
	}
	var s operclient.Solicitud
	if err := json.Unmarshal(rec.Body.Bytes(), &s); err != nil || s.Id != id || s.Estado != operclient.EstadoSolicitud("APROBADO") {
		t.Errorf("respuesta = %+v (%v)", s, err)
	}
	if viajesDe(t, ctx, id) != 1 {
		t.Error("la aprobación no creó el viaje con el id de la solicitud")
	}
	// Segunda aprobación: el jefe ya no ve la solicitud (404); el administrador, 409.
	if rec := llamar(t, mux, http.MethodPost, ruta("/aprobacion"), "jefe_operativo", ``); rec.Code != http.StatusNotFound {
		t.Errorf("segunda aprobación del jefe: status %d, quería 404", rec.Code)
	}
	rec = llamar(t, mux, http.MethodPost, ruta("/aprobacion"), "administrador", ``)
	var e operclient.Error
	_ = json.Unmarshal(rec.Body.Bytes(), &e)
	if rec.Code != http.StatusConflict || e.Codigo != "transicion_no_permitida" {
		t.Errorf("segunda aprobación del administrador: status %d codigo %q", rec.Code, e.Codigo)
	}
	if viajesDe(t, ctx, id) != 1 {
		t.Error("hay más de un viaje")
	}

	// El administrador hereda: rechaza una pagada.
	otra := sembrarPagada(t, ctx)
	rec = llamar(t, mux, http.MethodPost, "/api/v1/solicitudes/"+otra.String()+"/rechazo", "administrador", `{"motivo":"sin cobertura"}`)
	if rec.Code != http.StatusOK || viajesDe(t, ctx, otra) != 0 {
		t.Errorf("rechazo del administrador: status %d, cuerpo %s", rec.Code, rec.Body)
	}
	var r operclient.Solicitud
	_ = json.Unmarshal(rec.Body.Bytes(), &r)
	if r.MotivoRechazo == nil || *r.MotivoRechazo != "sin cobertura" {
		t.Errorf("motivo_rechazo = %v", r.MotivoRechazo)
	}
}
