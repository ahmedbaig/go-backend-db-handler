package api_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/ahmedbaig/go-backend-db"
	"github.com/ahmedbaig/go-backend-db-handler/internal/api"
	"github.com/ahmedbaig/go-backend-db-handler/internal/appconfig"
	"github.com/ahmedbaig/go-backend-db-handler/internal/docs"
	"github.com/ahmedbaig/go-backend-db-handler/internal/engine"
	"github.com/ahmedbaig/go-backend-db-handler/internal/kv"
	"github.com/ahmedbaig/go-backend-db-handler/internal/registry"
	"github.com/ahmedbaig/go-backend-http"
	testkit "github.com/ahmedbaig/go-backend-testing"
	"github.com/alicebob/miniredis/v2"
)

func storeServer(t *testing.T, kvp *kv.Pool, dp *docs.Pool) *db.Client {
	t.Helper()
	reg := registry.New()
	opts, _ := testkit.AppOptions(t, "db-handler")
	app := httpx.New(opts)
	api.Register(app, api.Deps{Engine: engine.New(reg, nil, engine.Options{}), Registry: reg, KV: kvp, Docs: dp})
	srv := httptest.NewServer(app.Handler())
	t.Cleanup(srv.Close)
	return db.NewClient(srv.URL)
}

type session struct {
	UserID int64  `json:"userId"`
	Role   string `json:"role"`
}

func TestRedisKVAndSessions(t *testing.T) {
	mr := miniredis.RunT(t)
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	kvp, err := kv.Open(context.Background(), []appconfig.RedisStore{{Name: "sessions", Addr: mr.Addr(), Prefix: "app:"}}, quiet)
	testkit.NoError(t, err)
	t.Cleanup(func() { _ = kvp.Close() })
	c := storeServer(t, kvp, nil)
	ctx := context.Background()
	k := c.KV("sessions")

	testkit.NoError(t, k.Set(ctx, "greeting", map[string]string{"hi": "there"}, time.Minute))
	testkit.True(t, mr.Exists("app:greeting"), "prefix applied")
	v, found, err := db.GetJSON[map[string]string](ctx, k, "greeting")
	testkit.NoError(t, err)
	testkit.True(t, found && v["hi"] == "there", "get json")

	_, found, err = k.Get(ctx, "missing")
	testkit.NoError(t, err)
	testkit.True(t, !found, "missing key")

	ok, err := k.SetNX(ctx, "greeting", "again", 0)
	testkit.NoError(t, err)
	testkit.True(t, !ok, "setnx on existing key")

	n, err := k.Incr(ctx, "counter", 5, time.Hour)
	testkit.NoError(t, err)
	testkit.Equal(t, n, int64(5))
	n, _ = k.Incr(ctx, "counter", 0, 0)
	testkit.Equal(t, n, int64(6))

	vals, err := k.MGet(ctx, "greeting", "nope", "counter")
	testkit.NoError(t, err)
	testkit.Equal(t, string(vals[1]), "null")
	testkit.Equal(t, string(vals[2]), "6")

	d, found, err := k.TTL(ctx, "counter")
	testkit.NoError(t, err)
	testkit.True(t, found && d > 59*time.Minute, "ttl "+d.String())

	removed, err := k.Del(ctx, "counter", "greeting", "nope")
	testkit.NoError(t, err)
	testkit.Equal(t, removed, int64(2))

	s := db.NewSessions[session](k, "sess:", 30*time.Minute)
	id, err := s.Create(ctx, session{UserID: 7, Role: "admin"})
	testkit.NoError(t, err)
	testkit.True(t, len(id) >= 40, "random session id")
	mr.FastForward(20 * time.Minute)
	got, found, err := s.Get(ctx, id)
	testkit.NoError(t, err)
	testkit.True(t, found && got.UserID == 7, "session read")
	testkit.True(t, mr.TTL("app:sess:"+id) == 30*time.Minute, "sliding expiry refreshed")
	mr.FastForward(31 * time.Minute)
	_, found, _ = s.Get(ctx, id)
	testkit.True(t, !found, "session expired")

	_, err = k.Publish(ctx, "events", map[string]int{"n": 1})
	testkit.NoError(t, err)

	_, err = c.KV("nope").Del(ctx, "x")
	testkit.ErrorIs(t, err, db.ErrNotFound)
	_, _, err = k.Get(ctx, "")
	testkit.ErrorIs(t, err, db.ErrValidation)
}

func TestMongoValidationWithoutServer(t *testing.T) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	dp, err := docs.Open(context.Background(), []appconfig.MongoStore{{Name: "analytics", URI: "mongodb://127.0.0.1:1", Database: "x", Collections: []string{"events"}, Timeout: 0}}, quiet)
	testkit.NoError(t, err)
	t.Cleanup(func() { _ = dp.Close() })
	c := storeServer(t, nil, dp)
	ctx := context.Background()
	d := c.Docs("analytics", "events")

	cases := map[string]func() error{
		"$where blocked": func() error {
			_, err := db.FindDocs[map[string]any](ctx, d, db.M{"$where": "sleep(1000)"}, db.FindOptions{})
			return err
		},
		"nested $function blocked": func() error {
			_, err := db.FindDocs[map[string]any](ctx, d, db.M{"a": db.M{"$expr": db.M{"$function": db.M{}}}}, db.FindOptions{})
			return err
		},
		"$out stage blocked": func() error {
			_, err := db.Aggregate[map[string]any](ctx, d, db.M{"$out": "stolen"})
			return err
		},
		"update needs operators": func() error {
			_, err := d.Update(ctx, db.M{"a": 1}, db.M{"replaced": true}, false, false)
			return err
		},
		"delete needs filter": func() error {
			_, err := d.Delete(ctx, db.M{}, true)
			return err
		},
		"limit capped": func() error {
			_, err := db.FindDocs[map[string]any](ctx, d, nil, db.FindOptions{Limit: 5000})
			return err
		},
	}
	for name, fn := range cases {
		if err := fn(); !errors.Is(err, db.ErrValidation) {
			t.Errorf("%s: want 400, got %v", name, err)
		}
	}
	_, err = c.Docs("analytics", "users").Count(ctx, nil)
	var re *db.RemoteError
	testkit.True(t, errors.As(err, &re) && re.Status == 403, "collection allowlist")
}

func TestMongoIntegration(t *testing.T) {
	uri := os.Getenv("MONGO_TEST_URI")
	if uri == "" {
		t.Skip("MONGO_TEST_URI not set")
	}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	dbName := "dbh_test_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	dp, err := docs.Open(context.Background(), []appconfig.MongoStore{{Name: "analytics", URI: uri, Database: dbName}}, quiet)
	testkit.NoError(t, err)
	t.Cleanup(func() { _ = dp.DropDatabase(context.Background(), "analytics"); _ = dp.Close() })
	testkit.NoError(t, dp.EnsureIndexes(context.Background()))
	c := storeServer(t, nil, dp)
	ctx := context.Background()

	d := c.Docs("analytics", "notes")
	ids, err := d.Insert(ctx, db.M{"title": "a", "n": 1, "at": db.M{"$date": "2026-01-02T03:04:05Z"}}, db.M{"title": "b", "n": 2})
	testkit.NoError(t, err)
	testkit.Equal(t, len(ids), 2)

	type note struct {
		Title string `json:"title"`
		N     int    `json:"n"`
	}
	list, err := db.FindDocs[note](ctx, d, db.M{"n": db.M{"$gte": 1}}, db.FindOptions{Sort: db.M{"n": -1}, Limit: 10})
	testkit.NoError(t, err)
	testkit.Equal(t, len(list), 2)
	testkit.Equal(t, list[0].Title, "b")

	one, err := db.FindOneDoc[note](ctx, d, db.M{"title": "a"})
	testkit.NoError(t, err)
	testkit.Equal(t, one.N, 1)
	_, err = db.FindOneDoc[note](ctx, d, db.M{"title": "zzz"})
	testkit.ErrorIs(t, err, db.ErrNotFound)

	res, err := d.Update(ctx, db.M{"title": "a"}, db.M{"$inc": db.M{"n": 10}}, false, false)
	testkit.NoError(t, err)
	testkit.Equal(t, res.Modified, int64(1))

	sums, err := db.Aggregate[struct {
		Total int `json:"total"`
	}](ctx, d, db.M{"$group": db.M{"_id": nil, "total": db.M{"$sum": "$n"}}})
	testkit.NoError(t, err)
	testkit.Equal(t, sums[0].Total, 13)

	n, err := d.Count(ctx, nil)
	testkit.NoError(t, err)
	testkit.Equal(t, n, int64(2))
	deleted, err := d.Delete(ctx, db.M{"n": db.M{"$gt": 0}}, true)
	testkit.NoError(t, err)
	testkit.Equal(t, deleted, int64(2))

	a := c.Analytics("analytics")
	day := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	acc, err := a.Track(ctx,
		db.Event{Name: "login", UserID: "u1", At: day},
		db.Event{Name: "view", UserID: "u1", At: day.Add(time.Hour), Props: map[string]any{"page": "/home"}},
		db.Event{Name: "login", UserID: "u2", At: day},
		db.Event{Name: "login", UserID: "u1", At: day.Add(24 * time.Hour)},
		db.Event{Name: "anon"},
	)
	testkit.NoError(t, err)
	testkit.Equal(t, acc, 5)
	points, err := a.DAU(ctx, day, day.Add(48*time.Hour))
	testkit.NoError(t, err)
	testkit.Equal(t, points, []db.DAUPoint{{Day: "2026-03-01", Users: 2}, {Day: "2026-03-02", Users: 1}})
	events, err := c.Docs("analytics", docs.EventsCollection).Count(ctx, nil)
	testkit.NoError(t, err)
	testkit.Equal(t, events, int64(5))
}
