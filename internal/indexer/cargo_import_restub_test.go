package indexer

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/resolver"
)

func TestCargoRestubLeavesUnmarkedImportsUnchanged(t *testing.T) {
	for _, file := range []string{"provider/src/lib.rs", "provider/main.go"} {
		t.Run(file, func(t *testing.T) {
			g := graph.New()
			g.AddNode(&graph.Node{ID: file, Kind: graph.KindFile, FilePath: file, RepoPrefix: "provider"})
			edge := &graph.Edge{From: "consumer/source", FilePath: "consumer/source", To: file, Kind: graph.EdgeImports, CrossRepo: true}
			if file == "provider/main.go" {
				edge.Meta = map[string]any{resolver.CargoImportStubMeta: "external::other_crate/action"}
			}
			g.AddEdge(edge)
			require.Empty(t, restubTrackedCargoRepoIncoming(g, "provider"))
			require.Equal(t, file, edge.To)
			require.True(t, edge.CrossRepo)
		})
	}
}

func TestCargoRestubKeepsSameRepoCallsOnNativePath(t *testing.T) {
	for _, mode := range []string{"point", "batch"} {
		t.Run(mode, func(t *testing.T) {
			g := graph.New()
			file, definition, caller := "repo/a.rs", "repo/a.rs::foo", "repo/b.rs::run"
			g.AddNode(&graph.Node{ID: file, FilePath: file, Kind: graph.KindFile, RepoPrefix: "repo"})
			g.AddNode(&graph.Node{ID: definition, FilePath: file, Name: "foo", Kind: graph.KindFunction, RepoPrefix: "repo"})
			g.AddNode(&graph.Node{ID: caller, FilePath: "repo/b.rs", Name: "run", Kind: graph.KindFunction, RepoPrefix: "repo"})
			edge := &graph.Edge{From: caller, FilePath: "repo/b.rs", Kind: graph.EdgeCalls, To: definition,
				Meta: map[string]any{resolver.CargoCallStubMeta: "unresolved::*.foo"}}
			g.AddEdge(edge)
			if mode == "point" {
				idx := &Indexer{graph: g}
				idx.restubIncomingRefs(file)
			} else {
				stages := []*incrementalBatchStage{{graphPath: file, priorNodes: g.GetFileNodes(file)}}
				view := loadIncrementalPriorView(g, stages)
				restubIncomingRefsFromView(g, stages, view)
			}
			require.Equal(t, "unresolved::foo", edge.To)
			require.False(t, edge.CrossRepo)
		})
	}
}

func TestCargoConsumerFrontierIncludesUnresolvedImportStubs(t *testing.T) {
	provider, consumer := t.TempDir(), t.TempDir()
	writeWorkspaceFile(t, provider, "Cargo.toml", "[package]\nname='library'\nversion='1.0.0'\n")
	writeWorkspaceFile(t, consumer, "Cargo.toml", "[package]\nname='app'\nversion='1.0.0'\n[dependencies]\nlibrary='1'\n")
	g := graph.New()
	g.AddNode(&graph.Node{ID: "provider/src/lib.rs", FilePath: "provider/src/lib.rs", Kind: graph.KindFile, RepoPrefix: "provider", Language: "rust"})
	for _, tc := range []struct{ file, target string }{
		{"consumer/a.rs", "external::library/action"},
		{"consumer/b.rs", "unresolved::import::library/action"},
		{"consumer/c.rs", "unresolved::import::unknown/action"},
	} {
		g.AddEdge(&graph.Edge{From: tc.file, FilePath: tc.file, To: tc.target, Kind: graph.EdgeImports})
	}
	mi := &MultiIndexer{graph: g, repos: map[string]*RepoMetadata{"provider": {RootPath: provider}, "consumer": {RootPath: consumer}}}
	require.Equal(t, []string{"consumer/a.rs", "consumer/b.rs"}, mi.cargoConsumerFilesForRepo("provider", false))
}

func TestCargoRestubClearsMatchedModuleEvidence(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, mode := range []string{"untrack", "file_eviction"} {
			t.Run(backend+"/"+mode, func(t *testing.T) {
				// Given a tracked import with its previously matched module path.
				g := cargoWarmStore(t, backend)
				file, target, stub := "consumer/src/lib.rs", "provider/src/util.rs", "external::other_crate/util"
				for _, path := range []string{file, target} {
					g.AddNode(&graph.Node{ID: path, FilePath: path, Kind: graph.KindFile, RepoPrefix: graph.RepoPrefixOfID(path), Language: "rust"})
				}
				g.AddEdge(&graph.Edge{From: file, FilePath: file, To: target, Kind: graph.EdgeImports, Line: 1, CrossRepo: true, Meta: map[string]any{resolver.CargoImportStubMeta: stub, resolver.CargoImportModuleMeta: "other_crate::util"}})
				// When untracking or file eviction restubs the import.
				var consumers []string
				if mode == "untrack" {
					consumers = restubTrackedCargoRepoIncoming(g, "provider")
				} else {
					consumers = restubTrackedCargoImportsFromView(g, map[string]struct{}{target: {}}, g.GetInEdgesByNodeIDs([]string{target}), false)
				}
				// Then its persisted state has the stub, not matched-module evidence.
				require.Equal(t, []string{file}, consumers)
				imports := g.GetOutEdges(file)
				require.Len(t, imports, 1)
				require.Equal(t, stub, imports[0].To)
				require.False(t, imports[0].CrossRepo)
				require.NotContains(t, imports[0].Meta, resolver.CargoImportModuleMeta)
				require.Equal(t, stub, imports[0].Meta[resolver.CargoImportStubMeta])
			})
		}
	}
}
