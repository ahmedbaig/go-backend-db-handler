package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/ahmedbaig/go-backend-config"
	"github.com/ahmedbaig/go-backend-db-handler/internal/api"
	"github.com/ahmedbaig/go-backend-db-handler/internal/appconfig"
	"github.com/ahmedbaig/go-backend-db-handler/internal/connections"
	"github.com/ahmedbaig/go-backend-db-handler/internal/engine"
	"github.com/ahmedbaig/go-backend-db-handler/internal/registry"
	"github.com/ahmedbaig/go-backend-db-handler/models"
	"github.com/ahmedbaig/go-backend-http"
	"github.com/ahmedbaig/go-backend-logging"
	"github.com/ahmedbaig/go-backend-monitoring"
)

const name = "db-handler"

var version = "dev"

func main() {
	if err := run(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	cfg, err := config.Load(name, appconfig.DefaultApp())
	if err != nil {
		return err
	}
	logCfg, err := config.Load(name+"-logger", logging.DefaultConfig())
	if err != nil {
		return err
	}
	dbCfg, err := config.Load(name+"-db", appconfig.DB{})
	if err != nil {
		return err
	}
	log, err := logging.New(logCfg, name)
	if err != nil {
		return err
	}
	defer log.Close()
	slog.SetDefault(log.Logger)

	reg := registry.New()
	models.Register(reg)

	pool, err := connections.Open(ctx, dbCfg, log.Logger)
	if err != nil {
		return err
	}

	prober := monitoring.NewProber(name, monitoring.WithVersion(version))
	if err := pool.RegisterProbes(prober); err != nil {
		return err
	}

	app := httpx.New(httpx.Options{
		Name:           name,
		Environment:    cfg.Environment,
		Logger:         log.Logger,
		Prober:         prober,
		RequestTimeout: cfg.RequestTimeout.Std(),
		MaxBodyBytes:   32 << 20,
	})
	api.Register(app, api.Deps{
		Engine:   engine.New(reg, pool, engine.Options{DefaultTake: cfg.DefaultTake, MaxTake: cfg.MaxTake}),
		Registry: reg,
		Pool:     pool,
		APIKeys:  cfg.APIKeys,
	})
	app.OnShutdown(func(context.Context) error { return pool.Close() })
	return app.Run(ctx, cfg.Addr())
}
