// Command api es el arranque standalone de operacion (invariante I5): compila y
// sirve /health y /ready SIN core, cableando el adaptador HTTP hacia catalogos.
// Producción no lo usa para operacion (vive dentro de core); existe para
// levantar y probar el módulo de forma aislada.
//
// Scaffolding: abre los cuatro pools por rol (si hay cadenas de conexión),
// construye el módulo con el adaptador HTTP de aptitud y sirve health/ready.
// Aún no monta rutas de negocio.
package main

import (
	"context"
	"log/slog"
	"os"

	catalogos "github.com/AI27-Tech/pf-pkg-contracts/packages/api-client-go/catalogos"
	"github.com/AI27-Tech/pf-pkg-platform-go/config"
	"github.com/AI27-Tech/pf-pkg-platform-go/observability"
	"github.com/AI27-Tech/pf-pkg-platform-go/pgxpool"
	"github.com/AI27-Tech/pf-pkg-platform-go/server"
	operacion "github.com/AI27-Tech/pf-svc-operacion"
	operhttp "github.com/AI27-Tech/pf-svc-operacion/adapters/http"
)

// Claves de entorno propias de operacion para las cadenas de conexión por rol y
// la URL de catalogos. Cada rol tiene su propia cadena (I1): nunca un pool
// compartido entre roles.
const (
	envDBViajes      = "DB_VIAJES_URL"
	envDBAsignacion  = "DB_ASIGNACION_URL"
	envDBBitacora    = "DB_BITACORA_URL"
	envDBAuditoria   = "DB_AUDITORIA_URL"
	envDBRoster      = "DB_ROSTER_URL"
	envCatalogosBase = "CATALOGOS_BASE_URL"
)

func main() {
	if err := run(); err != nil {
		slog.Error("operacion terminó con error", slog.Any("error", err))
		os.Exit(1)
	}
}

func run() error {
	ctx := context.Background()

	cfg, err := config.Load(envDBViajes, envDBAsignacion, envDBBitacora, envDBAuditoria, envDBRoster, envCatalogosBase)
	if err != nil {
		return err
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	logger.Info("arrancando operacion (standalone)", slog.String("service", cfg.ServiceName))

	shutdownOtel, err := observability.Init(ctx, observability.Config{
		ServiceName: cfg.ServiceName,
		ExporterURL: cfg.OtelExporterURL,
	})
	if err != nil {
		logger.Warn("observabilidad no inicializada", slog.Any("error", err))
	} else {
		defer func() { _ = shutdownOtel(context.Background()) }()
	}

	// Un pool por rol (I1); nunca compartido, aunque compartan transacción.
	viajes, err := pgxpool.New(ctx, "op_viajes", os.Getenv(envDBViajes))
	if err != nil {
		return err
	}
	defer viajes.Close()
	asignacion, err := pgxpool.New(ctx, "op_asignacion", os.Getenv(envDBAsignacion))
	if err != nil {
		return err
	}
	defer asignacion.Close()
	bitacora, err := pgxpool.New(ctx, "op_bitacora", os.Getenv(envDBBitacora))
	if err != nil {
		return err
	}
	defer bitacora.Close()
	auditoria, err := pgxpool.New(ctx, "op_auditoria", os.Getenv(envDBAuditoria))
	if err != nil {
		return err
	}
	defer auditoria.Close()
	roster, err := pgxpool.New(ctx, "op_roster", os.Getenv(envDBRoster))
	if err != nil {
		return err
	}
	defer roster.Close()

	// TODO(operacion): aplicar migrations.FS con cada rol dueño (viajes,
	// bitacora, auditoria primero; asignacion al final por el GRANT de excepción)
	// antes de servir tráfico. Pendiente del aplicador de migraciones acordado.

	// Adaptador HTTP hacia catalogos (standalone). En core se usa el en proceso.
	aptitud, err := operhttp.NewAptitudRecursosHTTP(os.Getenv(envCatalogosBase), (catalogos.HttpRequestDoer)(nil))
	if err != nil {
		return err
	}

	mod, err := operacion.New(operacion.Pools{
		Viajes:     viajes,
		Asignacion: asignacion,
		Bitacora:   bitacora,
		Auditoria:  auditoria,
		Roster:     roster,
	}, operacion.Deps{Aptitud: aptitud})
	if err != nil {
		return err
	}

	srv := server.New(cfg.HTTPAddr, server.WithLogger(logger))
	mod.Routes(srv.Mux())
	srv.Registry().RegisterCheck("db_viajes", viajes.CheckFunc())
	srv.Registry().RegisterCheck("db_asignacion", asignacion.CheckFunc())
	srv.Registry().RegisterCheck("db_bitacora", bitacora.CheckFunc())
	srv.Registry().RegisterCheck("db_auditoria", auditoria.CheckFunc())
	srv.Registry().RegisterCheck("db_roster", roster.CheckFunc())

	srv.MarkReady()
	return srv.Start(ctx)
}
