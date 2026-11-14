package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteTypesFromGormGenOutput(t *testing.T) {
	out := t.TempDir()
	if err := writeTypes("testdata/models", out); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(out, "shop_main", "types_gen.go"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	for _, want := range []string{
		`const Database = "shop-main"`,
		`const OrderTable = "orders"`,
		"ID         *int32          `json:\"id,omitempty\"`",
		"Meta       json.RawMessage `json:\"meta\"`",
		"DeletedAt  *time.Time      `json:\"deleted_at,omitempty\"`",
		`CustomerID: "customer_id"`,
		"func OrderService(c *db.Client) *db.Service[Order]",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("missing %q in\n%s", want, src)
		}
	}
	for _, bad := range []string{"gorm", "datatypes", "internal", "Ignored", "helper"} {
		if strings.Contains(src, bad) {
			t.Errorf("should not contain %q", bad)
		}
	}
	if _, err := os.Stat(filepath.Join(out, "go.mod")); err != nil {
		t.Fatal("go.mod not written")
	}
	if testing.Short() {
		return
	}
	cmd := exec.Command("go", "vet", "./...")
	cmd.Dir = out
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generated module does not compile: %v\n%s", err, b)
	}
}

func TestRewriteType(t *testing.T) {
	cases := map[string]string{
		"string": "string", "*int64": "*int64", "gorm.DeletedAt": "*time.Time",
		"*sql.NullString": "*string", "[]pq.StringArray": "[][]string",
		"some.Unknown": "json.RawMessage", "[]Post": "[]Post",
	}
	for in, want := range cases {
		if got := rewriteType(in); got != want {
			t.Errorf("rewriteType(%q) = %q, want %q", in, got, want)
		}
	}
}
