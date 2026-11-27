package store_pebble

import (
	"strings"
	"testing"

	"github.com/cockroachdb/pebble/v2"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildObserverOriginalKeyAndScratchOwnership(t *testing.T) {
	for _, mode := range []string{"checked move", "untracked move with destination receipt", "missing declaration", "changed declaration", "scratch occupied"} {
		t.Run(mode, func(t *testing.T) {
			registrar := setStateProofMethod("register", "subscribe", 2)
			dispatch := setStateProofMethod("dispatch", "notify", 4)
			callback := setStateProofMethod("callback", "callback", 6)
			access := &graph.Edge{From: registrar.ID, To: "field", Kind: graph.EdgeAccessesField, FilePath: registrar.FilePath, Line: 3}
			other := &graph.Edge{From: dispatch.ID, To: "field", Kind: graph.EdgeAccessesField, FilePath: dispatch.FilePath, Line: 5}
			raw := &graph.Edge{From: "setup", To: "unresolved::subscribe", Kind: graph.EdgeCalls, FilePath: registrar.FilePath, Line: 9}
			ref := &graph.Edge{From: "setup", To: callback.ID, Kind: graph.EdgeReferences, FilePath: registrar.FilePath, Line: 9}
			facts := setStateProofFacts([]*graph.Node{registrar, dispatch, callback}, access, other, raw, ref)
			b, _ := seedSyntheticCustody(t, facts)
			live := *raw
			live.To = registrar.ID
			move := []graph.EdgeReindex{{Edge: &live, OldTo: raw.To}}
			ctx := t.Context()
			if mode == "untracked move with destination receipt" {
				b.ReindexEdges(move)
				facts.Edges = append(facts.Edges, &live)
				custody, err := readRebuildEdgeCustody(b.pending, pendingEdgeKey(&live))
				require.NoError(t, err)
				require.Equal(t, rebuildProofUntrackedMove, custody.proof)
			} else {
				require.NoError(t, b.ApplyLinkReindexes(ctx, move))
				custody, err := readRebuildEdgeCustody(b.pending, pendingEdgeKey(&live))
				require.NoError(t, err)
				require.Equal(t, pendingEdgeKey(raw), custody.raw)
			}
			switch mode {
			case "missing declaration":
				facts.Nodes = facts.Nodes[:2]
			case "changed declaration":
				changed := *callback
				changed.StartLine++
				facts.Nodes[2] = &changed
			case "scratch occupied":
				require.NoError(t, b.pending.Set(observerScratch('z'), []byte("sentinel"), pebble.NoSync))
			}
			receipts := func(yield func(ParseFacts, error) bool) {
				require.NotEqual(t, "scratch occupied", mode)
				yield(facts, nil)
			}
			makeCall := func(from *graph.Node, to, field string) *graph.Edge {
				return setStateProofOutput(from, &graph.Node{ID: to}, field)
			}
			want := makeCall(dispatch, callback.ID, "field")
			before := rebuildFrameworkCustodyRows(t, b)
			stats, err := b.RebuildObserver(ctx, receipts,
				func(name string) bool { return name == "subscribe" },
				func(name string) bool { return name == "notify" },
				func(_, _ *graph.Node) bool { return true }, 40, makeCall)
			if mode == "checked move" {
				require.NoError(t, err)
				require.EqualValues(t, 1, stats.Edges)
				checkSetStateProofOutput(t, b, want, rebuildProofObserver)
				return
			}
			require.ErrorIs(t, err, ErrRebuildObserver)
			if mode == "scratch occupied" {
				require.ErrorContains(t, err, "Observer scratch range occupied")
			} else {
				var refusal *RebuildUnknownProvenanceError
				require.ErrorAs(t, err, &refusal)
				require.Equal(t, &RebuildUnknownProvenanceError{Count: 1, First: graph.EdgeIdentityFor(want)}, refusal)
			}
			require.Zero(t, stats)
			require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
		})
	}
}

func TestRebuildObserverReportsProposalProofFolds(t *testing.T) {
	for _, tc := range []struct {
		name                string
		lastField           string
		firstBad, secondBad bool
		bad                 byte
	}{
		{"same field bad then proved", "a", true, false, 0},
		{"same field proved then bad", "a", false, true, 0},
		{"same field both bad", "a", true, true, 1},
		{"later field can clear bad proof", "z", true, false, 0},
		{"later field cannot borrow earlier proof", "z", false, true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBuild(t, FullProfile)
			p := rebuildObserverPass{b: b}
			e := &graph.Edge{From: "dispatcher", To: "callback", Kind: graph.EdgeCalls, FilePath: "bus.go", Line: 5}
			key := pendingEdgeKey(e)
			require.NoError(t, p.reportProposal(key, "a", tc.firstBad))
			require.NoError(t, p.reportProposal(key, tc.lastField, tc.secondBad))
			value, err := p.get(append(observerScratch('x'), key...))
			require.NoError(t, err)
			require.Equal(t, append([]byte{tc.bad}, tc.lastField...), value)
		})
	}
}

func TestRebuildObserverSkipsIneligibleCallbackProof(t *testing.T) {
	for _, livePair := range []bool{false, true} {
		name := "no eligible pair"
		if livePair {
			name = "eligible pair alongside filtered callbacks"
		}
		t.Run(name, func(t *testing.T) {
			registrar := setStateProofMethod("registrar", "subscribe", 1)
			inactive := setStateProofMethod("inactive", "subscribe", 5)
			dispatch := setStateProofMethod("dispatch", "notify", 2)
			foreign := setStateProofMethod("foreign", "foreign", 3)
			foreign.WorkspaceID = "other"
			const field = "field"
			access := func(n *graph.Node) *graph.Edge {
				return &graph.Edge{From: n.ID, To: field, Kind: graph.EdgeAccessesField, FilePath: n.FilePath, Line: n.StartLine}
			}
			ref := func(n *graph.Node) *graph.Edge {
				return &graph.Edge{From: "setup", To: n.ID, Kind: graph.EdgeReferences, FilePath: n.FilePath, Line: 9}
			}
			in := &graph.Edge{From: "setup", To: registrar.ID, Kind: graph.EdgeCalls, FilePath: registrar.FilePath, Line: 9}
			regAccess, dispAccess, unusedAccess := access(registrar), access(dispatch), access(inactive)
			selfRef, foreignRef := ref(dispatch), ref(foreign)
			otherSite := &graph.Edge{From: "setup", To: inactive.ID, Kind: graph.EdgeCalls, FilePath: inactive.FilePath, Line: 10}
			proof := setStateProofFacts([]*graph.Node{registrar, dispatch}, regAccess, dispAccess, in)
			facts := setStateProofFacts([]*graph.Node{registrar, dispatch, foreign, inactive},
				regAccess, dispAccess, in, selfRef, foreignRef, unusedAccess, otherSite)
			allowed := setStateProofMethod("allowed", "allowed", 4)
			if livePair {
				edge := ref(allowed)
				facts.Nodes = append(facts.Nodes, allowed)
				facts.Edges = append(facts.Edges, edge)
				proof.Nodes = append(proof.Nodes, allowed)
				proof.Edges = append(proof.Edges, edge)
			}
			b, _ := seedSyntheticCustody(t, facts)
			receipts := func(yield func(ParseFacts, error) bool) {
				require.True(t, livePair, "filtered pairs do not demand any receipts")
				// Check demand directly. Ignoring an unproved route later
				// would otherwise hide an eager receipt request.
				for _, e := range []*graph.Edge{selfRef, foreignRef, unusedAccess, otherSite} {
					value, err := pendingGet(b.pending, append(observerScratch('v'), pendingEdgeKey(e)...), copyValue)
					require.NoError(t, err)
					require.Empty(t, value, "unexpected proof demand for %s to %s", e.From, e.To)
				}
				for _, n := range []*graph.Node{foreign, inactive} {
					value, err := pendingGet(b.pending, observerScratch('n', n.ID), copyValue)
					require.NoError(t, err)
					require.Empty(t, value, "unexpected declaration proof demand for %s", n.ID)
				}
				yield(proof, nil)
			}
			makeCall := func(from *graph.Node, to, field string) *graph.Edge {
				return setStateProofOutput(from, &graph.Node{ID: to}, field)
			}
			before := rebuildFrameworkCustodyRows(t, b)
			stats, err := b.RebuildObserver(t.Context(), receipts,
				func(name string) bool { return name == "subscribe" },
				func(name string) bool { return name == "notify" },
				func(from, to *graph.Node) bool { return from.WorkspaceID == to.WorkspaceID }, 40, makeCall)
			require.NoError(t, err)
			if livePair {
				require.EqualValues(t, 1, stats.Edges)
				checkSetStateProofOutput(t, b, makeCall(dispatch, allowed.ID, field), rebuildProofObserver)
			} else {
				require.Zero(t, stats)
				require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
			}
		})
	}
}

func TestRebuildObserverRefusalNamesOnlyRegistrarCallbacks(t *testing.T) {
	bad := setStateProofMethod("reg-bad", "subscribe", 1)
	good := setStateProofMethod("reg-good", "subscribe", 2)
	dispatch := setStateProofMethod("dispatch", "notify", 3)
	a := setStateProofMethod("a-callback", "a", 4)
	z := setStateProofMethod("z-callback", "z", 5)
	const field = "field"
	access := func(n *graph.Node) *graph.Edge {
		return &graph.Edge{From: n.ID, To: field, Kind: graph.EdgeAccessesField, FilePath: n.FilePath, Line: n.StartLine}
	}
	at := func(registrar, callback *graph.Node, line int) []*graph.Edge {
		return []*graph.Edge{
			{From: "setup", To: registrar.ID, Kind: graph.EdgeCalls, FilePath: registrar.FilePath, Line: line},
			{From: "setup", To: callback.ID, Kind: graph.EdgeReferences, FilePath: callback.FilePath, Line: line},
		}
	}
	// The proved registrar owns the lowest callback key. The unproved
	// access must not add it to the refused output set.
	edges := []*graph.Edge{access(bad), access(good), access(dispatch)}
	edges = append(edges, at(good, a, 8)...)
	edges = append(edges, at(bad, z, 9)...)
	makeCall := func(from *graph.Node, to, field string) *graph.Edge {
		return setStateProofOutput(from, &graph.Node{ID: to}, field)
	}
	for _, mode := range []string{"only the bad route", "shared with the proved registrar"} {
		t.Run(mode, func(t *testing.T) {
			input := edges
			if mode == "shared with the proved registrar" {
				// Good already has an inbound call at this site. A second
				// reference makes the same callback reachable via both.
				input = append(input, at(good, z, 8)[1])
			}
			facts := setStateProofFacts([]*graph.Node{bad, good, dispatch, a, z}, input...)
			b, complete := seedSyntheticCustody(t, facts)
			proof := facts
			proof.Edges = proof.Edges[1:]
			receipts := func(yield func(ParseFacts, error) bool) { yield(proof, nil) }
			run := func(receipts func(func(ParseFacts, error) bool)) (RebuildObserverStats, error) {
				return b.RebuildObserver(t.Context(), receipts,
					func(name string) bool { return name == "subscribe" },
					func(name string) bool { return name == "notify" },
					func(_, _ *graph.Node) bool { return true }, 40, makeCall)
			}
			before := rebuildFrameworkCustodyRows(t, b)
			stats, err := run(receipts)
			if mode == "only the bad route" {
				var refused *RebuildUnknownProvenanceError
				require.ErrorAs(t, err, &refused)
				require.Equal(t, &RebuildUnknownProvenanceError{
					Count: 1,
					First: graph.EdgeIdentityFor(makeCall(dispatch, z.ID, field)),
				}, refused)
				require.Zero(t, stats)
				require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
				stats, err = run(complete)
			}
			require.NoError(t, err)
			require.EqualValues(t, 2, stats.Edges)
			for _, cb := range []*graph.Node{a, z} {
				checkSetStateProofOutput(t, b, makeCall(dispatch, cb.ID, field), rebuildProofObserver)
			}
		})
	}
}

func TestRebuildObserverProofKinds(t *testing.T) {
	for _, tc := range []struct {
		kind   graph.EdgeKind
		proofs []rebuildEdgeProof
	}{
		{graph.EdgeAccessesField, []rebuildEdgeProof{rebuildProofDirect, rebuildProofReceiver}},
		{graph.EdgeCalls, []rebuildEdgeProof{rebuildProofTemporalSynthetic, rebuildProofEventChannel, rebuildProofRNBridge, rebuildProofObserver}},
		{graph.EdgeReferences, []rebuildEdgeProof{rebuildProofSwiftObjC, rebuildProofRNPair}},
	} {
		for _, proof := range tc.proofs {
			require.True(t, observerProofMatches(proof, tc.kind))
			require.False(t, observerProofMatches(proof, graph.EdgeReads))
		}
	}
	require.False(t, observerProofMatches(rebuildProofUntrackedMove, graph.EdgeCalls))
}

func TestRebuildObserverPredecessorCustody(t *testing.T) {
	for _, name := range []string{"authentic", "missing direct", "stale receiver", "forged event call", "wrong kind"} {
		t.Run(name, func(t *testing.T) {
			const (
				channelFile = "channel.go"
				appFile     = "app.ts"
				topic       = "event::emitter::bus::ready"
			)
			receiver := map[string]any{"receiver": "Bus"}
			registrar := &graph.Node{ID: channelFile + "::Bus.subscribe", Name: "subscribe", Kind: graph.KindMethod,
				FilePath: channelFile, Language: "go", StartLine: 2, Meta: receiver}
			dispatcher := &graph.Node{ID: channelFile + "::Bus.notify", Name: "notify", Kind: graph.KindMethod,
				FilePath: channelFile, Language: "go", StartLine: 8, Meta: receiver}
			field := &graph.Node{ID: channelFile + "::Bus.callbacks", Name: "callbacks", Kind: graph.KindField,
				FilePath: channelFile, Language: "go", Meta: receiver}
			caller := &graph.Node{ID: appFile + "::wire", Name: "wire", Kind: graph.KindFunction, FilePath: appFile, Language: "typescript"}
			onEvent := &graph.Node{ID: appFile + "::onEvent", Name: "onEvent", Kind: graph.KindFunction, FilePath: appFile, Language: "typescript"}
			onPlain := &graph.Node{ID: appFile + "::onPlain", Name: "onPlain", Kind: graph.KindFunction, FilePath: appFile, Language: "typescript"}
			nextDispatch := &graph.Node{ID: appFile + "::notifyNext", Name: "notifyNext", Kind: graph.KindFunction,
				FilePath: appFile, Language: "typescript", StartLine: 12}
			nextField := &graph.Node{ID: appFile + "::next", Name: "next", Kind: graph.KindField, FilePath: appFile, Language: "typescript"}
			channel := ParseFacts{
				EffectiveLanguage: "go",
				File:              graph.FileMetaRow{FilePath: channelFile, NodeCount: 3},
				Nodes:             []*graph.Node{registrar, dispatcher, field},
				Edges: []*graph.Edge{
					{From: registrar.ID, To: field.ID, Kind: graph.EdgeWrites, FilePath: channelFile, Line: 3},
					{From: dispatcher.ID, To: "unresolved::Lock", Kind: graph.EdgeCalls, FilePath: channelFile, Line: 9,
						Meta: map[string]any{"recv_field": "callbacks"}},
					{From: registrar.ID, To: topic, Kind: graph.EdgeListensOn, FilePath: channelFile, Line: 4},
					{From: dispatcher.ID, To: onPlain.ID, Kind: graph.EdgeReferences, FilePath: channelFile, Line: dispatcher.StartLine},
				},
			}
			app := ParseFacts{
				EffectiveLanguage: "typescript",
				File:              graph.FileMetaRow{FilePath: appFile, NodeCount: 6},
				Nodes: []*graph.Node{
					caller, onEvent, onPlain, nextDispatch, nextField,
					{ID: topic, Name: "ready", Kind: graph.KindString, FilePath: appFile, Language: "typescript"},
				},
				Edges: []*graph.Edge{
					{From: caller.ID, To: topic, Kind: graph.EdgeEmits, FilePath: appFile, Line: 5},
					{From: caller.ID, To: onEvent.ID, Kind: graph.EdgeReferences, FilePath: appFile, Line: 5},
					{From: caller.ID, To: registrar.ID, Kind: graph.EdgeCalls, FilePath: appFile, Line: 6},
					{From: caller.ID, To: onPlain.ID, Kind: graph.EdgeReads, FilePath: appFile, Line: 6},
					{From: onEvent.ID, To: nextField.ID, Kind: graph.EdgeAccessesField, FilePath: appFile, Line: 7},
					{From: nextDispatch.ID, To: nextField.ID, Kind: graph.EdgeAccessesField, FilePath: appFile, Line: 13},
				},
			}
			b := newBuild(t, FullProfile)
			var receiptsFacts []ParseFacts
			for _, f := range []ParseFacts{channel, app} {
				b.AddBatch(f.Nodes, f.Edges)
				raw, err := EncodeParseFacts(f)
				require.NoError(t, err)
				receipt, err := DecodeParseFacts(raw)
				require.NoError(t, err)
				receiptsFacts = append(receiptsFacts, receipt)
			}
			receipts := func(yield func(ParseFacts, error) bool) {
				for _, f := range receiptsFacts {
					if !yield(f, nil) {
						return
					}
				}
			}
			ctx := t.Context()
			stats, err := b.RebuildCapabilities(ctx, receipts,
				func(string) string { return "" }, func(string, string) (string, string) { return "", "" },
				ReceiverMutationRules{
					OwnerKey:        func(_ string, receiver string) string { return receiver },
					BareCallName:    func(to string) string { return strings.TrimPrefix(to, "unresolved::") },
					IsStdlibMutator: func(name string) bool { return name == "Lock" },
				}, 1<<20)
			require.NoError(t, err)
			require.Equal(t, uint64(1), stats.Emitted)

			eventCall := func(from, to, file string, line int) *graph.Edge {
				return &graph.Edge{From: from, To: to, Kind: graph.EdgeCalls, FilePath: file, Line: line,
					Origin: graph.OriginASTInferred, Confidence: 0.5, ConfidenceLabel: graph.ConfidenceLabelFor(graph.EdgeCalls, 0.5),
					Meta: map[string]any{
						"via":             "event.channel",
						"event_topic":     "bus::ready",
						"event_transport": "emitter",
						"synthesized_by":  "event-channel",
						"provenance":      "heuristic",
					},
				}
			}
			eventStats, err := b.RebuildEventChannel(ctx, receipts, []string{"event::emitter::"},
				func(string, string) (string, int) { return "emitter", 4 },
				func(_, _ string, emitter, listener *graph.Edge) *graph.Edge {
					return eventCall(emitter.From, listener.From, emitter.FilePath, emitter.Line)
				})
			require.NoError(t, err)
			require.Equal(t, uint64(1), eventStats.Pairs)

			direct := &graph.Edge{From: registrar.ID, To: field.ID, Kind: graph.EdgeAccessesField, FilePath: channelFile, Line: 3,
				Origin: graph.OriginASTResolved, Meta: map[string]any{"access": "write"}}
			indirect := &graph.Edge{From: dispatcher.ID, To: field.ID, Kind: graph.EdgeAccessesField, FilePath: channelFile, Line: 9,
				Origin: graph.OriginASTInferred, Meta: map[string]any{"access": "write", "indirect": true, "via": "Lock"}}
			call := eventCall(caller.ID, registrar.ID, appFile, 5)
			for _, check := range []struct {
				edge  *graph.Edge
				proof rebuildEdgeProof
			}{
				{direct, rebuildProofDirect},
				{indirect, rebuildProofReceiver},
				{call, rebuildProofEventChannel},
			} {
				key := pendingEdgeKey(check.edge)
				live, err := b.pendingEdge(key)
				require.NoError(t, err)
				require.Equal(t, check.edge, live)
				custody, err := readRebuildEdgeCustody(b.pending, key)
				require.NoError(t, err)
				require.Equal(t, rebuildEdgeCustody{proof: check.proof}, custody)
			}
			switch name {
			case "missing direct":
				require.NoError(t, b.pending.Delete(rebuildLinkRecorderKey(pendingEdgeKey(direct)), pebble.NoSync))
			case "stale receiver":
				// Change only the payload; deliberately bypass recorder invalidation.
				indirect.Via = "replacement"
				value, err := encodeEdge(indirect)
				require.NoError(t, err)
				require.NoError(t, b.pending.Set(pendingEdgeKey(indirect), value, pebble.NoSync))
			case "wrong kind":
				require.NoError(t, b.withWrites(func() error {
					return b.putEdgeWithCustody(direct, rebuildEdgeCustody{proof: rebuildProofSwiftObjC})
				}))
			case "forged event call":
				// An ordinary replacement can reproduce the producer's exact tags and bytes.
				require.NoError(t, b.addEdge(call))
				got, err := readRebuildEdgeCustody(b.pending, pendingEdgeKey(call))
				require.NoError(t, err)
				require.Zero(t, got)
			}

			actualRows := func() map[string]string {
				rows := make(map[string]string)
				for _, family := range []byte{familyNode, familyEdge} {
					require.NoError(t, pendingVisit(ctx, b.pending, []byte{family}, func(r row) error {
						rows[string(r.key)] = string(r.value)
						return nil
					}))
				}
				return rows
			}
			makeCall := func(from *graph.Node, to, field string) *graph.Edge {
				return &graph.Edge{From: from.ID, To: to, Kind: graph.EdgeCalls, FilePath: from.FilePath, Line: from.StartLine,
					Origin: graph.OriginASTInferred, Confidence: 0.5, ConfidenceLabel: graph.ConfidenceLabelFor(graph.EdgeCalls, 0.5),
					Meta: map[string]any{
						"via": "observer.channel", "channel_field": field, "synthesized_by": "observer-channel", "provenance": "heuristic",
					},
				}
			}
			want := actualRows()
			run := func() (RebuildObserverStats, error) {
				return b.RebuildObserver(ctx, receipts,
					func(name string) bool { return name == "subscribe" || name == "onEvent" },
					func(name string) bool { return name == "notify" || name == "notifyNext" },
					func(from, to *graph.Node) bool {
						return from != nil && to != nil && from.WorkspaceID == to.WorkspaceID
					}, 40, makeCall)
			}
			observerStats, err := run()
			if name != "authentic" {
				var refusal *RebuildUnknownProvenanceError
				require.ErrorAs(t, err, &refusal)
				count, first := uint64(2), onEvent.ID
				if name == "forged event call" {
					count = 1
				}
				require.Equal(t, &RebuildUnknownProvenanceError{
					Count: count,
					First: graph.EdgeIdentityFor(makeCall(dispatcher, first, field.ID)),
				}, refusal)
				require.Zero(t, observerStats)
				require.Equal(t, want, actualRows())
				return
			}
			require.NoError(t, err)
			require.Equal(t, uint64(2), observerStats.Edges)
			for _, to := range []string{onEvent.ID, onPlain.ID} {
				e := makeCall(dispatcher, to, field.ID)
				key := pendingEdgeKey(e)
				raw, err := encodeEdge(e)
				require.NoError(t, err)
				want[string(key)] = string(raw)
				want[string(pendingReverseKey(e, key))] = string(key)
				custody, err := readRebuildEdgeCustody(b.pending, key)
				require.NoError(t, err)
				require.Equal(t, rebuildEdgeCustody{proof: rebuildProofObserver}, custody)
			}
			require.Equal(t, want, actualRows())

			// The first Observer run installed the only call to the second
			// registrar. Change its saved payload without updating custody.
			firstHop := makeCall(dispatcher, onEvent.ID, field.ID)
			firstHop.Via = "replacement"
			key := pendingEdgeKey(firstHop)
			value, err := encodeEdge(firstHop)
			require.NoError(t, err)
			require.NoError(t, b.pending.Set(key, value, pebble.NoSync))
			want[string(key)] = string(value)
			observerStats, err = run()
			require.ErrorIs(t, err, ErrRebuildObserver)
			var refused *RebuildUnknownProvenanceError
			require.ErrorAs(t, err, &refused)
			require.EqualValues(t, 1, refused.Count)
			require.Equal(t, graph.EdgeIdentityFor(makeCall(nextDispatch, onPlain.ID, nextField.ID)), refused.First)
			require.Zero(t, observerStats)
			require.Equal(t, want, actualRows())
		})
	}
}
