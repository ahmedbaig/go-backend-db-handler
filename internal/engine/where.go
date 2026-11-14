package engine

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/ahmedbaig/go-backend-db"
	"github.com/ahmedbaig/go-backend-db-handler/internal/registry"
	"github.com/ahmedbaig/go-backend-http"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/schema"
)

const maxDepth = 8
const maxInValues = 1000

func fieldErr(field, rule, msg string) error {
	return httpx.E(httpx.CodeValidation).WithFields(httpx.FieldError{Field: field, Rule: rule, Message: msg})
}

func column(f *schema.Field) clause.Column {
	return clause.Column{Table: clause.CurrentTable, Name: f.DBName}
}

func buildWhere(m *registry.Model, dialect string, w db.Where, depth int) (clause.Expression, error) {
	if depth > maxDepth {
		return nil, fieldErr("where", "depth", fmt.Sprintf("may nest at most %d levels", maxDepth))
	}
	var exprs []clause.Expression

	names := make([]string, 0, len(w.Fields))
	for n := range w.Fields {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		f, ok := m.Field(name)
		if !ok {
			return nil, fieldErr("where."+name, "unknown", "is not a field of "+m.Service)
		}
		cond := w.Fields[name]
		ops := make([]string, 0, len(cond))
		for op := range cond {
			ops = append(ops, string(op))
		}
		sort.Strings(ops)
		for _, op := range ops {
			e, err := buildCondition(f, dialect, name, db.Operator(op), cond[db.Operator(op)])
			if err != nil {
				return nil, err
			}
			exprs = append(exprs, e)
		}
	}
	for _, sub := range w.And {
		e, err := buildWhere(m, dialect, sub, depth+1)
		if err != nil {
			return nil, err
		}
		if e != nil {
			exprs = append(exprs, e)
		}
	}
	if len(w.Or) > 0 {
		var ors []clause.Expression
		for _, sub := range w.Or {
			e, err := buildWhere(m, dialect, sub, depth+1)
			if err != nil {
				return nil, err
			}
			if e != nil {
				ors = append(ors, e)
			}
		}
		if len(ors) > 0 {
			exprs = append(exprs, clause.Or(ors...))
		}
	}
	if w.Not != nil {
		e, err := buildWhere(m, dialect, *w.Not, depth+1)
		if err != nil {
			return nil, err
		}
		if e != nil {
			exprs = append(exprs, clause.Not(e))
		}
	}
	switch len(exprs) {
	case 0:
		return nil, nil
	case 1:
		return exprs[0], nil
	}
	return clause.And(exprs...), nil
}

func buildCondition(f *schema.Field, dialect, name string, op db.Operator, raw any) (clause.Expression, error) {
	col := column(f)
	path := "where." + name + "." + string(op)
	switch op {
	case db.OpIsNull:
		b, ok := raw.(bool)
		if !ok {
			return nil, fieldErr(path, "type", "must be true or false")
		}
		if b {
			return clause.Expr{SQL: "? IS NULL", Vars: []any{col}}, nil
		}
		return clause.Expr{SQL: "? IS NOT NULL", Vars: []any{col}}, nil
	case db.OpContains, db.OpStartsWith, db.OpEndsWith:
		s, ok := raw.(string)
		if !ok {
			return nil, fieldErr(path, "type", "must be a string")
		}
		s = escapeLike(s)
		switch op {
		case db.OpContains:
			s = "%" + s + "%"
		case db.OpStartsWith:
			s = s + "%"
		default:
			s = "%" + s
		}
		if dialect == "mysql" {
			return clause.Expr{SQL: "? LIKE ?", Vars: []any{col, s}}, nil
		}
		return clause.Expr{SQL: "? LIKE ? ESCAPE '\\'", Vars: []any{col, s}}, nil
	case db.OpIn, db.OpNotIn:
		list, ok := raw.([]any)
		if !ok {
			return nil, fieldErr(path, "type", "must be an array")
		}
		if len(list) > maxInValues {
			return nil, fieldErr(path, "max", fmt.Sprintf("may list at most %d values", maxInValues))
		}
		vals := make([]any, len(list))
		for i, v := range list {
			c, err := coerce(f, v)
			if err != nil {
				return nil, fieldErr(path, "type", err.Error())
			}
			vals[i] = c
		}
		in := clause.IN{Column: col, Values: vals}
		if op == db.OpNotIn {
			return clause.Not(in), nil
		}
		return in, nil
	}

	if raw == nil {
		return nil, fieldErr(path, "type", "use isNull to compare with null")
	}
	v, err := coerce(f, raw)
	if err != nil {
		return nil, fieldErr(path, "type", err.Error())
	}
	switch op {
	case db.OpEq:
		return clause.Eq{Column: col, Value: v}, nil
	case db.OpNe:
		return clause.Neq{Column: col, Value: v}, nil
	case db.OpGt:
		return clause.Gt{Column: col, Value: v}, nil
	case db.OpGte:
		return clause.Gte{Column: col, Value: v}, nil
	case db.OpLt:
		return clause.Lt{Column: col, Value: v}, nil
	case db.OpLte:
		return clause.Lte{Column: col, Value: v}, nil
	}
	return nil, fieldErr(path, "operator", "unknown operator")
}

var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

func escapeLike(s string) string { return likeEscaper.Replace(s) }

func coerce(f *schema.Field, v any) (any, error) {
	if v == nil {
		return nil, nil
	}
	t := f.IndirectFieldType
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	p := reflect.New(t)
	if err := json.Unmarshal(b, p.Interface()); err != nil {
		return nil, fmt.Errorf("must be %s", describe(t))
	}
	return p.Elem().Interface(), nil
}

func describe(t reflect.Type) string {
	switch t.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "an integer"
	case reflect.Float32, reflect.Float64:
		return "a number"
	case reflect.Bool:
		return "true or false"
	case reflect.String:
		return "a string"
	}
	if t.String() == "time.Time" {
		return "an RFC 3339 time"
	}
	return t.String()
}
