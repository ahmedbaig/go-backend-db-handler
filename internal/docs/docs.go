package docs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/ahmedbaig/go-backend-db"
	"github.com/ahmedbaig/go-backend-db-handler/internal/appconfig"
	"github.com/ahmedbaig/go-backend-http"
	"github.com/ahmedbaig/go-backend-monitoring"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

const (
	defaultLimit = 100
	maxInsert    = 1000
	maxDepth     = 32
)

var forbiddenOps = map[string]bool{
	"$where": true, "$function": true, "$accumulator": true, "$out": true, "$merge": true,
}

type store struct {
	cfg    appconfig.MongoStore
	client *mongo.Client
	db     *mongo.Database
}

type Pool struct {
	stores map[string]*store
}

func Open(ctx context.Context, cfgs []appconfig.MongoStore, log *slog.Logger) (*Pool, error) {
	p := &Pool{stores: map[string]*store{}}
	for _, c := range cfgs {
		opts := options.Client().ApplyURI(c.URI).SetAppName("db-handler")
		if t := c.Timeout.Std(); t > 0 {
			opts.SetTimeout(t)
		}
		cl, err := mongo.Connect(opts)
		if err != nil {
			_ = p.Close()
			return nil, fmt.Errorf("mongo store %s: %w", c.Name, err)
		}
		pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if err := cl.Ping(pctx, nil); err != nil {
			log.Warn("mongo not reachable at startup", slog.String("store", c.Name), slog.Any("error", err))
		}
		cancel()
		if c.MaxLimit <= 0 {
			c.MaxLimit = 1000
		}
		p.stores[c.Name] = &store{cfg: c, client: cl, db: cl.Database(c.Database)}
		log.Info("mongo store ready", slog.String("store", c.Name), slog.String("database", c.Database))
	}
	return p, nil
}

func (p *Pool) Stores() []db.StoreInfo {
	out := make([]db.StoreInfo, 0, len(p.stores))
	for name, s := range p.stores {
		out = append(out, db.StoreInfo{Name: name, Kind: "mongo", DB: s.cfg.Database})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (p *Pool) RegisterProbes(pr *monitoring.Prober) error {
	var errs []error
	for name, s := range p.stores {
		cl := s.client
		errs = append(errs, pr.AddProbe("mongo:"+name, func(ctx context.Context) error {
			return cl.Ping(ctx, nil)
		}, monitoring.Interval(15*time.Second), monitoring.Timeout(5*time.Second)))
	}
	return errors.Join(errs...)
}

func (p *Pool) Close() error {
	var errs []error
	for _, s := range p.stores {
		errs = append(errs, s.client.Disconnect(context.Background()))
	}
	return errors.Join(errs...)
}

func fieldErr(field, rule, msg string) error {
	return httpx.E(httpx.CodeValidation).WithFields(httpx.FieldError{Field: field, Rule: rule, Message: msg})
}

func (p *Pool) get(name string) (*store, error) {
	s, ok := p.stores[name]
	if !ok {
		return nil, httpx.E(httpx.CodeNotFound).WithMessage("Unknown mongo store " + name)
	}
	return s, nil
}

func (s *store) collection(name string) (*mongo.Collection, error) {
	if name == "" || strings.ContainsAny(name, "$\x00") || strings.HasPrefix(name, "system.") || len(name) > 120 {
		return nil, fieldErr("collection", "name", "is not a valid collection name")
	}
	if len(s.cfg.Collections) > 0 && !slices.Contains(s.cfg.Collections, name) {
		return nil, httpx.E(httpx.CodeForbidden).WithMessage("Collection " + name + " is not exposed by this store")
	}
	return s.db.Collection(name), nil
}

func parseDoc(field string, raw json.RawMessage, required bool) (bson.D, error) {
	if len(raw) == 0 || string(raw) == "null" {
		if required {
			return nil, fieldErr(field, "required", "is required")
		}
		return bson.D{}, nil
	}
	var d bson.D
	if err := bson.UnmarshalExtJSON(raw, false, &d); err != nil {
		return nil, fieldErr(field, "json", "must be a JSON object (extended JSON allowed)")
	}
	if err := check(field, d, 0); err != nil {
		return nil, err
	}
	return d, nil
}

func check(path string, v any, depth int) error {
	if depth > maxDepth {
		return fieldErr(path, "depth", "is nested too deeply")
	}
	switch t := v.(type) {
	case bson.D:
		for _, e := range t {
			if forbiddenOps[e.Key] {
				return fieldErr(path+"."+e.Key, "forbidden", e.Key+" is not allowed")
			}
			if err := check(path+"."+e.Key, e.Value, depth+1); err != nil {
				return err
			}
		}
	case bson.M:
		for k, val := range t {
			if forbiddenOps[k] {
				return fieldErr(path+"."+k, "forbidden", k+" is not allowed")
			}
			if err := check(path+"."+k, val, depth+1); err != nil {
				return err
			}
		}
	case bson.A:
		for i, val := range t {
			if err := check(fmt.Sprintf("%s.%d", path, i), val, depth+1); err != nil {
				return err
			}
		}
	}
	return nil
}

func toJSON(v any) (json.RawMessage, error) {
	b, err := bson.MarshalExtJSON(v, false, false)
	if err != nil {
		return nil, err
	}
	return b, nil
}

func manyJSON(docs []bson.Raw) (json.RawMessage, error) {
	var b strings.Builder
	b.WriteByte('[')
	for i, d := range docs {
		if i > 0 {
			b.WriteByte(',')
		}
		j, err := bson.MarshalExtJSON(d, false, false)
		if err != nil {
			return nil, err
		}
		b.Write(j)
	}
	b.WriteByte(']')
	return json.RawMessage(b.String()), nil
}

func idJSON(v any) json.RawMessage {
	j, err := bson.MarshalExtJSON(bson.D{{Key: "id", Value: v}}, false, false)
	if err != nil {
		return nil
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(j, &m) != nil {
		return nil
	}
	return m["id"]
}

func updateDoc(raw json.RawMessage) (bson.D, error) {
	u, err := parseDoc("update", raw, true)
	if err != nil {
		return nil, err
	}
	allowed := map[string]bool{"$set": true, "$unset": true, "$inc": true, "$push": true, "$pull": true,
		"$addToSet": true, "$setOnInsert": true, "$min": true, "$max": true, "$currentDate": true, "$rename": true, "$mul": true}
	for _, e := range u {
		if !allowed[e.Key] {
			return nil, fieldErr("update."+e.Key, "operator", "update must only use update operators like $set, $inc, $push")
		}
	}
	return u, nil
}

func (p *Pool) Execute(ctx context.Context, storeName, collName string, op db.DocsOp, req db.DocsRequest) (db.DocsResult, error) {
	s, err := p.get(storeName)
	if err != nil {
		return db.DocsResult{}, err
	}
	coll, err := s.collection(collName)
	if err != nil {
		return db.DocsResult{}, err
	}
	switch op {
	case db.DocsInsert:
		if len(req.Documents) == 0 {
			return db.DocsResult{}, fieldErr("documents", "required", "is required")
		}
		if len(req.Documents) > maxInsert {
			return db.DocsResult{}, fieldErr("documents", "max", fmt.Sprintf("may insert at most %d documents", maxInsert))
		}
		docs := make([]any, len(req.Documents))
		for i, raw := range req.Documents {
			d, err := parseDoc(fmt.Sprintf("documents.%d", i), raw, true)
			if err != nil {
				return db.DocsResult{}, err
			}
			docs[i] = d
		}
		res, err := coll.InsertMany(ctx, docs)
		if err != nil {
			return db.DocsResult{}, translate(err)
		}
		out := db.DocsResult{Count: int64(len(res.InsertedIDs))}
		for _, id := range res.InsertedIDs {
			out.InsertedIDs = append(out.InsertedIDs, idJSON(id))
		}
		return out, nil

	case db.DocsFind:
		filter, err := parseDoc("filter", req.Filter, false)
		if err != nil {
			return db.DocsResult{}, err
		}
		limit := req.Limit
		if limit == 0 {
			limit = defaultLimit
		}
		if limit < 0 || limit > s.cfg.MaxLimit {
			return db.DocsResult{}, fieldErr("limit", "max", fmt.Sprintf("must be between 1 and %d", s.cfg.MaxLimit))
		}
		fo := options.Find().SetLimit(limit)
		if req.Skip > 0 {
			fo.SetSkip(req.Skip)
		}
		if len(req.Sort) > 0 {
			srt, err := parseDoc("sort", req.Sort, false)
			if err != nil {
				return db.DocsResult{}, err
			}
			fo.SetSort(srt)
		}
		if len(req.Projection) > 0 {
			pr, err := parseDoc("projection", req.Projection, false)
			if err != nil {
				return db.DocsResult{}, err
			}
			fo.SetProjection(pr)
		}
		cur, err := coll.Find(ctx, filter, fo)
		if err != nil {
			return db.DocsResult{}, translate(err)
		}
		var docs []bson.Raw
		if err := cur.All(ctx, &docs); err != nil {
			return db.DocsResult{}, translate(err)
		}
		data, err := manyJSON(docs)
		return db.DocsResult{Data: data, Count: int64(len(docs))}, err

	case db.DocsFindOne:
		filter, err := parseDoc("filter", req.Filter, true)
		if err != nil {
			return db.DocsResult{}, err
		}
		var doc bson.Raw
		if err := coll.FindOne(ctx, filter).Decode(&doc); err != nil {
			return db.DocsResult{}, translate(err)
		}
		data, err := toJSON(doc)
		return db.DocsResult{Data: data, Count: 1}, err

	case db.DocsCount:
		filter, err := parseDoc("filter", req.Filter, false)
		if err != nil {
			return db.DocsResult{}, err
		}
		n, err := coll.CountDocuments(ctx, filter)
		return db.DocsResult{Count: n}, translate(err)

	case db.DocsAggregate:
		if len(req.Pipeline) == 0 {
			return db.DocsResult{}, fieldErr("pipeline", "required", "is required")
		}
		pipe := make(bson.A, 0, len(req.Pipeline)+1)
		hasLimit := false
		for i, raw := range req.Pipeline {
			st, err := parseDoc(fmt.Sprintf("pipeline.%d", i), raw, true)
			if err != nil {
				return db.DocsResult{}, err
			}
			if len(st) > 0 && st[0].Key == "$limit" {
				hasLimit = true
			}
			pipe = append(pipe, st)
		}
		if !hasLimit {
			pipe = append(pipe, bson.D{{Key: "$limit", Value: s.cfg.MaxLimit}})
		}
		cur, err := coll.Aggregate(ctx, pipe)
		if err != nil {
			return db.DocsResult{}, translate(err)
		}
		var docs []bson.Raw
		if err := cur.All(ctx, &docs); err != nil {
			return db.DocsResult{}, translate(err)
		}
		data, err := manyJSON(docs)
		return db.DocsResult{Data: data, Count: int64(len(docs))}, err

	case db.DocsUpdate:
		filter, err := parseDoc("filter", req.Filter, true)
		if err != nil {
			return db.DocsResult{}, err
		}
		if len(filter) == 0 {
			return db.DocsResult{}, fieldErr("filter", "required", "update needs a non-empty filter")
		}
		upd, err := updateDoc(req.Update)
		if err != nil {
			return db.DocsResult{}, err
		}
		var res *mongo.UpdateResult
		if req.Many {
			res, err = coll.UpdateMany(ctx, filter, upd, options.UpdateMany().SetUpsert(req.Upsert))
		} else {
			res, err = coll.UpdateOne(ctx, filter, upd, options.UpdateOne().SetUpsert(req.Upsert))
		}
		if err != nil {
			return db.DocsResult{}, translate(err)
		}
		out := db.DocsResult{Matched: res.MatchedCount, Modified: res.ModifiedCount, Count: res.ModifiedCount + res.UpsertedCount}
		if res.UpsertedID != nil {
			out.UpsertedID = idJSON(res.UpsertedID)
		}
		return out, nil

	case db.DocsDelete:
		filter, err := parseDoc("filter", req.Filter, true)
		if err != nil {
			return db.DocsResult{}, err
		}
		if len(filter) == 0 {
			return db.DocsResult{}, fieldErr("filter", "required", "delete needs a non-empty filter")
		}
		var res *mongo.DeleteResult
		if req.Many {
			res, err = coll.DeleteMany(ctx, filter)
		} else {
			res, err = coll.DeleteOne(ctx, filter)
		}
		if err != nil {
			return db.DocsResult{}, translate(err)
		}
		return db.DocsResult{Count: res.DeletedCount}, nil
	}
	return db.DocsResult{}, fieldErr("op", "oneof", "is not a supported operation")
}

func translate(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, mongo.ErrNoDocuments):
		return httpx.E(httpx.CodeNotFound).Wrap(err)
	case mongo.IsDuplicateKeyError(err):
		return httpx.E(httpx.CodeConflict).WithMessage("A document with this key already exists").Wrap(err)
	}
	var ce mongo.CommandError
	if errors.As(err, &ce) && (ce.Code == 2 || ce.Code == 9 || ce.Code == 15 || ce.Code == 40324) {
		return httpx.E(httpx.CodeValidation).WithMessage(ce.Message).Wrap(err)
	}
	return err
}

func (p *Pool) DropDatabase(ctx context.Context, name string) error {
	s, err := p.get(name)
	if err != nil {
		return err
	}
	return s.db.Drop(ctx)
}
