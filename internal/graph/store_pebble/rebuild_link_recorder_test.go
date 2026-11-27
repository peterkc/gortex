package store_pebble

import (
	"errors"
	"iter"
	"slices"
	"testing"

	"github.com/cockroachdb/pebble/v2"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildLinkRecorderFollowsInstalledMoves(t *testing.T) {
	ctx := t.Context()
	tracked, control := newBuild(t, FullProfile), newBuild(t, FullProfile)
	raw := graph.Edge{From: "caller", To: "unresolved::a", Kind: graph.EdgeCalls, FilePath: "routes.ts", Line: 5, Via: "first"}
	loser, blocked, stationary := raw, raw, raw
	loser.To, loser.Via = "unresolved::b", "second"
	blocked.To, blocked.Via = "unresolved::c", "blocked"
	stationary.To, stationary.Kind, stationary.Via = "occupied", graph.EdgeReferences, "stationary"
	contract := &graph.Node{ID: "route", Kind: graph.KindContract, FilePath: raw.FilePath, Language: "typescript", Meta: map[string]any{"path": "/hello"}}
	framework := &graph.Edge{From: raw.From, To: contract.ID, Kind: graph.EdgeProvides, FilePath: raw.FilePath, Line: 2, Meta: map[string]any{"method": "GET"}}
	for _, b := range []*BuildStore{tracked, control} {
		b.AddBatch([]*graph.Node{contract}, []*graph.Edge{&raw, &loser, &blocked, &stationary, framework})
	}

	// ApplyLinkReindexes takes moves in complete old-key order.
	move := func(old graph.Edge, to string) graph.EdgeReindex {
		live := old
		live.To, live.Kind = to, graph.EdgeReferences
		return graph.EdgeReindex{Edge: &live, OldTo: old.To, OldKind: old.Kind}
	}
	winner := move(raw, "free")
	moves := []graph.EdgeReindex{winner, move(loser, "free"), move(blocked, stationary.To)}
	if err := tracked.ApplyLinkReindexes(ctx, moves); err != nil {
		t.Fatal(err)
	}
	if err := control.withWrites(func() error { return control.reindexChunkWithRecorders(moves, false) }); err != nil {
		t.Fatal(err)
	}

	// Strings retain the exact bytes and do not alias Pebble iterator buffers.
	readRows := func(b *BuildStore, prefix []byte) [][2]string {
		t.Helper()
		var rows [][2]string
		if err := pendingVisit(ctx, b.pending, prefix, func(r row) error {
			rows = append(rows, [2]string{string(r.key), string(r.value)})
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return rows
	}
	for _, family := range []struct {
		prefix []byte
		count  int
	}{
		{[]byte{familyNode}, 1},
		{[]byte{familyEdge, edgeOut}, 3},
		{[]byte{familyEdge, edgeIn}, 3},
	} {
		got, want := readRows(tracked, family.prefix), readRows(control, family.prefix)
		if len(want) != family.count || !slices.Equal(got, want) {
			t.Fatalf("pending rows under %q: got %q, want %d control rows %q", family.prefix, got, family.count, want)
		}
	}
	for _, edge := range []*graph.Edge{winner.Edge, &stationary} {
		got, err := tracked.pendingEdge(pendingEdgeKey(edge))
		if err != nil || got == nil || got.Via != edge.Via {
			t.Fatalf("installed edge at %q = %+v, %v; want Via %q", edge.To, got, err, edge.Via)
		}
	}

	rawCustody := rebuildEdgeCustody{raw: pendingEdgeKey(&raw)}
	requireRebuildLinkCustody(t, control, winner.Edge, rebuildEdgeCustody{proof: rebuildProofUntrackedMove})
	requireRebuildLinkCustody(t, tracked, winner.Edge, rawCustody)
	requireRebuildLinkCustody(t, tracked, &stationary, rebuildEdgeCustody{})
	requireRebuildLinkCustody(t, tracked, framework, rebuildEdgeCustody{})

	second := move(*winner.Edge, "second-live")
	if err := tracked.ApplyLinkReindexes(ctx, []graph.EdgeReindex{second}); err != nil {
		t.Fatal(err)
	}
	requireRebuildLinkCustody(t, tracked, winner.Edge, rebuildEdgeCustody{})
	requireRebuildLinkCustody(t, tracked, second.Edge, rawCustody)

	last := move(*second.Edge, "generic-live")
	tracked.ReindexEdges([]graph.EdgeReindex{last})
	requireRebuildLinkCustody(t, tracked, second.Edge, rebuildEdgeCustody{})
	requireRebuildLinkCustody(t, tracked, last.Edge, rawCustody)
}

func TestRebuildLinkRecorderFollowsSameChunkChain(t *testing.T) {
	ctx := t.Context()
	b := newBuild(t, FullProfile)
	raw := graph.Edge{From: "caller", To: "unresolved::a", Kind: graph.EdgeCalls, FilePath: "routes.ts", Line: 5}
	live, middle, final := raw, raw, raw
	live.To, middle.To, final.To = "first", "unstored", "last"
	b.AddEdge(&raw)
	require.NoError(t, b.ApplyLinkReindexes(ctx, []graph.EdgeReindex{{Edge: &live, OldTo: raw.To}}))
	b.ReindexEdges([]graph.EdgeReindex{
		{Edge: &middle, OldTo: live.To},
		{Edge: &final, OldTo: middle.To},
	})
	view := b.RebuildView()
	for _, e := range []*graph.Edge{&live, &middle} {
		id := graph.EdgeIdentityFor(e)
		stored, err := view.Edge(ctx, id)
		require.NoError(t, err)
		require.Nil(t, stored)
		requireRebuildLinkCustody(t, b, e, rebuildEdgeCustody{})
	}
	id := graph.EdgeIdentityFor(&final)
	stored, err := view.Edge(ctx, id)
	require.NoError(t, err)
	require.NotNil(t, stored)
	requireRebuildLinkCustody(t, b, &final, rebuildEdgeCustody{raw: pendingEdgeKey(&raw)})
}

func TestRebuildLinkRecorderFollowsGuardWinner(t *testing.T) {
	for _, tc := range []struct {
		name      string
		incumbent bool
		alias     bool
		tracked   bool
	}{
		{name: "free-revert", tracked: true},
		{name: "stationary-incumbent", incumbent: true, tracked: true},
		{name: "replacement-takes-source-recorder", incumbent: true, alias: true, tracked: true},
		{name: "untracked-replacement-clears-old-recorder", incumbent: true, alias: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			b := newBuild(t, FullProfile)
			raw := graph.Edge{From: "caller", To: "unresolved::original", Kind: graph.EdgeCalls, FilePath: "routes.ts", Line: 5, Via: "moved"}
			live := raw
			live.To, live.Origin = "resolved", graph.OriginASTInferred
			if tc.tracked {
				b.AddEdge(&raw)
				require.NoError(t, b.ApplyLinkReindexes(ctx, []graph.EdgeReindex{{Edge: &live, OldTo: raw.To}}))
			} else {
				b.AddEdge(&live)
			}
			wantRaw := &raw
			incumbentRaw := raw
			incumbentRaw.To, incumbentRaw.Via = "unresolved::other", "incumbent"
			const destination = "unresolved::reverted"
			if tc.incumbent {
				incumbent := incumbentRaw
				incumbent.To, incumbent.Origin = destination, graph.OriginASTInferred
				b.AddEdge(&incumbentRaw)
				require.NoError(t, b.ApplyLinkReindexes(ctx, []graph.EdgeReindex{{Edge: &incumbent, OldTo: incumbentRaw.To}}))
				if !tc.alias {
					wantRaw = &incumbentRaw
				}
			}
			// Guard duplicates borrow the same pointer. After the first revert
			// mutates To, the duplicate names the destination as its old key.
			reverted := live
			reverted.To = destination
			moves := []graph.EdgeReindex{{Edge: &reverted, OldTo: live.To}}
			if tc.alias {
				moves = append(moves, graph.EdgeReindex{Edge: &reverted, OldTo: destination})
			}
			_, err := b.ApplyRebuildGuardPage(ctx, moves, nil)
			require.NoError(t, err)
			view := b.RebuildView()
			liveID := graph.EdgeIdentityFor(&live)
			stored, err := view.Edge(ctx, liveID)
			require.NoError(t, err)
			require.Nil(t, stored)
			requireRebuildLinkCustody(t, b, &live, rebuildEdgeCustody{})
			winnerID := graph.EdgeIdentityFor(&reverted)
			winner, err := view.Edge(ctx, winnerID)
			require.NoError(t, err)
			require.NotNil(t, winner)
			require.Equal(t, wantRaw.Via, winner.Via)
			require.Empty(t, winner.Origin)
			want := rebuildEdgeCustody{proof: rebuildProofUntrackedMove}
			if tc.tracked {
				want = rebuildEdgeCustody{raw: pendingEdgeKey(wantRaw)}
			}
			requireRebuildLinkCustody(t, b, &reverted, want)
		})
	}
}

func TestRebuildLinkRecorderFollowsGuardSourceRestoration(t *testing.T) {
	ctx := t.Context()
	b := newBuild(t, FullProfile)
	call := graph.Edge{From: "caller", To: "resolved", Kind: graph.EdgeCalls, FilePath: "routes.ts", Line: 5}
	original := graph.Edge{From: call.To, To: "callee", Kind: graph.EdgeArgOf, FilePath: call.FilePath, Line: call.Line}
	live := original
	live.To = "callee#param:arg"
	b.AddEdge(&call)
	b.AddEdge(&original)
	require.NoError(t, b.ApplyLinkReindexes(ctx, []graph.EdgeReindex{{Edge: &live, OldTo: original.To}}))
	view := b.RebuildView()
	liveID := graph.EdgeIdentityFor(&live)
	rawCustody := rebuildEdgeCustody{raw: pendingEdgeKey(&original)}
	requireRebuildLinkCustody(t, b, &live, rawCustody)

	reverted := call
	reverted.To = "unresolved::callee"
	count, err := b.ApplyRebuildGuardPage(ctx, []graph.EdgeReindex{{Edge: &reverted, OldTo: call.To}}, []graph.PlaceholderRepoint{
		{OldFrom: call.To, NewFrom: reverted.To, FilePath: call.FilePath, Line: call.Line},
	})
	require.NoError(t, err)
	require.Equal(t, 1, count)
	stored, err := view.Edge(ctx, liveID)
	require.NoError(t, err)
	require.Nil(t, stored)
	requireRebuildLinkCustody(t, b, &live, rebuildEdgeCustody{})
	restored := live
	restored.From = reverted.To
	restoredID := graph.EdgeIdentityFor(&restored)
	stored, err = view.Edge(ctx, restoredID)
	require.NoError(t, err)
	require.Equal(t, &restored, stored)
	requireRebuildLinkCustody(t, b, &restored, rawCustody)
}

func requireRebuildLinkCustody(t *testing.T, b *BuildStore, e *graph.Edge, want rebuildEdgeCustody) {
	t.Helper()
	got, err := readRebuildEdgeCustody(b.pending, pendingEdgeKey(e))
	require.NoError(t, err)
	require.Equal(t, want, got, "custody for %+v", graph.EdgeIdentityFor(e))
}

func valueRefsScratchTestRules() RebuildValueRefsRules {
	// These stage tests need just one named declaration. The resolver oracle
	// exercises the real language policy and declaration election.
	return RebuildValueRefsRules{
		Name: func(e *graph.Edge) string {
			name, _ := e.Meta["name"].(string)
			return name
		},
		Distinctive: func(name string) bool { return name == "LIMIT_KEY" },
		Elect: func(_ int, decls iter.Seq2[string, int]) (string, bool) {
			for id := range decls {
				return id, false
			}
			return "", false
		},
		Shadows:   func(_, _ string) bool { return false },
		Generated: func(*graph.Node) bool { return false },
		Rebind: func(e *graph.Edge, target string, _ bool) (graph.EdgeReindex, bool) {
			old := e.To
			e.To = target
			return graph.EdgeReindex{Edge: e, OldTo: old}, true
		},
	}
}

func TestRebuildValueRefsStageInputs(t *testing.T) {
	for _, mode := range []string{"nil receipts", "nil name", "nil distinctive", "nil elect", "nil shadows", "nil generated", "nil rebind", "scratch occupied"} {
		t.Run(mode, func(t *testing.T) {
			b := newBuild(t, FullProfile)
			rules := valueRefsScratchTestRules()
			receipts := func(func(ParseFacts, error) bool) { t.Fatal("replayed receipts before admission") }
			want := "value refs inputs"
			switch mode {
			case "nil receipts":
				receipts = nil
			case "nil name":
				rules.Name = nil
			case "nil distinctive":
				rules.Distinctive = nil
			case "nil elect":
				rules.Elect = nil
			case "nil shadows":
				rules.Shadows = nil
			case "nil generated":
				rules.Generated = nil
			case "nil rebind":
				rules.Rebind = nil
			case "scratch occupied":
				want = "value refs scratch occupied"
				require.NoError(t, b.pending.Set(rebuildValueRefsPrefix, []byte("another owner's scratch"), pebble.NoSync))
			}
			before := rebuildFrameworkCustodyRows(t, b)
			count, err := b.RebuildValueRefs(t.Context(), receipts, rules)
			require.ErrorIs(t, err, ErrRebuildValueRefs)
			require.ErrorContains(t, err, want)
			require.Zero(t, count)
			require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
		})
	}
}

func TestRebuildValueRefsStageReceiptFailure(t *testing.T) {
	const file, name = "value.go", "LIMIT_KEY"
	reader := &graph.Node{ID: "reader", FilePath: file, Kind: graph.KindFunction, Language: "go"}
	decl := &graph.Node{ID: "declaration", FilePath: file, Kind: graph.KindConstant, Language: "go", Name: name}
	use := &graph.Edge{From: reader.ID, To: "unresolved::valueref::" + name, Kind: graph.EdgeReads, FilePath: file, Line: 10,
		Meta: map[string]any{"name": name}}
	facts := ParseFacts{
		EffectiveLanguage: "go",
		File:              graph.FileMetaRow{FilePath: file},
		Nodes:             []*graph.Node{reader, decl},
		Edges:             []*graph.Edge{use},
	}
	failure := errors.New("fixture receipt failed")
	for _, mode := range []string{"edge replay", "node replay", "unchanged rebind", "wrong rebind"} {
		t.Run(mode, func(t *testing.T) {
			b, exact := seedSyntheticCustody(t, facts)
			before := rebuildFrameworkCustodyRows(t, b)
			var replay int
			receipts := func(yield func(ParseFacts, error) bool) {
				replay++
				if yield(facts, nil) && (mode == "edge replay" || mode == "node replay" && replay == 2) {
					yield(ParseFacts{}, failure)
				}
			}
			rules := valueRefsScratchTestRules()
			switch mode {
			case "unchanged rebind":
				rules.Rebind = func(*graph.Edge, string, bool) (graph.EdgeReindex, bool) { return graph.EdgeReindex{}, false }
			case "wrong rebind":
				rules.Rebind = func(e *graph.Edge, _ string, _ bool) (graph.EdgeReindex, bool) {
					old := e.To
					e.To = "wrong"
					return graph.EdgeReindex{Edge: e, OldTo: old}, true
				}
			}
			count, err := b.RebuildValueRefs(t.Context(), receipts, rules)
			require.ErrorIs(t, err, ErrRebuildValueRefs)
			if mode == "edge replay" || mode == "node replay" {
				require.ErrorIs(t, err, failure)
			} else {
				require.ErrorContains(t, err, "value refs move did not rebind")
			}
			require.Zero(t, count)
			require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
			// A clean replay after a failed proof sees no leftover scratch.
			count, err = b.RebuildValueRefs(t.Context(), exact, valueRefsScratchTestRules())
			require.NoError(t, err)
			require.EqualValues(t, 1, count)
			live := *use
			live.To = decl.ID
			stored, err := b.pendingEdge(pendingEdgeKey(&live))
			require.NoError(t, err)
			require.Equal(t, &live, stored)
		})
	}
}

func TestRebuildValueRefsCaptureNarrowing(t *testing.T) {
	const file, other, name = "value.go", "other.go", "LIMIT_KEY"
	b := newBuild(t, FullProfile)
	reader := &graph.Node{ID: "reader", FilePath: file, Kind: graph.KindFunction, Language: "go"}
	decl := &graph.Node{ID: "declaration", FilePath: file, Kind: graph.KindConstant, Language: "go", Name: name}
	away := &graph.Node{ID: "unrelated", FilePath: other, Kind: graph.KindConstant, Language: "go", Name: name}
	use := &graph.Edge{From: reader.ID, To: "unresolved::valueref::" + name, Kind: graph.EdgeReads, FilePath: file, Line: 10,
		Meta: map[string]any{"name": name}}
	settled, ordinary, originated, nonValue := *use, *use, *use, *use
	settled.Line, ordinary.Line, originated.Line, nonValue.Line = 11, 12, 13, 14
	ordinary.To, nonValue.To = "ordinary", "originally-ordinary"
	b.AddBatch([]*graph.Node{reader, decl, away}, []*graph.Edge{use, &settled, &ordinary, &originated, &nonValue})
	settledMove, originatedMove, nonValueMove := settled, originated, nonValue
	settledMove.To, originatedMove.To, nonValueMove.To = decl.ID, "unresolved::valueref::moved", "unresolved::valueref::not-originated"
	require.NoError(t, b.ApplyLinkReindexes(t.Context(), []graph.EdgeReindex{
		{Edge: &nonValueMove, OldTo: nonValue.To}, {Edge: &settledMove, OldTo: settled.To}, {Edge: &originatedMove, OldTo: originated.To},
	}))
	// Deliberately corrupt an unrelated file's node bytes; a whole-node
	// scan fails here, even though that node cannot change this election.
	require.NoError(t, b.pending.Set(pendingNodeKey(away.ID), []byte{0xff}, pebble.NoSync))
	p := rebuildValueRefs{
		rebuildCallMoves: rebuildCallMoves{b: b, prefix: rebuildValueRefsPrefix, rowBytes: 16 << 20, label: "value refs", inputRow: "input row"},
		rules:            valueRefsScratchTestRules(),
	}
	require.NoError(t, p.captureReads(t.Context()))
	for _, pair := range []struct{ raw, live *graph.Edge }{
		{use, use}, {&settled, &settledMove}, {&originated, &originatedMove},
	} {
		claim, err := p.marked(append(p.column('r'), pendingEdgeKey(pair.raw)...))
		require.NoError(t, err)
		require.Equal(t, append([]byte{rebuildCapabilityWanted}, pendingEdgeKey(pair.live)...), claim)
	}
	for _, raw := range []*graph.Edge{&ordinary, &nonValue} {
		claim, err := p.marked(append(p.column('r'), pendingEdgeKey(raw)...))
		require.NoError(t, err)
		require.Empty(t, claim)
	}
	var claims int
	require.NoError(t, p.visit(t.Context(), p.column('r'), func(row) error {
		claims++
		return nil
	}))
	require.Equal(t, 3, claims)
	require.NoError(t, p.captureNodes(t.Context()))
	var declarations int
	require.NoError(t, p.visit(t.Context(), p.column('d'), func(r row) error {
		declarations++
		require.Equal(t, p.key('d', file, name, decl.ID), r.key)
		return nil
	}))
	require.Equal(t, 1, declarations)
}

func TestRebuildValueRefsSelectedFileIndexGuards(t *testing.T) {
	const file, name = "value.go", "LIMIT_KEY"
	for _, mode := range []string{"identity", "missing", "ownership"} {
		t.Run(mode, func(t *testing.T) {
			b := newBuild(t, FullProfile)
			decl := &graph.Node{ID: "declaration", FilePath: file, Kind: graph.KindConstant, Language: "go", Name: name}
			b.AddNode(decl)
			p := rebuildValueRefs{
				rebuildCallMoves: rebuildCallMoves{b: b, prefix: rebuildValueRefsPrefix, rowBytes: 16 << 20, label: "value refs", inputRow: "input row"},
				rules:            valueRefsScratchTestRules(),
			}
			require.NoError(t, p.put(p.key('q', file, name), nil))
			fileKey := keyStrings([]byte{familyFile}, file, decl.ID)
			switch mode {
			case "identity":
				require.NoError(t, b.pending.Set(fileKey, []byte("wrong"), pebble.NoSync))
			case "missing":
				require.NoError(t, b.pending.Delete(pendingNodeKey(decl.ID), pebble.NoSync))
			case "ownership":
				changed := *decl
				changed.FilePath = "other.go"
				encoded, err := encodeNode(&changed)
				require.NoError(t, err)
				require.NoError(t, b.pending.Set(pendingNodeKey(decl.ID), encoded, pebble.NoSync))
			}
			err := p.captureNodes(t.Context())
			require.ErrorIs(t, err, ErrRebuildView)
			want := "value refs file index " + mode
			if mode == "missing" {
				want = "value refs indexed node missing"
			}
			require.ErrorContains(t, err, want)
		})
	}
}
