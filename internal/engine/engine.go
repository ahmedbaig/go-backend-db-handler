package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"reflect"
	"sort"
	"strings"

	"github.com/ahmedbaig/go-backend-db"
	"github.com/ahmedbaig/go-backend-db-handler/internal/registry"
	"github.com/ahmedbaig/go-backend-http"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type Connections interface {
	Get(database, instance string) (*gorm.DB, error)
}

type Options struct {
	DefaultTake int
	MaxTake     int
	BatchSize   int
}

type Engine struct {
	reg   *registry.Registry
	conns Connections
	opts  Options
}

func New(reg *registry.Registry, conns Connections, o Options) *Engine {
	if o.DefaultTake <= 0 {
		o.DefaultTake = 100
	}
	if o.MaxTake <= 0 {
		o.MaxTake = 1000
	}
	if o.DefaultTake > o.MaxTake {
		o.DefaultTake = o.MaxTake
	}
	if o.BatchSize <= 0 {
		o.BatchSize = 500
	}
	return &Engine{reg: reg, conns: conns, opts: o}
}

func (e *Engine) MaxTake() int { return e.opts.MaxTake }

func (e *Engine) Execute(ctx context.Context, q db.Query) (db.Result, error) {
	conn, m, err := e.resolve(q.Database, q.Instance, q.Service, q.Method)
	if err != nil {
		return db.Result{}, err
	}
	if q.Method.Writes() && needsTx(q.Method) {
		var res db.Result
		err := conn.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			var err error
			res, err = e.run(ctx, tx, m, q)
			return err
		})
		return res, translate(err)
	}
	res, err := e.run(ctx, conn.WithContext(ctx), m, q)
	return res, translate(err)
}

func (e *Engine) Transaction(ctx context.Context, t db.Transaction) (db.TransactionResult, error) {
	conn, err := e.conns.Get(t.Database, t.Instance)
	if err != nil {
		return db.TransactionResult{}, err
	}
	type step struct {
		m *registry.Model
		q db.Query
	}
	steps := make([]step, 0, len(t.Queries))
	for i, q := range t.Queries {
		if q.Database != "" && q.Database != t.Database {
			return db.TransactionResult{}, fieldErr("queries."+itoa(i)+".database", "mismatch", "every query must target "+t.Database)
		}
		q.Database, q.Instance = t.Database, t.Instance
		_, m, err := e.resolve(q.Database, q.Instance, q.Service, q.Method)
		if err != nil {
			return db.TransactionResult{}, err
		}
		steps = append(steps, step{m: m, q: q})
	}
	out := db.TransactionResult{Results: make([]db.Result, 0, len(steps))}
	err = conn.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, s := range steps {
			r, err := e.run(ctx, tx, s.m, s.q)
			if err != nil {
				return err
			}
			out.Results = append(out.Results, r)
		}
		return nil
	})
	if err != nil {
		return db.TransactionResult{}, translate(err)
	}
	return out, nil
}

func needsTx(m db.Method) bool {
	switch m {
	case db.Update, db.Delete, db.Upsert, db.CreateMany:
		return true
	}
	return false
}

func (e *Engine) resolve(database, instance, service string, method db.Method) (*gorm.DB, *registry.Model, error) {
	if !method.Valid() {
		return nil, nil, fieldErr("method", "oneof", "is not a supported method")
	}
	m, ok := e.reg.Get(database, service)
	if !ok {
		return nil, nil, httpx.E(httpx.CodeNotFound).WithMessage("Unknown service " + database + "." + service)
	}
	if m.ReadOnly && method.Writes() {
		return nil, nil, httpx.E(httpx.CodeForbidden).WithMessage(database + "." + service + " is read-only")
	}
	conn, err := e.conns.Get(database, instance)
	if err != nil {
		return nil, nil, err
	}
	return conn, m, nil
}

func (e *Engine) scope(tx *gorm.DB, m *registry.Model, p db.Params, method db.Method) (*gorm.DB, error) {
	dialect := tx.Dialector.Name()
	q := tx.Model(m.New())
	where, err := buildWhere(m, dialect, p.Where, 0)
	if err != nil {
		return nil, err
	}
	if method.Writes() && method != db.Create && method != db.CreateMany && method != db.Upsert && where == nil {
		return nil, fieldErr("where", "required", string(method)+" needs a where filter")
	}
	if method == db.FindOne && where == nil {
		return nil, fieldErr("where", "required", "findOne needs a where filter")
	}
	if where != nil {
		q = q.Where(where)
	}
	return q, nil
}

func (e *Engine) readOptions(q *gorm.DB, m *registry.Model, p db.Params) (*gorm.DB, error) {
	if len(p.Select) > 0 {
		cols := make([]string, 0, len(p.Select))
		for _, s := range p.Select {
			f, ok := m.Field(s)
			if !ok {
				return nil, fieldErr("select."+s, "unknown", "is not a field of "+m.Service)
			}
			cols = append(cols, f.DBName)
		}
		for _, pk := range m.Schema.PrimaryFields {
			cols = append(cols, pk.DBName)
		}
		q = q.Select(dedupe(cols))
	}
	for _, inc := range p.Include {
		rn, ok := m.Relation(inc)
		if !ok {
			return nil, fieldErr("include."+inc, "unknown", "is not a relation of "+m.Service)
		}
		q = q.Preload(rn)
	}
	for _, o := range p.OrderBy {
		f, ok := m.Field(o.Field)
		if !ok {
			return nil, fieldErr("orderBy."+o.Field, "unknown", "is not a field of "+m.Service)
		}
		desc := false
		switch strings.ToLower(string(o.Direction)) {
		case "", "asc":
		case "desc":
			desc = true
		default:
			return nil, fieldErr("orderBy."+o.Field, "oneof", "direction must be asc or desc")
		}
		q = q.Order(clause.OrderByColumn{Column: column(f), Desc: desc})
	}
	if len(p.OrderBy) == 0 {
		for _, pk := range m.Schema.PrimaryFields {
			q = q.Order(clause.OrderByColumn{Column: column(pk)})
		}
	}
	if p.Skip < 0 {
		return nil, fieldErr("skip", "min", "must be at least 0")
	}
	if p.Skip > 0 {
		q = q.Offset(p.Skip)
	}
	return q, nil
}

func (e *Engine) run(ctx context.Context, tx *gorm.DB, m *registry.Model, q db.Query) (db.Result, error) {
	p := q.Params
	base, err := e.scope(tx, m, p, q.Method)
	if err != nil {
		return db.Result{}, err
	}

	switch q.Method {
	case db.Count:
		var n int64
		if err := base.Count(&n).Error; err != nil {
			return db.Result{}, err
		}
		return db.Result{Count: &n}, nil

	case db.FindMany:
		rq, err := e.readOptions(base, m, p)
		if err != nil {
			return db.Result{}, err
		}
		take := e.opts.DefaultTake
		if p.Take == nil {
			httpx.LoggerFrom(ctx).WarnContext(ctx, "findMany without take, using default", slog.String("query", q.String()), slog.Int("take", take))
		} else {
			take = *p.Take
		}
		if take < 0 || take > e.opts.MaxTake {
			return db.Result{}, fieldErr("take", "max", "must be between 0 and "+itoa(e.opts.MaxTake))
		}
		out := m.NewSlice()
		if err := rq.Limit(take).Find(out).Error; err != nil {
			return db.Result{}, err
		}
		return dataResult(out)

	case db.FindFirst:
		rq, err := e.readOptions(base, m, p)
		if err != nil {
			return db.Result{}, err
		}
		out := m.New()
		res := rq.Limit(1).Find(out)
		if res.Error != nil {
			return db.Result{}, res.Error
		}
		if res.RowsAffected == 0 {
			return db.Result{Data: json.RawMessage("null")}, nil
		}
		return dataResult(out)

	case db.FindOne:
		rq, err := e.readOptions(base, m, p)
		if err != nil {
			return db.Result{}, err
		}
		rec, err := one(rq, m)
		if err != nil {
			return db.Result{}, err
		}
		return dataResult(rec)

	case db.Create:
		rec, err := decodeRecord(m, p.Data)
		if err != nil {
			return db.Result{}, err
		}
		if err := tx.Create(rec).Error; err != nil {
			return db.Result{}, err
		}
		return dataResult(rec)

	case db.CreateMany:
		recs, n, err := decodeRecords(m, p.Data)
		if err != nil {
			return db.Result{}, err
		}
		if n == 0 {
			zero := int64(0)
			return db.Result{Count: &zero}, nil
		}
		res := tx.CreateInBatches(recs, e.opts.BatchSize)
		if res.Error != nil {
			return db.Result{}, res.Error
		}
		affected := res.RowsAffected
		return db.Result{Count: &affected}, nil

	case db.Update:
		changes, err := decodeChanges(m, p.Data)
		if err != nil {
			return db.Result{}, err
		}
		rec, err := one(base, m)
		if err != nil {
			return db.Result{}, err
		}
		if err := tx.Model(rec).Updates(changes).Error; err != nil {
			return db.Result{}, err
		}
		fresh := m.New()
		if err := tx.Model(m.New()).Where(pkWhere(m, rec)).Take(fresh).Error; err != nil {
			return db.Result{}, err
		}
		return dataResult(fresh)

	case db.UpdateMany:
		changes, err := decodeChanges(m, p.Data)
		if err != nil {
			return db.Result{}, err
		}
		res := base.Updates(changes)
		if res.Error != nil {
			return db.Result{}, res.Error
		}
		n := res.RowsAffected
		return db.Result{Count: &n}, nil

	case db.Upsert:
		return e.upsert(tx, m, p)

	case db.Delete:
		rec, err := one(base, m)
		if err != nil {
			return db.Result{}, err
		}
		if err := tx.Delete(rec).Error; err != nil {
			return db.Result{}, err
		}
		return dataResult(rec)

	case db.DeleteMany:
		res := base.Delete(m.New())
		if res.Error != nil {
			return db.Result{}, res.Error
		}
		n := res.RowsAffected
		return db.Result{Count: &n}, nil
	}
	return db.Result{}, fieldErr("method", "oneof", "is not a supported method")
}

func (e *Engine) upsert(tx *gorm.DB, m *registry.Model, p db.Params) (db.Result, error) {
	if len(p.Conflict) == 0 {
		return db.Result{}, fieldErr("conflict", "required", "list the unique fields that identify the record")
	}
	rec, err := decodeRecord(m, p.Create)
	if err != nil {
		return db.Result{}, err
	}
	var cols []clause.Column
	where := clause.AndConditions{}
	rv := reflect.ValueOf(rec).Elem()
	for _, c := range p.Conflict {
		f, ok := m.Field(c)
		if !ok {
			return db.Result{}, fieldErr("conflict."+c, "unknown", "is not a field of "+m.Service)
		}
		cols = append(cols, clause.Column{Name: f.DBName})
		v, _ := f.ValueOf(tx.Statement.Context, rv)
		where.Exprs = append(where.Exprs, clause.Eq{Column: column(f), Value: v})
	}
	oc := clause.OnConflict{Columns: cols}
	if len(p.Data) > 0 && string(p.Data) != "null" {
		changes, err := decodeChanges(m, p.Data)
		if err != nil {
			return db.Result{}, err
		}
		oc.DoUpdates = clause.Assignments(changes)
	}
	if len(oc.DoUpdates) == 0 {
		oc.DoNothing = true
	}
	if err := tx.Clauses(oc).Create(rec).Error; err != nil {
		return db.Result{}, err
	}
	fresh := m.New()
	if err := tx.Model(m.New()).Where(where).Take(fresh).Error; err != nil {
		return db.Result{}, err
	}
	return dataResult(fresh)
}

var errMultiple = httpx.E(httpx.CodeConflict).WithMessage("The filter matches more than one record")

func one(q *gorm.DB, m *registry.Model) (any, error) {
	out := m.NewSlice()
	if err := q.Limit(2).Find(out).Error; err != nil {
		return nil, err
	}
	s := reflect.ValueOf(out).Elem()
	switch s.Len() {
	case 0:
		return nil, gorm.ErrRecordNotFound
	case 1:
		return s.Index(0).Interface(), nil
	}
	return nil, errMultiple
}

func pkWhere(m *registry.Model, rec any) clause.Expression {
	rv := reflect.ValueOf(rec)
	for rv.Kind() == reflect.Pointer {
		rv = rv.Elem()
	}
	var exprs []clause.Expression
	for _, pk := range m.Schema.PrimaryFields {
		v, _ := pk.ValueOf(context.Background(), rv)
		exprs = append(exprs, clause.Eq{Column: column(pk), Value: v})
	}
	return clause.And(exprs...)
}

func strictDecode(data []byte, into any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	return dec.Decode(into)
}

func decodeRecord(m *registry.Model, data json.RawMessage) (any, error) {
	if len(data) == 0 || string(data) == "null" {
		return nil, fieldErr("data", "required", "is required")
	}
	rec := m.New()
	if err := strictDecode(data, rec); err != nil {
		return nil, bodyErr("data", err)
	}
	return rec, nil
}

func decodeRecords(m *registry.Model, data json.RawMessage) (any, int, error) {
	if len(data) == 0 || string(data) == "null" {
		return nil, 0, fieldErr("data", "required", "is required")
	}
	recs := m.NewSlice()
	if err := strictDecode(data, recs); err != nil {
		return nil, 0, bodyErr("data", err)
	}
	return recs, reflect.ValueOf(recs).Elem().Len(), nil
}

func decodeChanges(m *registry.Model, data json.RawMessage) (map[string]any, error) {
	if len(data) == 0 || string(data) == "null" {
		return nil, fieldErr("data", "required", "is required")
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, bodyErr("data", err)
	}
	if len(raw) == 0 {
		return nil, fieldErr("data", "min", "must change at least one field")
	}
	out := make(map[string]any, len(raw))
	keys := make([]string, 0, len(raw))
	for k := range raw {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		f, ok := m.Field(k)
		if !ok {
			return nil, fieldErr("data."+k, "unknown", "is not a field of "+m.Service)
		}
		var v any
		if string(raw[k]) != "null" {
			p := reflect.New(f.IndirectFieldType)
			if err := json.Unmarshal(raw[k], p.Interface()); err != nil {
				return nil, fieldErr("data."+k, "type", "must be "+describe(f.IndirectFieldType))
			}
			v = p.Elem().Interface()
		}
		out[f.DBName] = v
	}
	return out, nil
}

func bodyErr(field string, err error) error {
	var ute *json.UnmarshalTypeError
	if errors.As(err, &ute) {
		return fieldErr(field+"."+ute.Field, "type", "must be "+ute.Type.String())
	}
	if strings.HasPrefix(err.Error(), "json: unknown field") {
		name := strings.Trim(strings.TrimPrefix(err.Error(), "json: unknown field "), `"`)
		return fieldErr(field+"."+name, "unknown", "is not a field")
	}
	return httpx.E(httpx.CodeInvalidJSON).Wrap(err)
}

func dataResult(v any) (db.Result, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return db.Result{}, err
	}
	return db.Result{Data: b}, nil
}

func translate(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, gorm.ErrRecordNotFound):
		return httpx.E(httpx.CodeNotFound).Wrap(err)
	case errors.Is(err, gorm.ErrDuplicatedKey):
		return httpx.E(httpx.CodeConflict).WithMessage("A record with these unique values already exists").Wrap(err)
	case errors.Is(err, gorm.ErrForeignKeyViolated):
		return httpx.E(httpx.CodeConflict).WithMessage("The record references, or is referenced by, another record").Wrap(err)
	case errors.Is(err, gorm.ErrInvalidData), errors.Is(err, gorm.ErrInvalidField):
		return httpx.E(httpx.CodeValidation).Wrap(err)
	}
	return err
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := in[:0]
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
