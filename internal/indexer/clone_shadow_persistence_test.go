package indexer

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/search"
)

type cloneCorpusResetCountingStore struct {
	*store_sqlite.Store
	resets   int
	resetErr error
}

func (s *cloneCorpusResetCountingStore) ResetCloneCorpus(prefix string) error {
	s.resets++
	if s.resetErr != nil {
		return s.resetErr
	}
	return s.Store.ResetCloneCorpus(prefix)
}

func TestSQLiteShadowCloneCorpusReplacement(t *testing.T) {
	for _, tc := range []struct {
		name      string
		shadowMax string
		streaming string
		batched   bool
	}{
		{name: "cold_batched", shadowMax: "1000000", streaming: "0", batched: true},
		{name: "cold_runtime_track", shadowMax: "1000000", streaming: "0"},
		{name: "streaming_batched", shadowMax: "0", streaming: "1", batched: true},
		{name: "streaming_runtime_track", shadowMax: "0", streaming: "1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GORTEX_SHADOW_MAX_FILES", tc.shadowMax)
			t.Setenv("GORTEX_STREAMING_FLUSH", tc.streaming)
			t.Setenv("GORTEX_STREAMING_CHUNK_SIZE", "1")
			repo := t.TempDir()
			writeFile(t, filepath.Join(repo, "clone1.go"), mrRepoAClone1)
			writeFile(t, filepath.Join(repo, "clone2.go"), mrRepoAClone2)
			writeFile(t, filepath.Join(repo, "other.go"), mrRepoBClone1)

			disk, err := store_sqlite.Open(filepath.Join(t.TempDir(), "clone.sqlite"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, disk.Close()) })
			store := &cloneCorpusResetCountingStore{Store: disk}
			// A full replacement must remove even orphaned rows from an older corpus.
			require.NoError(t, disk.BulkSetCloneCorpus("repo", []graph.CloneCorpusRow{{
				NodeID: "repo/stale.go::Old", Shingles: []uint64{9}, Signature: "stale", Finalized: true,
			}}))
			cfgPath := filepath.Join(t.TempDir(), "config.yaml")
			gc := &config.GlobalConfig{}
			gc.SetConfigPath(cfgPath)
			require.NoError(t, gc.Save())
			cm, err := config.NewConfigManager(cfgPath)
			require.NoError(t, err)
			mi := NewMultiIndexer(store, newTestRegistry(), search.NewNull(), cm, zap.NewNop())
			if tc.batched {
				mi.BeginBatch()
			}
			_, err = mi.TrackRepoCtx(context.Background(), config.RepoEntry{Path: repo, Name: "repo"})
			require.NoError(t, err)
			require.Contains(t, mi.indexers, "repo")
			assert.Equal(t, tc.streaming == "0", mi.indexers["repo"].indexCount.Load() > 0,
				"the requested whole-shadow or streaming path must be exercised")
			assert.Equal(t, 1, store.resets, "reset once per full replacement, not once per chunk")

			if tc.batched {
				rows, err := disk.CloneCorpusPage("repo", "", 10)
				require.NoError(t, err)
				assert.Len(t, rows, 3, "the drain must preserve pending shingles before EndBatch")
				for _, row := range rows {
					assert.False(t, row.Finalized)
					assert.Empty(t, row.Signature)
				}
				assert.Empty(t, similarToEdges(disk), "clone detection must stay deferred")
				mi.EndBatch()
			}

			rows, err := disk.CloneCorpusPage("repo", "", 10)
			require.NoError(t, err)
			require.Len(t, rows, 3)
			wantIDs := []string{"repo/clone1.go::sumActiveItems", "repo/clone2.go::sumEnabledRecords", "repo/other.go::scanOpenRows"}
			for i, row := range rows {
				assert.Equal(t, wantIDs[i], row.NodeID)
				assert.True(t, row.Finalized)
				assert.NotEmpty(t, row.Shingles)
				assert.NotEmpty(t, row.Signature)
				node := disk.GetNode(row.NodeID)
				require.NotNil(t, node)
				assert.Equal(t, row.Signature, node.Meta[cloneSigMetaKey])
			}
			assert.Equal(t, map[[2]string]struct{}{
				{wantIDs[0], wantIDs[1]}: {},
				{wantIDs[1], wantIDs[0]}: {},
			}, repoSimilarEdgeSet(disk, "repo"))
		})
	}
}

func TestSQLiteShadowCloneCorpusResetFailure(t *testing.T) {
	for _, shadowMax := range []string{"1000000", "0"} {
		t.Run("shadow_max_"+shadowMax, func(t *testing.T) {
			t.Setenv("GORTEX_SHADOW_MAX_FILES", shadowMax)
			t.Setenv("GORTEX_STREAMING_FLUSH", "1")
			repo := t.TempDir()
			writeFile(t, filepath.Join(repo, "main.go"), cloneRepoSource)
			disk, err := store_sqlite.Open(filepath.Join(t.TempDir(), "clone.sqlite"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, disk.Close()) })
			resetErr := errors.New("injected clone corpus reset failure")
			store := &cloneCorpusResetCountingStore{Store: disk, resetErr: resetErr}
			idx := newTestIndexer(store)

			_, err = idx.Index(repo)
			require.ErrorIs(t, err, resetErr)
			assert.Equal(t, 1, store.resets)
			assert.Same(t, store, idx.graph, "a rejected drain must restore the durable graph")
			assert.Zero(t, disk.NodeCount(), "no shadow nodes may drain after a failed reset")
		})
	}
}

type cloneCorpusDrainFailingStore struct {
	*store_sqlite.Store
	flushErr error
}

func (s *cloneCorpusDrainFailingStore) FlushBulk() error {
	if err := s.Store.FlushBulk(); err != nil {
		return err
	}
	return s.flushErr
}

func TestSQLiteShadowCloneCorpusDrainInitialization(t *testing.T) {
	for _, shadowMax := range []string{"1000000", "0"} {
		for _, fail := range []bool{true, false} {
			t.Run(fmt.Sprintf("shadow_max_%s/fail_%t", shadowMax, fail), func(t *testing.T) {
				t.Setenv("GORTEX_SHADOW_MAX_FILES", shadowMax)
				t.Setenv("GORTEX_STREAMING_FLUSH", "1")
				repo := t.TempDir()
				writeFile(t, filepath.Join(repo, "main.go"), "package main\nconst Value = 1\n")
				path := filepath.Join(t.TempDir(), "clone.sqlite")
				disk, err := store_sqlite.Open(path)
				require.NoError(t, err)
				t.Cleanup(func() { _ = disk.Close() })
				// A previous successful empty index already has an initialization marker.
				require.NoError(t, disk.ReplaceCloneCorpus("repo", nil))
				var flushErr error
				if fail {
					flushErr = errors.New("injected clone corpus drain failure")
				}
				store := &cloneCorpusDrainFailingStore{Store: disk, flushErr: flushErr}
				idx := newTestIndexer(store)
				idx.SetRepoPrefix("repo")

				_, err = idx.Index(repo)
				if fail {
					require.ErrorIs(t, err, flushErr)
					if shadowMax == "0" {
						require.ErrorContains(t, err, "streaming-flush chunk")
					} else {
						require.ErrorContains(t, err, "persist bulk graph")
					}
				} else {
					require.NoError(t, err)
				}
				rows, err := disk.CloneCorpusPage("repo", "", 10)
				require.NoError(t, err)
				require.Empty(t, rows)
				initialized, err := disk.CloneCorpusInitialized("repo")
				require.NoError(t, err)
				assert.Equal(t, !fail, initialized, "only a successful drain may leave an authoritative empty corpus")
				require.NoError(t, disk.Close())

				reopened, err := store_sqlite.Open(path)
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, reopened.Close()) })
				initialized, err = reopened.CloneCorpusInitialized("repo")
				require.NoError(t, err)
				assert.Equal(t, !fail, initialized, "restart must retain the clone-corpus initialization state")
			})
		}
	}
}
