package viajes_test

import (
	"context"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/estados"
	operclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/operacion"
	"github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/roles"
	platformerrors "github.com/AI27-Tech/pf-pkg-platform-go/errors"
	"github.com/AI27-Tech/pf-svc-operacion/internal/dbtest"
	"github.com/AI27-Tech/pf-svc-operacion/migrations"
	"github.com/google/uuid"
)

func contarViajes(t *testing.T, ctx context.Context) int {
	t.Helper()
	admin, cerrar := dbtest.AdminConn(t, ctx)
	defer cerrar()
	var n int
	if err := admin.QueryRow(ctx, "SELECT count(*) FROM viajes.viaje").Scan(&n); err != nil {
		t.Fatalf("contar viajes: %v", err)
	}
	return n
}

// TestCrearSoloJefeOperativoYAdministrador — ADR 38: crear un viaje es de
// Operación. Cada rol del contrato que no sea jefe_operativo ni administrador
// recibe 403 y no deja nada escrito; iterar roles.Todos cubre también los roles
// que el contrato agregue después.
func TestCrearSoloJefeOperativoYAdministrador(t *testing.T) {
	ctx := context.Background()
	svc := prepara(t, ctx)
	cli := uuid.New()
	entrada := operclient.CrearViajeEntrada{ClienteId: &cli, Origen: "O", Destinos: []operclient.Destino{{Nombre: "D"}}}

	for _, rol := range roles.Todos {
		if rol == roles.RolJefeOperativo || rol == roles.RolAdministrador {
			continue
		}
		if _, err := svc.Crear(ctx, entrada, string(rol)); platformerrors.AsCategory(err) != platformerrors.CategoryProhibido {
			t.Errorf("Crear (%s): got %v, want Prohibido", rol, err)
		}
	}
	if n := contarViajes(t, ctx); n != 0 {
		t.Errorf("los roles sin facultad no debían escribir viajes: got %d", n)
	}

	for _, rol := range []roles.Rol{roles.RolJefeOperativo, roles.RolAdministrador} {
		v, err := svc.Crear(ctx, entrada, string(rol))
		if err != nil || v.Estado != operclient.APROBADO {
			t.Errorf("Crear (%s): got %+v err %v, want viaje APROBADO", rol, v, err)
		}
	}
}

// TestCheckEstadoCoincideConElContrato — el CHECK de viajes.viaje.estado
// (migración 0007) duplica los tokens de estados.EstadosViaje. Si el contrato
// agrega o quita un estado y el CHECK no se actualiza, esta prueba falla.
func TestCheckEstadoCoincideConElContrato(t *testing.T) {
	ctx := context.Background()
	_ = prepara(t, ctx)
	admin, cerrar := dbtest.AdminConn(t, ctx)
	defer cerrar()

	var def string
	if err := admin.QueryRow(ctx,
		`SELECT pg_get_constraintdef(oid) FROM pg_constraint
		  WHERE conrelid = 'viajes.viaje'::regclass AND conname = 'estado_token_valido'`).Scan(&def); err != nil {
		t.Fatalf("leer el CHECK de estado: %v", err)
	}
	var enCheck []string
	for _, m := range regexp.MustCompile(`'([A-Z_]+)'`).FindAllStringSubmatch(def, -1) {
		enCheck = append(enCheck, m[1])
	}
	var enContrato []string
	for _, e := range estados.EstadosViaje {
		enContrato = append(enContrato, string(e))
	}
	slices.Sort(enCheck)
	slices.Sort(enContrato)
	if !slices.Equal(enCheck, enContrato) {
		t.Errorf("el CHECK de viaje.estado diverge del contrato:\n  CHECK:    %v\n  contrato: %v", enCheck, enContrato)
	}
}

// TestNacioDeSolicitudSinDefault — la columna no tiene DEFAULT (0007): un INSERT
// que la omite falla, para que la aprobación (E10-T8) no pueda nacer sin decidir
// la penalización.
func TestNacioDeSolicitudSinDefault(t *testing.T) {
	ctx := context.Background()
	_ = prepara(t, ctx)
	admin, cerrar := dbtest.AdminConn(t, ctx)
	defer cerrar()
	if _, err := admin.Exec(ctx,
		`INSERT INTO viajes.viaje (id, estado, cliente_id, origen) VALUES (gen_random_uuid(), 'APROBADO', gen_random_uuid(), 'O')`); err == nil {
		t.Error("INSERT sin nacio_de_solicitud debió fallar (columna sin DEFAULT)")
	}
}

// TestMigracion0007FallaConEstadosRetirados — con filas en estados retirados por
// el ADR 38, la 0007 se detiene con un mensaje que nombra cada estado y su
// conteo; sin ellas, se reaplica sin error (idempotente).
func TestMigracion0007FallaConEstadosRetirados(t *testing.T) {
	ctx := context.Background()
	_ = prepara(t, ctx)
	admin, cerrar := dbtest.AdminConn(t, ctx)
	defer cerrar()
	sql, err := migrations.FS.ReadFile("viajes/0007_estados_v2.sql")
	if err != nil {
		t.Fatal(err)
	}
	// Vuelve al CHECK de la 0001 (13 estados) y siembra dos estados retirados.
	dbtest.Exec(t, ctx, admin, `ALTER TABLE viajes.viaje DROP CONSTRAINT estado_token_valido;
		ALTER TABLE viajes.viaje ADD CONSTRAINT estado_token_valido CHECK (estado IN ('SOLICITADO','APROBADO','RECHAZADO_POR_AI27',
		'RECHAZADO_POR_CLIENTE','ASIGNADO','APTO_PARA_SALIDA','EN_EMBARQUE','PATRULLA_EN_CAMINO','EN_TRANSITO','EN_DESTINO',
		'EN_DESCARGA','CERRADO','CANCELADO'));
		INSERT INTO viajes.viaje (id, estado, cliente_id, origen, nacio_de_solicitud) VALUES
		 (gen_random_uuid(),'SOLICITADO',gen_random_uuid(),'O',false),
		 (gen_random_uuid(),'SOLICITADO',gen_random_uuid(),'O',false),
		 (gen_random_uuid(),'CANCELADO',gen_random_uuid(),'O',false)`)
	_, err = admin.Exec(ctx, string(sql))
	if err == nil || !strings.Contains(err.Error(), "CANCELADO = 1, SOLICITADO = 2") {
		t.Fatalf("0007 con estados retirados: got %v, want error con 'CANCELADO = 1, SOLICITADO = 2'", err)
	}
	dbtest.Exec(t, ctx, admin, "TRUNCATE viajes.viaje CASCADE")
	dbtest.Exec(t, ctx, admin, string(sql)) // sin filas retiradas: migra
	dbtest.Exec(t, ctx, admin, string(sql)) // y reaplicarla no falla
}
