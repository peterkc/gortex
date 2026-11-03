package indexer

import (
	"context"
	"path/filepath"
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
	refuse bool
	reads  int
}

func (s *warmupIncomingRefusalStore) GetInEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	s.reads++
	rows := s.Store.GetInEdgesByNodeIDs(ids)
	if s.refuse && len(ids) > 0 {
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

func TestWarmupExactFrontierReportsIncomingRefusal(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, phase := range []string{"success", "same_repo", "cross_repo"} {
			t.Run(backend+"/"+phase, func(t *testing.T) {
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
				store := &warmupIncomingRefusalStore{Store: base, refuse: phase == "same_repo"}
				manager, err := config.NewConfigManager(filepath.Join(t.TempDir(), "config.yaml"))
				require.NoError(t, err)
				mi := NewMultiIndexer(store, newTestRegistry(), search.NewNull(), manager, zap.NewNop())
				t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
				ready := false
				err = mi.RunPreEnrichResolveFiles(context.Background(), []string{file}, func() {
					ready = true
					store.refuse = phase == "cross_repo"
				})
				if phase == "success" {
					require.NoError(t, err)
				} else {
					require.ErrorContains(t, err, "incoming")
				}
				require.Equal(t, phase != "same_repo", ready)
				require.Positive(t, store.reads, "the real bounded admission must be exercised")
			})
		}
	}
}
