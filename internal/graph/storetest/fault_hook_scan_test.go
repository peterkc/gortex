package storetest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// TestStorageFaultDefaultOpenPathsSetNoHook shows that every default open
// path sets no fault hook (FR-6): no production Go file in the module names a
// backend's WithFaultHook option except the file that declares it, so
// OpenBackend, NewSharedServer, the public Engine and the eval and init paths
// all open their store without one. The suite's own support packages under
// internal/graph/storetest (backendtest passes the option) are not production:
// the scan also shows that no production file imports them. Each backend's own
// test shows that an open without the option leaves the hook nil.
func TestStorageFaultDefaultOpenPathsSetNoHook(t *testing.T) {
	t.Run("ScannerReportsAPlantedCaller", func(t *testing.T) {
		dir := t.TempDir()
		writeFixtureFiles(t, dir, map[string]string{
			"backend/option.go":      "package backend\n\nfunc WithFaultHook(hook func(string) error) int { return 0 }\n",
			"backend/option_test.go": "package backend\n\nvar _ = WithFaultHook(nil)\n",
			"server/open.go":         "package server\n\nimport \"example.test/backend\"\n\nvar _ = backend.WithFaultHook(nil)\n",
			"server/comment.go":      "package server\n\n// Production never passes WithFaultHook.\n",
			"server/support.go":      "package server\n\nimport _ \"example.test/support/adapter\"\n",
			"support/adapter/a.go":   "package adapter\n\nimport \"example.test/backend\"\n\nvar _ = backend.WithFaultHook(nil)\n",
		})
		got := faultHookReferences(t, dir, filepath.Join(dir, "support"), "example.test/support")
		want := []string{filepath.Join(dir, "server", "open.go"), filepath.Join(dir, "server", "support.go")}
		if !slices.Equal(got, want) {
			t.Fatalf("references = %v, want %v", got, want)
		}
	})
	t.Run("NoProductionFileSetsAHook", func(t *testing.T) {
		root, err := filepath.Abs(filepath.Join("..", "..", ".."))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
			t.Fatalf("module root %s: %v", root, err)
		}
		support := filepath.Join(root, "internal", "graph", "storetest")
		if got := faultHookReferences(t, root, support, "github.com/zzet/gortex/internal/graph/storetest"); len(got) > 0 {
			t.Fatalf("production files pass a fault hook or import the suite: %v", got)
		}
	})
}

// faultHookReferences lists the non-test Go files under root, outside the
// support tree, that refer to an identifier named WithFaultHook without
// declaring a function of that name, or that import a package of the support
// tree (supportImport and below).
func faultHookReferences(t *testing.T, root, support, supportImport string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if path == support || path != root && (strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") || name == "testdata" || name == "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		imports := false
		for _, spec := range file.Imports {
			if p, err := strconv.Unquote(spec.Path.Value); err == nil && (p == supportImport || strings.HasPrefix(p, supportImport+"/")) {
				imports = true
			}
		}
		declared, referenced := false, false
		ast.Inspect(file, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.FuncDecl:
				if n.Recv == nil && n.Name.Name == "WithFaultHook" {
					declared = true
				}
			case *ast.Ident:
				if n.Name == "WithFaultHook" {
					referenced = true
				}
			}
			return true
		})
		if imports || referenced && !declared {
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan %s: %v", root, err)
	}
	return out
}
