package indexer

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/search"
)

type warmupIncomingRefusalStore struct {
	graph.Store
	refuse atomic.Bool
	reads  atomic.Int64
}

func (s *warmupIncomingRefusalStore) GetInEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	s.reads.Add(1)
	rows := s.Store.GetInEdgesByNodeIDs(ids)
	if s.refuse.Load() && len(ids) > 0 {
		// Charge the real admission boundary without creating a large graph.
		edge := &graph.Edge{From: "caller", To: ids[0], Kind: graph.EdgeCalls}
		batch := make([]*graph.Edge, graph.MaxIncomingSourceCandidateRows+1)
		for i := range batch {
			batch[i] = edge
		}
		rows[ids[0]] = batch
	}
	return rows
}

type warmupRefusalScanFailureStore struct {
	*warmupIncomingRefusalStore
	graph.UnresolvedEdgePager
	resolveErr error
	scans      atomic.Int64
}

func (s *warmupRefusalScanFailureStore) BeginUnresolvedEdgeScan(context.Context) (graph.UnresolvedEdgeScan, error) {
	s.scans.Add(1)
	return graph.UnresolvedEdgeScan{}, s.resolveErr
}

func TestWarmupExactFrontierReportsIncomingRefusal(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, phase := range []string{"success", "same_repo", "cross_repo"} {
			t.Run(backend+"/"+phase, func(t *testing.T) {
				// Given an exact warm frontier and a bounded incoming refusal.
				var base graph.Store = graph.New()
				if backend == "sqlite" {
					disk, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
					require.NoError(t, err)
					t.Cleanup(func() { require.NoError(t, disk.Close()) })
					base = disk
				}
				const file = "repo/target.go"
				base.AddNode(&graph.Node{ID: file + "::Target", Kind: graph.KindFunction,
					Name: "Target", FilePath: file, RepoPrefix: "repo", Language: "go"})
				store := &warmupIncomingRefusalStore{Store: base}
				store.refuse.Store(phase == "same_repo")
				manager, err := config.NewConfigManager(filepath.Join(t.TempDir(), "config.yaml"))
				require.NoError(t, err)
				mi := NewMultiIndexer(store, newTestRegistry(), search.NewNull(), manager, zap.NewNop())
				t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
				var hooks atomic.Int64

				// When the exact resolve reaches the refused admission boundary.
				err = mi.RunPreEnrichResolveFiles(context.Background(), []string{file}, func() {
					hooks.Add(1)
					store.refuse.Store(phase == "cross_repo")
				})

				// Then refusal falls back to the full pass without losing compute-ready.
				require.NoError(t, err)
				require.EqualValues(t, 1, hooks.Load())
				require.Positive(t, store.reads.Load(), "the real bounded admission must be exercised")
			})
		}
	}
}

func TestWarmupExactFrontierRefusalMatchesFullResolve(t *testing.T) {
	for _, phase := range []string{"same_repo", "cross_repo"} {
		t.Run(phase, func(t *testing.T) {
			// Given a stamped graph with an unresolved call outside the exact frontier.
			mi, base, call := newPreEnrichSlugFixture(t, true)
			t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
			localCall := &graph.Edge{From: "a/a.go::Call", To: "a::unresolved::Local", Kind: graph.EdgeCalls, FilePath: "a/a.go", Line: 4}
			if phase == "same_repo" {
				base.AddNode(&graph.Node{ID: "a/a.go::Local", Kind: graph.KindFunction, Name: "Local", FilePath: "a/a.go", Language: "go", RepoPrefix: "a", WorkspaceID: "shared"})
				base.AddEdge(localCall)
			}
			store := &warmupIncomingRefusalStore{Store: base}
			store.refuse.Store(phase == "same_repo")
			mi.graph = store
			var hooks atomic.Int64
			var localResolvedAtReady atomic.Bool

			// When bounded admission refuses and resolution escalates.
			err := mi.RunPreEnrichResolveFiles(context.Background(), []string{"c/c.go"}, func() {
				hooks.Add(1)
				localResolvedAtReady.Store(localCall.To == "a/a.go::Local")
				store.refuse.Store(phase == "cross_repo")
			})

			// Then the complete graph matches the existing full warmup pass.
			require.NoError(t, err)
			require.EqualValues(t, 1, hooks.Load())
			require.Equal(t, "b/b.go::Serve", call.To)
			if phase == "same_repo" {
				require.True(t, localResolvedAtReady.Load(), "same-repo references must resolve before compute-ready")
			}
			full, fullStore, _ := newPreEnrichSlugFixture(t, true)
			t.Cleanup(func() { require.NoError(t, full.Close(context.Background())) })
			if phase == "same_repo" {
				fullStore.AddNode(&graph.Node{ID: "a/a.go::Local", Kind: graph.KindFunction, Name: "Local", FilePath: "a/a.go", Language: "go", RepoPrefix: "a", WorkspaceID: "shared"})
				fullStore.AddEdge(&graph.Edge{From: "a/a.go::Call", To: "a::unresolved::Local", Kind: graph.EdgeCalls, FilePath: "a/a.go", Line: 4})
			}
			require.NoError(t, full.RunPreEnrichResolve(context.Background(), nil, nil))
			require.ElementsMatch(t, fullStore.AllEdges(), base.AllEdges())
		})
	}
}

func TestWarmupExactFrontierRefusalReturnsFullResolveFailure(t *testing.T) {
	for _, phase := range []string{"same_repo", "cross_repo"} {
		t.Run(phase, func(t *testing.T) {
			// Given a refused exact frontier whose full-pass scanner fails.
			disk, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, disk.Close()) })
			const file = "repo/target.go"
			disk.AddNode(&graph.Node{ID: file + "::Target", Kind: graph.KindFunction,
				Name: "Target", FilePath: file, RepoPrefix: "repo", Language: "go"})
			incoming := &warmupIncomingRefusalStore{Store: disk}
			incoming.refuse.Store(phase == "same_repo")
			resolveErr := errors.New("injected full resolve scan failure")
			store := &warmupRefusalScanFailureStore{warmupIncomingRefusalStore: incoming, UnresolvedEdgePager: disk, resolveErr: resolveErr}
			manager, err := config.NewConfigManager(filepath.Join(t.TempDir(), "config.yaml"))
			require.NoError(t, err)
			mi := NewMultiIndexer(store, newTestRegistry(), search.NewNull(), manager, zap.NewNop())
			t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
			var hooks atomic.Int64

			// When the exact resolve escalates to the failing full pass.
			err = mi.RunPreEnrichResolveFiles(context.Background(), []string{file}, func() {
				hooks.Add(1)
				incoming.refuse.Store(phase == "cross_repo")
			})

			// Then the full-pass error is retained, not the bounded refusal.
			require.ErrorIs(t, err, resolveErr)
			require.EqualValues(t, 1, store.scans.Load())
			wantHooks := int64(0)
			if phase == "cross_repo" {
				wantHooks = 1
			}
			require.Equal(t, wantHooks, hooks.Load())
		})
	}
}
