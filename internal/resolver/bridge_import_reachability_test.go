package resolver

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestTrackedBridgeImportsDoNotAuthorizeBareNames(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, tc := range []struct{ language, file, provider, marker, spec string }{
			{"go", "main.go", "lib.go", "go_import", "example.org/lib"},
			{"rust", "src/lib.rs", "src/lib.rs", CargoImportStubMeta, "external::library/action"},
			{"python", "app.py", "pkg/image.py", PythonImportStubMeta, "external::pkg.image"},
		} {
			t.Run(backend+"/"+tc.language, func(t *testing.T) {
				var g graph.Store = graph.New()
				if backend == "sqlite" {
					db, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
					require.NoError(t, err)
					t.Cleanup(func() { require.NoError(t, db.Close()) })
					g = db
				}
				file, provider := "consumer/"+tc.file, "provider/"+tc.provider
				g.AddNode(&graph.Node{ID: file, Kind: graph.KindFile, FilePath: file, RepoPrefix: "consumer", Language: tc.language})
				g.AddNode(&graph.Node{ID: file + "::run", Name: "run", Kind: graph.KindFunction, FilePath: file, RepoPrefix: "consumer", Language: tc.language})
				g.AddNode(&graph.Node{ID: provider, Kind: graph.KindFile, FilePath: provider, RepoPrefix: "provider", Language: tc.language})
				// Neither imported module exports hidden. Two other modules do.
				for _, decoy := range []string{"provider/other", "provider/decoy"} {
					g.AddNode(&graph.Node{ID: decoy + "::hidden", Name: "hidden", Kind: graph.KindFunction, FilePath: decoy, RepoPrefix: "provider", Language: tc.language})
					g.AddNode(&graph.Node{ID: decoy + "::Client.Close", Name: "Close", Kind: graph.KindMethod, FilePath: decoy, RepoPrefix: "provider", Language: tc.language, Meta: map[string]any{"receiver": "Client"}})
				}
				g.AddEdge(&graph.Edge{From: file, To: provider, Kind: graph.EdgeImports, FilePath: file, Line: 1, CrossRepo: true, Meta: map[string]any{tc.marker: tc.spec}})
				// A foreign-typed resp.Body.Close() has no evidence for Client.Close.
				for i, target := range []string{"unresolved::hidden", "unresolved::*.Close"} {
					g.AddEdge(&graph.Edge{From: file + "::run", To: target, Kind: graph.EdgeCalls, FilePath: file, Line: i + 2, Meta: map[string]any{"receiver_type": "io.ReadCloser"}})
				}
				cr := NewCrossRepo(g)
				_, err := cr.ResolveAllContext(t.Context())
				require.NoError(t, err)
				calls := outEdgesOfKind(g, file+"::run", graph.EdgeCalls)
				require.Len(t, calls, 2)
				for _, call := range calls {
					assert.True(t, graph.IsUnresolvedTarget(call.To), "unimported symbol must stay unresolved: %s", call.To)
					assert.False(t, call.CrossRepo)
				}
			})
		}
	}
}

func TestCargoCallsStayWithinImportedItem(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, scenario := range []string{"function", "constructor", "method", "foreign_type", "ambiguous"} {
			t.Run(backend+"/"+scenario, func(t *testing.T) {
				var g graph.Store = graph.New()
				if backend == "sqlite" {
					db, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
					require.NoError(t, err)
					t.Cleanup(func() { require.NoError(t, db.Close()) })
					g = db
				}
				file, imported := "consumer/src/lib.rs", "provider/src/util.rs"
				g.AddNode(&graph.Node{ID: file, FilePath: file, Kind: graph.KindFile, RepoPrefix: "consumer", Language: "rust"})
				kind, name, target, item := graph.KindFunction, "action", "unresolved::action", "action"
				meta := map[string]any{}
				if scenario == "constructor" {
					kind, name, target, item = graph.KindMethod, "new", "unresolved::new", "Foo"
					meta["rust_path"] = "Foo::new"
				}
				if scenario == "method" || scenario == "foreign_type" {
					kind, name, target, item = graph.KindMethod, "close", "unresolved::*.close", "Foo"
					meta["receiver_type"] = "Foo"
					if scenario == "foreign_type" {
						meta["receiver_type"] = "Foreign"
					}
				}
				// A same-named decoy in another module must never compete with util.
				for _, path := range []string{"provider/src/aaa.rs", imported} {
					g.AddNode(&graph.Node{ID: path, FilePath: path, Kind: graph.KindFile, RepoPrefix: "provider", Language: "rust"})
					g.AddNode(&graph.Node{ID: path + "::" + name, FilePath: path, Name: name, Kind: kind, RepoPrefix: "provider", Language: "rust", Meta: map[string]any{"receiver": "Foo"}})
				}
				g.AddEdge(&graph.Edge{From: file, FilePath: file, To: imported, Kind: graph.EdgeImports, Line: 1, CrossRepo: true, Meta: map[string]any{CargoImportStubMeta: "external::library/util/" + item}})
				if scenario == "ambiguous" {
					g.AddEdge(&graph.Edge{From: file, FilePath: file, To: "provider/src/aaa.rs", Kind: graph.EdgeImports, Line: 2, CrossRepo: true, Meta: map[string]any{CargoImportStubMeta: "external::library/aaa/" + item}})
				}
				g.AddEdge(&graph.Edge{From: file, FilePath: file, To: target, Kind: graph.EdgeCalls, Line: 3, Meta: meta})
				_, err := NewCrossRepo(g).ResolveAllContext(t.Context())
				require.NoError(t, err)
				calls := outEdgesOfKind(g, file, graph.EdgeCalls)
				require.Len(t, calls, 1)
				if scenario == "foreign_type" || scenario == "ambiguous" {
					require.Equal(t, target, calls[0].To)
					require.False(t, calls[0].CrossRepo)
				} else {
					require.Equal(t, imported+"::"+name, calls[0].To)
					require.True(t, calls[0].CrossRepo)
				}
			})
		}
	}
}

func TestPythonMethodsStayWithinImportedType(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, scenario := range []string{"method", "foreign_type", "ambiguous"} {
			t.Run(backend+"/"+scenario, func(t *testing.T) {
				var g graph.Store = graph.New()
				if backend == "sqlite" {
					db, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
					require.NoError(t, err)
					t.Cleanup(func() { require.NoError(t, db.Close()) })
					g = db
				}
				file := "consumer/app.py"
				g.AddNode(&graph.Node{ID: file, FilePath: file, Kind: graph.KindFile, RepoPrefix: "consumer", Language: "python"})
				for _, provider := range []string{"provider/pkg/aaa.py", "provider/pkg/image.py"} {
					g.AddNode(&graph.Node{ID: provider, FilePath: provider, Kind: graph.KindFile, RepoPrefix: "provider", Language: "python"})
					g.AddNode(&graph.Node{ID: provider + "::Image.show", FilePath: provider, Name: "show", Kind: graph.KindMethod, RepoPrefix: "provider", Language: "python", Meta: map[string]any{"receiver": "Image"}})
				}
				g.AddEdge(&graph.Edge{From: file, FilePath: file, To: "provider/pkg/image.py", Kind: graph.EdgeImports, Line: 1, CrossRepo: true, Meta: map[string]any{PythonImportStubMeta: "external::pkg.image", "python_import": "pkg.image"}})
				if scenario == "ambiguous" {
					g.AddEdge(&graph.Edge{From: file, FilePath: file, To: "provider/pkg/aaa.py", Kind: graph.EdgeImports, Line: 2, CrossRepo: true, Meta: map[string]any{PythonImportStubMeta: "external::pkg.aaa", "python_import": "pkg.aaa"}})
				}
				receiver := "Image"
				if scenario == "foreign_type" {
					receiver = "Foreign"
				}
				g.AddEdge(&graph.Edge{From: file, FilePath: file, To: "unresolved::*.show", Kind: graph.EdgeCalls, Line: 3, Meta: map[string]any{"receiver_type": receiver}})
				_, err := NewCrossRepo(g).ResolveAllContext(t.Context())
				require.NoError(t, err)
				calls := outEdgesOfKind(g, file, graph.EdgeCalls)
				require.Len(t, calls, 1)
				if scenario == "method" {
					require.Equal(t, "provider/pkg/image.py::Image.show", calls[0].To)
					require.True(t, calls[0].CrossRepo)
				} else {
					require.Equal(t, "unresolved::*.show", calls[0].To)
					require.False(t, calls[0].CrossRepo)
				}
			})
		}
	}
}
