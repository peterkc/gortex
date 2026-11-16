package resolver

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestCrossRepoDerivedReadChangeMatchesAllCallerForms(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, caller := range []string{"full", "skip", "repos", "files", "mutation_source", "mutation_incident", "mutation_both", "reindexes"} {
			t.Run(backend+"/"+caller, func(t *testing.T) {
				// Given all base kinds, duplicates and excluded relationships,
				// when each caller runs through ownership change and deletion/re-add,
				// then complete durable edge sets equal the old fallback path.
				open := func() graph.Store {
					if backend == "memory" {
						return graph.New()
					}
					base := openFrameworkScopedSQLiteStore(t).(*store_sqlite.Store)
					for _, generation := range []int64{1, 3} {
						seedDerivedCrossRepoRows(base.AtGeneration(generation), "stale", true)
					}
					return base.AtGeneration(2)
				}
				store, legacy := open(), open()
				for _, phase := range []struct {
					name, owner string
					present bool
					want int
				}{
					{"initial", "b", true, 4},
					{"changed_target_unchanged_source", "c", true, 4},
					{"same_repo_no_output", "a", true, 0},
					{"deleted", "b", false, 0},
					{"readded", "b", true, 4},
				} {
					t.Run(phase.name, func(t *testing.T) {
						// A target-file edit evicts both edge layers; source text
						// stays unchanged and its resolved base edges are re-landed.
						for _, g := range []graph.Store{store, legacy} {
							g.EvictFile("b/target.go")
							seedDerivedCrossRepoRows(g, phase.owner, phase.present)
						}
						var repos, sources, incidents []string
						switch caller {
						case "repos":
							repos = []string{phase.owner, phase.owner, ""}
						case "files", "mutation_both":
							sources = []string{"edge-source.go", "edge-source.go", ""}
							incidents = []string{"b/target.go", "b/target.go", ""}
						case "mutation_source":
							sources = []string{"edge-source.go", "edge-source.go", ""}
						case "mutation_incident":
							incidents = []string{"b/target.go", "b/target.go", ""}
						}
						var repoSet, sourceSet, incidentSet map[string]struct{}
						if repos != nil {
							repoSet = stringSet(repos)
						}
						if sources != nil || incidents != nil {
							sourceSet, incidentSet = stringSet(sources), stringSet(incidents)
						}
						rows := crossRepoCandidatesFallbackScopes(legacy, graph.BaseKindsForCrossRepo(), repoSet, sourceSet, incidentSet)
						require.Len(t, rows, phase.want, "fixture must expose the complete expected relationship set")
						want, wantErr := materializeCrossRepoCandidates(legacy, rows)
						for replay := 0; replay < 2; replay++ {
							got, err := runDerivedCrossRepoCaller(store, caller, repos, sources, incidents)
							require.NoError(t, wantErr)
							require.NoError(t, err)
							require.Equal(t, phase.want, got)
							require.Equal(t, want, got)
							require.ElementsMatch(t, legacy.AllEdges(), store.AllEdges(), "all fields, flags, identities and metadata must match")
						}
					})
				}
			})
		}
	}
}

func seedDerivedCrossRepoRows(g graph.Store, targetRepo string, present bool) {
	nodes := []*graph.Node{
		{ID: "a/source.go::source", Kind: graph.KindFunction, FilePath: "a/source.go", RepoPrefix: "a"},
		{ID: "a/source.go::local", Kind: graph.KindFunction, FilePath: "a/source.go", RepoPrefix: "a"},
		{ID: "loose", Kind: graph.KindFunction, FilePath: "loose.go"},
	}
	if present {
		nodes = append(nodes, &graph.Node{ID: "b/target.go::target", Kind: graph.KindFunction, FilePath: "b/target.go", RepoPrefix: targetRepo})
	}
	var edges []*graph.Edge
	for i, kind := range graph.BaseKindsForCrossRepo() {
		edge := &graph.Edge{From: nodes[0].ID, To: "b/target.go::target", Kind: kind, FilePath: "edge-source.go", Line: i+1, Confidence: 0.8, Origin: graph.OriginASTResolved, Meta: map[string]any{"opaque": "retain"}}
		copy := *edge
		edges = append(edges, edge, &copy)
	}
	for i, target := range []string{"a/source.go::local", "unresolved::missing", "external::pkg", "loose"} {
		edges = append(edges, &graph.Edge{From: nodes[0].ID, To: target, Kind: graph.EdgeCalls, FilePath: "edge-source.go", Line: 10+i})
	}
	edges = append(edges, &graph.Edge{From: nodes[0].ID, To: "b/target.go::target", Kind: graph.EdgeReferences, FilePath: "edge-source.go", Line: 20})
	g.AddBatch(nodes, edges)
}

func runDerivedCrossRepoCaller(g graph.Store, caller string, repos, sources, incidents []string) (int, error) {
	switch caller {
	case "full":
		return DetectCrossRepoEdges(g)
	case "skip":
		count, _, err := DetectCrossRepoEdgesWithSkip(g)
		return count, err
	case "repos":
		return DetectCrossRepoEdgesForRepos(g, repos)
	case "files":
		return DetectCrossRepoEdgesForFiles(g, append(slices.Clone(sources), incidents...))
	case "mutation_source", "mutation_incident", "mutation_both":
		return DetectCrossRepoEdgesForMutationFiles(g, sources, incidents)
	case "reindexes":
		var batch []graph.EdgeReindex
		for edge := range graph.EdgesLightSeq(g, graph.BaseKindsForCrossRepo()...) {
			batch = append(batch, graph.EdgeReindex{Edge: edge})
		}
		return DetectCrossRepoEdgesForReindexes(g, batch)
	default:
		panic("unknown cross-repo test caller")
	}
}
