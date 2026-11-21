package graph

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestStoreSQLiteImportsAreFenced(t *testing.T) {
	root, err := findRepoRoot()
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	err = filepath.WalkDir(root, func(file string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if _, skip := fenceSkippedDirs[entry.Name()]; skip {
				return filepath.SkipDir
			}
			nested, err := nestedGitCheckout(root, file)
			if err == nil && nested {
				return filepath.SkipDir
			}
			return err
		}
		if !strings.HasSuffix(file, ".go") || strings.HasSuffix(file, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, file)
		if err != nil {
			return err
		}
		switch filepath.ToSlash(filepath.Dir(rel)) {
		case "internal/graph/store_sqlite", "internal/storage/backend":
			return nil
		}
		parsed, err := parser.ParseFile(fset, file, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range parsed.Imports {
			name, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				return err
			}
			if name == graphImportPath+"/store_sqlite" {
				t.Errorf("%s:%d: non-test store_sqlite import outside backend", filepath.ToSlash(rel), fset.Position(imp.Pos()).Line)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
