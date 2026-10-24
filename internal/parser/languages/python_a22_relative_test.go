package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

// TestPython_RelativeImport_SameDirSubmodule pins the bare
// `from . import foo` case: the parser must emit an import edge to the
// project-rooted stem so the resolver-side relative-import pass can
// land it on `<dir>/foo.py`.
func TestPython_RelativeImport_SameDirSubmodule(t *testing.T) {
	src := []byte(`from . import util

def caller():
    util.do()
`)
	e := NewPythonExtractor()
	res, err := e.Extract("app/main.py", src)
	require.NoError(t, err)

	require.True(t, hasEdgeBetween(res.Edges, graph.EdgeImports,
		"app/main.py", "unresolved::pyrel::app/util"),
		"`from . import util` in app/main.py should target the project-rooted stem app/util")
}

// TestPython_RelativeImport_WithModulePath pins `from .util import bar`
// — one edge to the module-stem, with `bar` bound in the alias map.
func TestPython_RelativeImport_WithModulePath(t *testing.T) {
	src := []byte(`from .util import bar

def caller():
    bar()
`)
	e := NewPythonExtractor()
	res, err := e.Extract("app/main.py", src)
	require.NoError(t, err)

	require.True(t, hasEdgeBetween(res.Edges, graph.EdgeImports,
		"app/main.py", "unresolved::pyrel::app/util"))
}

// TestPython_RelativeImport_ParentPackageDots verifies dot-prefix
// arithmetic: `from ..parent import x` in `app/sub/inner.py` resolves
// to the stem `app/parent`.
func TestPython_RelativeImport_ParentPackageDots(t *testing.T) {
	src := []byte(`from ..parent import x

def caller():
    x()
`)
	e := NewPythonExtractor()
	res, err := e.Extract("app/sub/inner.py", src)
	require.NoError(t, err)

	require.True(t, hasEdgeBetween(res.Edges, graph.EdgeImports,
		"app/sub/inner.py", "unresolved::pyrel::app/parent"),
		"`from ..parent import x` in app/sub/inner.py should target app/parent")
}

// TestPython_RelativeImport_AliasedName pins
// `from .util import bar as bz` — the alias map binds the renamed
// symbol, not the original.
func TestPython_RelativeImport_AliasedName(t *testing.T) {
	src := []byte(`from .util import bar as bz

def caller():
    bz()
`)
	e := NewPythonExtractor()
	res, err := e.Extract("app/main.py", src)
	require.NoError(t, err)

	require.True(t, hasEdgeBetween(res.Edges, graph.EdgeImports,
		"app/main.py", "unresolved::pyrel::app/util"))
}

// TestPython_RelativeImport_OutOfBoundsSkipped pins the safety guard:
// a `from ....x import y` whose dot count walks above the repo root
// must not emit a garbage import edge.
func TestPython_RelativeImport_OutOfBoundsSkipped(t *testing.T) {
	src := []byte(`from ....x import y
`)
	e := NewPythonExtractor()
	res, err := e.Extract("app/main.py", src)
	require.NoError(t, err)

	for _, edge := range res.Edges {
		if edge.Kind != graph.EdgeImports {
			continue
		}
		assert.NotContains(t, edge.To, "pyrel::",
			"out-of-bounds relative import must not emit a pyrel:: edge")
	}
}

// TestPyResolveRelativeStem_CornerCases walks the helper directly to
// guard the dot-count arithmetic across edge cases.
func TestPyResolveRelativeStem_CornerCases(t *testing.T) {
	cases := []struct {
		filePath string
		dots     int
		mod      string
		want     string
	}{
		{"app/main.py", 1, "", "app"},
		{"app/main.py", 1, "util", "app/util"},
		{"app/sub/inner.py", 2, "parent", "app/parent"},
		{"app/sub/inner.py", 3, "x", ""},
		{"app/sub/inner.py", 1, "util.deep", "app/sub/util/deep"},
		{"main.py", 1, "x", ""},
		{"", 1, "x", ""},
	}
	for _, c := range cases {
		got := pyResolveRelativeStem(c.filePath, c.dots, c.mod)
		require.Equal(t, c.want, got,
			"filePath=%q dots=%d mod=%q", c.filePath, c.dots, c.mod)
	}
}

func TestPythonPackageReExportMetadata(t *testing.T) {
	for _, tc := range []struct {
		name, file, source string
		want               map[string]any
	}{
		{"plain and alias", "pkg/__init__.py", "from .impl import Plain, Original as Public\n", map[string]any{"Plain": "Plain", "Public": "Original"}},
		{"ordinary module", "pkg/app.py", "from .impl import Plain\n", nil},
		{"function local", "pkg/__init__.py", "def local():\n    from .impl import Plain\n", nil},
		{"conditional", "pkg/__init__.py", "if flag:\n    from .impl import Plain\n", nil},
		{"wildcard", "pkg/__init__.py", "from .impl import *\n", map[string]any{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := NewPythonExtractor().Extract(tc.file, []byte(tc.source))
			require.NoError(t, err)
			var imports []*graph.Edge
			for _, edge := range result.Edges {
				if edge.Kind == graph.EdgeImports {
					imports = append(imports, edge)
				}
			}
			require.Len(t, imports, 1)
			if tc.want == nil {
				require.NotContains(t, imports[0].Meta, "python_reexports")
			} else {
				require.Equal(t, tc.want, imports[0].Meta["python_reexports"])
				require.Equal(t, "pkg/impl", imports[0].Meta["python_reexport_stem"])
			}
		})
	}
}
