package solicitudes

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/estados"
	eventosv1 "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/eventos/ai27/eventos/v1"
	operclient "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/operacion"
	platformerrors "github.com/AI27-Tech/pf-pkg-platform-go/errors"
	"github.com/AI27-Tech/pf-pkg-platform-go/outbox"
	"github.com/AI27-Tech/pf-svc-operacion/internal/dbtest"
	"github.com/AI27-Tech/pf-svc-operacion/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/encoding/protojson"
)

// testCtx abrevia el contexto de las pruebas.
type testCtx = context.Context

// adminConn abre una conexión de administración.
func adminConn(t *testing.T, ctx context.Context) (*pgx.Conn, func()) {
	t.Helper()
	return dbtest.AdminConn(t, ctx)
}

func ptr[T any](v T) *T { return &v }

// prepara aplica las migraciones del esquema solicitudes (idempotentes), limpia
// los datos y devuelve el servicio sobre un pool del rol op_solicitudes con
// search_path=solicitudes, como en producción.
func prepara(t *testing.T) (context.Context, *Servicio) {
	t.Helper()
	ctx := context.Background()
	dbtest.Serializar(t, ctx) // no correr a la vez que otros paquetes (misma base)
	admin, cerrar := dbtest.AdminConn(t, ctx)
	defer cerrar()
	archivos, err := migrations.ArchivosDe("solicitudes")
	if err != nil {
		t.Fatalf("listar migraciones solicitudes: %v", err)
	}
	for _, f := range archivos {
		sql, err := migrations.FS.ReadFile(f)
		if err != nil {
			t.Fatalf("leer %s: %v", f, err)
		}
		dbtest.Exec(t, ctx, admin, string(sql))
	}
	dbtest.Exec(t, ctx, admin, "TRUNCATE "+tablasDelEsquema(t, ctx, admin)+" RESTART IDENTITY CASCADE")
	pool, cerrarPool := dbtest.PoolAs(t, ctx, "op_solicitudes", "solicitudes")
	t.Cleanup(cerrarPool)
	return ctx, NewServicio(pool, outbox.NewPublisher())
}

func tablasDelEsquema(t *testing.T, ctx context.Context, c *pgx.Conn) string {
	t.Helper()
	rows, err := c.Query(ctx, `SELECT 'solicitudes.' || tablename FROM pg_tables WHERE schemaname = 'solicitudes'`)
	if err != nil {
		t.Fatalf("listar tablas: %v", err)
	}
	defer rows.Close()
	var ts []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		ts = append(ts, n)
	}
	return strings.Join(ts, ", ")
}

// admin ejecuta una consulta de un valor como administrador.
func consulta[T any](t *testing.T, ctx context.Context, sql string, args ...any) T {
	t.Helper()
	c, cerrar := dbtest.AdminConn(t, ctx)
	defer cerrar()
	var v T
	if err := c.QueryRow(ctx, sql, args...).Scan(&v); err != nil {
		t.Fatalf("consulta %q: %v", sql, err)
	}
	return v
}

func ejecuta(t *testing.T, ctx context.Context, sql string, args ...any) {
	t.Helper()
	c, cerrar := dbtest.AdminConn(t, ctx)
	defer cerrar()
	if _, err := c.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("ejecutar %q: %v", sql, err)
	}
}

// sembrar inserta una solicitud directamente en el estado dado (como admin: la
// prueba arma el punto de partida sin pasar por la máquina).
func sembrar(t *testing.T, ctx context.Context, estado estados.EstadoSolicitud) (uuid.UUID, uuid.UUID) {
	t.Helper()
	id, cliente := uuid.New(), uuid.New()
	ejecuta(t, ctx,
		`INSERT INTO solicitudes.solicitud (id, cliente_id, estado, marca_penalizacion, origen, destinos, creado_en)
		 VALUES ($1,$2,$3,$4,'Origen','[{"nombre":"Destino"}]'::jsonb, now())`, id, cliente, string(estado), false)
	return id, cliente
}

func actorCliente(cliente uuid.UUID) Actor {
	return Actor{Subject: "cli-sub", Rol: "cliente", TenantID: cliente.String()}
}

// correr ejecuta fn en una transacción: commit si no hay error, rollback si lo hay.
func correr(ctx context.Context, s *Servicio, fn func(pgx.Tx) error) error {
	tx, err := s.tx.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// transicionarId carga la solicitud bloqueada y la transiciona, como un comando.
func transicionarId(ctx context.Context, s *Servicio, id uuid.UUID, c Cambio) error {
	return correr(ctx, s, func(tx pgx.Tx) error {
		f, err := cargar(ctx, tx, id, true)
		if err != nil {
			return err
		}
		return s.transicionar(ctx, tx, f, c)
	})
}

func codigoDe(err error) string { return platformerrors.CodigoDe(err) }

func categoriaDe(err error) platformerrors.Category { return platformerrors.AsCategory(err) }

// cambioValido arma el Cambio correcto de una arista (disparador y motivo).
func cambioValido(de, a estados.EstadoSolicitud) Cambio {
	r := aristas[arista{de, a}]
	return Cambio{A: a, Disparador: r.disparador, Actor: Actor{Subject: "actor-1", Rol: string(r.disparador)}, Motivo: "porque sí"}
}

// TestTransicionarGrafoCompleto recorre los 64 pares de estados contra Postgres.
// Cada arista del ADR 38 se recorre y deja su fila de histórico y su evento; cada
// par que no es arista se rechaza con transicion_no_permitida sin tocar nada.
func TestTransicionarGrafoCompleto(t *testing.T) {
	ctx, svc := prepara(t)
	for _, de := range estados.EstadosSolicitud {
		for _, a := range estados.EstadosSolicitud {
			t.Run(fmt.Sprintf("%s_a_%s", de, a), func(t *testing.T) {
				id, _ := sembrar(t, ctx, de)
				if !esAristaADR38(de, a) {
					err := transicionarId(ctx, svc, id, Cambio{A: a, Disparador: DisparadorCotizacion, Actor: Actor{Subject: "x"}})
					if codigoDe(err) != "transicion_no_permitida" || categoriaDe(err) != platformerrors.CategoryConflicto {
						t.Fatalf("esperaba 409 transicion_no_permitida, obtuve %v", err)
					}
					if got := consulta[string](t, ctx, `SELECT estado FROM solicitudes.solicitud WHERE id=$1`, id); got != string(de) {
						t.Errorf("el estado cambió a %s tras un rechazo", got)
					}
					if n := consulta[int](t, ctx, `SELECT count(*) FROM solicitudes.transicion_solicitud WHERE solicitud_id=$1`, id); n != 0 {
						t.Errorf("transicion_solicitud: %d filas tras un rechazo", n)
					}
					if n := consulta[int](t, ctx, `SELECT count(*) FROM solicitudes.outbox WHERE agregado_id=$1`, id.String()); n != 0 {
						t.Errorf("outbox: %d eventos tras un rechazo", n)
					}
					return
				}
				if err := transicionarId(ctx, svc, id, cambioValido(de, a)); err != nil {
					t.Fatalf("la arista del ADR 38 se rechazó: %v", err)
				}
				if got := consulta[string](t, ctx, `SELECT estado FROM solicitudes.solicitud WHERE id=$1`, id); got != string(a) {
					t.Errorf("estado = %s, quería %s", got, a)
				}
				wantMarca := de == estados.EstadoSolicitudPagado && a == estados.EstadoSolicitudCanceladoPorCliente
				if got := consulta[bool](t, ctx, `SELECT marca_penalizacion FROM solicitudes.solicitud WHERE id=$1`, id); got != wantMarca {
					t.Errorf("marca_penalizacion = %v, quería %v", got, wantMarca)
				}
				var accion, disp string
				var mDe, mA string
				c, cerrar := dbtest.AdminConn(t, ctx)
				defer cerrar()
				if err := c.QueryRow(ctx, `SELECT de_estado, a_estado, accion, disparador FROM solicitudes.transicion_solicitud WHERE solicitud_id=$1`, id).
					Scan(&mDe, &mA, &accion, &disp); err != nil {
					t.Fatalf("fila de histórico: %v", err)
				}
				r := aristas[arista{de, a}]
				if mDe != string(de) || mA != string(a) || accion != r.accion || disp != string(r.disparador) {
					t.Errorf("histórico = %s %s %s %s", mDe, mA, accion, disp)
				}
				// El evento es el mensaje generado del contrato, decodificado en modo estricto.
				payload := consulta[[]byte](t, ctx, `SELECT payload FROM solicitudes.outbox WHERE agregado_id=$1 AND tipo=$2`, id.String(), TipoSolicitudEstadoCambiado)
				var ev eventosv1.SolicitudEstadoCambiado
				if err := (protojson.UnmarshalOptions{}).Unmarshal(payload, &ev); err != nil {
					t.Fatalf("payload fuera del contrato: %v (%s)", err, payload)
				}
				if ev.SolicitudId != id.String() || ev.DeEstado != string(de) || ev.AEstado != string(a) || ev.CambiadoEn == nil {
					t.Errorf("evento = %+v", &ev)
				}
				if strings.Contains(string(payload), "marca") || strings.Contains(string(payload), "monto") {
					t.Errorf("el payload lleva datos que el contrato excluye: %s", payload)
				}
			})
		}
	}
}

// TestTransicionarRechazos: disparador equivocado y guarda de motivo.
func TestTransicionarRechazos(t *testing.T) {
	ctx, svc := prepara(t)

	t.Run("disparador distinto al de la arista", func(t *testing.T) {
		id, _ := sembrar(t, ctx, estados.EstadoSolicitudPagado)
		err := transicionarId(ctx, svc, id, Cambio{A: estados.EstadoSolicitudAprobado, Disparador: DisparadorCliente})
		if categoriaDe(err) != platformerrors.CategoryProhibido {
			t.Fatalf("esperaba 403, obtuve %v", err)
		}
		if got := consulta[string](t, ctx, `SELECT estado FROM solicitudes.solicitud WHERE id=$1`, id); got != "PAGADO" {
			t.Errorf("estado = %s", got)
		}
	})
	for _, c := range []struct{ de, a estados.EstadoSolicitud }{
		{estados.EstadoSolicitudPagado, estados.EstadoSolicitudCotizado},
		{estados.EstadoSolicitudPagado, estados.EstadoSolicitudRechazadoPorAi27},
	} {
		t.Run("motivo obligatorio "+string(c.a), func(t *testing.T) {
			for _, motivo := range []string{"", "   "} {
				id, _ := sembrar(t, ctx, c.de)
				in := cambioValido(c.de, c.a)
				in.Motivo = motivo
				err := transicionarId(ctx, svc, id, in)
				if categoriaDe(err) != platformerrors.CategoryValidacion {
					t.Fatalf("motivo %q: esperaba 400, obtuve %v", motivo, err)
				}
				if got := consulta[string](t, ctx, `SELECT estado FROM solicitudes.solicitud WHERE id=$1`, id); got != string(c.de) {
					t.Errorf("estado = %s tras rechazo", got)
				}
			}
		})
	}
	t.Run("solicitud inexistente", func(t *testing.T) {
		err := transicionarId(ctx, svc, uuid.New(), cambioValido(estados.EstadoSolicitudSolicitado, estados.EstadoSolicitudCotizado))
		if categoriaDe(err) != platformerrors.CategoryNoEncontrado {
			t.Fatalf("esperaba 404, obtuve %v", err)
		}
	})
}

// TestEstadoFueraDelEnumImposible: la base rechaza un estado fuera del enum y una
// marca de penalización fuera de CANCELADO_POR_CLIENTE, aunque la app fallara.
func TestEstadoFueraDelEnumImposible(t *testing.T) {
	ctx, _ := prepara(t)
	c, cerrar := dbtest.AdminConn(t, ctx)
	defer cerrar()
	ins := `INSERT INTO solicitudes.solicitud (id, cliente_id, estado, marca_penalizacion, origen, destinos, creado_en)
	        VALUES (gen_random_uuid(), gen_random_uuid(), $1, $2, 'O', '[{"nombre":"D"}]'::jsonb, now())`
	for _, estado := range []string{"EN_TRANSITO", "SOLICITADA", ""} {
		if _, err := c.Exec(ctx, ins, estado, false); err == nil || !strings.Contains(err.Error(), "estado_token_valido") {
			t.Errorf("estado %q debía violar estado_token_valido: %v", estado, err)
		}
	}
	if _, err := c.Exec(ctx, ins, "PAGADO", true); err == nil || !strings.Contains(err.Error(), "marca_solo_en_cancelacion") {
		t.Errorf("marca fuera de la cancelación debía violar su CHECK: %v", err)
	}
	// Cada estado del contrato SÍ se acepta: el CHECK coincide con el enum generado.
	for _, e := range estados.EstadosSolicitud {
		if _, err := c.Exec(ctx, ins, string(e), false); err != nil {
			t.Errorf("el estado %s del contrato fue rechazado por la base: %v", e, err)
		}
	}
}

// TestAtomicidadTransicionHistorialEvento: una falla en el histórico o en el
// outbox revierte también el cambio de estado.
func TestAtomicidadTransicionHistorialEvento(t *testing.T) {
	ctx, svc := prepara(t)
	for _, tabla := range []string{"transicion_solicitud", "outbox"} {
		t.Run("falla en "+tabla, func(t *testing.T) {
			ejecuta(t, ctx, fmt.Sprintf(`CREATE OR REPLACE FUNCTION solicitudes.falla() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'falla inyectada'; END $$;
				CREATE TRIGGER falla_prueba BEFORE INSERT ON solicitudes.%s FOR EACH ROW EXECUTE FUNCTION solicitudes.falla()`, tabla))
			t.Cleanup(func() {
				ejecuta(t, ctx, fmt.Sprintf(`DROP TRIGGER IF EXISTS falla_prueba ON solicitudes.%s; DROP FUNCTION IF EXISTS solicitudes.falla()`, tabla))
			})
			id, _ := sembrar(t, ctx, estados.EstadoSolicitudSolicitado)
			err := transicionarId(ctx, svc, id, cambioValido(estados.EstadoSolicitudSolicitado, estados.EstadoSolicitudCotizado))
			if err == nil || !strings.Contains(err.Error(), "falla inyectada") {
				t.Fatalf("esperaba la falla inyectada, obtuve %v", err)
			}
			if got := consulta[string](t, ctx, `SELECT estado FROM solicitudes.solicitud WHERE id=$1`, id); got != "SOLICITADO" {
				t.Errorf("estado = %s: el cambio sobrevivió a la falla", got)
			}
			if n := consulta[int](t, ctx, `SELECT count(*) FROM solicitudes.transicion_solicitud WHERE solicitud_id=$1`, id); n != 0 {
				t.Errorf("histórico: %d filas", n)
			}
			if n := consulta[int](t, ctx, `SELECT count(*) FROM solicitudes.outbox WHERE agregado_id=$1`, id.String()); n != 0 {
				t.Errorf("outbox: %d eventos", n)
			}
		})
	}
}

// TestPrivilegiosDeOpSolicitudes: el rol solo puede lo que el módulo necesita.
func TestPrivilegiosDeOpSolicitudes(t *testing.T) {
	ctx, _ := prepara(t)
	id, _ := sembrar(t, ctx, estados.EstadoSolicitudSolicitado)
	pool, cerrar := dbtest.PoolAs(t, ctx, "op_solicitudes", "solicitudes")
	defer cerrar()
	deniega := func(sql string) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, id); err == nil || !strings.Contains(err.Error(), "permission denied") {
			t.Errorf("%q debía denegarse: %v", sql, err)
		}
	}
	deniega(`DELETE FROM solicitudes.solicitud WHERE id = $1`)
	deniega(`UPDATE solicitudes.solicitud SET cliente_id = gen_random_uuid() WHERE id = $1`)
	deniega(`UPDATE solicitudes.solicitud SET origen = 'otro' WHERE id = $1`)
	deniega(`UPDATE solicitudes.transicion_solicitud SET motivo = 'x' WHERE solicitud_id = $1`)
	deniega(`DELETE FROM solicitudes.transicion_solicitud WHERE solicitud_id = $1`)
	deniega(`DELETE FROM solicitudes.outbox WHERE agregado_id = $1::text`)
	if _, err := pool.Exec(ctx, `UPDATE solicitudes.solicitud SET fecha_hora_servicio = now() WHERE id = $1`, id); err != nil {
		t.Errorf("fecha_hora_servicio sí se puede actualizar: %v", err)
	}
}

// TestCrear cubre E1-HU-001 / E10-T3.
func TestCrear(t *testing.T) {
	ctx, svc := prepara(t)
	cliente := uuid.New()
	a := actorCliente(cliente)
	base := func() operclient.CrearSolicitudEntrada {
		return operclient.CrearSolicitudEntrada{Origen: "CDMX", Destinos: []operclient.Destino{{Nombre: "Guadalajara"}}}
	}

	t.Run("sin fecha, con el tenant del token y evento por outbox", func(t *testing.T) {
		antes := time.Now()
		s, err := svc.Crear(ctx, a, base())
		if err != nil {
			t.Fatalf("Crear: %v", err)
		}
		if s.Estado != operclient.EstadoSolicitud(estados.EstadoSolicitudSolicitado) || s.FechaHoraServicio != nil ||
			s.ClienteId == nil || *s.ClienteId != cliente || s.MarcaPenalizacion || s.Avisos == nil {
			t.Fatalf("solicitud = %+v", s)
		}
		if s.CreadoEn.Before(antes.Add(-time.Second)) {
			t.Errorf("creado_en = %v", s.CreadoEn)
		}
		payload := consulta[[]byte](t, ctx, `SELECT payload FROM solicitudes.outbox WHERE agregado_id=$1 AND tipo='solicitud.creada'`, s.Id.String())
		var ev eventosv1.SolicitudCreada
		if err := (protojson.UnmarshalOptions{}).Unmarshal(payload, &ev); err != nil {
			t.Fatalf("payload fuera del contrato: %v (%s)", err, payload)
		}
		if ev.SolicitudId != s.Id.String() || ev.ClienteId != cliente.String() || ev.CreadaEn == nil {
			t.Errorf("evento = %+v", &ev)
		}
		if n := consulta[int](t, ctx, `SELECT count(*) FROM solicitudes.outbox WHERE agregado_id=$1`, s.Id.String()); n != 1 {
			t.Errorf("eventos = %d, quería 1", n)
		}
	})
	t.Run("con fecha, se guarda tal cual", func(t *testing.T) {
		in := base()
		f := time.Date(2026, 12, 1, 15, 30, 0, 0, time.UTC)
		in.FechaHoraServicio, in.TipoServicio = &f, ptr("escolta")
		in.Tracto = &operclient.Tracto{Placas: ptr("ABC-123")}
		s, err := svc.Crear(ctx, a, in)
		if err != nil {
			t.Fatal(err)
		}
		if s.FechaHoraServicio == nil || !s.FechaHoraServicio.Equal(f) || *s.TipoServicio != "escolta" || *s.Tracto.Placas != "ABC-123" {
			t.Errorf("solicitud = %+v", s)
		}
	})
	rechazos := map[string]struct {
		mod func(*operclient.CrearSolicitudEntrada)
		act Actor
		cat platformerrors.Category
	}{
		"sin origen":         {func(in *operclient.CrearSolicitudEntrada) { in.Origen = "  " }, a, platformerrors.CategoryValidacion},
		"sin destinos":       {func(in *operclient.CrearSolicitudEntrada) { in.Destinos = nil }, a, platformerrors.CategoryValidacion},
		"destino sin nombre": {func(in *operclient.CrearSolicitudEntrada) { in.Destinos[0].Nombre = "" }, a, platformerrors.CategoryValidacion},
		"sin tenant":         {func(*operclient.CrearSolicitudEntrada) {}, Actor{Rol: "cliente"}, platformerrors.CategoryProhibido},
		"tenant no es uuid":  {func(*operclient.CrearSolicitudEntrada) {}, Actor{Rol: "cliente", TenantID: "acme"}, platformerrors.CategoryProhibido},
	}
	for nombre, c := range rechazos {
		t.Run(nombre, func(t *testing.T) {
			antes := consulta[int](t, ctx, `SELECT count(*) FROM solicitudes.solicitud`)
			in := base()
			c.mod(&in)
			if _, err := svc.Crear(ctx, c.act, in); categoriaDe(err) != c.cat {
				t.Fatalf("esperaba %v, obtuve %v", c.cat, err)
			}
			if n := consulta[int](t, ctx, `SELECT count(*) FROM solicitudes.solicitud`); n != antes {
				t.Errorf("se creó una solicitud pese al rechazo")
			}
		})
	}
	t.Run("atomicidad: sin evento no hay solicitud", func(t *testing.T) {
		ejecuta(t, ctx, `CREATE OR REPLACE FUNCTION solicitudes.falla() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'falla inyectada'; END $$;
			CREATE TRIGGER falla_prueba BEFORE INSERT ON solicitudes.outbox FOR EACH ROW EXECUTE FUNCTION solicitudes.falla()`)
		defer ejecuta(t, ctx, `DROP TRIGGER IF EXISTS falla_prueba ON solicitudes.outbox; DROP FUNCTION IF EXISTS solicitudes.falla()`)
		antes := consulta[int](t, ctx, `SELECT count(*) FROM solicitudes.solicitud`)
		if _, err := svc.Crear(ctx, a, base()); err == nil {
			t.Fatal("esperaba error")
		}
		if n := consulta[int](t, ctx, `SELECT count(*) FROM solicitudes.solicitud`); n != antes {
			t.Errorf("la solicitud sobrevivió al fallo del evento")
		}
	})
}
