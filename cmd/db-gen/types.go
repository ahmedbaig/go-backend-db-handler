package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

const typesModule = modulePath + "/types"

type modelField struct {
	name     string
	typ      string
	json     string
	relation bool
}

type modelStruct struct {
	name   string
	table  string
	fields []modelField
}

type modelPackage struct {
	pkg      string
	database string
	structs  []modelStruct
}

var typeRewrites = map[string]string{
	"gorm.DeletedAt":        "*time.Time",
	"datatypes.JSON":        "json.RawMessage",
	"datatypes.JSONMap":     "map[string]any",
	"datatypes.Date":        "time.Time",
	"datatypes.Time":        "string",
	"datatypes.UUID":        "string",
	"datatypes.BinUUID":     "string",
	"sql.NullString":        "*string",
	"sql.NullInt64":         "*int64",
	"sql.NullInt32":         "*int32",
	"sql.NullInt16":         "*int16",
	"sql.NullFloat64":       "*float64",
	"sql.NullBool":          "*bool",
	"sql.NullTime":          "*time.Time",
	"sql.NullByte":          "*byte",
	"decimal.Decimal":       "string",
	"decimal.NullDecimal":   "*string",
	"uuid.UUID":             "string",
	"pq.StringArray":        "[]string",
	"pq.Int64Array":         "[]int64",
	"pgtype.JSONB":          "json.RawMessage",
	"datatypes.JSONSlice":   "json.RawMessage",
	"datatypes.JSONType":    "json.RawMessage",
	"gorm.Model":            "",
	"schema.Tabler":         "",
	"time.Duration":         "time.Duration",
	"time.Time":             "time.Time",
	"json.RawMessage":       "json.RawMessage",
	"datatypes.NullString":  "*string",
	"datatypes.NullInt64":   "*int64",
	"datatypes.NullTime":    "*time.Time",
	"datatypes.NullFloat64": "*float64",
}

func exprString(fset *token.FileSet, e ast.Expr) string {
	var b bytes.Buffer
	_ = printer.Fprint(&b, fset, e)
	return b.String()
}

func rewriteType(s string) string {
	prefix := ""
	for {
		switch {
		case strings.HasPrefix(s, "*"):
			prefix += "*"
			s = s[1:]
			continue
		case strings.HasPrefix(s, "[]"):
			prefix += "[]"
			s = s[2:]
			continue
		}
		break
	}
	if !strings.Contains(s, ".") {
		return prefix + s
	}
	if r, ok := typeRewrites[s]; ok {
		if strings.HasPrefix(r, "*") && strings.HasSuffix(prefix, "*") {
			return prefix + r[1:]
		}
		return prefix + r
	}
	return prefix + "json.RawMessage"
}

func parseModels(dir, pkg string) (modelPackage, error) {
	mp := modelPackage{pkg: pkg, database: pkg}
	fset := token.NewFileSet()
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		return mp, err
	}
	sort.Strings(files)
	structs := map[string]*modelStruct{}
	var order []string
	tables := map[string]string{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			return mp, err
		}
		for _, decl := range af.Decls {
			switch d := decl.(type) {
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch s := spec.(type) {
					case *ast.TypeSpec:
						st, ok := s.Type.(*ast.StructType)
						if !ok || !s.Name.IsExported() {
							continue
						}
						ms := &modelStruct{name: s.Name.Name}
						for _, fld := range st.Fields.List {
							typ := exprString(fset, fld.Type)
							var tag reflect.StructTag
							if fld.Tag != nil {
								raw, _ := strconv.Unquote(fld.Tag.Value)
								tag = reflect.StructTag(raw)
							}
							if tag.Get("gorm") == "-" || tag.Get("json") == "-" {
								continue
							}
							rt := rewriteType(typ)
							if rt == "" {
								continue
							}
							for _, n := range fld.Names {
								if !n.IsExported() {
									continue
								}
								j, _, _ := strings.Cut(tag.Get("json"), ",")
								if j == "" {
									j = n.Name
								}
								ms.fields = append(ms.fields, modelField{name: n.Name, typ: rt, json: j})
							}
						}
						structs[ms.name] = ms
						order = append(order, ms.name)
					case *ast.ValueSpec:
						for i, n := range s.Names {
							if n.Name == "Database" && i < len(s.Values) {
								if lit, ok := s.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
									mp.database, _ = strconv.Unquote(lit.Value)
								}
							}
						}
					}
				}
			case *ast.FuncDecl:
				if d.Name.Name == "TableName" && d.Recv != nil && len(d.Recv.List) == 1 && d.Body != nil {
					if t := recvType(d.Recv.List[0].Type); t != "" {
						tables[t] = returnedString(d.Body)
					}
				}
			}
		}
	}
	for _, n := range order {
		s := structs[n]
		if t, ok := tables[n]; ok && t != "" {
			s.table = t
			mp.structs = append(mp.structs, *s)
		}
	}
	known := map[string]bool{}
	for _, s := range mp.structs {
		known[s.name] = true
	}
	for i := range mp.structs {
		var kept []modelField
		for _, f := range mp.structs[i].fields {
			base := strings.TrimLeft(f.typ, "*[]")
			if !strings.Contains(base, ".") && !isBuiltin(base) && !known[base] {
				continue
			}
			f.relation = known[base]
			kept = append(kept, f)
		}
		mp.structs[i].fields = kept
	}
	return mp, nil
}

func isBuiltin(t string) bool {
	switch t {
	case "string", "bool", "byte", "rune", "int", "int8", "int16", "int32", "int64",
		"uint", "uint8", "uint16", "uint32", "uint64", "float32", "float64", "any":
		return true
	}
	return strings.HasPrefix(t, "map[")
}

func renderTypes(mp modelPackage) []byte {
	var body bytes.Buffer
	w := func(f string, a ...any) { fmt.Fprintf(&body, f, a...) }
	w("const Database = %q\n\n", mp.database)
	for _, s := range mp.structs {
		w("const %sTable = %q\n\n", s.name, s.table)
		w("type %s struct {\n", s.name)
		for _, f := range s.fields {
			tag := f.json
			if strings.HasPrefix(f.typ, "*") || strings.HasPrefix(f.typ, "[]") {
				tag += ",omitempty"
			}
			w("\t%s %s `json:%q`\n", f.name, f.typ, tag)
		}
		w("}\n\n")
		writeNames(w, s.name+"Fields", s.fields, false)
		writeNames(w, s.name+"Relations", s.fields, true)
		w("func %sService(c *db.Client) *db.Service[%s] {\n\treturn db.NewService[%s](c, Database, %sTable)\n}\n\n", s.name, s.name, s.name, s.name)
	}
	src := body.String()
	var head bytes.Buffer
	fmt.Fprintf(&head, "// Code generated by db-gen. DO NOT EDIT.\n\npackage %s\n\nimport (\n", mp.pkg)
	if strings.Contains(src, "json.RawMessage") {
		head.WriteString("\t\"encoding/json\"\n")
	}
	if strings.Contains(src, "time.") {
		head.WriteString("\t\"time\"\n")
	}
	head.WriteString("\n\t\"github.com/ahmedbaig/go-backend-db\"\n)\n\n")
	out, err := format.Source(append(head.Bytes(), src...))
	if err != nil {
		return append(head.Bytes(), src...)
	}
	return out
}

const typesGoMod = `module ` + typesModule + `

go 1.27

require github.com/ahmedbaig/go-backend-db v0.2.0
`

func writeTypes(modelsDir, typesDir string) error {
	dirs, err := os.ReadDir(modelsDir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(typesDir, 0o755); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(typesDir, "go.mod")); os.IsNotExist(err) {
		if err := os.WriteFile(filepath.Join(typesDir, "go.mod"), []byte(typesGoMod), 0o644); err != nil {
			return err
		}
	}
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		mp, err := parseModels(filepath.Join(modelsDir, d.Name()), d.Name())
		if err != nil {
			return err
		}
		if len(mp.structs) == 0 {
			continue
		}
		dst := filepath.Join(typesDir, d.Name())
		if err := os.MkdirAll(dst, 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dst, "types_gen.go"), renderTypes(mp), 0o644); err != nil {
			return err
		}
		fmt.Printf("types %s -> %s (%d tables)\n", mp.database, dst, len(mp.structs))
	}
	return nil
}

func writeNames(w func(string, ...any), name string, fields []modelField, relations bool) {
	var sel []modelField
	for _, f := range fields {
		if f.relation == relations {
			sel = append(sel, f)
		}
	}
	if len(sel) == 0 {
		return
	}
	w("var %s = struct {\n", name)
	for _, f := range sel {
		w("\t%s string\n", f.name)
	}
	w("}{\n")
	for _, f := range sel {
		w("\t%s: %q,\n", f.name, f.json)
	}
	w("}\n\n")
}
