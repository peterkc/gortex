package store_sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestCrossRepoFrontierConstrainsGenerationBeforeEndpointReads(t *testing.T) {
	for _, matches := range []bool{true, false} {
		t.Run(fmt.Sprintf("matches_%t", matches), func(t *testing.T) {
			// Given three physical generations with the same logical identities,
			// when any scoped production CTE runs, then only the selected
			// generation reaches endpoint reads, even if those reads emit nothing.
			store := openReindexReceiptTestStore(t)
			for _, generation := range []int64{1, 2, 3} {
				targetRepo := "b"
				if generation == 2 && !matches {
					targetRepo = "a"
				}
				nodes := []*graph.Node{
					{ID: "from", Kind: graph.KindFunction, FilePath: "from.go", RepoPrefix: "a"},
					{ID: "to", Kind: graph.KindFunction, FilePath: "to.go", RepoPrefix: targetRepo},
				}
				var edges []*graph.Edge
				for line := 1; line <= 3; line++ {
					edges = append(edges, &graph.Edge{From: "from", To: "to", Kind: graph.EdgeCalls, FilePath: "source.go", Line: line})
				}
				require.NoError(t, store.AtGeneration(generation).AddBatchChecked(nodes, edges))
			}
			selected := store.AtGeneration(2)
			targetRepo := "b"
			if !matches {
				targetRepo = "a"
			}
			for _, scope := range []struct {
				name string
				repos, sources, incidents []string
			}{
				{"repo_source", []string{"a", "a", ""}, nil, nil},
				{"repo_target", []string{targetRepo}, nil, nil},
				{"edge_source", nil, []string{"source.go", "source.go", ""}, nil},
				{"incident_source", nil, nil, []string{"from.go", "from.go", ""}},
				{"incident_target", nil, nil, []string{"to.go"}},
				{"combined", nil, []string{"source.go"}, []string{"from.go", "to.go"}},
			} {
				t.Run(scope.name, func(t *testing.T) {
					query, args, ok := selected.crossRepoCandidatesQuery([]graph.EdgeKind{graph.EdgeCalls, graph.EdgeCalls, ""}, scope.repos, scope.sources, scope.incidents)
					require.True(t, ok)
					// Keep the exact production CTE, replacing only its final
					// endpoint projection with counts of the physical candidate IDs.
					end := strings.Index(query, "\nSELECT e.from_id")
					require.Positive(t, end)
					countQuery := query[:end] + `
SELECT COUNT(*), MIN(e.view_gen), MAX(e.view_gen)
FROM candidate_edges ce JOIN edges e ON e.id = ce.id`
					var count int
					var minGen, maxGen int64
					require.NoError(t, selected.db.QueryRow(countQuery, args[:len(args)-1]...).Scan(&count, &minGen, &maxGen))
					require.Positive(t, count, "zero final outputs must not hide frontier work")
					require.Equal(t, 3, count, "frontier must select 3 physical IDs, not all 9 across generations")
					require.Equal(t, int64(2), minGen)
					require.Equal(t, int64(2), maxGen)
					rows, err := selected.db.Query(query, args...)
					require.NoError(t, err)
					defer rows.Close()
					var resultCount int
					for rows.Next() {
						resultCount++
					}
					require.NoError(t, rows.Err())
					want := 0
					if matches {
						want = 3
					}
					require.Equal(t, want, resultCount)
				})
			}
		})
	}
}

func TestCrossRepoEndpointPlansUseExactPrimaryKeys(t *testing.T) {
	// Given absent, repaired and freshly analyzed statistics, when every
	// caller's query is planned, then both endpoint joins seek (id, view_gen).
	store := newGenerationScanPlanLockStore(t)
	for _, regime := range []string{"no_stats", "repair_stats", "fresh_analyze"} {
		t.Run(regime, func(t *testing.T) {
			withReceiverPlanLockWriter(t, store, func(ctx context.Context, conn *sql.Conn) {
				switch regime {
				case "no_stats":
					_, err := conn.ExecContext(ctx, `DROP TABLE IF EXISTS sqlite_stat1`)
					require.NoError(t, err)
				case "repair_stats":
					_, err := conn.ExecContext(ctx, `ANALYZE`)
					require.NoError(t, err)
					_, err = conn.ExecContext(ctx, `UPDATE sqlite_stat1 SET stat = '185194 501 501' WHERE tbl = 'nodes'`)
					require.NoError(t, err)
				case "fresh_analyze":
					_, err := conn.ExecContext(ctx, `ANALYZE`)
					require.NoError(t, err)
				}
				reloadPlannerStats(t, ctx, conn)
				for _, scope := range []struct {
					name string
					repos, sources, incidents []string
				}{
					{"full", nil, nil, nil},
					{"repos", []string{"repo"}, nil, nil},
					{"source", nil, []string{"pkg/file03.go"}, nil},
					{"incident", nil, nil, []string{"pkg/file03.go"}},
					{"combined", nil, []string{"pkg/file03.go"}, []string{"pkg/file03.go"}},
				} {
					query, args, ok := store.crossRepoCandidatesQuery(graph.BaseKindsForCrossRepo(), scope.repos, scope.sources, scope.incidents)
					require.True(t, ok)
					plan := explainOnConn(t, ctx, conn, query, args...)
					assertCrossRepoEndpointPrimaryKeys(t, scope.name, plan)
				}
			})
		})
	}
}

func assertCrossRepoEndpointPrimaryKeys(t *testing.T, label string, plan []string) {
	t.Helper()
	joined := strings.Join(plan, "\n")
	for _, alias := range []string{"nf", "nt"} {
		want := "SEARCH " + alias + " USING PRIMARY KEY (id=? AND view_gen=?)"
		require.Contains(t, joined, want, "%s: endpoints must use exact physical primary keys", label)
	}
	assertNoGenerationWalk(t, label, plan, "nf", "nt")
}

func TestCrossRepoCandidatesWithoutFileIndexMatchIndexed(t *testing.T) {
	for _, matches := range []bool{true, false} {
		t.Run(fmt.Sprintf("matches_%t", matches), func(t *testing.T) {
			// Given three generations and a positive or zero-output frontier,
			// when bulk loading drops edges_by_file, then the production reader
			// returns exactly the indexed rows without a missing-index panic.
			store := openReindexReceiptTestStore(t)
			for _, generation := range []int64{1, 2, 3} {
				targetRepo := "b"
				if generation == 2 && !matches {
					targetRepo = "a"
				}
				require.NoError(t, store.AtGeneration(generation).AddBatchChecked([]*graph.Node{
					{ID: "from", Kind: graph.KindFunction, FilePath: "from.go", RepoPrefix: "a"},
					{ID: "to", Kind: graph.KindFunction, FilePath: "to.go", RepoPrefix: targetRepo},
				}, []*graph.Edge{
					{From: "from", To: "to", Kind: graph.EdgeCalls, FilePath: "source.go", Line: 1, Confidence: 0.8, Origin: graph.OriginASTResolved},
					{From: "to", To: "from", Kind: graph.EdgeCalls, FilePath: "source.go", Line: 2, Confidence: 0.6, Origin: graph.OriginASTInferred},
				}))
			}
			selected := store.AtGeneration(2)
			kinds := []graph.EdgeKind{graph.EdgeCalls, "", graph.EdgeCalls}
			files := []string{"source.go", "", "source.go"}
			want := selected.CrossRepoCandidatesForMutation(kinds, files, nil)
			if matches {
				require.Len(t, want, 2, "positive rows prove that empty is not a valid error fallback")
			} else {
				require.Empty(t, want)
			}
			_, err := store.writerDB.Exec(`DROP INDEX edges_by_file`)
			require.NoError(t, err)
			got := selected.CrossRepoCandidatesForMutation(kinds, files, nil)
			require.ElementsMatch(t, want, got, "all candidate fields must survive the missing-index fallback")
			got = selected.CrossRepoCandidatesForMutation(kinds, files, []string{"from.go", "to.go"})
			require.ElementsMatch(t, want, got, "the combined frontier retains all roles when its source index is absent")
		})
	}
}
