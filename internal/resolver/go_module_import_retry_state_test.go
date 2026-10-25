package resolver

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func goImportReviewStore(t *testing.T, backend string) graph.Store {
	t.Helper()
	if backend == "memory" {
		return graph.New()
	}
	s, err := store_sqlite.Open(filepath.Join(t.TempDir(), "review.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	return s
}

func TestGoModuleImportFullPassNewConsumer(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			g := goImportReviewStore(t, backend)
			g.AddNode(&graph.Node{ID: "b/lib.go", Name: "lib.go", Kind: graph.KindFile, FilePath: "b/lib.go", RepoPrefix: "b", Language: "go"})
			state := &GoModuleDepRetry{}
			resolve := func() {
				cr := NewCrossRepo(g)
				cr.SetGoModules(map[string]string{"b": "example.org/lib"})
				cr.SetGoModuleDepRetry(state, 1)
				_, err := cr.ResolveAllContext(t.Context())
				require.NoError(t, err)
			}
			for _, repo := range []string{"a", "c"} {
				file := repo + "/main.go"
				g.AddBatch([]*graph.Node{
					{ID: file, Name: "main.go", Kind: graph.KindFile, FilePath: file, RepoPrefix: repo, Language: "go"},
					{ID: file + "::import::example.org/lib", Name: "lib", Kind: graph.KindImport, FilePath: file, RepoPrefix: repo, Language: "go", StartLine: 2, Meta: map[string]any{"path": "example.org/lib"}},
				}, []*graph.Edge{{From: file, To: "dep::example.org/lib", Kind: graph.EdgeImports, FilePath: file, Line: 2}})
				resolve()
				rows := g.GetOutEdges(file)
				require.Len(t, rows, 1)
				assert.Equal(t, "b/lib.go", rows[0].To, "a full pass must include current import identities even with shared retry state")
				assert.True(t, rows[0].CrossRepo)
			}
		})
	}
}

func TestGoModuleImportUnconfiguredRetryState(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, route := range []string{"full", "consumer", "provider", "retry"} {
			for _, configured := range []bool{false, true} {
				name := backend + "/" + route + "/unconfigured"
				if configured {
					name = backend + "/" + route + "/removed_modules"
				}
				t.Run(name, func(t *testing.T) {
					g := goImportReviewStore(t, backend)
					g.AddBatch([]*graph.Node{
						{ID: "a/main.go", Name: "main.go", Kind: graph.KindFile, FilePath: "a/main.go", RepoPrefix: "a", Language: "go"},
						{ID: "a/main.go::Caller", Name: "Caller", Kind: graph.KindFunction, FilePath: "a/main.go", RepoPrefix: "a", Language: "go"},
						{ID: "b/lib.go", Name: "lib.go", Kind: graph.KindFile, FilePath: "b/lib.go", RepoPrefix: "b", Language: "go"},
						{ID: "b/lib.go::Hello", Name: "Hello", Kind: graph.KindFunction, FilePath: "b/lib.go", RepoPrefix: "b", Language: "go"},
					}, []*graph.Edge{{From: "a/main.go::Caller", To: "dep::example.org/lib::Hello", Kind: graph.EdgeCalls, FilePath: "a/main.go", Line: 3}})
					bound := NewCrossRepo(g)
					bound.SetGoModules(map[string]string{"b": "example.org/lib"})
					_, err := bound.ResolveAllContext(t.Context())
					require.NoError(t, err)
					require.Equal(t, "b/lib.go::Hello", g.GetOutEdges("a/main.go::Caller")[0].To)
					cr := NewCrossRepo(g)
					if configured {
						cr.SetGoModules(map[string]string{})
						cr.SetGoModuleDepRetry(&GoModuleDepRetry{}, 1)
					}
					switch route {
					case "consumer":
						cr.ResolveMutationFrontiers([]string{"a/main.go"}, nil, nil)
					case "provider":
						cr.ResolveMutationFrontiers(nil, nil, []string{"b/lib.go"})
					case "retry":
						require.NoError(t, cr.RetryGoModuleDeps(t.Context()))
					}
					_, err = cr.ResolveAllContext(t.Context())
					require.NoError(t, err)
					var rows []*graph.Edge
					for _, edge := range g.GetOutEdges("a/main.go::Caller") {
						if edge.Kind == graph.EdgeCalls {
							rows = append(rows, edge)
						}
					}
					require.Len(t, rows, 1)
					if configured {
						assert.Equal(t, "dep::example.org/lib::Hello", rows[0].To)
						assert.False(t, rows[0].CrossRepo)
					} else {
						assert.Equal(t, "b/lib.go::Hello", rows[0].To)
						assert.True(t, rows[0].CrossRepo)
						assert.Nil(t, cr.goModuleRetry, "no module context must not allocate retry state")
					}
				})
			}
		}
	}
}

func TestGoModuleImportReplacementDirectory(t *testing.T) {
	for _, tc := range []struct{ name, directory, path, want string }{
		{"local", "a/third_party/library", "example.org/library", "a/third_party/library/lib.go"},
		{"sub_package", "a/third_party/library", "example.org/library/sub", "a/third_party/library/sub/a.go"},
		{"denied", "", "example.org/library", ""},
		{"missing", "a/missing", "example.org/library", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := graph.New()
			for _, file := range []string{"a/third_party/library/lib.go", "a/third_party/library/sub/a.go", "b/lib.go", "b/sub/a.go"} {
				repo := file[:1]
				g.AddNode(&graph.Node{ID: file, Name: file, Kind: graph.KindFile, FilePath: file, RepoPrefix: repo, Language: "go"})
			}
			cr := NewCrossRepo(g)
			cr.SetGoModules(map[string]string{"a": "example.org/caller", "b": "example.org/library"})
			cr.SetGoModuleReplacements(map[string][]GoModuleReplacement{"a": {{Module: "example.org/library", Directory: tc.directory}}})
			cr.goModuleImportState()
			match := cr.goModuleImportCandidate(tc.path, "a", "")
			if tc.want == "" {
				assert.Nil(t, match, "a replacement must never fall through to the remote module")
			} else {
				require.NotNil(t, match)
				assert.Equal(t, tc.want, match.ID)
				assert.Equal(t, "a", match.RepoPrefix)
			}
		})
	}
}
