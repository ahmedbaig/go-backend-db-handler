package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"

	"github.com/ahmedbaig/go-backend-config"
	"github.com/ahmedbaig/go-backend-db-handler/internal/api"
	"github.com/ahmedbaig/go-backend-db-handler/internal/appconfig"
	"github.com/ahmedbaig/go-backend-db-handler/internal/connections"
	"github.com/ahmedbaig/go-backend-db-handler/internal/docs"
	"github.com/ahmedbaig/go-backend-db-handler/internal/engine"
	"github.com/ahmedbaig/go-backend-db-handler/internal/grpcapi"
	"github.com/ahmedbaig/go-backend-db-handler/internal/kv"
	"github.com/ahmedbaig/go-backend-db-handler/internal/registry"
	"github.com/ahmedbaig/go-backend-db-handler/models"
	"github.com/ahmedbaig/go-backend-http"
	"github.com/ahmedbaig/go-backend-logging"
	"github.com/ahmedbaig/go-backend-monitoring"
	dbv1 "github.com/ahmedbaig/go-backend-proto/gen/go/ahmedbaig/dbhandler/v1"
	"github.com/ahmedbaig/go-backend-proto/grpcx"
	"google.golang.org/grpc"
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

	kvPool, err := kv.Open(ctx, dbCfg.Redis, log.Logger)
	if err != nil {
		return err
	}
	docPool, err := docs.Open(ctx, dbCfg.Mongo, log.Logger)
	if err != nil {
		return err
	}
	if err := docPool.EnsureIndexes(ctx); err != nil {
		log.Warn("could not create analytics indexes", logging.Err(err))
	}

	prober := monitoring.NewProber(name, monitoring.WithVersion(version))
	for _, reg := range []func(*monitoring.Prober) error{pool.RegisterProbes, kvPool.RegisterProbes, docPool.RegisterProbes} {
		if err := reg(prober); err != nil {
			return err
		}
	}

	app := httpx.New(httpx.Options{
		Name:           name,
		Environment:    cfg.Environment,
		Logger:         log.Logger,
		Prober:         prober,
		RequestTimeout: cfg.RequestTimeout.Std(),
		MaxBodyBytes:   32 << 20,
	})
	deps := api.Deps{
		Engine:   engine.New(reg, pool, engine.Options{DefaultTake: cfg.DefaultTake, MaxTake: cfg.MaxTake}),
		Registry: reg,
		Pool:     pool,
		KV:       kvPool,
		Docs:     docPool,
		APIKeys:  cfg.APIKeys,
	}
	api.Register(app, deps)
	if cfg.GRPCPort > 0 {
		if err := serveGRPC(cfg, deps, log.Logger, app); err != nil {
			return err
		}
	}
	app.OnShutdown(func(context.Context) error { return pool.Close() })
	app.OnShutdown(func(context.Context) error { return kvPool.Close() })
	app.OnShutdown(func(context.Context) error { return docPool.Close() })
	return app.Run(ctx, cfg.Addr())
}

func serveGRPC(cfg appconfig.App, deps api.Deps, log *slog.Logger, app *httpx.App) error {
	lis, err := net.Listen("tcp", cfg.GRPCAddr())
	if err != nil {
		return err
	}
	srv := grpcx.NewServer(grpcx.ServerOptions{
		Name:          name,
		Logger:        log,
		ServiceTokens: cfg.APIKeys,
		Reflection:    cfg.Environment == "localhost",
		Extra:         []grpc.ServerOption{grpc.MaxRecvMsgSize(32 << 20), grpc.MaxSendMsgSize(64 << 20)},
	})
	dbv1.RegisterDBHandlerServiceServer(srv, grpcapi.New(deps))
	srv.MarkServing(dbv1.DBHandlerService_ServiceDesc.ServiceName)
	go func() {
		if err := srv.Serve(lis); err != nil {
			log.Error("grpc server stopped", logging.Err(err))
		}
	}()
	log.Info("grpc server started", slog.String("addr", lis.Addr().String()))
	app.OnShutdown(srv.Shutdown)
	return nil
}
