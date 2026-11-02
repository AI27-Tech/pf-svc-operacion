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
