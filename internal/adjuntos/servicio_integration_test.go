package adjuntos_test

import (
	"context"
	"testing"
	"time"

	operclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/operacion"
	platformerrors "github.com/AI27-Tech/pf-pkg-platform-go/errors"
	"github.com/AI27-Tech/pf-svc-operacion/internal/adjuntos"
	"github.com/AI27-Tech/pf-svc-operacion/internal/dbtest"
	"github.com/AI27-Tech/pf-svc-operacion/migrations"
	"github.com/google/uuid"
)

const checksumValido = "a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90"

// prepara aplica todas las migraciones en dos fases (estructura, luego grants) y
// devuelve el servicio con dos pools por rol: op_bitacora (metadata del adjunto)
// y op_auditoria (rastro probatorio). Nunca comparte pool entre roles (I1).
func prepara(t *testing.T, ctx context.Context) *adjuntos.Servicio {
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
	for _, esquema := range migrations.Orden {
		archivos, err := migrations.ArchivosDe(esquema)
		if err != nil || len(archivos) == 0 {
			t.Fatalf("archivos de %s: %v", esquema, err)
		}
		aplica(archivos[0])
	}
	for _, esquema := range migrations.Orden {
		archivos, _ := migrations.ArchivosDe(esquema)
		for _, f := range archivos[1:] {
			aplica(f)
		}
	}
	dbtest.Exec(t, ctx, admin, "TRUNCATE bitacora.adjunto, auditoria.registro_auditoria")

	poolBit, cerrarB := dbtest.PoolAs(t, ctx, "op_bitacora", "bitacora")
	t.Cleanup(cerrarB)
	poolAud, cerrarA := dbtest.PoolAs(t, ctx, "op_auditoria", "auditoria")
	t.Cleanup(cerrarA)
	return adjuntos.NewServicio(poolBit, poolAud, adjuntos.AlmacenStub{Base: "https://almacen.test"})
}

func contarAuditoria(t *testing.T, ctx context.Context, viajeID, accion string) int {
	t.Helper()
	admin, cerrar := dbtest.AdminConn(t, ctx)
	defer cerrar()
	var n int
	if err := admin.QueryRow(ctx,
		"SELECT count(*) FROM auditoria.registro_auditoria WHERE viaje_id=$1 AND accion=$2",
		viajeID, accion,
	).Scan(&n); err != nil {
		t.Fatalf("contar auditoría: %v", err)
	}
	return n
}

func entrada() operclient.IntencionSubidaAdjuntoEntrada {
	cs := checksumValido
	return operclient.IntencionSubidaAdjuntoEntrada{
		Nombre: "acta.pdf", TipoMedio: "application/pdf", TamanoBytes: 2048, ChecksumSha256: cs,
	}
}

// TestCrearIntencionSubidaEscribeMetadataPendienteYAudita: crear la intención
// inserta exactamente un adjunto en estado 'pendiente', devuelve la URL de subida
// de vida corta y deja un registro de auditoría de la subida.
func TestCrearIntencionSubidaEscribeMetadataPendienteYAudita(t *testing.T) {
	ctx := context.Background()
	svc := prepara(t, ctx)
	viajeID := operclient.Id(uuid.New())

	res, err := svc.CrearIntencionSubida(ctx, viajeID, entrada(), "usuario-1")
	if err != nil {
		t.Fatalf("CrearIntencionSubida: %v", err)
	}
	if res.UrlSubida == "" || !res.ExpiraEn.After(time.Now()) {
		t.Fatalf("intención = %+v, se esperaba url y expiración futura", res)
	}

	// La metadata quedó y se lista en 'pendiente', con el nombre reenviado.
	pag, err := svc.Listar(ctx, viajeID)
	if err != nil {
		t.Fatalf("Listar: %v", err)
	}
	if len(pag.Items) != 1 {
		t.Fatalf("adjuntos = %d, se esperaba 1", len(pag.Items))
	}
	a := pag.Items[0]
	if a.Id != res.AdjuntoId || a.Nombre != "acta.pdf" || a.EstadoEscaneo != operclient.Pendiente {
		t.Errorf("adjunto = %+v, se esperaba id=%v nombre=acta.pdf estado=pendiente", a, res.AdjuntoId)
	}
	if a.ChecksumSha256 == nil || *a.ChecksumSha256 != checksumValido {
		t.Errorf("checksum = %v, se esperaba %s", a.ChecksumSha256, checksumValido)
	}
	if n := contarAuditoria(t, ctx, viajeID.String(), "adjunto_intencion_subida"); n != 1 {
		t.Errorf("auditoría de subida = %d, se esperaba 1", n)
	}
}

// TestCrearIntencionSubidaValida: nombre vacío o tamaño no positivo es 400 y no
// escribe metadata ni auditoría.
func TestCrearIntencionSubidaValida(t *testing.T) {
	ctx := context.Background()
	svc := prepara(t, ctx)
	viajeID := operclient.Id(uuid.New())

	casos := map[string]operclient.IntencionSubidaAdjuntoEntrada{
		"sin nombre":      {Nombre: "", TipoMedio: "application/pdf", TamanoBytes: 1, ChecksumSha256: checksumValido},
		"tamaño cero":     {Nombre: "x.pdf", TipoMedio: "application/pdf", TamanoBytes: 0, ChecksumSha256: checksumValido},
		"tamaño negativo": {Nombre: "x.pdf", TipoMedio: "application/pdf", TamanoBytes: -5, ChecksumSha256: checksumValido},
	}
	for nombre, in := range casos {
		t.Run(nombre, func(t *testing.T) {
			if _, err := svc.CrearIntencionSubida(ctx, viajeID, in, "usuario-1"); err == nil {
				t.Fatal("se esperaba error de validación")
			}
		})
	}
	if pag, _ := svc.Listar(ctx, viajeID); len(pag.Items) != 0 {
		t.Errorf("no debió escribirse metadata: %d adjuntos", len(pag.Items))
	}
}

// TestDescargarAdjuntoExistente: descargar un adjunto existente devuelve la URL
// de descarga y deja un registro de auditoría de la descarga.
func TestDescargarAdjuntoExistente(t *testing.T) {
	ctx := context.Background()
	svc := prepara(t, ctx)
	viajeID := operclient.Id(uuid.New())

	res, err := svc.CrearIntencionSubida(ctx, viajeID, entrada(), "usuario-1")
	if err != nil {
		t.Fatalf("CrearIntencionSubida: %v", err)
	}

	desc, err := svc.Descargar(ctx, viajeID, res.AdjuntoId, "usuario-2")
	if err != nil {
		t.Fatalf("Descargar: %v", err)
	}
	if desc.UrlDescarga == "" || !desc.ExpiraEn.After(time.Now()) {
		t.Fatalf("descarga = %+v, se esperaba url y expiración futura", desc)
	}
	if n := contarAuditoria(t, ctx, viajeID.String(), "adjunto_descarga"); n != 1 {
		t.Errorf("auditoría de descarga = %d, se esperaba 1", n)
	}
}

// TestDescargarAdjuntoInexistente: descargar un adjunto que no existe (o de otro
// viaje) da 404 y no deja auditoría de descarga.
func TestDescargarAdjuntoInexistente(t *testing.T) {
	ctx := context.Background()
	svc := prepara(t, ctx)
	viajeID := operclient.Id(uuid.New())

	res, err := svc.CrearIntencionSubida(ctx, viajeID, entrada(), "usuario-1")
	if err != nil {
		t.Fatalf("CrearIntencionSubida: %v", err)
	}

	t.Run("adjunto inexistente", func(t *testing.T) {
		_, err := svc.Descargar(ctx, viajeID, operclient.Id(uuid.New()), "usuario-2")
		if got := platformerrors.AsCategory(err); got != platformerrors.CategoryNoEncontrado {
			t.Fatalf("categoría = %v, se esperaba NoEncontrado (err=%v)", got, err)
		}
	})
	t.Run("adjunto de otro viaje", func(t *testing.T) {
		otroViaje := operclient.Id(uuid.New())
		_, err := svc.Descargar(ctx, otroViaje, res.AdjuntoId, "usuario-2")
		if got := platformerrors.AsCategory(err); got != platformerrors.CategoryNoEncontrado {
			t.Fatalf("categoría = %v, se esperaba NoEncontrado: el adjunto no es de ese viaje (err=%v)", got, err)
		}
	})
	if n := contarAuditoria(t, ctx, viajeID.String(), "adjunto_descarga"); n != 0 {
		t.Errorf("no debió auditarse una descarga fallida: %d", n)
	}
}

// TestAdjuntoNoSeEditaNiBorra: op_bitacora solo tiene SELECT/INSERT sobre
// bitacora.adjunto (sin UPDATE ni DELETE): la evidencia no se edita ni se borra
// desde la app. Ambos intentos fallan por permisos.
func TestAdjuntoNoSeEditaNiBorra(t *testing.T) {
	ctx := context.Background()
	svc := prepara(t, ctx)
	viajeID := operclient.Id(uuid.New())
	res, err := svc.CrearIntencionSubida(ctx, viajeID, entrada(), "usuario-1")
	if err != nil {
		t.Fatalf("CrearIntencionSubida: %v", err)
	}

	pool, cerrar := dbtest.PoolAs(t, ctx, "op_bitacora", "bitacora")
	defer cerrar()
	if _, err := pool.Exec(ctx,
		"UPDATE bitacora.adjunto SET estado_escaneo='limpio' WHERE id=$1", res.AdjuntoId); err == nil {
		t.Error("UPDATE sobre el adjunto debió ser rechazado (sin grant de UPDATE en V1)")
	}
	if _, err := pool.Exec(ctx,
		"DELETE FROM bitacora.adjunto WHERE id=$1", res.AdjuntoId); err == nil {
		t.Error("DELETE sobre el adjunto debió ser rechazado")
	}
	if pag, _ := svc.Listar(ctx, viajeID); len(pag.Items) != 1 || pag.Items[0].EstadoEscaneo != operclient.Pendiente {
		t.Error("el adjunto debió quedar intacto en pendiente")
	}
}
