package registry

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"

	"github.com/ahmedbaig/go-backend-db"
	"gorm.io/gorm/schema"
)

type Option func(*Model)

func ReadOnly() Option { return func(m *Model) { m.ReadOnly = true } }

type Model struct {
	Database string
	Service  string
	Type     reflect.Type
	ReadOnly bool
	Schema   *schema.Schema
	fields   map[string]*schema.Field
}

func (m *Model) New() any { return reflect.New(m.Type).Interface() }

func (m *Model) NewSlice() any {
	return reflect.New(reflect.SliceOf(reflect.PointerTo(m.Type))).Interface()
}

func (m *Model) Field(name string) (*schema.Field, bool) {
	f, ok := m.fields[name]
	if !ok {
		f, ok = m.fields[strings.ToLower(name)]
	}
	return f, ok
}

func (m *Model) Relation(name string) (string, bool) {
	if m.Schema == nil {
		return "", false
	}
	for rn := range m.Schema.Relationships.Relations {
		if strings.EqualFold(rn, name) {
			return rn, true
		}
	}
	return "", false
}

func (m *Model) Info() db.ServiceInfo {
	info := db.ServiceInfo{Name: m.Service, Table: m.Schema.Table, ReadOnly: m.ReadOnly}
	for _, f := range m.Schema.Fields {
		if f.DBName == "" {
			continue
		}
		info.Fields = append(info.Fields, db.Field{
			Name:       JSONName(f),
			Column:     f.DBName,
			Type:       string(f.DataType),
			PrimaryKey: f.PrimaryKey,
			Nullable:   !f.NotNull && !f.PrimaryKey,
			Unique:     f.Unique,
		})
	}
	for rn := range m.Schema.Relationships.Relations {
		info.Relations = append(info.Relations, rn)
	}
	sort.Strings(info.Relations)
	return info
}

func JSONName(f *schema.Field) string {
	if tag, ok := f.StructField.Tag.Lookup("json"); ok {
		if n, _, _ := strings.Cut(tag, ","); n != "" && n != "-" {
			return n
		}
	}
	return f.Name
}

type Registry struct {
	mu     sync.RWMutex
	models map[string]map[string]*Model
	naming schema.Namer
	cache  *sync.Map
}

func New() *Registry {
	return &Registry{models: map[string]map[string]*Model{}, naming: schema.NamingStrategy{}, cache: &sync.Map{}}
}

func (r *Registry) WithNamer(n schema.Namer) *Registry {
	r.naming = n
	return r
}

func (r *Registry) Add(database, service string, model any, opts ...Option) {
	t := reflect.TypeOf(model)
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		panic(fmt.Sprintf("registry: %s.%s model must be a struct, got %s", database, service, t))
	}
	s, err := schema.Parse(reflect.New(t).Interface(), r.cache, r.naming)
	if err != nil {
		panic(fmt.Sprintf("registry: %s.%s: %v", database, service, err))
	}
	m := &Model{Database: database, Service: service, Type: t, Schema: s, fields: map[string]*schema.Field{}}
	for _, f := range s.Fields {
		if f.DBName == "" {
			continue
		}
		for _, k := range []string{JSONName(f), f.Name, f.DBName} {
			if _, taken := m.fields[k]; !taken {
				m.fields[k] = f
			}
			if lk := strings.ToLower(k); lk != k {
				if _, taken := m.fields[lk]; !taken {
					m.fields[lk] = f
				}
			}
		}
	}
	for _, o := range opts {
		o(m)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.models[database] == nil {
		r.models[database] = map[string]*Model{}
	}
	if _, dup := r.models[database][service]; dup {
		panic(fmt.Sprintf("registry: %s.%s registered twice", database, service))
	}
	r.models[database][service] = m
}

func (r *Registry) Get(database, service string) (*Model, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	m, ok := r.models[database][service]
	return m, ok
}

func (r *Registry) Databases() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.models))
	for d := range r.models {
		out = append(out, d)
	}
	sort.Strings(out)
	return out
}

func (r *Registry) Services(database string) []*Model {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Model, 0, len(r.models[database]))
	for _, m := range r.models[database] {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Service < out[j].Service })
	return out
}
