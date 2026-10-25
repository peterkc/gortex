package indexer

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/search"
	"go.uber.org/zap"
)

func revisionColdGraph(t *testing.T, backend string, repos []config.RepoEntry) graph.Store {
	t.Helper()
	var g graph.Store = graph.New()
	if backend == "sqlite" {
		db, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, db.Close()) })
		g = db
	}
	cm := newTestConfigManager(t)
	cm.Global().Repos = repos
	reg := newTestRegistry()
	reg.Register(languages.NewTypeScriptExtractor())
	mi := NewMultiIndexer(g, reg, search.NewNull(), cm, zap.NewNop())
	t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
	_, err := mi.IndexAll()
	require.NoError(t, err)
	return g
}

func revisionCallTargets(g graph.Store, caller string, kind graph.EdgeKind) []string {
	var targets []string
	for _, e := range g.GetOutEdges(caller) {
		if e.Kind == kind {
			targets = append(targets, e.To)
		}
	}
	return targets
}

// Given nested package calls on one line, when provider availability changes,
// then every call must match a cold index, independently of track order.
func TestGoModuleSameLineCallsColdEquivalent(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, mode := range []string{"provider_first", "caller_first", "rename_back", "retrack"} {
			t.Run(backend+"/"+mode, func(t *testing.T) {
				mi, g, repos := goModuleLifecycleFixture(t, backend)
				writeFile(t, filepath.Join(repos[0].Path, "main.go"), "package caller\nimport library \"example.org/library\"\nfunc Caller() { library.Hello(library.World()) }\n")
				writeFile(t, filepath.Join(repos[1].Path, "lib.go"), "package library\nfunc Hello(v int) {}\nfunc World() int { return 1 }\n")
				order := []config.RepoEntry{repos[1], repos[0]}
				if mode == "caller_first" {
					order[0], order[1] = order[1], order[0]
				}
				for _, entry := range order {
					_, err := mi.TrackRepoCtx(t.Context(), entry)
					require.NoError(t, err)
				}
				if mode == "rename_back" {
					manifest := filepath.Join(repos[1].Path, "go.mod")
					writeFile(t, manifest, "module example.org/renamed\n")
					mi.goModuleChecked = time.Time{}
					mi.RunGlobalResolve()
					writeFile(t, manifest, "module example.org/library\n")
					mi.goModuleChecked = time.Time{}
					mi.RunGlobalResolve()
				}
				if mode == "retrack" {
					_, _, err := mi.UntrackRepoChecked(t.Context(), "b")
					require.NoError(t, err)
					mi.RunGlobalResolve()
					_, err = mi.TrackRepoCtx(t.Context(), repos[1])
					require.NoError(t, err)
				}
				cold := revisionColdGraph(t, backend, repos[:2])
				expected := []string{"b/lib.go::Hello", "b/lib.go::World"}
				require.ElementsMatch(t, expected, revisionCallTargets(cold, "a/main.go::Caller", graph.EdgeCalls))
				assert.ElementsMatch(t, expected, revisionCallTargets(g, "a/main.go::Caller", graph.EdgeCalls))
			})
		}
	}
}

// Given a bound provider or a refused tie, when either untrack path removes it,
// then surviving callers settle immediately to the cold graph without a save.
func TestGoModuleUntrackSettlesColdEquivalent(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, mode := range []string{"untrack", "purge", "tie"} {
			t.Run(backend+"/"+mode, func(t *testing.T) {
				mi, g, repos := goModuleLifecycleFixture(t, backend)
				order := []config.RepoEntry{repos[1], repos[0]}
				if mode == "tie" {
					order = append(order, repos[2])
				}
				for _, entry := range order {
					_, err := mi.TrackRepoCtx(t.Context(), entry)
					require.NoError(t, err)
				}
				if mode == "purge" {
					_, _, err := mi.purgeRepoChecked(t.Context(), "b", nil)
					require.NoError(t, err)
				} else {
					_, _, err := mi.UntrackRepoChecked(t.Context(), "b")
					require.NoError(t, err)
				}
				coldRepos := repos[:1]
				want := "external-call::dep::example.org/library"
				if mode == "tie" {
					coldRepos = []config.RepoEntry{repos[0], repos[2]}
					want = "fork/lib.go::Hello"
				}
				cold := revisionColdGraph(t, backend, coldRepos)
				require.Equal(t, want, goModuleLifecycleCall(t, cold).To)
				assert.Equal(t, want, goModuleLifecycleCall(t, g).To)
			})
		}
	}
}

// Given a module-bound caller, when a provider is reparsed or loses its export,
// then its settled payload has no restub handoff or stale binding markers.
func TestGoModuleRestubPayloadColdEquivalent(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, mode := range []string{"rebind", "demote", "disabled"} {
			t.Run(backend+"/"+mode, func(t *testing.T) {
				mi, g, repos := goModuleLifecycleFixture(t, backend)
				if mode == "disabled" {
					writeFile(t, filepath.Join(repos[0].Path, ".gortex.yaml"), "index:\n  synthesize_external_calls: false\n")
				}
				for _, entry := range []config.RepoEntry{repos[1], repos[0]} {
					_, err := mi.TrackRepoCtx(t.Context(), entry)
					require.NoError(t, err)
				}
				src := "package library\nfunc Hello() { println(1) }\n"
				if mode != "rebind" {
					src = "package library\nfunc Other() {}\n"
				}
				writeFile(t, filepath.Join(repos[1].Path, "lib.go"), src)
				require.NoError(t, mi.GetIndexer("b").IndexFile(filepath.Join(repos[1].Path, "lib.go")))
				cold := revisionColdGraph(t, backend, repos[:2])
				edge, want := goModuleLifecycleCall(t, g), goModuleLifecycleCall(t, cold)
				require.Equal(t, want.To, edge.To)
				for _, key := range []string{"restub_prev_to", "restub_prev_origin", "restub_prev_tier", "restub_prev_conf"} {
					assert.NotContains(t, edge.Meta, key)
				}
				assert.Equal(t, want.Meta["go_import"], edge.Meta["go_import"])
				assert.Equal(t, want.Meta["npm_import"], edge.Meta["npm_import"])
				assert.Equal(t, want.Confidence, edge.Confidence)
				assert.Equal(t, want.Origin, edge.Origin)
			})
		}
	}
}
