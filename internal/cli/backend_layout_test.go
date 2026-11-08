package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestBackendFileLayout(t *testing.T) {
	conditionDir := conditionPackageDir(t)
	rejectAggregateBackendFiles(t, conditionDir)
	rejectAggregateBackendFiles(t, filepath.Join(filepath.Dir(conditionDir), "cli"))

	for backend := range backendParsers {
		t.Run(backend, func(t *testing.T) {
			source := filepath.Join(conditionDir, backend+".go")
			testSource := filepath.Join(conditionDir, backend+"_test.go")
			requireBackendSource(t, source, backend)
			requireBackendTests(t, testSource)
		})
	}
}

func conditionPackageDir(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate backend layout test source")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(filename), "..", "condition"))
}

func rejectAggregateBackendFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() && (strings.Contains(name, "extra_backend") || strings.Contains(name, "_extra.")) {
			t.Errorf("aggregate backend file %q is forbidden; use backend-owned files", filepath.Join(dir, name))
		}
	}
}

func requireBackendSource(t *testing.T, filename, backend string) {
	t.Helper()
	file := parseLayoutFile(t, filename)
	found := false
	ast.Inspect(file, func(node ast.Node) bool {
		pair, ok := node.(*ast.KeyValueExpr)
		if !ok {
			return true
		}
		key, ok := pair.Key.(*ast.Ident)
		value, valueOK := pair.Value.(*ast.BasicLit)
		if !ok || !valueOK || key.Name != "Backend" || value.Kind != token.STRING {
			return true
		}
		literal, err := strconv.Unquote(value.Value)
		if err == nil && literal == backend {
			found = true
		}
		return true
	})
	if !found {
		t.Errorf("%s must own Descriptor.Backend %q", filename, backend)
	}
}

func requireBackendTests(t *testing.T, filename string) {
	t.Helper()
	file := parseLayoutFile(t, filename)
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Recv == nil && strings.HasPrefix(fn.Name.Name, "Test") {
			return
		}
	}
	t.Errorf("%s must contain at least one test", filename)
}

func parseLayoutFile(t *testing.T, filename string) *ast.File {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), filename, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", filename, err)
	}
	return file
}
