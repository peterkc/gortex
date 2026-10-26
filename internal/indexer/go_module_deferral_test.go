package indexer

import (
	"fmt"
	"iter"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

type goDeferralStore struct {
	graph.Store
	rechecks int
}

func (s *goDeferralStore) RepoNodeIdentitiesSeq(repos []string, kinds ...graph.NodeKind) iter.Seq[graph.RepoNodeIdentity] {
	if len(kinds) == 4 {
		s.rechecks++
	}
	return graph.RepoNodeIdentitiesSeq(s.Store, repos, kinds...)
}

func TestGoModuleMutationDeferralRunsFullFallback(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			provider, consumer := t.TempDir(), t.TempDir()
			writeWorkspaceFile(t, provider, "go.mod", "module example.org/library\n")
			writeWorkspaceFile(t, consumer, "go.mod", "module example.org/app\n")
			g := &goDeferralStore{Store: pythonLocalStore(t, backend)}
			g.AddNode(&graph.Node{ID: "provider/lib.go", FilePath: "provider/lib.go", Name: "lib.go", Kind: graph.KindFile, Language: "go", RepoPrefix: "provider", WorkspaceID: "shared"})
			for i := range 2 {
				file := fmt.Sprintf("consumer/f%d.go", i)
				g.AddBatch([]*graph.Node{
					{ID: file, FilePath: file, Kind: graph.KindFile, Language: "go", RepoPrefix: "consumer", WorkspaceID: "shared"},
					{ID: file + "::import::example.org/library", FilePath: file, Kind: graph.KindImport, Language: "go", RepoPrefix: "consumer", WorkspaceID: "shared", StartLine: 1, Meta: map[string]any{"path": "example.org/library"}},
				}, []*graph.Edge{{From: file, FilePath: file, To: "dep::example.org/library", Kind: graph.EdgeImports, Line: 1}})
			}
			core, logs := observer.New(zap.InfoLevel)
			mi := &MultiIndexer{graph: g, logger: zap.New(core), repos: map[string]*RepoMetadata{"provider": {RootPath: provider}, "consumer": {RootPath: consumer}}}
			mi.runCrossRepoResolveMutationFrontiers(nil, nil, []string{"consumer/f0.go"}, nil)
			deferrals := logs.FilterMessage("Go module import identity recheck deferred to full pass").Len()
			if os.Getenv("GX_SFR16_SMALL_GO_BUDGET") == "1" {
				require.Equal(t, 1, deferrals, "the native budget overlay must exercise refusal")
			}
			require.Equal(t, deferrals, logs.FilterMessage("cross-repo resolve: completing deferred Go module imports").Len(), "every independent Go deferral needs a full continuation")
			for i := range 2 {
				require.Contains(t, g.GetOutEdges(fmt.Sprintf("consumer/f%d.go", i)), &graph.Edge{From: fmt.Sprintf("consumer/f%d.go", i), FilePath: fmt.Sprintf("consumer/f%d.go", i), To: "provider/lib.go", Kind: graph.EdgeImports, Line: 1, CrossRepo: true, Meta: map[string]any{"go_import": "example.org/library", "go_import_fallback": "dep::example.org/library"}})
			}
			require.Equal(t, 1+deferrals, g.rechecks, "one bounded identity recheck and, only after refusal, one full recovery recheck")
			before := g.rechecks
			for range 2 {
				mi.runCrossRepoResolveMutationFrontiers(nil, nil, []string{"consumer/f0.go"}, nil)
			}
			require.Equal(t, before, g.rechecks, "later Go mutations must not repeat the all-repo recheck")
			require.Equal(t, deferrals, logs.FilterMessage("cross-repo resolve: completing deferred Go module imports").Len(), "completed revisions must not request another full pass")
		})
	}
}
