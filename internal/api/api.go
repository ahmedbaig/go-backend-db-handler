package api

import (
	"context"
	"crypto/subtle"
	"net/http"
	"time"

	"github.com/ahmedbaig/go-backend-db"
	"github.com/ahmedbaig/go-backend-db-handler/internal/connections"
	"github.com/ahmedbaig/go-backend-db-handler/internal/docs"
	"github.com/ahmedbaig/go-backend-db-handler/internal/engine"
	"github.com/ahmedbaig/go-backend-db-handler/internal/kv"
	"github.com/ahmedbaig/go-backend-db-handler/internal/registry"
	"github.com/ahmedbaig/go-backend-http"
)

type Deps struct {
	Engine   *engine.Engine
	Registry *registry.Registry
	Pool     *connections.Pool
	KV       *kv.Pool
	Docs     *docs.Pool
	APIKeys  []string
}

func Register(app *httpx.App, d Deps) {
	v1 := app.Group("/api/v1", apiKey(d.APIKeys))

	v1.Post("/query", httpx.Handle(func(ctx context.Context, q db.Query) (db.Result, error) {
		return d.Engine.Execute(ctx, q)
	}, httpx.MaxBodyBytes(32<<20)))

	v1.Post("/transaction", httpx.Handle(func(ctx context.Context, t db.Transaction) (db.TransactionResult, error) {
		return d.Engine.Transaction(ctx, t)
	}, httpx.MaxBodyBytes(32<<20)))

	v1.Post("/{database}/{service}/{method}", httpx.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
		p, err := httpx.Bind[db.Params](r, httpx.MaxBodyBytes(32<<20))
		if err != nil {
			return err
		}
		res, err := d.Engine.Execute(r.Context(), db.Query{
			Database: r.PathValue("database"),
			Instance: r.URL.Query().Get("instance"),
			Service:  r.PathValue("service"),
			Method:   db.Method(r.PathValue("method")),
			Params:   p,
		})
		if err != nil {
			return err
		}
		httpx.OK(w, res)
		return nil
	}))

	if d.KV != nil {
		v1.Post("/kv/{store}/{op}", httpx.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
			req, err := httpx.Bind[db.KVRequest](r, httpx.MaxBodyBytes(2<<20))
			if err != nil {
				return err
			}
			res, err := d.KV.Execute(r.Context(), r.PathValue("store"), db.KVOp(r.PathValue("op")), req)
			if err != nil {
				return err
			}
			httpx.OK(w, res)
			return nil
		}))
	}

	if d.Docs != nil {
		v1.Post("/docs/{store}/{collection}/{op}", httpx.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
			req, err := httpx.Bind[db.DocsRequest](r, httpx.MaxBodyBytes(16<<20))
			if err != nil {
				return err
			}
			res, err := d.Docs.Execute(r.Context(), r.PathValue("store"), r.PathValue("collection"), db.DocsOp(r.PathValue("op")), req)
			if err != nil {
				return err
			}
			httpx.OK(w, res)
			return nil
		}))
		v1.Post("/analytics/{store}/track", httpx.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
			req, err := httpx.Bind[db.TrackRequest](r, httpx.MaxBodyBytes(4<<20))
			if err != nil {
				return err
			}
			res, err := d.Docs.Track(r.Context(), r.PathValue("store"), req.Events, httpx.RequestID(r.Context()), time.Now())
			if err != nil {
				return err
			}
			httpx.JSON(w, http.StatusAccepted, res)
			return nil
		}))
		v1.Get("/analytics/{store}/dau", httpx.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
			req, err := httpx.Bind[db.DAURequest](r, httpx.NoBody())
			if err != nil {
				return err
			}
			res, err := d.Docs.DAU(r.Context(), r.PathValue("store"), req.From, req.To)
			if err != nil {
				return err
			}
			httpx.OK(w, res)
			return nil
		}))
	}

	v1.Get("/capabilities", httpx.HandlerFunc(func(w http.ResponseWriter, r *http.Request) error {
		httpx.OK(w, Capabilities(d))
		return nil
	}))
}

func Capabilities(d Deps) db.Capabilities {
	c := db.Capabilities{Methods: db.Methods, Operators: db.Operators, MaxTake: d.Engine.MaxTake()}
	for _, name := range d.Registry.Databases() {
		info := db.DatabaseInfo{Name: name}
		if d.Pool != nil {
			info.Driver = d.Pool.Driver(name)
			info.Instances = d.Pool.Instances(name)
		}
		for _, m := range d.Registry.Services(name) {
			info.Services = append(info.Services, m.Info())
		}
		c.Databases = append(c.Databases, info)
	}
	if d.KV != nil {
		c.Stores = append(c.Stores, d.KV.Stores()...)
		c.KVOps = db.KVOps
	}
	if d.Docs != nil {
		c.Stores = append(c.Stores, d.Docs.Stores()...)
		c.DocsOps = db.DocsOps
	}
	return c
}

func apiKey(keys []string) httpx.Middleware {
	return func(next http.Handler) http.Handler {
		if len(keys) == 0 {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			got := []byte(r.Header.Get("X-Api-Key"))
			for _, k := range keys {
				if subtle.ConstantTimeCompare(got, []byte(k)) == 1 {
					next.ServeHTTP(w, r)
					return
				}
			}
			httpx.WriteError(w, r, httpx.E(httpx.CodeUnauthenticated))
		})
	}
}
