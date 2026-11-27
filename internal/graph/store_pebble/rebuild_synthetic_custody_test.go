package store_pebble

import (
	"iter"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildSyntheticCustodyCapabilities(t *testing.T) {
	const file, caller, field, process, env = "bound.go", "bound.go::T.Run", "bound.go::T.mu", "exec.Command", "cfg::env::MODE"
	receiver := map[string]any{"receiver": "T"}
	nodes := []*graph.Node{
		{ID: caller, Kind: graph.KindMethod, Name: "Run", FilePath: file, Language: "go", Meta: receiver},
		{ID: field, Kind: graph.KindField, Name: "mu", FilePath: file, Language: "go", Meta: receiver},
		{ID: process, Kind: graph.KindFunction, Name: "Command", FilePath: file, Language: "go"},
		{ID: env, Kind: graph.KindConfigKey, Name: "MODE", FilePath: file, Language: "go"},
	}
	edges := []*graph.Edge{
		{From: caller, To: env, Kind: graph.EdgeReadsConfig, FilePath: file, Line: 1},
		{From: caller, To: field, Kind: graph.EdgeReads, FilePath: file, Line: 2},
		{From: caller, To: "unresolved::Lock", Kind: graph.EdgeCalls, FilePath: file, Line: 3, Meta: map[string]any{"recv_field": "mu"}},
		{From: caller, To: "unresolved::Lock", Kind: graph.EdgeCalls, FilePath: file, Line: 4, Meta: map[string]any{"recv_field": "mu"}},
		{From: caller, To: process, Kind: graph.EdgeCalls, FilePath: file, Line: 5},
	}
	b, receipts := seedSyntheticCustody(t, ParseFacts{
		EffectiveLanguage: "go", File: graph.FileMetaRow{FilePath: file}, Nodes: nodes, Edges: edges,
	})
	overwrite := graph.Edge{From: caller, To: env, Kind: graph.EdgeReadsEnv, FilePath: file, Line: 1, Origin: graph.OriginASTInferred}
	keeper := overwrite
	keeper.Line, keeper.Via = 6, "keep"
	b.AddBatch(nil, []*graph.Edge{&overwrite, &keeper})
	mechanism := func(to string) string {
		if to == process {
			return process
		}
		return ""
	}
	noImport := func(string, string) (string, string) { return "", "" }
	rules := ReceiverMutationRules{
		OwnerKey:        func(_ string, receiver string) string { return receiver },
		BareCallName:    func(to string) string { return strings.TrimPrefix(to, "unresolved::") },
		IsStdlibMutator: func(name string) bool { return name == "Lock" },
	}
	stats, err := b.RebuildCapabilities(t.Context(), receipts, mechanism, noImport, rules, 1)
	require.NoError(t, err)
	require.Equal(t, uint64(1), stats.Emitted)
	kept, err := b.pendingEdge(pendingEdgeKey(&keeper))
	require.NoError(t, err)
	require.Equal(t, &keeper, kept)
	live, err := b.pendingEdge(pendingEdgeKey(&overwrite))
	require.NoError(t, err)
	require.NotNil(t, live)
	require.Equal(t, graph.OriginASTResolved, live.Origin)
	loser := graph.Edge{From: caller, To: field, Kind: graph.EdgeAccessesField, FilePath: file, Line: 4}
	live, err = b.pendingEdge(pendingEdgeKey(&loser))
	require.NoError(t, err)
	require.Nil(t, live)
	requireSyntheticCustody(t, b, map[graph.EdgeIdentity]rebuildEdgeProof{
		graph.EdgeIdentityFor(&overwrite):                                                                             rebuildProofDirect,
		{From: caller, To: field, Kind: graph.EdgeAccessesField, FilePath: file, Line: 2}:                             rebuildProofDirect,
		{From: caller, To: field, Kind: graph.EdgeAccessesField, FilePath: file, Line: 3}:                             rebuildProofReceiver,
		{From: caller, To: "string::process::exec.Command", Kind: graph.EdgeExecutesProcess, FilePath: file, Line: 5}: rebuildProofDirect,
	})
}

func TestRebuildSyntheticCustodyEventChannel(t *testing.T) {
	const file = "events.ts"
	nodes := []*graph.Node{
		{ID: "emit", Kind: graph.KindFunction, FilePath: file, Language: "typescript"},
		{ID: "listen", Kind: graph.KindFunction, FilePath: file, Language: "typescript"},
		{ID: "keep", Kind: graph.KindFunction, FilePath: file, Language: "typescript"},
		{ID: "event:a", Kind: graph.KindString, FilePath: file, Language: "typescript"},
		{ID: "event:z", Kind: graph.KindString, FilePath: file, Language: "typescript"},
	}
	keeper := &graph.Edge{From: "emit", To: "keep", Kind: graph.EdgeCalls, FilePath: file, Line: 1, Via: "keeper"}
	edges := []*graph.Edge{keeper}
	for _, topic := range []string{"event:a", "event:z"} {
		edges = append(edges,
			&graph.Edge{From: "emit", To: topic, Kind: graph.EdgeEmits, FilePath: file, Line: 1},
			&graph.Edge{From: "listen", To: topic, Kind: graph.EdgeListensOn, FilePath: file, Line: 2},
			&graph.Edge{From: "keep", To: topic, Kind: graph.EdgeListensOn, FilePath: file, Line: 3})
	}
	b, receipts := seedSyntheticCustody(t, ParseFacts{
		EffectiveLanguage: "typescript", File: graph.FileMetaRow{FilePath: file}, Nodes: nodes, Edges: edges,
	})
	stats, err := b.RebuildEventChannel(t.Context(), receipts, 1,
		[]string{"event:"}, func(_ string, label string) (string, int) { return label, 4 },
		func(event, _ string, emitter, listener *graph.Edge) *graph.Edge {
			return &graph.Edge{From: emitter.From, To: listener.From, Kind: graph.EdgeCalls, FilePath: emitter.FilePath, Line: emitter.Line, Via: event}
		})
	require.NoError(t, err)
	require.EqualValues(t, 1, stats.Pairs)
	winner := &graph.Edge{From: "emit", To: "listen", Kind: graph.EdgeCalls, FilePath: file, Line: 1, Via: "event:z"}
	for _, want := range []*graph.Edge{winner, keeper} {
		live, err := b.pendingEdge(pendingEdgeKey(want))
		require.NoError(t, err)
		require.Equal(t, want, live)
	}
	requireSyntheticCustody(t, b, map[graph.EdgeIdentity]rebuildEdgeProof{graph.EdgeIdentityFor(winner): rebuildProofEventChannel})
}

func TestRebuildSyntheticCustodySwiftObjC(t *testing.T) {
	swift := &graph.Node{ID: "swift", Kind: graph.KindMethod, Name: "foo", FilePath: "x.swift", Language: "swift", StartLine: 3,
		Meta: map[string]any{"objc_selector": "foo"}}
	objc := &graph.Node{ID: "objc", Kind: graph.KindMethod, Name: "foo", FilePath: "x.m", Language: "objc", StartLine: 4}
	winner := &graph.Edge{From: swift.ID, To: objc.ID, Kind: graph.EdgeReferences, FilePath: swift.FilePath, Line: swift.StartLine}
	keeper := &graph.Edge{From: objc.ID, To: swift.ID, Kind: graph.EdgeReferences, FilePath: objc.FilePath, Line: objc.StartLine, Via: "keeper"}
	b, receipts := seedSyntheticCustody(t,
		ParseFacts{EffectiveLanguage: swift.Language, File: graph.FileMetaRow{FilePath: swift.FilePath}, Nodes: []*graph.Node{swift}},
		ParseFacts{EffectiveLanguage: objc.Language, File: graph.FileMetaRow{FilePath: objc.FilePath}, Nodes: []*graph.Node{objc}, Edges: []*graph.Edge{keeper}},
	)
	reduce := func(_ RebuildView, propose func(*graph.Edge) error) error {
		// Explicit and heuristic selector matches can propose the same pair.
		for _, e := range []*graph.Edge{winner, keeper, winner} {
			if err := propose(e); err != nil {
				return err
			}
		}
		return nil
	}
	stats, err := b.RebuildSwiftObjC(t.Context(), func(func(ParseFacts, error) bool) {}, 1, reduce)
	require.ErrorIs(t, err, ErrRebuildUnknownProvenance)
	require.Zero(t, stats)
	live, err := b.pendingEdge(pendingEdgeKey(winner))
	require.NoError(t, err)
	require.Nil(t, live)
	requireSyntheticCustody(t, b, nil)

	stats, err = b.RebuildSwiftObjC(t.Context(), receipts, 1, reduce)
	require.NoError(t, err)
	require.EqualValues(t, 1, stats.Edges)
	requireSyntheticCustody(t, b, map[graph.EdgeIdentity]rebuildEdgeProof{graph.EdgeIdentityFor(winner): rebuildProofSwiftObjC})
}

func TestRebuildSyntheticCustodyReactNative(t *testing.T) {
	meta := map[string]any{"rn_module": "Module", "rn_method": "run", "via": "rn.native"}
	caller := &graph.Node{ID: "caller", Kind: graph.KindFunction, FilePath: "x.ts", Language: "typescript"}
	ios := &graph.Node{ID: "ios", Kind: graph.KindMethod, FilePath: "x.swift", Language: "swift", StartLine: 3, Meta: meta}
	android := &graph.Node{ID: "android", Kind: graph.KindMethod, FilePath: "x.kt", Language: "kotlin", StartLine: 4, Meta: meta}
	raw := &graph.Edge{From: caller.ID, To: "unresolved::run", Kind: graph.EdgeCalls, FilePath: caller.FilePath, Line: 1, Meta: meta}
	loser := *raw
	loser.Line = 2
	call := func(e *graph.Edge, n *graph.Node) *graph.Edge {
		return &graph.Edge{From: e.From, To: n.ID, Kind: graph.EdgeCalls, FilePath: e.FilePath, Line: e.Line, Via: "bridge"}
	}
	pair := func(from, to *graph.Node, _, _ string) *graph.Edge {
		return &graph.Edge{From: from.ID, To: to.ID, Kind: graph.EdgeReferences, FilePath: from.FilePath, Line: from.StartLine, Via: "pair"}
	}
	callKeeper, pairKeeper := call(raw, android), pair(android, ios, "", "")
	callKeeper.Via, pairKeeper.Via = "keep bridge", "keep pair"
	b, receipts := seedSyntheticCustody(t,
		ParseFacts{EffectiveLanguage: caller.Language, File: graph.FileMetaRow{FilePath: caller.FilePath}, Nodes: []*graph.Node{caller}, Edges: []*graph.Edge{raw, &loser, callKeeper}},
		ParseFacts{EffectiveLanguage: ios.Language, File: graph.FileMetaRow{FilePath: ios.FilePath}, Nodes: []*graph.Node{ios}},
		ParseFacts{EffectiveLanguage: android.Language, File: graph.FileMetaRow{FilePath: android.FilePath}, Nodes: []*graph.Node{android}, Edges: []*graph.Edge{pairKeeper}},
	)
	bridgeStats, err := b.RebuildReactNativeBridge(t.Context(), receipts, 1,
		func(lang string) bool { return lang == "typescript" },
		func(e *graph.Edge) bool { return e.Kind == graph.EdgeCalls && e.Meta["via"] == "rn.native" }, call)
	require.NoError(t, err)
	require.EqualValues(t, 1, bridgeStats.Edges)
	pairStats, err := b.RebuildReactNativeNativePairing(t.Context(), receipts, 1,
		func(lang string) string {
			switch lang {
			case "swift":
				return "ios"
			case "kotlin":
				return "android"
			default:
				return ""
			}
		}, pair)
	require.NoError(t, err)
	require.EqualValues(t, 1, pairStats.Edges)
	for _, want := range []*graph.Edge{callKeeper, pairKeeper} {
		live, err := b.pendingEdge(pendingEdgeKey(want))
		require.NoError(t, err)
		require.Equal(t, want, live)
	}
	requireSyntheticCustody(t, b, map[graph.EdgeIdentity]rebuildEdgeProof{
		graph.EdgeIdentityFor(call(raw, ios)):             rebuildProofRNBridge,
		graph.EdgeIdentityFor(pair(ios, android, "", "")): rebuildProofRNPair,
	})
}

func seedSyntheticCustody(t *testing.T, facts ...ParseFacts) (*BuildStore, iter.Seq2[ParseFacts, error]) {
	t.Helper()
	b := newBuild(t, FullProfile)
	for _, f := range facts {
		b.AddBatch(f.Nodes, f.Edges)
	}
	return b, func(yield func(ParseFacts, error) bool) {
		for _, f := range facts {
			if !yield(f, nil) {
				return
			}
		}
	}
}

func requireSyntheticCustody(t *testing.T, b *BuildStore, want map[graph.EdgeIdentity]rebuildEdgeProof) {
	t.Helper()
	ctx := t.Context()
	for id, proof := range want {
		live := pendingEdgeKey(&graph.Edge{From: id.From, To: id.To, Kind: id.Kind, FilePath: id.FilePath, Line: id.Line})
		edge, err := b.pendingEdge(live)
		require.NoError(t, err)
		require.NotNil(t, edge, "%+v", id)
		custody, err := readRebuildEdgeCustody(b.pending, live)
		require.NoError(t, err)
		require.Equal(t, proof, custody.proof, "%+v", id)
		require.Empty(t, custody.raw)
	}
	var rows int
	require.NoError(t, pendingVisit(ctx, b.pending, rebuildLinkRecorderPrefix, func(row) error {
		rows++
		return nil
	}))
	// No other row, including a stationary keeper, losing proposal or node,
	// may have a recorder. Each expected winner was checked above.
	require.Equal(t, len(want), rows)
}
