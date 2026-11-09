package operacion_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	catalogos "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/catalogos"
	operclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/operacion"
	"github.com/AI27-Tech/pf-pkg-platform-go/auth"
	platformerrors "github.com/AI27-Tech/pf-pkg-platform-go/errors"
	"github.com/AI27-Tech/pf-svc-operacion"
	"github.com/AI27-Tech/pf-svc-operacion/internal/dbtest"
	"github.com/google/uuid"
)

// atomosFake es catalogos en memoria para el armado del roster: vehículos y
// custodios por id. Lo comparten las pruebas de moduloCompleto; cada prueba da
// de alta sus propios átomos con ids nuevos.
type atomosFake struct {
	mu        sync.Mutex
	vehiculos map[uuid.UUID]catalogos.Vehiculo
	custodios map[uuid.UUID]catalogos.Custodio
}

var catalogo = &atomosFake{vehiculos: map[uuid.UUID]catalogos.Vehiculo{}, custodios: map[uuid.UUID]catalogos.Custodio{}}

func (f *atomosFake) ObtenerVehiculo(_ context.Context, id catalogos.Id) (*catalogos.Vehiculo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.vehiculos[id]
	if !ok {
		return nil, platformerrors.NewNoEncontrado("catalogos: recurso no encontrado")
	}
	return &v, nil
}

func (f *atomosFake) ObtenerCustodio(_ context.Context, id catalogos.Id) (*catalogos.Custodio, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.custodios[id]
	if !ok {
		return nil, platformerrors.NewNoEncontrado("catalogos: recurso no encontrado")
	}
	return &c, nil
}

// vehiculo da de alta un vehículo del tipo y estado dados y devuelve su id.
func (f *atomosFake) vehiculo(tipo catalogos.VehiculoTipo, estado catalogos.VehiculoEstado) uuid.UUID {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := uuid.New()
	f.vehiculos[id] = catalogos.Vehiculo{Id: id, Tipo: tipo, Estado: estado}
	return id
}

// custodio da de alta un custodio con el estado dado y devuelve su id.
func (f *atomosFake) custodio(estado catalogos.CustodioEstado) uuid.UUID {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := uuid.New()
	f.custodios[id] = catalogos.Custodio{Id: id, Nombre: "custodio", Estado: estado}
	return id
}

func coche() uuid.UUID {
	return catalogo.vehiculo(catalogos.VehiculoTipoCoche, catalogos.VehiculoEstadoActiva)
}
func moto() uuid.UUID {
	return catalogo.vehiculo(catalogos.VehiculoTipoMoto, catalogos.VehiculoEstadoActiva)
}
func custodio() uuid.UUID {
	return catalogo.custodio(catalogos.CustodioEstadoActivo)
}

var claimsAdmin = &auth.Claims{Subject: "adm-1", Rol: "administrador", Audiencia: auth.AudienciaInterno}

func ids(v ...uuid.UUID) []operclient.Id { return v }
func ptr[T any](v T) *T                  { return &v }

// crear arma una unidad como jefe_operativo y falla la prueba si no se puede.
func crear(t *testing.T, api operacion.ConsultasAPI, vehiculo uuid.UUID, custodios ...uuid.UUID) *operclient.Unidad {
	t.Helper()
	u, err := api.CrearUnidad(auth.ContextWithClaims(context.Background(), claimsJefe),
		operclient.UnidadEntrada{VehiculoId: vehiculo, CustodiosIds: custodios})
	if err != nil {
		t.Fatalf("crear unidad: %v", err)
	}
	return u
}

// pedir hace una petición HTTP como jefe_operativo (token "x") o con el token dado.
func pedir(m *operacion.Module, method, path, body, token string) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	m.Routes(mux)
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// quiereCodigo afirma el 409 con su código de máquina en la fachada.
func quiereCodigo(t *testing.T, err error, codigo string) {
	t.Helper()
	if platformerrors.AsCategory(err).HTTPStatus() != http.StatusConflict || platformerrors.CodigoDe(err) != codigo {
		t.Errorf("err %v (codigo %q); quiere 409 %s", err, platformerrors.CodigoDe(err), codigo)
	}
}

// quiere400 afirma una validación (400) en la fachada.
func quiere400(t *testing.T, err error, caso string) {
	t.Helper()
	if platformerrors.AsCategory(err) != platformerrors.CategoryValidacion {
		t.Errorf("%s: err %v; quiere 400 validacion", caso, err)
	}
}

// TestUnidadesAlta: el jefe_operativo arma una patrulla (coche, dos custodios)
// por HTTP (201) y el administrador una moto (un custodio) por la fachada; el
// tipo sale del vehículo y `activa` omitida vale true. obtenerUnidad la lee
// igual por fachada y HTTP; un `tipo` en el cuerpo es 400 y una unidad
// inexistente, 404.
func TestUnidadesAlta(t *testing.T) {
	ctx := context.Background()
	m := moduloCompleto(t, ctx)
	api := m.Consultas()

	veh, c1, c2 := coche(), custodio(), custodio()
	rec := pedir(m, http.MethodPost, "/api/v1/unidades",
		`{"vehiculo_id":"`+veh.String()+`","custodios_ids":["`+c1.String()+`","`+c2.String()+`"]}`, "x")
	if rec.Code != http.StatusCreated {
		t.Fatalf("HTTP alta patrulla: status %d cuerpo %s; quiere 201", rec.Code, rec.Body)
	}
	var p operclient.Unidad
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("decodificar unidad: %v", err)
	}
	if p.Tipo == nil || *p.Tipo != operclient.Patrulla || p.VehiculoId != veh || !p.Activa || len(p.CustodiosIds) != 2 {
		t.Errorf("patrulla: got %+v; quiere patrulla activa con %s y 2 custodios", p, veh)
	}

	vm, c3 := moto(), custodio()
	mo, err := api.CrearUnidad(auth.ContextWithClaims(ctx, claimsAdmin), operclient.UnidadEntrada{VehiculoId: vm, CustodiosIds: ids(c3)})
	if err != nil {
		t.Fatalf("administrador alta moto: %v", err)
	}
	if mo.Tipo == nil || *mo.Tipo != operclient.Moto || !mo.Activa || len(mo.CustodiosIds) != 1 || mo.CustodiosIds[0] != c3 {
		t.Errorf("moto: got %+v; quiere moto activa con el custodio %s", mo, c3)
	}

	inactiva, err := api.CrearUnidad(auth.ContextWithClaims(ctx, claimsJefe),
		operclient.UnidadEntrada{VehiculoId: moto(), CustodiosIds: ids(custodio()), Activa: ptr(false)})
	if err != nil || inactiva.Activa {
		t.Errorf("alta inactiva: %+v err %v; quiere activa=false", inactiva, err)
	}

	got, err := api.ObtenerUnidad(auth.ContextWithClaims(ctx, claimsJefe), p.Id)
	if err != nil || got.Id != p.Id || *got.Tipo != operclient.Patrulla || len(got.CustodiosIds) != 2 {
		t.Errorf("obtener: %+v err %v; quiere la patrulla %s", got, err, p.Id)
	}
	if rec := pedir(m, http.MethodGet, "/api/v1/unidades/"+mo.Id.String(), "", "x"); rec.Code != http.StatusOK ||
		!strings.Contains(rec.Body.String(), `"tipo":"moto"`) {
		t.Errorf("HTTP obtener: status %d cuerpo %s; quiere 200 con la moto", rec.Code, rec.Body)
	}

	if rec := pedir(m, http.MethodPost, "/api/v1/unidades",
		`{"vehiculo_id":"`+moto().String()+`","custodios_ids":["`+custodio().String()+`"],"tipo":"moto"}`, "x"); rec.Code != http.StatusBadRequest {
		t.Errorf("HTTP con tipo en el cuerpo: status %d cuerpo %s; quiere 400", rec.Code, rec.Body)
	}

	otra := uuid.New()
	_, err = api.ObtenerUnidad(auth.ContextWithClaims(ctx, claimsJefe), otra)
	mismoStatus(t, m, err, http.MethodGet, "/api/v1/unidades/"+otra.String(), "", 404)
}

// TestUnidadesCustodiosPorTipo: una moto lleva exactamente un custodio y una
// patrulla uno o dos; repetidos, vacíos y referencias inexistentes o inactivas
// en catalogos son 400. Al cambiar el vehículo de una patrulla de dos custodios
// a una moto, la regla se aplica a la unidad resultante.
func TestUnidadesCustodiosPorTipo(t *testing.T) {
	ctx := context.Background()
	m := moduloCompleto(t, ctx)
	api := m.Consultas()
	ctxJefe := auth.ContextWithClaims(ctx, claimsJefe)
	alta := func(v uuid.UUID, cs ...uuid.UUID) error {
		_, err := api.CrearUnidad(ctxJefe, operclient.UnidadEntrada{VehiculoId: v, CustodiosIds: cs})
		return err
	}

	vm, c1, c2 := moto(), custodio(), custodio()
	err := alta(vm, c1, c2)
	quiere400(t, err, "moto con dos custodios")
	mismoStatus(t, m, err, http.MethodPost, "/api/v1/unidades",
		`{"vehiculo_id":"`+vm.String()+`","custodios_ids":["`+c1.String()+`","`+c2.String()+`"]}`, 400)
	quiere400(t, alta(coche()), "patrulla sin custodios")
	quiere400(t, alta(coche(), custodio(), custodio(), custodio()), "patrulla con tres custodios")
	quiere400(t, alta(coche(), c1, c1), "custodio repetido")
	quiere400(t, alta(uuid.New(), custodio()), "vehículo inexistente")
	quiere400(t, alta(catalogo.vehiculo(catalogos.VehiculoTipoCoche, catalogos.VehiculoEstadoInactiva), custodio()), "vehículo inactivo")
	quiere400(t, alta(coche(), uuid.New()), "custodio inexistente")
	quiere400(t, alta(coche(), catalogo.custodio(catalogos.CustodioEstadoInactivo)), "custodio inactivo")

	p := crear(t, api, coche(), custodio(), custodio())
	_, err = api.ModificarUnidad(ctxJefe, p.Id, operclient.UnidadModificacion{VehiculoId: ptr(moto())})
	quiere400(t, err, "patrulla de dos custodios pasa a moto")
	solo := custodio()
	mo, err := api.ModificarUnidad(ctxJefe, p.Id, operclient.UnidadModificacion{VehiculoId: ptr(moto()), CustodiosIds: ptr(ids(solo))})
	if err != nil || *mo.Tipo != operclient.Moto || len(mo.CustodiosIds) != 1 || mo.CustodiosIds[0] != solo {
		t.Fatalf("pasar a moto con un custodio: %+v err %v; quiere moto con %s", mo, err, solo)
	}
	_, err = api.ModificarUnidad(ctxJefe, p.Id, operclient.UnidadModificacion{CustodiosIds: ptr(ids(solo, custodio()))})
	quiere400(t, err, "moto recibe un segundo custodio")
}

// TestUnidadesExclusividadVehiculo: un vehículo está en una sola unidad activa.
// Una segunda alta con el mismo vehículo es 409 vehiculo_en_otra_unidad (fachada
// y HTTP); desactivada la primera, el vehículo se puede usar en otra, y entonces
// reactivar la primera o cambiar otra unidad a ese vehículo vuelve a ser 409.
func TestUnidadesExclusividadVehiculo(t *testing.T) {
	ctx := context.Background()
	m := moduloCompleto(t, ctx)
	api := m.Consultas()
	ctxJefe := auth.ContextWithClaims(ctx, claimsJefe)

	veh := coche()
	a := crear(t, api, veh, custodio())
	c := custodio()
	_, err := api.CrearUnidad(ctxJefe, operclient.UnidadEntrada{VehiculoId: veh, CustodiosIds: ids(c)})
	quiereCodigo(t, err, "vehiculo_en_otra_unidad")
	if rec := pedir(m, http.MethodPost, "/api/v1/unidades", `{"vehiculo_id":"`+veh.String()+`","custodios_ids":["`+c.String()+`"]}`, "x"); rec.Code != http.StatusConflict ||
		!strings.Contains(rec.Body.String(), `"codigo":"vehiculo_en_otra_unidad"`) {
		t.Errorf("HTTP alta con vehículo ocupado: status %d cuerpo %s; quiere 409 vehiculo_en_otra_unidad", rec.Code, rec.Body)
	}

	if _, err := api.ModificarUnidad(ctxJefe, a.Id, operclient.UnidadModificacion{Activa: ptr(false)}); err != nil {
		t.Fatalf("desactivar A: %v", err)
	}
	crear(t, api, veh, c)
	_, err = api.ModificarUnidad(ctxJefe, a.Id, operclient.UnidadModificacion{Activa: ptr(true)})
	quiereCodigo(t, err, "vehiculo_en_otra_unidad")
	if rec := pedir(m, http.MethodPatch, "/api/v1/unidades/"+a.Id.String(), `{"activa":true}`, "x"); rec.Code != http.StatusConflict ||
		!strings.Contains(rec.Body.String(), `"codigo":"vehiculo_en_otra_unidad"`) {
		t.Errorf("HTTP reactivar: status %d cuerpo %s; quiere 409 vehiculo_en_otra_unidad", rec.Code, rec.Body)
	}

	otra := crear(t, api, coche(), custodio())
	_, err = api.ModificarUnidad(ctxJefe, otra.Id, operclient.UnidadModificacion{VehiculoId: ptr(veh)})
	quiereCodigo(t, err, "vehiculo_en_otra_unidad")
}

// TestUnidadesExclusividadCustodio: un custodio está en una sola unidad activa.
// Misma regla que el vehículo, por la tabla puente: alta, reactivación y cambio
// de custodios chocan; una unidad desactivada no cuenta, y reactivar cambiando
// a la vez el custodio ocupado por uno libre sí procede.
func TestUnidadesExclusividadCustodio(t *testing.T) {
	ctx := context.Background()
	m := moduloCompleto(t, ctx)
	api := m.Consultas()
	ctxJefe := auth.ContextWithClaims(ctx, claimsJefe)

	c1 := custodio()
	a := crear(t, api, coche(), c1, custodio())
	v := moto()
	_, err := api.CrearUnidad(ctxJefe, operclient.UnidadEntrada{VehiculoId: v, CustodiosIds: ids(c1)})
	quiereCodigo(t, err, "custodio_en_otra_unidad")
	if rec := pedir(m, http.MethodPost, "/api/v1/unidades", `{"vehiculo_id":"`+v.String()+`","custodios_ids":["`+c1.String()+`"]}`, "x"); rec.Code != http.StatusConflict ||
		!strings.Contains(rec.Body.String(), `"codigo":"custodio_en_otra_unidad"`) {
		t.Errorf("HTTP alta con custodio ocupado: status %d cuerpo %s; quiere 409 custodio_en_otra_unidad", rec.Code, rec.Body)
	}

	if _, err := api.ModificarUnidad(ctxJefe, a.Id, operclient.UnidadModificacion{Activa: ptr(false)}); err != nil {
		t.Fatalf("desactivar A: %v", err)
	}
	crear(t, api, v, c1)
	_, err = api.ModificarUnidad(ctxJefe, a.Id, operclient.UnidadModificacion{Activa: ptr(true)})
	quiereCodigo(t, err, "custodio_en_otra_unidad")
	if got, err := api.ObtenerUnidad(ctxJefe, a.Id); err != nil || got.Activa {
		t.Errorf("tras el rechazo A sigue inactiva: %+v err %v", got, err)
	}
	libre := custodio()
	re, err := api.ModificarUnidad(ctxJefe, a.Id, operclient.UnidadModificacion{Activa: ptr(true), CustodiosIds: ptr(ids(libre))})
	if err != nil || !re.Activa || len(re.CustodiosIds) != 1 || re.CustodiosIds[0] != libre {
		t.Errorf("reactivar con un custodio libre: %+v err %v; quiere activa con %s", re, err, libre)
	}

	if _, err := api.CrearUnidad(ctxJefe, operclient.UnidadEntrada{VehiculoId: moto(), CustodiosIds: ids(c1), Activa: ptr(false)}); err != nil {
		t.Errorf("alta inactiva con un custodio ocupado: %v; una unidad desactivada no cuenta", err)
	}
	otra := crear(t, api, moto(), custodio())
	_, err = api.ModificarUnidad(ctxJefe, otra.Id, operclient.UnidadModificacion{CustodiosIds: ptr(ids(c1))})
	quiereCodigo(t, err, "custodio_en_otra_unidad")
}

// TestUnidadesAltaConcurrenteMismoCustodio: dos altas simultáneas con el mismo
// custodio (y vehículos distintos): exactamente una gana y la otra es 409
// custodio_en_otra_unidad. Se repite para que las dos transacciones se crucen.
func TestUnidadesAltaConcurrenteMismoCustodio(t *testing.T) {
	ctx := context.Background()
	m := moduloCompleto(t, ctx)
	api := m.Consultas()
	ctxJefe := auth.ContextWithClaims(ctx, claimsJefe)

	for i := 0; i < 10; i++ {
		c := custodio()
		vehiculos := []uuid.UUID{moto(), moto()}
		salida := make(chan struct{})
		errs := make([]error, 2)
		var wg sync.WaitGroup
		for j := range vehiculos {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-salida
				_, errs[j] = api.CrearUnidad(ctxJefe, operclient.UnidadEntrada{VehiculoId: vehiculos[j], CustodiosIds: ids(c)})
			}()
		}
		close(salida)
		wg.Wait()
		ganan := 0
		for _, err := range errs {
			if err == nil {
				ganan++
			} else if platformerrors.CodigoDe(err) != "custodio_en_otra_unidad" {
				t.Fatalf("vuelta %d: err %v; quiere 409 custodio_en_otra_unidad", i, err)
			}
		}
		if ganan != 1 {
			t.Fatalf("vuelta %d: ganaron %d altas; quiere exactamente 1 (errs %v)", i, ganan, errs)
		}
	}
}

// TestUnidadesConReservaVigente: una unidad reservada en un viaje no terminal no
// se modifica (vehículo, custodios ni activa): 409 unidad_con_reserva_vigente,
// por fachada y HTTP, y la unidad queda intacta. Con el viaje terminal, la
// misma unidad se edita. Tras reasignar la escolta, la unidad liberada se edita
// y la nueva no.
func TestUnidadesConReservaVigente(t *testing.T) {
	ctx := context.Background()
	m := moduloCompleto(t, ctx)
	api := m.Consultas()
	ctxJefe := auth.ContextWithClaims(ctx, claimsJefe)

	c1 := custodio()
	u := crear(t, api, coche(), c1)
	viaje := viajeAprobado(t, ctx, api)
	if _, err := api.AsignarRecursos(ctxJefe, viaje, operclient.AsignacionEntrada{UnidadesIds: ids(u.Id)}); err != nil {
		t.Fatalf("asignar: %v", err)
	}

	for caso, cambio := range map[string]operclient.UnidadModificacion{
		"desactivar":       {Activa: ptr(false)},
		"cambiar custodio": {CustodiosIds: ptr(ids(custodio()))},
		"cambiar vehículo": {VehiculoId: ptr(coche())},
	} {
		_, err := api.ModificarUnidad(ctxJefe, u.Id, cambio)
		if platformerrors.CodigoDe(err) != "unidad_con_reserva_vigente" || platformerrors.AsCategory(err).HTTPStatus() != http.StatusConflict {
			t.Errorf("%s: err %v; quiere 409 unidad_con_reserva_vigente", caso, err)
		}
	}
	if rec := pedir(m, http.MethodPatch, "/api/v1/unidades/"+u.Id.String(), `{"activa":false}`, "x"); rec.Code != http.StatusConflict ||
		!strings.Contains(rec.Body.String(), `"codigo":"unidad_con_reserva_vigente"`) {
		t.Errorf("HTTP desactivar reservada: status %d cuerpo %s; quiere 409 unidad_con_reserva_vigente", rec.Code, rec.Body)
	}
	if got, err := api.ObtenerUnidad(ctxJefe, u.Id); err != nil || !got.Activa || got.VehiculoId != u.VehiculoId ||
		len(got.CustodiosIds) != 1 || got.CustodiosIds[0] != c1 {
		t.Errorf("unidad reservada intacta: %+v err %v", got, err)
	}

	admin, cerrar := dbtest.AdminConn(t, ctx)
	defer cerrar()
	dbtest.Exec(t, ctx, admin, "UPDATE viajes.viaje SET estado='CERRADO' WHERE id='"+viaje.String()+"'")
	if got, err := api.ModificarUnidad(ctxJefe, u.Id, operclient.UnidadModificacion{Activa: ptr(false)}); err != nil || got.Activa {
		t.Errorf("viaje terminal: %+v err %v; quiere la unidad desactivada", got, err)
	}

	vieja, nueva := crear(t, api, coche(), custodio()), crear(t, api, coche(), custodio())
	otro := viajeAprobado(t, ctx, api)
	if _, err := api.AsignarRecursos(ctxJefe, otro, operclient.AsignacionEntrada{UnidadesIds: ids(vieja.Id)}); err != nil {
		t.Fatalf("asignar otro: %v", err)
	}
	if rec := pedir(m, http.MethodPatch, "/api/v1/viajes/"+otro.String()+"/asignacion",
		`{"unidades_ids":["`+nueva.Id.String()+`"],"motivo":"cambio de patrulla"}`, "x"); rec.Code != http.StatusOK {
		t.Fatalf("reasignar: status %d cuerpo %s", rec.Code, rec.Body)
	}
	if _, err := api.ModificarUnidad(ctxJefe, vieja.Id, operclient.UnidadModificacion{Activa: ptr(false)}); err != nil {
		t.Errorf("unidad liberada por la reasignación: %v; quiere editable", err)
	}
	_, err := api.ModificarUnidad(ctxJefe, nueva.Id, operclient.UnidadModificacion{Activa: ptr(false)})
	quiereCodigo(t, err, "unidad_con_reserva_vigente")
}

// TestUnidadesRoles: solo jefe_operativo y administrador arman el roster
// (DEC-142 #3). Monitorista y supervisor reciben 403 (fachada y HTTP), el
// Cliente 404 (no confirma que la unidad exista) y sin claims, 401.
func TestUnidadesRoles(t *testing.T) {
	ctx := context.Background()
	m := moduloCompleto(t, ctx)
	api := m.Consultas()
	u := crear(t, api, moto(), custodio())
	alta := operclient.UnidadEntrada{VehiculoId: moto(), CustodiosIds: ids(custodio())}
	cambio := operclient.UnidadModificacion{Activa: ptr(false)}
	cliente := &auth.Claims{Subject: "cli-1", TenantID: uuid.NewString(), Rol: "cliente", Audiencia: auth.AudienciaCliente}

	for _, c := range []struct {
		claims *auth.Claims
		status int
	}{{claimsMonitorista, 403}, {claimsSupervisor, 403}, {cliente, 404}, {nil, 401}} {
		cx := ctx
		if c.claims != nil {
			cx = auth.ContextWithClaims(ctx, c.claims)
		}
		_, e1 := api.CrearUnidad(cx, alta)
		_, e2 := api.ObtenerUnidad(cx, u.Id)
		_, e3 := api.ModificarUnidad(cx, u.Id, cambio)
		for op, err := range map[string]error{"crearUnidad": e1, "obtenerUnidad": e2, "modificarUnidad": e3} {
			if got := platformerrors.AsCategory(err).HTTPStatus(); got != c.status {
				t.Errorf("%v %s: status %d (err %v); quiere %d", c.claims, op, got, err, c.status)
			}
		}
	}
	if rec := pedir(m, http.MethodGet, "/api/v1/unidades/"+u.Id.String(), "", "monitorista"); rec.Code != http.StatusForbidden {
		t.Errorf("HTTP monitorista obtiene: status %d; quiere 403", rec.Code)
	}
	if rec := pedir(m, http.MethodPatch, "/api/v1/unidades/"+u.Id.String(), `{"activa":false}`, "monitorista"); rec.Code != http.StatusForbidden {
		t.Errorf("HTTP monitorista modifica: status %d; quiere 403", rec.Code)
	}
	if got, err := api.ObtenerUnidad(auth.ContextWithClaims(ctx, claimsJefe), u.Id); err != nil || !got.Activa {
		t.Errorf("la unidad no cambió por los rechazos: %+v err %v", got, err)
	}
}

// TestUnidadesAsignarSoloActivas: desactivar una unidad en /roster la saca de la
// asignación: asignarla es 409 por fachada y HTTP, y una unidad que no está en
// el roster es 400. Reactivada, se asigna.
func TestUnidadesAsignarSoloActivas(t *testing.T) {
	ctx := context.Background()
	m := moduloCompleto(t, ctx)
	api := m.Consultas()
	ctxJefe := auth.ContextWithClaims(ctx, claimsJefe)

	u := crear(t, api, moto(), custodio())
	if _, err := api.ModificarUnidad(ctxJefe, u.Id, operclient.UnidadModificacion{Activa: ptr(false)}); err != nil {
		t.Fatalf("desactivar: %v", err)
	}
	viaje := viajeAprobado(t, ctx, api)
	_, err := api.AsignarRecursos(ctxJefe, viaje, operclient.AsignacionEntrada{UnidadesIds: ids(u.Id)})
	mismoStatus(t, m, err, http.MethodPost, "/api/v1/viajes/"+viaje.String()+"/asignacion", `{"unidades_ids":["`+u.Id.String()+`"]}`, 409)
	otra := uuid.New()
	_, err = api.AsignarRecursos(ctxJefe, viaje, operclient.AsignacionEntrada{UnidadesIds: ids(otra)})
	mismoStatus(t, m, err, http.MethodPost, "/api/v1/viajes/"+viaje.String()+"/asignacion", `{"unidades_ids":["`+otra.String()+`"]}`, 400)

	if _, err := api.ModificarUnidad(ctxJefe, u.Id, operclient.UnidadModificacion{Activa: ptr(true)}); err != nil {
		t.Fatalf("reactivar: %v", err)
	}
	if _, err := api.AsignarRecursos(ctxJefe, viaje, operclient.AsignacionEntrada{UnidadesIds: ids(u.Id)}); err != nil {
		t.Errorf("asignar la unidad reactivada: %v", err)
	}
}
