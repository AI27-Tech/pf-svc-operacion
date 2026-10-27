package bitacora_test

import (
	"context"
	"testing"

	operclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/operacion"
	"github.com/AI27-Tech/pf-svc-operacion/internal/bitacora"
	"github.com/AI27-Tech/pf-svc-operacion/internal/dbtest"
	"github.com/AI27-Tech/pf-svc-operacion/migrations"
	"github.com/google/uuid"
)

// prepara aplica las migraciones de bitacora (idempotentes) y limpia los datos,
// luego devuelve el servicio sobre un pool del rol op_bitacora. bitacora no tiene
// outbox ni escribe en otros esquemas (no audita en V1, ver servicio.go), así que
// el search_path se deja por defecto y las tablas se califican con el esquema.
func prepara(t *testing.T, ctx context.Context) *bitacora.Servicio {
	t.Helper()
	dbtest.Serializar(t, ctx) // no correr a la vez que otras pruebas (misma base)
	admin, cerrar := dbtest.AdminConn(t, ctx)
	defer cerrar()

	archivos, err := migrations.ArchivosDe("bitacora")
	if err != nil {
		t.Fatalf("listar migraciones bitacora: %v", err)
	}
	for _, f := range archivos {
		sql, err := migrations.FS.ReadFile(f)
		if err != nil {
			t.Fatalf("leer %s: %v", f, err)
		}
		dbtest.Exec(t, ctx, admin, string(sql))
	}
	dbtest.Exec(t, ctx, admin, "TRUNCATE bitacora.entrada_bitacora, bitacora.incidencia")

	pool, cerrarPool := dbtest.PoolAs(t, ctx, "op_bitacora", "")
	t.Cleanup(cerrarPool)
	return bitacora.NewServicio(pool)
}

// TestRegistrarYListarEnOrden comprueba el flujo central del caso de uso:
// registrar varias entradas y leerlas en orden cronológico por (viaje_id,
// creado_en). El id público devuelto es estable entre registrar y listar.
func TestRegistrarYListarEnOrden(t *testing.T) {
	ctx := context.Background()
	svc := prepara(t, ctx)

	viaje := operclient.Id(uuid.New())
	otro := operclient.Id(uuid.New()) // un segundo viaje, para probar el filtro

	// Registrar tres entradas del viaje, en orden. El autor viaja como el sujeto
	// del token (aquí un valor fijo para comprobar el round-trip).
	textos := []string{"salida de origen", "parada en caseta", "llegada a destino"}
	var ids []operclient.Id
	for _, txt := range textos {
		e, err := svc.RegistrarEntrada(ctx, viaje, operclient.EntradaBitacoraEntrada{Texto: txt}, "custodio-7")
		if err != nil {
			t.Fatalf("RegistrarEntrada(%q): %v", txt, err)
		}
		if e.Texto != txt {
			t.Errorf("texto devuelto: got %q, want %q", e.Texto, txt)
		}
		if e.Autor != "custodio-7" {
			t.Errorf("autor devuelto: got %q, want %q", e.Autor, "custodio-7")
		}
		if e.ViajeId != viaje {
			t.Errorf("viaje_id devuelto: got %v, want %v", e.ViajeId, viaje)
		}
		if e.Id == (operclient.Id{}) {
			t.Error("id devuelto es el UUID cero")
		}
		ids = append(ids, e.Id)
	}

	// Una entrada de OTRO viaje: no debe aparecer al listar el primero.
	if _, err := svc.RegistrarEntrada(ctx, otro, operclient.EntradaBitacoraEntrada{Texto: "ajena"}, "custodio-9"); err != nil {
		t.Fatalf("RegistrarEntrada (otro viaje): %v", err)
	}

	// Listar el viaje: las tres entradas, en el orden en que se escribieron.
	pag, err := svc.ListarBitacora(ctx, viaje, operclient.ListarBitacoraParams{})
	if err != nil {
		t.Fatalf("ListarBitacora: %v", err)
	}
	if len(pag.Items) != 3 {
		t.Fatalf("entradas listadas: got %d, want 3", len(pag.Items))
	}
	for i, it := range pag.Items {
		if it.Texto != textos[i] {
			t.Errorf("orden cronológico roto en %d: got %q, want %q", i, it.Texto, textos[i])
		}
		// El id público es estable: el mismo que devolvió RegistrarEntrada.
		if it.Id != ids[i] {
			t.Errorf("id no estable en %d: listar=%v, registrar=%v", i, it.Id, ids[i])
		}
		if it.ViajeId != viaje {
			t.Errorf("entrada %d con viaje_id ajeno: %v", i, it.ViajeId)
		}
	}
	if pag.CursorSiguiente != nil {
		t.Errorf("no debe haber página siguiente para 3 entradas: got %v", *pag.CursorSiguiente)
	}
}

// TestTextoObligatorio comprueba que una entrada sin texto se rechaza (validación),
// sin tocar la base.
func TestTextoObligatorio(t *testing.T) {
	ctx := context.Background()
	svc := prepara(t, ctx)

	if _, err := svc.RegistrarEntrada(ctx, operclient.Id(uuid.New()), operclient.EntradaBitacoraEntrada{Texto: "   "}, "x"); err == nil {
		t.Error("RegistrarEntrada con texto en blanco debió fallar")
	}
}

// TestAppendOnlyEnLaBase comprueba, al nivel del privilegio, que el rol op_bitacora
// NO puede editar ni borrar entradas: la append-only está garantizada por el GRANT
// (bitacora/0002_grants.sql), no por la ausencia de un método. Un UPDATE o un
// DELETE con el rol del caso de uso debe fallar con permission denied.
func TestAppendOnlyEnLaBase(t *testing.T) {
	ctx := context.Background()
	svc := prepara(t, ctx)

	viaje := operclient.Id(uuid.New())
	if _, err := svc.RegistrarEntrada(ctx, viaje, operclient.EntradaBitacoraEntrada{Texto: "original"}, "custodio-1"); err != nil {
		t.Fatalf("RegistrarEntrada: %v", err)
	}

	pool, cerrar := dbtest.PoolAs(t, ctx, "op_bitacora", "")
	defer cerrar()

	// UPDATE del texto: prohibido para op_bitacora (solo SELECT + INSERT).
	if _, err := pool.Exec(ctx, "UPDATE bitacora.entrada_bitacora SET texto = 'editado' WHERE viaje_id = $1", viaje); err == nil {
		t.Error("UPDATE de una entrada debió fallar: la bitácora es append-only")
	}
	// DELETE de la entrada: igualmente prohibido.
	if _, err := pool.Exec(ctx, "DELETE FROM bitacora.entrada_bitacora WHERE viaje_id = $1", viaje); err == nil {
		t.Error("DELETE de una entrada debió fallar: la bitácora es append-only")
	}
}

// TestRegistrarDesdeEventoEsIdempotente comprueba contra Postgres real que
// proyectar la misma nota (mismo evento_id) dos veces inserta UNA sola entrada:
// la idempotencia durable del histórico (migración 0003, E7/DEC-100). Una
// reentrega del evento tras un reinicio no duplica el histórico.
func TestRegistrarDesdeEventoEsIdempotente(t *testing.T) {
	ctx := context.Background()
	svc := prepara(t, ctx)

	viaje := uuid.New().String()
	evento := "nota-" + uuid.New().String()

	ins1, err := svc.RegistrarDesdeEvento(ctx, viaje, evento, "bloqueo en la caseta", "")
	if err != nil {
		t.Fatalf("primera proyección: %v", err)
	}
	if !ins1 {
		t.Fatal("la primera proyección debió insertar")
	}

	// Reentrega del mismo evento: no inserta (idempotente), no es error.
	ins2, err := svc.RegistrarDesdeEvento(ctx, viaje, evento, "bloqueo en la caseta", "")
	if err != nil {
		t.Fatalf("reentrega: %v", err)
	}
	if ins2 {
		t.Fatal("la reentrega del mismo evento no debió insertar un segundo")
	}

	// La bitácora del viaje tiene exactamente una entrada.
	pag, err := svc.ListarBitacora(ctx, operclient.Id(uuid.MustParse(viaje)), operclient.ListarBitacoraParams{})
	if err != nil {
		t.Fatalf("ListarBitacora: %v", err)
	}
	if len(pag.Items) != 1 {
		t.Fatalf("el histórico debe tener 1 entrada tras la reentrega, tiene %d", len(pag.Items))
	}
}
