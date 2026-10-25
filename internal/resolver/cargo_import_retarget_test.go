package resolver

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

type cargoRetargetWriteStore struct {
	graph.Store
	writes int
}

func (s *cargoRetargetWriteStore) ReindexEdges(batch []graph.EdgeReindex) {
	s.writes += len(batch)
	s.Store.ReindexEdges(batch)
}

func TestCargoRetargetOnlyMarkedProviderImports(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, scenario := range []string{"unresolved_marker", "external_marker", "unmarked", "other_provider", "already_correct", "disabled", "light_projection", "multiple_enabled", "nil_input", "wrong_kind"} {
			t.Run(backend+"/"+scenario, func(t *testing.T) {
				var native graph.Store = graph.New()
				if backend == "sqlite" {
					db, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
					require.NoError(t, err)
					t.Cleanup(func() { require.NoError(t, db.Close()) })
					native = db
				}
				g := &cargoRetargetWriteStore{Store: native}
				file, root, module := "consumer/src/lib.rs", "provider/src/lib.rs", "provider/src/util.rs"
				for _, path := range []string{file, root, module, "other/src/lib.rs"} {
					g.AddNode(&graph.Node{ID: path, FilePath: path, Kind: graph.KindFile, RepoPrefix: graph.RepoPrefixOfID(path), WorkspaceID: "shared", Language: "rust"})
				}
				original := "unresolved::import::other_crate/util"
				if scenario == "external_marker" {
					original = "external::other_crate/util"
				}
				edge := &graph.Edge{From: file, FilePath: file, Kind: graph.EdgeImports, To: root, CrossRepo: true, Line: 1,
					Meta: map[string]any{CargoImportStubMeta: original}}
				if scenario == "unmarked" {
					edge.Meta = nil
				}
				if scenario == "other_provider" {
					edge.To = "other/src/lib.rs"
				}
				if scenario == "already_correct" {
					edge.To = module
				}
				priorTarget := edge.To
				g.AddEdge(edge)
				cr := NewCrossRepo(g)
				cr.SetCargoCrateLookup(func(string, string) []CargoCrate { return []CargoCrate{{Root: root, Name: "other_crate"}} })
				if scenario == "multiple_enabled" {
					cr.SetCargoImportRetargetRepo("other", "provider")
				} else if scenario != "disabled" {
					cr.SetCargoImportRetargetRepo("provider")
				}
				stats := &CrossRepoStats{ByRepo: map[string]int{}}
				edges := slices.Values([]*graph.Edge{edge})
				switch scenario {
				case "light_projection":
					edges = graph.EdgesLightSeq(g, graph.EdgeImports)
				case "nil_input":
					edges = slices.Values([]*graph.Edge{nil})
				case "wrong_kind":
					projected := *edge
					projected.Kind = graph.EdgeCalls
					edges = slices.Values([]*graph.Edge{&projected})
				}
				require.NoError(t, cr.resolveCargoImports(context.Background(), edges, stats))
				imports := outEdgesOfKind(g, file, graph.EdgeImports)
				require.Len(t, imports, 1)
				changed := scenario == "unresolved_marker" || scenario == "external_marker" || scenario == "light_projection" || scenario == "multiple_enabled"
				if changed {
					require.Equal(t, module, imports[0].To)
					require.Equal(t, 1, g.writes)
				} else {
					require.Equal(t, priorTarget, imports[0].To)
					require.Zero(t, g.writes)
				}
				if scenario != "unmarked" {
					require.Equal(t, original, imports[0].Meta[CargoImportStubMeta])
				}
				priorWrites := g.writes
				if scenario == "nil_input" || scenario == "wrong_kind" {
					require.Zero(t, stats.Resolved)
					return
				}
				require.NoError(t, cr.resolveCargoImports(context.Background(), slices.Values(imports), stats))
				require.Equal(t, priorWrites, g.writes, "an unchanged native match must not reindex")
			})
		}
	}
}

func TestCargoRevisitedImportRestoresStubOnRefusal(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, reason := range []string{"undeclared", "ambiguous"} {
			for _, stub := range []string{"external::other_crate/util", "unresolved::import::other_crate/util"} {
				t.Run(backend+"/"+reason+"/"+stub, func(t *testing.T) {
					g := goImportReviewStore(t, backend)
					for _, file := range []string{"consumer/src/lib.rs", "provider/src/lib.rs", "provider/src/util.rs", "fork/src/lib.rs"} {
						g.AddNode(&graph.Node{ID: file, FilePath: file, Kind: graph.KindFile, RepoPrefix: graph.RepoPrefixOfID(file), Language: "rust"})
					}
					g.AddEdge(&graph.Edge{From: "consumer/src/lib.rs", FilePath: "consumer/src/lib.rs", To: "provider/src/util.rs", Kind: graph.EdgeImports, Line: 1, CrossRepo: true, Meta: map[string]any{CargoImportStubMeta: stub}})
					cr := NewCrossRepo(g)
					cr.SetCargoImportRetargetRepo("provider")
					cr.SetCargoCrateLookup(func(string, string) []CargoCrate {
						if reason == "undeclared" {
							return nil
						}
						return []CargoCrate{{Root: "provider/src/lib.rs", Name: "other_crate"}, {Root: "fork/src/lib.rs", Name: "other_crate"}}
					})
					require.NoError(t, cr.resolveCargoImports(t.Context(), graph.EdgesLightSeq(g, graph.EdgeImports), &CrossRepoStats{ByRepo: map[string]int{}}))
					imports := outEdgesOfKind(g, "consumer/src/lib.rs", graph.EdgeImports)
					require.Len(t, imports, 1)
					require.Equal(t, stub, imports[0].To)
					require.False(t, imports[0].CrossRepo)
				})
			}
		}
	}
}
