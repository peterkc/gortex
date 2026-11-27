package store_pebble

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/cockroachdb/pebble/v2"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildFamilyDropRequiresSavedPayload(t *testing.T) {
	b, err := OpenBuild(filepath.Join(t.TempDir(), "working"), FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	e := &graph.Edge{From: "a", To: "b", Kind: graph.EdgeReferences, FilePath: "a.ts", Line: 2,
		Meta: map[string]any{"marker": "current"}}
	b.AddEdge(e)
	key := pendingEdgeKey(e)
	want, err := pendingGet(b.pending, key, copyValue)
	require.NoError(t, err)
	require.NotEmpty(t, want)
	require.NoError(t, b.proveRebuildEdgePayload(key, want))
	changed := *e
	changed.Meta = map[string]any{"marker": "stale"}
	stale, err := encodeEdge(&changed)
	require.NoError(t, err)
	require.ErrorIs(t, b.proveRebuildEdgePayload(key, stale), ErrRebuildUnknownProvenance)
	got, err := pendingGet(b.pending, key, copyValue)
	require.NoError(t, err)
	require.Equal(t, want, got)

	missing := *e
	missing.Line++
	var refused *RebuildUnknownProvenanceError
	require.ErrorAs(t, b.proveRebuildEdgePayload(pendingEdgeKey(&missing), want), &refused)
	require.EqualValues(t, 1, refused.Count)
	require.Equal(t, graph.EdgeIdentityFor(&missing), refused.First)
}

func TestRebuildFamilyDropRemovesExactPendingRows(t *testing.T) {
	b := newBuild(t, FullProfile)
	b.AddNode(&graph.Node{ID: "a", Language: "typescript"})
	b.AddNode(&graph.Node{ID: "b", Language: "java"})
	drop := &graph.Edge{From: "a", To: "b", Kind: graph.EdgeReferences, FilePath: "a.ts", Line: 2,
		Meta: map[string]any{"synth": "drop"}}
	keep := *drop
	keep.Line = 3
	keep.Meta = map[string]any{"synth": "keep"}
	proof := rebuildEdgeCustody{proof: rebuildProofUntrackedMove}
	require.NoError(t, b.putEdgeWithCustody(drop, proof))
	require.NoError(t, b.putEdgeWithCustody(&keep, proof))
	want := rebuildFrameworkCustodyRows(t, b)
	key := pendingEdgeKey(drop)
	for _, k := range [][]byte{key, pendingReverseKey(drop, key), rebuildLinkRecorderKey(key)} {
		require.Contains(t, want, string(k))
		delete(want, string(k))
	}
	count, err := b.RebuildFrameworkFamilyGate(t.Context(), func(e *graph.Edge) bool { return e.Meta["synth"] == "drop" },
		func(from, to string) bool { return from == "typescript" && to == "java" })
	require.NoError(t, err)
	require.EqualValues(t, 1, count)
	require.Equal(t, want, rebuildFrameworkCustodyRows(t, b))
}

func TestRebuildFrameworkFamilyGateCapsOnlyDroppedEndpoints(t *testing.T) {
	padding := strings.Repeat("x", 16<<20)
	for _, role := range []string{"from", "to", "same family"} {
		t.Run(role, func(t *testing.T) {
			b := newBuild(t, FullProfile)
			from := &graph.Node{ID: "from", Kind: graph.KindFunction, Language: "go"}
			to := &graph.Node{ID: "to", Kind: graph.KindFunction, Language: "javascript"}
			large := &graph.Node{ID: "large", Kind: graph.KindFunction, Language: "javascript",
				Meta: map[string]any{"padding": padding}}
			drop := &graph.Edge{From: from.ID, To: to.ID, Kind: graph.EdgeReferences, FilePath: "from.go", Line: 1}
			extra := &graph.Edge{From: large.ID, To: to.ID, Kind: graph.EdgeReferences, FilePath: "large", Line: 1}
			switch role {
			case "from":
				large.Language = from.Language
			case "to":
				extra.From, extra.To = from.ID, large.ID
			}
			b.AddBatch([]*graph.Node{from, to, large}, []*graph.Edge{drop, extra})
			before := rebuildFrameworkCustodyRows(t, b)
			count, err := b.RebuildFrameworkFamilyGate(t.Context(), func(*graph.Edge) bool { return true },
				func(from, to string) bool { return from != to })
			if role != "same family" {
				require.ErrorIs(t, err, ErrRebuildFrameworkFamilyGate)
				require.ErrorIs(t, err, ErrRebuildLinkInputSize)
				require.ErrorContains(t, err, "framework family endpoint node")
				require.Zero(t, count)
				require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
				return
			}
			require.NoError(t, err)
			require.EqualValues(t, 1, count)
			kept, err := b.pendingEdge(pendingEdgeKey(extra))
			require.NoError(t, err)
			require.Equal(t, extra, kept)
			gone, err := b.pendingEdge(pendingEdgeKey(drop))
			require.NoError(t, err)
			require.Nil(t, gone)
		})
	}
}

func TestRebuildFrameworkFamilyGateRefusals(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		cause        error
	}{
		{"occupied scratch", "framework family gate scratch occupied", errInvalidRecord},
		{"oversized crossing edge", "framework family edge", ErrRebuildLinkInputSize},
		{"edge identity", "framework family edge identity", errInvalidRecord},
		{"endpoint identity", "framework family endpoint identity", errInvalidRecord},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBuild(t, FullProfile)
			from := &graph.Node{ID: "caller", Language: "typescript"}
			to := &graph.Node{ID: "backend", Language: "java"}
			first := &graph.Edge{From: from.ID, To: to.ID, Kind: graph.EdgeReferences, FilePath: "caller.ts", Line: 1}
			last := *first
			last.Line = 2
			if tc.name == "oversized crossing edge" {
				last.Context = strings.Repeat("x", rebuildFamilyRowBytes)
			}
			b.AddBatch([]*graph.Node{from, to}, []*graph.Edge{first, &last})
			switch tc.name {
			case "occupied scratch":
				require.NoError(t, b.pending.Set(rebuildFamilyDropPrefix, []byte("prior"), pebble.NoSync))
			case "edge identity":
				wrong, err := encodeEdge(first)
				require.NoError(t, err)
				require.NoError(t, b.pending.Set(pendingEdgeKey(&last), wrong, pebble.NoSync))
			case "endpoint identity":
				wrong, err := encodeNode(from)
				require.NoError(t, err)
				require.NoError(t, b.pending.Set(pendingNodeKey(to.ID), wrong, pebble.NoSync))
			}
			before := rebuildFrameworkCustodyRows(t, b)
			count, err := b.RebuildFrameworkFamilyGate(t.Context(), func(*graph.Edge) bool { return true },
				func(from, to string) bool { return from != to })
			require.ErrorIs(t, err, ErrRebuildFrameworkFamilyGate)
			require.ErrorIs(t, err, tc.cause)
			require.ErrorContains(t, err, tc.reason)
			require.Zero(t, count)
			require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
		})
	}
}

func TestRebuildDjangoDescriptorLazyMethodIndex(t *testing.T) {
	const descriptor = "unresolved::_iterable_class"
	method := &graph.Node{ID: "model.py::__iter__", Kind: graph.KindMethod, Name: "__iter__", FilePath: "model.py",
		Meta: map[string]any{"receiver": "ModelIterable", "padding": strings.Repeat("x", 16<<20)}}
	for _, tc := range []struct {
		kind        graph.EdgeKind
		to          string
		wantRefusal bool
	}{
		{graph.EdgeCalls, "unresolved::other", false},
		{graph.EdgeImports, descriptor, false},
		{graph.EdgeCalls, descriptor, true},
		{graph.EdgeReferences, descriptor, true},
	} {
		t.Run(string(tc.kind)+"/"+tc.to, func(t *testing.T) {
			b := newBuild(t, FullProfile)
			edge := &graph.Edge{From: "caller", To: tc.to, Kind: tc.kind, FilePath: "caller.py", Line: 1}
			b.AddBatch([]*graph.Node{method}, []*graph.Edge{edge})
			rules := RebuildDjangoRules{
				ClaimsTo:     func(to string) bool { return to == descriptor },
				Rebind:       func(*graph.Edge, string) { t.Fatal("unexpected rebind") },
				DefaultClass: "ModelIterable",
			}
			unread := func(func(ParseFacts, error) bool) { t.Fatal("unexpected receipt read") }
			before := rebuildFrameworkCustodyRows(t, b)
			count, err := b.RebuildDjangoDescriptor(t.Context(), unread, rules)
			if tc.wantRefusal {
				require.ErrorIs(t, err, ErrRebuildDjangoDescriptor)
				require.ErrorIs(t, err, ErrRebuildLinkInputSize)
				require.ErrorContains(t, err, "Django descriptor node row")
			} else {
				require.NoError(t, err)
			}
			require.Zero(t, count)
			require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
		})
	}
}
