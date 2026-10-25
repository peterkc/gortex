package resolver

import (
	"fmt"
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
		for _, scenario := range []string{"function", "glob", "constructor", "method", "foreign_type", "ambiguous"} {
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
				// Given a glob use, the ordinary function call still needs exact file evidence.
				if scenario == "glob" {
					item = "*"
				}
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
		for _, scenario := range []string{"method", "foreign_type", "ambiguous", "local_type", "other_file_type", "alias", "unbound"} {
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
				g.AddEdge(&graph.Edge{From: file, FilePath: file, To: "provider/pkg/image.py", Kind: graph.EdgeImports, Line: 1, CrossRepo: true, Meta: map[string]any{PythonImportStubMeta: "external::pkg.image", "python_import": "pkg.image", "python_bindings": map[string]any{"Image": "Image"}}})
				if scenario == "ambiguous" {
					g.AddEdge(&graph.Edge{From: file, FilePath: file, To: "provider/pkg/aaa.py", Kind: graph.EdgeImports, Line: 2, CrossRepo: true, Meta: map[string]any{PythonImportStubMeta: "external::pkg.aaa", "python_import": "pkg.aaa", "python_bindings": map[string]any{"Image": "Image"}}})
				}
				// Given a same-named type, only a definition in this caller file shadows the import.
				if scenario == "local_type" || scenario == "other_file_type" {
					typeFile := file
					if scenario == "other_file_type" {
						typeFile = "consumer/other.py"
					}
					g.AddNode(&graph.Node{ID: typeFile + "::Image", Name: "Image", FilePath: typeFile, Kind: graph.KindType, RepoPrefix: "consumer", Language: "python"})
				}
				if scenario == "alias" || scenario == "unbound" {
					imports := outEdgesOfKind(g, file, graph.EdgeImports)
					imports[0].Meta["python_bindings"] = map[string]any{"Picture": "Image"}
					g.ReindexEdges([]graph.EdgeReindex{{Edge: imports[0], OldTo: imports[0].To}})
				}
				receiver := "Image"
				if scenario == "alias" {
					receiver = "Picture"
				}
				if scenario == "foreign_type" {
					receiver = "Foreign"
				}
				g.AddEdge(&graph.Edge{From: file, FilePath: file, To: "unresolved::*.show", Kind: graph.EdgeCalls, Line: 3, Meta: map[string]any{"receiver_type": receiver}})
				_, err := NewCrossRepo(g).ResolveAllContext(t.Context())
				require.NoError(t, err)
				calls := outEdgesOfKind(g, file, graph.EdgeCalls)
				require.Len(t, calls, 1)
				if scenario == "method" || scenario == "alias" || scenario == "other_file_type" {
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

type bridgeNameCountStore struct {
	graph.Store
	broad, scoped int
}

func (s *bridgeNameCountStore) FindNodesByName(name string) []*graph.Node {
	s.broad++
	return s.Store.FindNodesByName(name)
}
func (s *bridgeNameCountStore) FindNodesByNameInRepo(name, repo string) []*graph.Node {
	s.scoped++
	return s.Store.FindNodesByNameInRepo(name, repo)
}
func TestBridgeCandidatesLookupOncePerProvider(t *testing.T) {
	for _, language := range []string{"rust", "python"} {
		t.Run(language, func(t *testing.T) {
			g := &bridgeNameCountStore{Store: graph.New()}
			cr := NewCrossRepo(g)
			file := "consumer/app.py"
			name := "show"
			target := "*.show"
			if language == "rust" {
				file = "consumer/src/lib.rs"
				name = "action"
				target = "action"
			}
			g.AddNode(&graph.Node{ID: file, FilePath: file, Kind: graph.KindFile, RepoPrefix: "consumer", Language: language})
			var imports []*graph.Edge
			for i := 0; i < 20; i++ {
				provider := fmt.Sprintf("provider/pkg/mod%d.py", i)
				marker := map[string]any{PythonImportStubMeta: "external::pkg.mod", "python_import": fmt.Sprintf("pkg.mod%d", i), "python_bindings": map[string]any{"Image": "Image"}}
				if language == "rust" {
					provider = fmt.Sprintf("provider/src/mod%d.rs", i)
					marker = map[string]any{CargoImportStubMeta: "external::library/action"}
				}
				imports = append(imports, &graph.Edge{From: file, FilePath: file, To: provider, Meta: marker})
				if i == 19 {
					kind := graph.KindFunction
					if language == "python" {
						kind = graph.KindMethod
					}
					g.AddNode(&graph.Node{ID: provider + "::" + name, FilePath: provider, Name: name, Kind: kind, RepoPrefix: "provider", Language: language, Meta: map[string]any{"receiver": "Image"}})
				}
			}
			cr.bridgeImportsByFile = map[string][]*graph.Edge{file: imports}
			e := &graph.Edge{From: file, FilePath: file, Kind: graph.EdgeCalls, Meta: map[string]any{"receiver_type": "Image"}}
			if language == "rust" {
				delete(e.Meta, "receiver_type")
				require.True(t, cr.resolveCargoImportedCall(e, target))
			} else {
				require.True(t, cr.resolvePythonImportedMethod(e, target))
			}
			require.Zero(t, g.broad, "bridge lookups must not scan all repositories")
			require.LessOrEqual(t, g.scoped, 2, "one provider lookup plus Python local-type check")
		})
	}
}

func TestPythonUnimportedReceiverAvoidsNameLookups(t *testing.T) {
	// Given a Python file with no bridge import binding the receiver.
	g := &bridgeNameCountStore{Store: graph.New()}
	file := "consumer/tests.py"
	g.AddNode(&graph.Node{ID: file, FilePath: file, Kind: graph.KindFile, RepoPrefix: "consumer", Language: "python"})
	cr := NewCrossRepo(g)
	edge := &graph.Edge{From: file, FilePath: file, Kind: graph.EdgeCalls, Meta: map[string]any{"receiver_type": "TestCase"}}
	// When a typed method is considered by the tracked bridge.
	require.False(t, cr.resolvePythonImportedMethod(edge, "*.assertEqual"))
	// Then it performs no store name lookup.
	require.Zero(t, g.broad)
	require.Zero(t, g.scoped)
}
