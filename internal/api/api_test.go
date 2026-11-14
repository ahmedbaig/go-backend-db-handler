package api_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/ahmedbaig/go-backend-db"
	"github.com/ahmedbaig/go-backend-db-handler/internal/api"
	"github.com/ahmedbaig/go-backend-db-handler/internal/appconfig"
	"github.com/ahmedbaig/go-backend-db-handler/internal/connections"
	"github.com/ahmedbaig/go-backend-db-handler/internal/engine"
	"github.com/ahmedbaig/go-backend-db-handler/internal/registry"
	"github.com/ahmedbaig/go-backend-db-handler/models"
	"github.com/ahmedbaig/go-backend-db-handler/models/demo"
	"github.com/ahmedbaig/go-backend-http"
	testkit "github.com/ahmedbaig/go-backend-testing"
)

type env struct {
	authors *db.Service[demo.Author]
	posts   *db.Service[demo.Post]
	client  *db.Client
	url     string
}

func setup(t *testing.T, keys ...string) env {
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

	reg := registry.New()
	models.Register(reg)
	opts, _ := testkit.AppOptions(t, "db-handler")
	app := httpx.New(opts)
	api.Register(app, api.Deps{Engine: engine.New(reg, pool, engine.Options{MaxTake: 50}), Registry: reg, Pool: pool, APIKeys: keys})
	srv := httptest.NewServer(app.Handler())
	t.Cleanup(srv.Close)

	var copts []db.ClientOption
	if len(keys) > 0 {
		copts = append(copts, db.WithHeader("X-Api-Key", keys[0]))
	}
	c := db.NewClient(srv.URL, copts...)
	return env{authors: db.NewService[demo.Author](c, "demo", "authors"), posts: db.NewService[demo.Post](c, "demo", "posts"), client: c, url: srv.URL}
}

func TestCRUDFlow(t *testing.T) {
	e := setup(t)
	ctx := context.Background()

	ada, err := e.authors.Create(ctx, demo.Author{Email: "ada@example.com", Name: "Ada"})
	testkit.NoError(t, err)
	testkit.True(t, ada.ID > 0, "id assigned")

	_, err = e.authors.Create(ctx, demo.Author{Email: "ada@example.com", Name: "Dup"})
	testkit.ErrorIs(t, err, db.ErrConflict)

	n, err := e.posts.CreateMany(ctx, []demo.Post{
		{AuthorID: ada.ID, Title: "100% real", Views: 5, Published: true},
		{AuthorID: ada.ID, Title: "Draft", Views: 1},
		{AuthorID: ada.ID, Title: "Hello_world", Views: 9, Published: true},
	})
	testkit.NoError(t, err)
	testkit.Equal(t, n, int64(3))

	list, err := e.posts.FindMany(ctx, db.Params{
		Where:   db.W().Eq("published", true).Field("views", db.Gte(5)),
		OrderBy: []db.Order{{Field: "views", Direction: db.Desc}},
		Take:    db.Take(10),
		Include: []string{"author"},
	})
	testkit.NoError(t, err)
	testkit.Equal(t, len(list), 2)
	testkit.Equal(t, list[0].Title, "Hello_world")
	testkit.True(t, list[0].Author != nil && list[0].Author.Name == "Ada", "author preloaded")

	pct, err := e.posts.FindMany(ctx, db.Params{Where: db.W().Field("title", db.Contains("%")), Take: db.Take(10)})
	testkit.NoError(t, err)
	testkit.Equal(t, len(pct), 1, "LIKE wildcard must be escaped")
	und, _ := e.posts.FindMany(ctx, db.Params{Where: db.W().Field("title", db.Contains("o_w")), Take: db.Take(10)})
	testkit.Equal(t, len(und), 1)

	cnt, err := e.posts.Count(ctx, db.OrOf(db.W().Eq("title", "Draft"), db.W().Field("views", db.Gt(8))))
	testkit.NoError(t, err)
	testkit.Equal(t, cnt, int64(2))

	_, err = e.posts.FindOne(ctx, db.W().Eq("authorId", ada.ID))
	testkit.ErrorIs(t, err, db.ErrConflict)
	_, err = e.posts.FindOne(ctx, db.W().Eq("id", 999))
	testkit.ErrorIs(t, err, db.ErrNotFound)

	upd, err := e.posts.Update(ctx, db.W().Eq("title", "Draft"), map[string]any{"published": true, "views": 2})
	testkit.NoError(t, err)
	testkit.True(t, upd.Published && upd.Views == 2, "updated")

	changed, err := e.posts.UpdateMany(ctx, db.W().Field("views", db.Lt(100)), map[string]any{"views": 0})
	testkit.NoError(t, err)
	testkit.Equal(t, changed, int64(3))

	up, err := e.authors.Upsert(ctx, []string{"email"}, demo.Author{Email: "ada@example.com", Name: "ignored"}, map[string]any{"name": "Ada L"})
	testkit.NoError(t, err)
	testkit.Equal(t, up.Name, "Ada L")
	testkit.Equal(t, up.ID, ada.ID)

	gone, err := e.posts.Delete(ctx, db.W().Eq("title", "Draft"))
	testkit.NoError(t, err)
	testkit.Equal(t, gone.Title, "Draft")

	removed, err := e.posts.DeleteMany(ctx, db.W().Eq("authorId", ada.ID))
	testkit.NoError(t, err)
	testkit.Equal(t, removed, int64(2))

	first, err := e.posts.FindFirst(ctx, db.Params{})
	testkit.NoError(t, err)
	testkit.Equal(t, first.ID, int64(0), "findFirst on empty table returns null")
}

func TestGuards(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	cases := map[string]db.Query{
		"write without where": {Database: "demo", Service: "posts", Method: db.DeleteMany},
		"unknown field":       {Database: "demo", Service: "posts", Method: db.Count, Params: db.Params{Where: db.W().Eq("secret", 1)}},
		"injection as column": {Database: "demo", Service: "posts", Method: db.Count, Params: db.Params{Where: db.W().Eq("id = 1 OR 1", 1)}},
		"bad value type":      {Database: "demo", Service: "posts", Method: db.Count, Params: db.Params{Where: db.W().Eq("views", "lots")}},
		"take over max":       {Database: "demo", Service: "posts", Method: db.FindMany, Params: db.Params{Take: db.Take(51)}},
		"unknown include":     {Database: "demo", Service: "posts", Method: db.FindMany, Params: db.Params{Take: db.Take(1), Include: []string{"comments"}}},
		"unknown order field": {Database: "demo", Service: "posts", Method: db.FindMany, Params: db.Params{Take: db.Take(1), OrderBy: []db.Order{{Field: "x"}}}},
		"unknown data field":  {Database: "demo", Service: "posts", Method: db.Create, Params: db.Params{Data: []byte(`{"title":"t","hack":1}`)}},
		"unknown method":      {Database: "demo", Service: "posts", Method: "truncate"},
	}
	for name, q := range cases {
		_, err := e.client.Query(ctx, q)
		if !errors.Is(err, db.ErrValidation) {
			t.Errorf("%s: want 400, got %v", name, err)
		}
	}
	_, err := e.client.Query(ctx, db.Query{Database: "demo", Service: "nope", Method: db.Count})
	testkit.ErrorIs(t, err, db.ErrNotFound)
}

func TestTransactionRollsBack(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	_, err := e.client.Transaction(ctx, db.Transaction{Database: "demo", Queries: []db.Query{
		{Service: "authors", Method: db.Create, Params: db.Params{Data: []byte(`{"email":"x@example.com","name":"X"}`)}},
		{Service: "authors", Method: db.Create, Params: db.Params{Data: []byte(`{"email":"x@example.com","name":"X again"}`)}},
	}})
	testkit.ErrorIs(t, err, db.ErrConflict)
	n, _ := e.authors.Count(ctx, db.Where{})
	testkit.Equal(t, n, int64(0), "first insert rolled back")

	res, err := e.client.Transaction(ctx, db.Transaction{Database: "demo", Queries: []db.Query{
		{Service: "authors", Method: db.Create, Params: db.Params{Data: []byte(`{"email":"y@example.com","name":"Y"}`)}},
		{Service: "authors", Method: db.Count},
	}})
	testkit.NoError(t, err)
	testkit.Equal(t, *res.Results[1].Count, int64(1))
}

func TestCapabilitiesRESTAndAPIKey(t *testing.T) {
	e := setup(t, "secret-key")
	ctx := context.Background()
	caps, err := e.client.Capabilities(ctx)
	testkit.NoError(t, err)
	testkit.Equal(t, caps.MaxTake, 50)
	testkit.Equal(t, caps.Databases[0].Name, "demo")
	testkit.Equal(t, caps.Databases[0].Driver, "sqlite")
	testkit.Equal(t, len(caps.Databases[0].Services), 2)

	req, _ := http.NewRequest("POST", e.url+"/api/v1/demo/authors/count", nil)
	req.Header.Set("X-Api-Key", "secret-key")
	res, err := http.DefaultClient.Do(req)
	testkit.NoError(t, err)
	res.Body.Close()
	testkit.Equal(t, res.StatusCode, 200)

	_, err = db.NewClient(e.url).Capabilities(ctx)
	var re *db.RemoteError
	testkit.True(t, errors.As(err, &re) && re.Status == 401, "missing key rejected")
}
