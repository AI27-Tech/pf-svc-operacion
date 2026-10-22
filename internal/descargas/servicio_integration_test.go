package descargas_test

import (
	"context"
	"testing"
	"time"

	operclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/operacion"
	"github.com/AI27-Tech/pf-svc-operacion/internal/dbtest"
	"github.com/AI27-Tech/pf-svc-operacion/internal/descargas"
	"github.com/AI27-Tech/pf-svc-operacion/migrations"
	"github.com/google/uuid"
)

// prepara aplica TODAS las migraciones de operacion en dos fases (todos los 0001
// de estructura/roles primero, luego el resto por esquema), de modo que existan
// los esquemas viajes y auditoria con sus roles op_viajes y op_auditoria.
// Devuelve el servicio con dos pools, uno por rol: op_viajes (lectura del viaje)
// y op_auditoria (escritura del registro probatorio). Nunca comparte pool entre
// roles (I1), igual que en producción.
func prepara(t *testing.T, ctx context.Context) *descargas.Servicio {
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
	// Fase 1: 0001 de cada esquema (crea esquemas y roles).
	for _, esquema := range migrations.Orden {
		archivos, err := migrations.ArchivosDe(esquema)
		if err != nil || len(archivos) == 0 {
			t.Fatalf("archivos de %s: %v", esquema, err)
		}
		aplica(archivos[0])
	}
	// Fase 2: 0002.. de cada esquema (grants cruzados y tablas posteriores).
	for _, esquema := range migrations.Orden {
		archivos, _ := migrations.ArchivosDe(esquema)
		for _, f := range archivos[1:] {
			aplica(f)
		}
	}
	dbtest.Exec(t, ctx, admin,
		"TRUNCATE viajes.viaje, viajes.destino, auditoria.registro_auditoria")

	// op_viajes lee el viaje; op_auditoria escribe el registro. search_path a su
	// esquema para resolver las tablas sin calificar.
	poolViajes, cerrarV := dbtest.PoolAs(t, ctx, "op_viajes", "viajes")
	t.Cleanup(cerrarV)
	poolAud, cerrarA := dbtest.PoolAs(t, ctx, "op_auditoria", "auditoria")
	t.Cleanup(cerrarA)
	return descargas.NewServicio(poolViajes, poolAud)
}

// sembrarViaje inserta un viaje directamente (la creación vive en el paquete
// viajes; aquí solo se necesita un viaje que descargar). Devuelve su id.
func sembrarViaje(t *testing.T, ctx context.Context) operclient.Id {
	t.Helper()
	admin, cerrar := dbtest.AdminConn(t, ctx)
	defer cerrar()
	id := uuid.New()
	dbtest.Exec(t, ctx, admin, "SET search_path TO viajes")
	if _, err := admin.Exec(ctx,
		`INSERT INTO viajes.viaje (id, estado, cliente_id, origen, fecha_hora_servicio, creado_en)
		 VALUES ($1, 'EN_TRANSITO', $2, 'Origen', $3, now())`,
		id, uuid.New(), time.Now().Add(24*time.Hour).UTC(),
	); err != nil {
		t.Fatalf("sembrar viaje: %v", err)
	}
	if _, err := admin.Exec(ctx,
		`INSERT INTO viajes.destino (id, viaje_id, orden, nombre) VALUES ($1, $2, 0, 'Destino')`,
		uuid.New(), id,
	); err != nil {
		t.Fatalf("sembrar destino: %v", err)
	}
	return id
}

// registrosDe cuenta y, si hay exactamente uno, devuelve el registro de
// auditoría de un viaje: actor, acción, módulo y el instante.
func registrosDe(t *testing.T, ctx context.Context, viajeID string) (n int, actor, accion, modulo string, ocurrido time.Time) {
	t.Helper()
	admin, cerrar := dbtest.AdminConn(t, ctx)
	defer cerrar()
	if err := admin.QueryRow(ctx,
		"SELECT count(*) FROM auditoria.registro_auditoria WHERE viaje_id=$1", viajeID,
	).Scan(&n); err != nil {
		t.Fatalf("contar registros: %v", err)
	}
	if n == 1 {
		if err := admin.QueryRow(ctx,
			"SELECT actor_id, accion, modulo, ocurrido_en FROM auditoria.registro_auditoria WHERE viaje_id=$1",
			viajeID,
		).Scan(&actor, &accion, &modulo, &ocurrido); err != nil {
			t.Fatalf("leer registro: %v", err)
		}
	}
	return n, actor, accion, modulo, ocurrido
}

// TestRegistrarDescargaEscribeUnRegistro — AI27-80: una llamada a
// registrarDescargaViaje escribe EXACTAMENTE un registro de auditoría con el
// actor, el viaje, la acción descarga_expediente y el momento; y devuelve el
// Viaje descargado.
func TestRegistrarDescargaEscribeUnRegistro(t *testing.T) {
	ctx := context.Background()
	svc := prepara(t, ctx)

	viajeID := sembrarViaje(t, ctx)
	const actor = "usuario-123"

	antes := time.Now().UTC()
	v, err := svc.Registrar(ctx, viajeID, actor)
	if err != nil {
		t.Fatalf("Registrar: %v", err)
	}
	despues := time.Now().UTC()

	// Devuelve el Viaje descargado (con su id y datos leídos de op_viajes).
	if v == nil || v.Id != viajeID {
		t.Fatalf("Viaje devuelto: got %+v, want id %v", v, viajeID)
	}
	if v.Origen != "Origen" {
		t.Errorf("origen del viaje: got %q, want Origen", v.Origen)
	}

	// Exactamente un registro, con actor/viaje/accion/momento correctos.
	n, regActor, accion, modulo, ocurrido := registrosDe(t, ctx, viajeID.String())
	if n != 1 {
		t.Fatalf("registros de auditoría: got %d, want 1", n)
	}
	if regActor != actor {
		t.Errorf("actor del registro: got %q, want %q", regActor, actor)
	}
	if accion != "descarga_expediente" {
		t.Errorf("acción del registro: got %q, want descarga_expediente", accion)
	}
	if modulo != "auditoria" {
		t.Errorf("módulo del registro: got %q, want auditoria", modulo)
	}
	if ocurrido.Before(antes.Add(-time.Second)) || ocurrido.After(despues.Add(time.Second)) {
		t.Errorf("momento del registro fuera de la ventana de la llamada: %v (ventana %v..%v)", ocurrido, antes, despues)
	}
}

// TestRegistrarDescargaViajeInexistente — descargar un expediente que no existe
// da NoEncontrado y NO escribe ningún registro: no se audita la descarga de algo
// inexistente.
func TestRegistrarDescargaViajeInexistente(t *testing.T) {
	ctx := context.Background()
	svc := prepara(t, ctx)

	inexistente := uuid.New()
	if _, err := svc.Registrar(ctx, inexistente, "usuario-123"); err == nil {
		t.Fatal("descargar un viaje inexistente debió fallar (404)")
	}
	if n, _, _, _, _ := registrosDe(t, ctx, inexistente.String()); n != 0 {
		t.Errorf("no debió escribirse registro para un viaje inexistente: got %d, want 0", n)
	}
}

// TestRegistroDescargaEsAppendOnly — el registro de la descarga es probatorio y
// append-only: el rol op_auditoria NO puede editarlo ni borrarlo (el esquema no
// otorga UPDATE ni DELETE a nadie, migrations/auditoria). Se verifica que ambos
// intentos fallen por permisos, dejando el registro intacto.
func TestRegistroDescargaEsAppendOnly(t *testing.T) {
	ctx := context.Background()
	svc := prepara(t, ctx)

	viajeID := sembrarViaje(t, ctx)
	if _, err := svc.Registrar(ctx, viajeID, "usuario-123"); err != nil {
		t.Fatalf("Registrar: %v", err)
	}
	if n, _, _, _, _ := registrosDe(t, ctx, viajeID.String()); n != 1 {
		t.Fatalf("precondición: debía existir 1 registro, hay %d", n)
	}

	// Con el rol op_auditoria, UPDATE y DELETE deben ser rechazados por permisos:
	// el esquema solo otorga SELECT/INSERT. Un registro editable o borrable
	// produciría huecos indistinguibles de manipulación.
	pool, cerrar := dbtest.PoolAs(t, ctx, "op_auditoria", "auditoria")
	defer cerrar()
	if _, err := pool.Exec(ctx,
		"UPDATE auditoria.registro_auditoria SET actor_id='otro' WHERE viaje_id=$1", viajeID.String()); err == nil {
		t.Error("UPDATE sobre el registro de auditoría debió ser rechazado (append-only)")
	}
	if _, err := pool.Exec(ctx,
		"DELETE FROM auditoria.registro_auditoria WHERE viaje_id=$1", viajeID.String()); err == nil {
		t.Error("DELETE sobre el registro de auditoría debió ser rechazado (append-only)")
	}

	// El registro sigue intacto tras los intentos fallidos.
	if n, actor, _, _, _ := registrosDe(t, ctx, viajeID.String()); n != 1 || actor != "usuario-123" {
		t.Errorf("el registro debió quedar intacto: n=%d actor=%q", n, actor)
	}
}
