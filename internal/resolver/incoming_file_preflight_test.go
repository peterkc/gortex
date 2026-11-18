package resolver

import (
	"context"
	"errors"
	"fmt"
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
				fact, err := PreflightIncomingFiles(t.Context(), store, files, nil)
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
	fact, err := PreflightIncomingFiles(ctx, store, []string{file}, nil)
	require.ErrorIs(t, err, context.Canceled)
	require.False(t, fact.Refused)
}

type preflightFileIndexStore struct {
	graph.Store
	graph.ScopedProjectionSequencer
	graph.NodesInFilesByKindStreamer
	t                                             *testing.T
	fileReads, fileRows, identities, identityRows int
}

func (s *preflightFileIndexStore) NodesLightInScopeSeq(_, _ []string) iter.Seq[*graph.Node] {
	s.t.Fatal("preflight used generation-scanning scoped projection")
	return nil
}

func (s *preflightFileIndexStore) NodesInFilesByKindSeq(files []string, kinds []graph.NodeKind) iter.Seq2[string, []*graph.Node] {
	s.fileReads++
	return func(yield func(string, []*graph.Node) bool) {
		for file, nodes := range s.NodesInFilesByKindStreamer.NodesInFilesByKindSeq(files, kinds) {
			s.fileRows += len(nodes)
			for _, node := range nodes {
				require.True(s.t, graph.IsReferenceableSymbol(node.Kind))
			}
			if !yield(file, nodes) {
				return
			}
		}
	}
}

func (s *preflightFileIndexStore) GetInEdgeIdentitiesByNodeIDs(ids []string) map[string][]graph.EdgeIdentity {
	s.identities++
	rows := graph.InEdgeIdentitiesByNodeIDs(s.Store, ids)
	for _, batch := range rows {
		s.identityRows += len(batch)
	}
	return rows
}

func (s *preflightFileIndexStore) GetFileNodesByPaths([]string) map[string][]*graph.Node {
	s.t.Fatal("preflight hydrated file-node payloads")
	return nil
}

func (s *preflightFileIndexStore) GetOutEdgesByNodeIDs([]string) map[string][]*graph.Edge {
	s.t.Fatal("preflight read outgoing adjacency")
	return nil
}

func TestIncomingFilePreflightReadsFileIndexRows(t *testing.T) {
	memory, changed := incomingFanOutGraph(t, 64)
	memory.GetNode(changed + "::Close").Meta = map[string]any{"doc": strings.Repeat("payload", 1024)}
	disk, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, disk.Close()) })
	disk.AddBatch(memory.AllNodes(), memory.AllEdges())
	store := &preflightFileIndexStore{Store: disk, ScopedProjectionSequencer: disk, NodesInFilesByKindStreamer: disk, t: t}
	fact, err := PreflightIncomingFiles(t.Context(), store, []string{changed}, nil)
	require.NoError(t, err)
	require.False(t, fact.Refused)
	require.Equal(t, 1, store.fileReads)
	require.Equal(t, 1, store.fileRows)
	require.Equal(t, 1, store.identities)
	require.Equal(t, 64, store.identityRows)
}

// The fallback must bound each file read too, and stop before another chunk.
type preflightCancelFileStore struct {
	graph.Store
	cancel context.CancelFunc
	t      *testing.T
	reads  int
}

func (s *preflightCancelFileStore) GetFileNodesByPaths(files []string) map[string][]*graph.Node {
	s.reads++
	require.LessOrEqual(s.t, len(files), 256, "fallback file read must be chunked")
	s.cancel()
	return s.Store.GetFileNodesByPaths(files)
}

func TestIncomingFilePreflightStopsBetweenFileChunks(t *testing.T) {
	store, file := incomingFanOutGraph(t, 1)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	wrapped := &preflightCancelFileStore{Store: store, cancel: cancel, t: t}
	files := []string{file}
	for i := 0; i < 2000; i++ {
		files = append(files, fmt.Sprintf("missing/%d.go", i))
	}
	fact, err := PreflightIncomingFiles(ctx, wrapped, files, nil)
	require.ErrorIs(t, err, context.Canceled)
	require.False(t, fact.Refused)
	require.Equal(t, 1, wrapped.reads)
}
