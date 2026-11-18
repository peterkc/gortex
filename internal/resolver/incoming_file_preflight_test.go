package resolver

import (
	"context"
	"errors"
	"iter"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestIncomingFilePreflightMatchesCollectorAdmission(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, fanIn := range []int{64, graph.MaxIncomingSourceCandidateRows + 1} {
			name := "admitted"
			if fanIn > graph.MaxIncomingSourceCandidateRows {
				name = "refused"
			}
			t.Run(backend+"/"+name, func(t *testing.T) {
				memory, changed := incomingFanOutGraph(t, fanIn)
				memory.AddNode(&graph.Node{ID: "fileless::Ignored", Kind: graph.KindFunction, Name: "Ignored"})
				var store graph.Store = memory
				if backend == "sqlite" {
					disk, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
					require.NoError(t, err)
					t.Cleanup(func() { require.NoError(t, disk.Close()) })
					disk.AddBatch(memory.AllNodes(), memory.AllEdges())
					store = disk
				}
				files := []string{changed, changed, "", "missing.go"}
				fact, err := PreflightIncomingFiles(t.Context(), store, files)
				frontier := New(store).collectIncrementalFileFrontier(files)
				require.Equal(t, frontier.incomingAdmission, fact)
				var preflightLimit, collectorLimit *graph.BoundedLocalizationLimitError
				require.Equal(t, errors.As(err, &preflightLimit), errors.As(frontier.incomingRefusal, &collectorLimit))
				require.Equal(t, collectorLimit, preflightLimit)
				require.Equal(t, fanIn > graph.MaxIncomingSourceCandidateRows, fact.Refused)
				require.Equal(t, fanIn, fact.Inspected)
			})
		}
	}
}

func TestIncomingFilePreflightCanceledAdmissionDoesNotRefuse(t *testing.T) {
	store, file := incomingFanOutGraph(t, 1)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	fact, err := PreflightIncomingFiles(ctx, store, []string{file})
	require.ErrorIs(t, err, context.Canceled)
	require.False(t, fact.Refused)
}

type preflightProjectionStore struct {
	graph.Store
	graph.ScopedProjectionSequencer
	t                                                *testing.T
	summaries, summaryRows, identities, identityRows int
}

func (s *preflightProjectionStore) NodesLightInScopeSeq(repos, files []string) iter.Seq[*graph.Node] {
	s.summaries++
	return func(yield func(*graph.Node) bool) {
		for node := range s.ScopedProjectionSequencer.NodesLightInScopeSeq(repos, files) {
			s.summaryRows++
			require.Nil(s.t, node.Meta, "preflight must not decode retrieval payloads")
			if !yield(node) {
				return
			}
		}
	}
}

func (s *preflightProjectionStore) GetInEdgeIdentitiesByNodeIDs(ids []string) map[string][]graph.EdgeIdentity {
	s.identities++
	rows := graph.InEdgeIdentitiesByNodeIDs(s.Store, ids)
	for _, batch := range rows {
		s.identityRows += len(batch)
	}
	return rows
}

func (s *preflightProjectionStore) GetFileNodesByPaths([]string) map[string][]*graph.Node {
	s.t.Fatal("preflight hydrated file-node payloads")
	return nil
}

func (s *preflightProjectionStore) GetOutEdgesByNodeIDs([]string) map[string][]*graph.Edge {
	s.t.Fatal("preflight read outgoing adjacency")
	return nil
}

func TestIncomingFilePreflightReadsOnlyCompactRows(t *testing.T) {
	memory, changed := incomingFanOutGraph(t, 64)
	memory.GetNode(changed + "::Close").Meta = map[string]any{"doc": strings.Repeat("payload", 1024)}
	disk, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, disk.Close()) })
	disk.AddBatch(memory.AllNodes(), memory.AllEdges())
	store := &preflightProjectionStore{Store: disk, ScopedProjectionSequencer: disk, t: t}
	fact, err := PreflightIncomingFiles(t.Context(), store, []string{changed})
	require.NoError(t, err)
	require.False(t, fact.Refused)
	require.Equal(t, 1, store.summaries)
	require.Equal(t, 2, store.summaryRows)
	require.Equal(t, 1, store.identities)
	require.Equal(t, 64, store.identityRows)
}
