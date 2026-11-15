package grpcapi_test

import (
	"context"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/ahmedbaig/go-backend-db"
	"github.com/ahmedbaig/go-backend-db-handler/internal/api"
	"github.com/ahmedbaig/go-backend-db-handler/internal/appconfig"
	"github.com/ahmedbaig/go-backend-db-handler/internal/connections"
	"github.com/ahmedbaig/go-backend-db-handler/internal/engine"
	"github.com/ahmedbaig/go-backend-db-handler/internal/grpcapi"
	"github.com/ahmedbaig/go-backend-db-handler/internal/kv"
	"github.com/ahmedbaig/go-backend-db-handler/internal/registry"
	"github.com/ahmedbaig/go-backend-db-handler/models"
	"github.com/ahmedbaig/go-backend-db-handler/models/demo"
	"github.com/ahmedbaig/go-backend-db/dbgrpc"
	dbv1 "github.com/ahmedbaig/go-backend-proto/gen/go/ahmedbaig/dbhandler/v1"
	"github.com/ahmedbaig/go-backend-proto/grpcx"
	testkit "github.com/ahmedbaig/go-backend-testing"
	"github.com/alicebob/miniredis/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/test/bufconn"
)

func setup(t *testing.T, token string) *db.Client {
	t.Helper()
	ctx := context.Background()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	pool, err := connections.Open(ctx, appconfig.DB{Connections: []appconfig.Connection{
		{Database: "demo", Driver: "sqlite", Name: filepath.Join(t.TempDir(), "demo.db") + "?_pragma=foreign_keys(1)"},
	}}, quiet)
	testkit.NoError(t, err)
	t.Cleanup(func() { _ = pool.Close() })
	g, _ := pool.Get("demo", "")
	testkit.NoError(t, g.AutoMigrate(models.All()["demo"]...))
	mr := miniredis.RunT(t)
	kvp, err := kv.Open(ctx, []appconfig.RedisStore{{Name: "sessions", Addr: mr.Addr()}}, quiet)
	testkit.NoError(t, err)
	t.Cleanup(func() { _ = kvp.Close() })

	reg := registry.New()
	models.Register(reg)
	deps := api.Deps{Engine: engine.New(reg, pool, engine.Options{MaxTake: 50}), Registry: reg, Pool: pool, KV: kvp}

	lis := bufconn.Listen(1 << 20)
	srv := grpcx.NewServer(grpcx.ServerOptions{Logger: quiet, ServiceTokens: []string{"secret"}})
	dbv1.RegisterDBHandlerServiceServer(srv, grpcapi.New(deps))
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	c, conn, err := dbgrpc.Dial("passthrough:///bufnet", grpcx.ClientOptions{
		ServiceToken: token,
		Extra: []grpc.DialOption{grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		})},
	})
	testkit.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return c
}

func TestCRUDOverGRPC(t *testing.T) {
	c := setup(t, "secret")
	ctx := context.Background()
	authors := db.NewService[demo.Author](c, "demo", "authors")
	posts := db.NewService[demo.Post](c, "demo", "posts")

	ada, err := authors.Create(ctx, demo.Author{Email: "ada@example.com", Name: "Ada"})
	testkit.NoError(t, err)
	_, err = authors.Create(ctx, demo.Author{Email: "ada@example.com", Name: "Dup"})
	testkit.ErrorIs(t, err, db.ErrConflict)

	n, err := posts.CreateMany(ctx, []demo.Post{
		{AuthorID: ada.ID, Title: "One", Published: true},
		{AuthorID: ada.ID, Title: "Two"},
	})
	testkit.NoError(t, err)
	testkit.Equal(t, n, int64(2))

	published, err := posts.Count(ctx, db.W().Eq("published", true))
	testkit.NoError(t, err)
	testkit.Equal(t, published, int64(1))

	_, err = posts.FindOne(ctx, db.W().Eq("id", 999))
	testkit.ErrorIs(t, err, db.ErrNotFound)

	_, err = c.Query(ctx, db.Query{Database: "demo", Service: "posts", Method: "explode"})
	testkit.ErrorIs(t, err, db.ErrValidation)

	res, err := c.Transaction(ctx, db.Transaction{Database: "demo", Queries: []db.Query{
		{Database: "demo", Service: "posts", Method: db.Count},
		{Database: "demo", Service: "authors", Method: db.Count},
	}})
	testkit.NoError(t, err)
	testkit.Equal(t, len(res.Results), 2)

	caps, err := c.Capabilities(ctx)
	testkit.NoError(t, err)
	testkit.Equal(t, caps.Databases[0].Name, "demo")
}

func TestKVOverGRPC(t *testing.T) {
	c := setup(t, "secret")
	ctx := context.Background()
	s := c.KV("sessions")
	testkit.NoError(t, s.Set(ctx, "sid-1", map[string]any{"user": 7}, time.Minute))
	v, found, err := s.Get(ctx, "sid-1")
	testkit.NoError(t, err)
	testkit.True(t, found, "found")
	testkit.Equal(t, string(v), `{"user":7}`)
}

func TestWrongTokenRejected(t *testing.T) {
	c := setup(t, "nope")
	_, err := db.NewService[demo.Post](c, "demo", "posts").Count(context.Background(), db.Where{})
	var re *db.RemoteError
	testkit.True(t, errorsAs(err, &re) && re.Status == 401, "401")
}

func errorsAs(err error, target **db.RemoteError) bool {
	re, ok := err.(*db.RemoteError)
	if ok {
		*target = re
	}
	return ok
}
