package resolver

import (
	"context"
	"fmt"
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
		for _, scenario := range []string{"unresolved_marker", "external_marker", "unmarked", "other_provider", "already_correct", "already_correct_legacy", "disabled", "light_projection", "multiple_enabled", "nil_input", "wrong_kind"} {
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
				if scenario == "already_correct" || scenario == "already_correct_legacy" {
					edge.To = module
					if scenario == "already_correct" {
						edge.Meta[CargoImportModuleMeta] = "other_crate::util"
					}
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
				_, err := cr.resolveCargoImports(context.Background(), edges, stats)
				require.NoError(t, err)
				imports := outEdgesOfKind(g, file, graph.EdgeImports)
				require.Len(t, imports, 1)
				changed := scenario == "unresolved_marker" || scenario == "external_marker" || scenario == "light_projection" || scenario == "multiple_enabled" || scenario == "already_correct_legacy"
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
				if changed || scenario == "already_correct" {
					require.Equal(t, "other_crate::util", imports[0].Meta[CargoImportModuleMeta])
				}
				if scenario == "already_correct_legacy" {
					require.Zero(t, stats.Resolved, "refreshing metadata is not a new binding")
				}

				priorWrites := g.writes
				if scenario == "nil_input" || scenario == "wrong_kind" {
					require.Zero(t, stats.Resolved)
					return
				}
				_, err = cr.resolveCargoImports(context.Background(), slices.Values(imports), stats)
				require.NoError(t, err)
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
					g.AddEdge(&graph.Edge{From: "consumer/src/lib.rs", FilePath: "consumer/src/lib.rs", To: "provider/src/util.rs::action", Kind: graph.EdgeCalls, Line: 2, CrossRepo: true, Meta: map[string]any{CargoCallStubMeta: "unresolved::action"}})
					cr := NewCrossRepo(g)
					cr.SetCargoImportRetargetRepo("provider")
					cr.SetCargoCrateLookup(func(string, string) []CargoCrate {
						if reason == "undeclared" {
							return nil
						}
						return []CargoCrate{{Root: "provider/src/lib.rs", Name: "other_crate"}, {Root: "fork/src/lib.rs", Name: "other_crate"}}
					})
					_, err := cr.resolveCargoImports(t.Context(), graph.EdgesLightSeq(g, graph.EdgeImports), &CrossRepoStats{ByRepo: map[string]int{}})
					require.NoError(t, err)
					imports := outEdgesOfKind(g, "consumer/src/lib.rs", graph.EdgeImports)
					require.Len(t, imports, 1)
					require.Equal(t, stub, imports[0].To)
					require.False(t, imports[0].CrossRepo)
					calls := outEdgesOfKind(g, "consumer/src/lib.rs", graph.EdgeCalls)
					require.Len(t, calls, 1)
					require.Equal(t, "unresolved::action", calls[0].To, "warm call must equal a cold unbound call")
					require.False(t, calls[0].CrossRepo)
				})
			}
		}
	}
}

func TestCargoRefusedImportWarmMatchesCold(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, scoped := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/scoped=%v", backend, scoped), func(t *testing.T) {
				targets := []string{}
				for _, warm := range []bool{false, true} {
					g := goImportReviewStore(t, backend)
					file, provider := "consumer/src/lib.rs", "provider/src/util.rs"
					for _, path := range []string{file, "provider/src/lib.rs", provider} {
						g.AddNode(&graph.Node{ID: path, FilePath: path, Kind: graph.KindFile, RepoPrefix: graph.RepoPrefixOfID(path), Language: "rust"})
					}
					g.AddNode(&graph.Node{ID: provider + "::action", FilePath: provider, Name: "action", Kind: graph.KindFunction, RepoPrefix: "provider", Language: "rust"})
					imp, call := "external::other_crate/util/action", "unresolved::action"
					if warm {
						imp, call = provider, provider+"::action"
					}
					g.AddEdge(&graph.Edge{From: file, FilePath: file, To: imp, Kind: graph.EdgeImports, Line: 1, CrossRepo: warm, Meta: map[string]any{CargoImportStubMeta: "external::other_crate/util/action"}})
					g.AddEdge(&graph.Edge{From: file, FilePath: file, To: call, Kind: graph.EdgeCalls, Line: 2, CrossRepo: warm, Meta: map[string]any{CargoCallStubMeta: "unresolved::action"}})
					cr := NewCrossRepo(g)
					cr.SetCargoCrateLookup(func(string, string) []CargoCrate { return nil })
					cr.SetCargoImportRetargetRepo("provider")
					if scoped {
						cr.ResolveForFile("consumer", "src/lib.rs")
					} else {
						_, err := cr.ResolveAllContext(t.Context())
						require.NoError(t, err)
					}
					edge := outEdgesOfKind(g, file, graph.EdgeCalls)[0]
					require.False(t, edge.CrossRepo)
					targets = append(targets, edge.To)
				}
				require.Equal(t, targets[0], targets[1])
				require.Equal(t, "unresolved::action", targets[0])
			})
		}
	}
}

func TestCargoRestubCallsPreservesUnrelatedLinks(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			// Given a revoked import and three marked calls with different binding evidence.
			g := goImportReviewStore(t, backend)
			file, target := "consumer/src/lib.rs", "provider/src/util.rs"
			g.AddNode(&graph.Node{ID: file, FilePath: file, Kind: graph.KindFile, RepoPrefix: "consumer", Language: "rust"})
			for _, row := range []struct {
				to    string
				cross bool
				line  int
			}{
				{target + "::action", true, 1},
				{"other/src/lib.rs::action", true, 2},
				{target + "::local", false, 3},
			} {
				g.AddEdge(&graph.Edge{From: file, FilePath: file, Kind: graph.EdgeCalls, To: row.to, Line: row.line, CrossRepo: row.cross, Meta: map[string]any{CargoCallStubMeta: "unresolved::action"}})
			}
			// When only calls through the revoked target are restubbed.
			reopened := NewCrossRepo(g).restubCargoImportCalls(file, target)
			// Then the other provider and non-cross-repo call keep their links.
			require.Len(t, reopened, 1)
			for _, call := range outEdgesOfKind(g, file, graph.EdgeCalls) {
				switch call.Line {
				case 1:
					require.Equal(t, "unresolved::action", call.To)
					require.False(t, call.CrossRepo)
				case 2:
					require.Equal(t, "other/src/lib.rs::action", call.To)
					require.True(t, call.CrossRepo)
				case 3:
					require.Equal(t, target+"::local", call.To)
					require.False(t, call.CrossRepo)
				}
			}
		})
	}
}
