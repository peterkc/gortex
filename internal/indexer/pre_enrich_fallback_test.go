package indexer

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/search"
)

type preEnrichHydrationStore struct {
	*warmupIncomingRefusalStore
	graph.UnresolvedEdgePager
	graph.ScopedProjectionSequencer
	graph.NodesInFilesByKindStreamer
	fullStarted    atomic.Bool
	fileHydrations atomic.Int64
	outgoingReads  atomic.Int64
}

func (s *preEnrichHydrationStore) BeginUnresolvedEdgeScan(ctx context.Context) (graph.UnresolvedEdgeScan, error) {
	s.fullStarted.Store(true)
	return s.UnresolvedEdgePager.BeginUnresolvedEdgeScan(ctx)
}

func (s *preEnrichHydrationStore) GetFileNodesByPaths(paths []string) map[string][]*graph.Node {
	if !s.fullStarted.Load() {
		s.fileHydrations.Add(1)
	}
	return s.Store.GetFileNodesByPaths(paths)
}

func (s *preEnrichHydrationStore) GetOutEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	if !s.fullStarted.Load() {
		s.outgoingReads.Add(1)
	}
	return s.Store.GetOutEdgesByNodeIDs(ids)
}

type preEnrichLSP struct{ calls atomic.Int64 }

func (h *preEnrichLSP) SupportsPath(path string) bool { return filepath.Ext(path) == ".ts" }
func (h *preEnrichLSP) Definition(_ string, _ int, _ string) (string, int, bool) {
	h.calls.Add(1)
	return "repo/target.ts", 7, true
}

func newPreEnrichHydrationFixture(t *testing.T, refuse bool) (*MultiIndexer, *preEnrichHydrationStore, *preEnrichLSP) {
	t.Helper()
	disk, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, disk.Close()) })
	disk.AddBatch([]*graph.Node{
		{ID: "repo/caller.ts", Kind: graph.KindFile, Name: "caller.ts", FilePath: "repo/caller.ts", Language: "typescript", RepoPrefix: "repo"},
		{ID: "repo/target.ts", Kind: graph.KindFile, Name: "target.ts", FilePath: "repo/target.ts", Language: "typescript", RepoPrefix: "repo"},
		{ID: "repo/caller.ts::callIt", Kind: graph.KindFunction, Name: "callIt", FilePath: "repo/caller.ts", Language: "typescript", RepoPrefix: "repo", StartLine: 3, EndLine: 5},
		{ID: "repo/target.ts::doWork", Kind: graph.KindFunction, Name: "doWork", FilePath: "repo/target.ts", Language: "typescript", RepoPrefix: "repo", StartLine: 7, EndLine: 9},
	}, []*graph.Edge{
		{From: "repo/caller.ts::callIt", To: "repo::unresolved::doWork", Kind: graph.EdgeCalls, FilePath: "repo/caller.ts", Line: 4},
		{From: "repo/caller.ts", To: "repo/target.ts", Kind: graph.EdgeImports, FilePath: "repo/caller.ts", Line: 1},
	})
	incoming := &warmupIncomingRefusalStore{Store: disk}
	incoming.refuse.Store(refuse)
	store := &preEnrichHydrationStore{warmupIncomingRefusalStore: incoming, UnresolvedEdgePager: disk, ScopedProjectionSequencer: disk, NodesInFilesByKindStreamer: disk}
	manager, err := config.NewConfigManager(filepath.Join(t.TempDir(), "config.yaml"))
	require.NoError(t, err)
	mi := NewMultiIndexer(store, newTestRegistry(), search.NewNull(), manager, zap.NewNop())
	t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
	helper := &preEnrichLSP{}
	mi.SetResolverLSPHelper(helper)
	return mi, store, helper
}

func TestPreEnrichRefusalSkipsOutgoingHydrationAndDefersLSP(t *testing.T) {
	mi, store, helper := newPreEnrichHydrationFixture(t, true)
	hooks := 0
	require.NoError(t, mi.RunPreEnrichResolveFiles(t.Context(), []string{"repo/caller.ts"}, func() {
		hooks++
		assert.True(t, store.fullStarted.Load(), "startup must select the whole-graph pass")
		assert.Zero(t, store.fileHydrations.Load(), "refusal must precede frontier node hydration")
		assert.Zero(t, store.outgoingReads.Load(), "refusal must precede outgoing adjacency")
		assert.Zero(t, helper.calls.Load(), "LSP must wait until whole-graph compute-ready")
	}))
	require.Equal(t, 1, hooks)
	require.EqualValues(t, 1, helper.calls.Load())
	full, fullStore, fullHelper := newPreEnrichHydrationFixture(t, false)
	require.NoError(t, full.RunPreEnrichResolve(t.Context(), nil, nil))
	require.ElementsMatch(t, fullStore.AllEdges(), store.AllEdges(), "fallback must match full resolver payloads")
	require.Equal(t, fullHelper.calls.Load(), helper.calls.Load())
}

func TestPreEnrichAdmittedDeltaKeepsExactInlineLSP(t *testing.T) {
	mi, store, helper := newPreEnrichHydrationFixture(t, false)
	hooks := 0
	require.NoError(t, mi.RunPreEnrichResolveFiles(t.Context(), []string{"repo/caller.ts"}, func() {
		hooks++
		assert.False(t, store.fullStarted.Load(), "small admitted delta must stay exact")
		assert.Positive(t, store.fileHydrations.Load())
		assert.Positive(t, store.outgoingReads.Load())
		assert.EqualValues(t, 1, helper.calls.Load(), "small exact pass retains inline LSP")
	}))
	require.Equal(t, 1, hooks)
}

type preEnrichLateRefusalStore struct {
	*preEnrichHydrationStore
	incomingReads int
}

func (s *preEnrichLateRefusalStore) GetInEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	s.incomingReads++
	s.refuse.Store(s.incomingReads > 1)
	return s.warmupIncomingRefusalStore.GetInEdgesByNodeIDs(ids)
}

func TestPreEnrichLateRefusalStillRunsFullResolve(t *testing.T) {
	// Given preflight admits, when a later incoming read refuses, then the
	// whole-graph fallback still runs and readiness fires exactly once.
	mi, base, _ := newPreEnrichHydrationFixture(t, false)
	mi.SetResolverLSPHelper(nil)
	store := &preEnrichLateRefusalStore{preEnrichHydrationStore: base}
	mi.graph = store
	hooks := 0
	require.NoError(t, mi.RunPreEnrichResolveFiles(t.Context(), []string{"repo/caller.ts"}, func() {
		hooks++
		assert.True(t, store.fullStarted.Load(), "late refusal must select the whole-graph pass")
	}))
	require.Greater(t, store.incomingReads, 1)
	require.Equal(t, 1, hooks)
	full, fullStore, _ := newPreEnrichHydrationFixture(t, false)
	full.SetResolverLSPHelper(nil)
	require.NoError(t, full.RunPreEnrichResolve(t.Context(), nil, nil))
	require.ElementsMatch(t, fullStore.AllEdges(), store.AllEdges())
}

type preEnrichCancelReadStore struct {
	*preEnrichHydrationStore
	cancel context.CancelCauseFunc
	cause  error
}

func (s *preEnrichCancelReadStore) GetInEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	rows := s.preEnrichHydrationStore.GetInEdgesByNodeIDs(ids)
	s.cancel(s.cause)
	return rows
}

func TestPreEnrichCancellationDuringPreflightReturnsCause(t *testing.T) {
	// Given an incoming read cancels with a cause, when preflight returns,
	// then startup preserves that cause and does not start resolve or readiness.
	mi, base, _ := newPreEnrichHydrationFixture(t, false)
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	cause := errors.New("preflight read canceled")
	mi.graph = &preEnrichCancelReadStore{preEnrichHydrationStore: base, cancel: cancel, cause: cause}
	hooks := 0
	err := mi.RunPreEnrichResolveFiles(ctx, []string{"repo/caller.ts"}, func() { hooks++ })
	require.ErrorIs(t, err, cause)
	require.Zero(t, hooks)
	require.False(t, base.fullStarted.Load())
	require.Zero(t, base.fileHydrations.Load())
	require.Zero(t, base.outgoingReads.Load())
}

func TestPreEnrichPreflightRefusalKeepsPreparationWarning(t *testing.T) {
	// Given preflight refuses, when startup falls back, then retain the same
	// preparation Warn message and fields that the exact pass used to emit.
	mi, _, _ := newPreEnrichHydrationFixture(t, true)
	core, logs := observer.New(zap.WarnLevel)
	mi.logger = zap.New(core)
	require.NoError(t, mi.RunPreEnrichResolveFiles(t.Context(), []string{"repo/caller.ts"}, nil))
	entries := logs.FilterMessage("resolver: incoming stub admission refused").All()
	var preparation []observer.LoggedEntry
	for _, entry := range entries {
		if entry.ContextMap()["leg"] == "preparation" {
			preparation = append(preparation, entry)
		}
	}
	require.Len(t, preparation, 1)
	fields := preparation[0].ContextMap()
	require.Equal(t, zap.WarnLevel, preparation[0].Level)
	require.EqualValues(t, 4, fields["stub_keys"])
	require.EqualValues(t, graph.MaxIncomingSourceCandidateRows+1, fields["inspected"])
	require.EqualValues(t, graph.MaxIncomingSourceCandidateRows+1, fields["dropped"])
	require.EqualValues(t, graph.MaxIncomingSourceCandidateRows, fields["limit"])
	require.NotEmpty(t, fields["error"])
}
