package resolver_test

import (
	"context"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/resolver"
	"github.com/zzet/gortex/internal/search"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

type goDeferralStore struct {
	graph.Store
	rechecks    int
	importSeeds int
}

func (s *goDeferralStore) RepoNodeIdentitiesSeq(repos []string, kinds ...graph.NodeKind) iter.Seq[graph.RepoNodeIdentity] {
	if resolver.IsGoModuleBindingRecheckForTest(kinds) {
		s.rechecks++
	}
	if repos == nil && len(kinds) == 1 && kinds[0] == graph.KindImport {
		s.importSeeds++
	}
	return graph.RepoNodeIdentitiesSeq(s.Store, repos, kinds...)
}

func newGoDeferralIndexer(t *testing.T, backend string) (*indexer.MultiIndexer, *goDeferralStore, *observer.ObservedLogs) {
	t.Helper()
	var store graph.Store = graph.New()
	if backend == "sqlite" {
		db, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, db.Close()) })
		store = db
	}
	g := &goDeferralStore{Store: store}
	cm, err := config.NewConfigManager(filepath.Join(t.TempDir(), "config.yaml"))
	require.NoError(t, err)
	reg := parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())
	core, logs := observer.New(zap.InfoLevel)
	mi := indexer.NewMultiIndexer(g, reg, search.NewNull(), cm, zap.New(core))
	t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
	mi.BeginBatch()
	for _, repo := range []struct{ name, module string }{{"provider", "example.org/library"}, {"consumer", "example.org/app"}} {
		root := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(root, "go.mod"), []byte("module "+repo.module+"\n"), 0600))
		_, err := mi.TrackRepo(config.RepoEntry{Path: root, Name: repo.name, Workspace: "shared"})
		require.NoError(t, err)
	}
	mi.ResetBatch()
	// Persisted graph identities are enough for the restart recovery boundary;
	// no source parse or dependency fetch is needed to construct this fixture.
	g.AddNode(&graph.Node{ID: "provider/lib.go", FilePath: "provider/lib.go", Name: "lib.go", Kind: graph.KindFile, Language: "go", RepoPrefix: "provider", WorkspaceID: "shared"})
	for i := range 2 {
		file := fmt.Sprintf("consumer/f%d.go", i)
		g.AddBatch([]*graph.Node{
			{ID: file, FilePath: file, Kind: graph.KindFile, Language: "go", RepoPrefix: "consumer", WorkspaceID: "shared"},
			{ID: file + "::import::example.org/library", FilePath: file, Kind: graph.KindImport, Language: "go", RepoPrefix: "consumer", WorkspaceID: "shared", StartLine: 1, Meta: map[string]any{"path": "example.org/library"}},
		}, []*graph.Edge{{From: file, FilePath: file, To: "dep::example.org/library", Kind: graph.EdgeImports, Line: 1}})
	}
	g.rechecks = 0
	g.importSeeds = 0
	logs.TakeAll()
	return mi, g, logs
}

func requireGoDeferralImports(t *testing.T, g graph.Store) {
	t.Helper()
	for i := range 2 {
		file := fmt.Sprintf("consumer/f%d.go", i)
		require.Contains(t, g.GetOutEdges(file), &graph.Edge{From: file, FilePath: file, To: "provider/lib.go", Kind: graph.EdgeImports, Line: 1, CrossRepo: true, Meta: map[string]any{"go_import": "example.org/library", "go_import_fallback": "dep::example.org/library"}})
	}
}

func TestGoModuleMutationDeferralRunsFullFallback(t *testing.T) {
	t.Cleanup(resolver.SetGoModuleImportRecoveryBudgetForTest(1))
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			mi, g, logs := newGoDeferralIndexer(t, backend)
			require.NoError(t, mi.RunPreEnrichResolveFiles(context.Background(), []string{"consumer/f0.go"}, nil))
			require.Equal(t, 1, logs.FilterMessage("Go module import identity recheck deferred to full pass").Len(), "the committed test must exercise the bounded refusal")
			require.Equal(t, 1, logs.FilterMessage("cross-repo resolve: completing deferred Go module imports").Len())
			requireGoDeferralImports(t, g)
			require.Equal(t, 2, g.rechecks, "one bounded recheck and one full continuation")
			require.Equal(t, 2, g.importSeeds, "the bounded revision seeds once and the full continuation reseeds once")
			for range 2 {
				require.NoError(t, mi.RunPreEnrichResolveFiles(context.Background(), []string{"consumer/f0.go"}, nil))
			}
			require.Equal(t, 2, g.rechecks, "later Go saves must not repeat the all-repository recheck")
			require.Equal(t, 2, g.importSeeds, "later saves must not start another full pass or reseed")
			require.Equal(t, 1, logs.FilterMessage("cross-repo resolve: completing deferred Go module imports").Len())
			requireGoDeferralImports(t, g)
		})
	}
}

func TestGoModuleProviderSkipsDoNotRunFullFallback(t *testing.T) {
	t.Cleanup(resolver.SetGoModuleImportRecoveryBudgetForTest(1))
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			mi, g, logs := newGoDeferralIndexer(t, backend)
			mi.RunGlobalResolve()
			requireGoDeferralImports(t, g)
			g.rechecks = 0
			g.importSeeds = 0
			logs.TakeAll()
			for _, file := range []string{"provider/lib.go", "provider/lib.go", "consumer/f0.go"} {
				require.NoError(t, mi.RunPreEnrichResolveFiles(context.Background(), []string{file}, nil))
			}
			require.Equal(t, 2, logs.FilterMessage("cross-repo resolve: provider consumers stay stale until next full pass").Len())
			require.Zero(t, logs.FilterMessage("Go module import identity recheck deferred to full pass").Len())
			require.Zero(t, logs.FilterMessage("cross-repo resolve: completing deferred Go module imports").Len())
			require.Zero(t, g.rechecks, "two provider saves and a third Go save must not recheck all repositories")
			require.Zero(t, g.importSeeds, "a full pass always reseeds imports, so no reseed proves no full pass ran")
			requireGoDeferralImports(t, g)
		})
	}
}

func TestGoModuleDeferralCounterExactBindingKinds(t *testing.T) {
	g := &goDeferralStore{Store: graph.New()}
	for range g.RepoNodeIdentitiesSeq(nil, graph.KindFunction, graph.KindType, graph.KindInterface, graph.KindFile) {
	}
	require.Equal(t, 1, g.rechecks)
	g.rechecks = 0
	for range g.RepoNodeIdentitiesSeq(nil, graph.KindFunction, graph.KindType, graph.KindInterface, graph.KindImport) {
	}
	require.Zero(t, g.rechecks, "four unrelated kinds must not count as the binding recheck")
}
