package store_pebble

import (
	"bytes"
	"iter"
	"slices"
	"strings"
	"testing"

	"github.com/cockroachdb/pebble/v2"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

const (
	storeFactoryTestBinding = "useStore"
	storeFactoryTestMember  = "reset"
	storeFactoryTestStub    = "unresolved::*.reset"
)

func TestRebuildStoreFactoryRejectsInvalidInputs(t *testing.T) {
	b := newBuild(t, FullProfile)
	before := rebuildFrameworkCustodyRows(t, b)
	for _, input := range []string{"receipts", "action", "stub", "getter", "rebind"} {
		t.Run(input, func(t *testing.T) {
			receipts := unreadStoreFactoryReceipts(t)
			rules := storeFactoryTestRules()
			switch input {
			case "receipts":
				receipts = nil
			case "action":
				rules.Action = nil
			case "stub":
				rules.Stub = nil
			case "getter":
				rules.Getter = nil
			case "rebind":
				rules.Rebind = nil
			}
			stats, err := b.RebuildStoreFactory(t.Context(), receipts, rules)
			require.ErrorIs(t, err, ErrRebuildStoreFactory)
			require.ErrorIs(t, err, ErrRebuildLinkInputSize)
			require.ErrorContains(t, err, "invalid Store Factory inputs")
			require.Zero(t, stats)
			require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
		})
	}
}

func TestRebuildStoreFactoryRejectsOccupiedScratch(t *testing.T) {
	b := newBuild(t, FullProfile)
	require.NoError(t, b.pending.Set(append(bytes.Clone(rebuildStoreFactoryPrefix), 'z'), []byte("sentinel"), pebble.NoSync))
	before := rebuildFrameworkCustodyRows(t, b)
	stats, err := b.RebuildStoreFactory(t.Context(), unreadStoreFactoryReceipts(t), storeFactoryTestRules())
	require.ErrorIs(t, err, ErrRebuildStoreFactory)
	require.ErrorIs(t, err, errInvalidRecord)
	require.ErrorContains(t, err, "Store Factory scratch range occupied")
	require.Zero(t, stats)
	require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
}

func TestRebuildStoreFactoryRowCaps(t *testing.T) {
	large := strings.Repeat("x", rebuildStoreFactoryRowBytes)
	for _, tc := range []struct{ name, row string }{
		{"action", "node row"},
		{"caller", "node row"},
		{"unrelated node", ""},
		{"owned Call", "edge row"},
		{"foreign Call", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			call := storeFactoryTestCall(storeFactoryTestStub, 2)
			action := storeFactoryTestAction("store.ts::reset", "store.ts", "workspace")
			b, _ := seedSyntheticCustody(t, producerProofFacts(action))
			var key, raw []byte
			var err error
			switch tc.name {
			case "owned Call", "foreign Call":
				call.Context = large
				if tc.name == "foreign Call" {
					delete(call.Meta, "via")
				}
				key = pendingEdgeKey(call)
				raw, err = encodeEdge(call)
			default:
				node := action
				switch tc.name {
				case "unrelated node":
					node = storeFactoryTestGetter("getter", "store.ts")
					node.Name = "unrelated"
				case "caller":
					node = storeFactoryTestCaller()
					b.AddEdge(call)
				}
				node.Meta["large"] = large
				key = pendingNodeKey(node.ID)
				raw, err = encodeNode(node)
			}
			require.NoError(t, err)
			require.Greater(t, len(key)+len(raw), rebuildStoreFactoryRowBytes)
			require.NoError(t, b.pending.Set(key, raw, pebble.NoSync))
			before := rebuildFrameworkCustodyRows(t, b)
			stats, err := b.RebuildStoreFactory(t.Context(), unreadStoreFactoryReceipts(t), storeFactoryTestRules())
			if tc.row == "" {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, ErrRebuildStoreFactory)
				require.ErrorIs(t, err, ErrRebuildLinkInputSize)
				require.ErrorContains(t, err, "Store Factory "+tc.row)
			}
			require.Zero(t, stats)
			require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
		})
	}
}

func TestRebuildStoreFactoryPreservesOversizedIncumbent(t *testing.T) {
	old := storeFactoryTestCall(storeFactoryTestStub, 2)
	caller := storeFactoryTestCaller()
	action := storeFactoryTestAction("store.ts::reset", "store.ts", caller.WorkspaceID)
	source := storeFactoryTestSource(old)
	source.Nodes = []*graph.Node{caller}
	b, receipts := seedSyntheticCustody(t, source, producerProofFacts(action))
	incumbent := storeFactoryTestCall(action.ID, old.Line)
	incumbent.Meta = map[string]any{"via": "other.producer"}
	incumbent.Context = strings.Repeat("x", rebuildStoreFactoryRowBytes)
	require.NoError(t, b.withWrites(func() error {
		return b.addEdgeWithCustody(incumbent, rebuildEdgeCustody{proof: rebuildProofReceiver})
	}))
	before := rebuildFrameworkCustodyRows(t, b)
	key := pendingEdgeKey(incumbent)
	require.Greater(t, len(key)+len(before[string(key)]), rebuildStoreFactoryRowBytes)
	stats, err := b.RebuildStoreFactory(t.Context(), receipts, storeFactoryTestRules())
	require.NoError(t, err)
	require.Equal(t, RebuildStoreFactoryStats{Stubs: 1, Resolved: 1, Moves: 1}, stats)
	oldKey := pendingEdgeKey(old)
	delete(before, string(oldKey))
	delete(before, string(pendingReverseKey(old, oldKey)))
	require.Equal(t, before, rebuildFrameworkCustodyRows(t, b), "keep the incumbent bytes and custody")
}

func TestRebuildStoreFactoryStaleCustody(t *testing.T) {
	for _, name := range []string{"stationary", "moving", "malformed"} {
		t.Run(name, func(t *testing.T) {
			action := storeFactoryTestAction("store.ts::reset", "store.ts", "")
			raw := storeFactoryTestCall(action.ID, 2)
			b, receipts := seedSyntheticCustody(t, storeFactoryTestSource(raw), producerProofFacts(action))
			mover := *raw
			mover.To = "alias"
			require.NoError(t, b.ApplyLinkReindexes(t.Context(), []graph.EdgeReindex{{Edge: &mover, OldTo: raw.To}}))
			b.AddEdge(raw)

			// A producer stamp binds the payload digest. Raw lineage alone
			// does not. Only the alias may claim the raw key when the
			// stationary incumbent has a stale producer stamp.
			suspect := raw
			if name == "moving" {
				suspect = &mover
			}
			require.NoError(t, b.withWrites(func() error {
				return b.addEdgeWithCustody(suspect, rebuildEdgeCustody{proof: rebuildProofReceiver})
			}))
			key := pendingEdgeKey(suspect)
			recorder := rebuildLinkRecorderKey(key)
			switch name {
			case "malformed":
				require.NoError(t, b.pending.Set(recorder, []byte{rebuildEdgeProofFormat}, pebble.NoSync))
			default:
				changed := *suspect
				changed.Context = "new payload without new custody digest"
				val, err := encodeEdge(&changed)
				require.NoError(t, err)
				require.NoError(t, b.pending.Set(key, val, pebble.NoSync))
				_, err = readRebuildEdgeCustody(b.pending, key)
				require.ErrorIs(t, err, ErrRebuildUnknownProvenance)
			}
			before := rebuildFrameworkCustodyRows(t, b)
			stats, err := b.RebuildStoreFactory(t.Context(), receipts, storeFactoryTestRules())
			switch name {
			case "stationary":
				require.NoError(t, err)
				require.Equal(t, RebuildStoreFactoryStats{Stubs: 2, Resolved: 2, Moves: 1}, stats)
				old := pendingEdgeKey(&mover)
				delete(before, string(old))
				delete(before, string(pendingReverseKey(&mover, old)))
				delete(before, string(rebuildLinkRecorderKey(old)))
			case "moving":
				requireStoreFactoryProofRefusal(t, err, stats, 1, raw)
			case "malformed":
				require.ErrorIs(t, err, ErrRebuildStoreFactory)
				require.ErrorIs(t, err, errInvalidRecord)
				require.ErrorContains(t, err, "edge custody format")
				require.Zero(t, stats)
			}
			require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
		})
	}
}

func TestRebuildStoreFactoryRejectsStationaryRawClaimCollision(t *testing.T) {
	// The stationary key sorts between the aliases in the Call scan.
	for _, alias := range []string{"a-mover", "z-mover"} {
		t.Run(alias, func(t *testing.T) {
			action := storeFactoryTestAction("middle", "store.ts", "")
			raw := storeFactoryTestCall(action.ID, 2)
			b, receipts := seedSyntheticCustody(t, storeFactoryTestSource(raw), producerProofFacts(action))
			mover := *raw
			mover.To = alias
			require.NoError(t, b.ApplyLinkReindexes(t.Context(), []graph.EdgeReindex{{Edge: &mover, OldTo: raw.To}}))
			b.AddEdge(raw)
			before := rebuildFrameworkCustodyRows(t, b)
			stats, err := b.RebuildStoreFactory(t.Context(), receipts, storeFactoryTestRules())
			requireStoreFactoryProofRefusal(t, err, stats, 1, raw)
			require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
		})
	}
}

func TestRebuildStoreFactoryElection(t *testing.T) {
	local := storeFactoryTestAction("app.ts::reset", "app.ts", "workspace")
	remote := storeFactoryTestAction("store.ts::reset", "store.ts", "workspace")
	foreign := storeFactoryTestAction("peer.ts::reset", "peer.ts", "other")
	getter := storeFactoryTestGetter("store.ts::getter", remote.FilePath)
	decoy := storeFactoryTestGetter("peer.ts::getter", foreign.FilePath)
	decoy.Meta["store_factory"] = false
	for _, tc := range []struct {
		name   string
		caller bool
		inputs []*graph.Node
		want   *graph.Node
	}{
		{"global singleton without caller", false, []*graph.Node{remote}, remote},
		{"workspace singleton", true, []*graph.Node{remote, foreign}, remote},
		{"global ambiguity without caller", false, []*graph.Node{remote, foreign}, nil},
		{"getter wins with two declarations in one file", true, []*graph.Node{local, remote, getter,
			storeFactoryTestGetter("store.ts::secondGetter", remote.FilePath)}, remote},
		{"two getter files fall back to caller", true, []*graph.Node{local, remote, getter,
			storeFactoryTestGetter("peer.ts::getter", foreign.FilePath)}, local},
		{"two actions in getter file fall back to caller", true, []*graph.Node{local, remote, getter,
			storeFactoryTestAction("store.ts::secondReset", remote.FilePath, remote.WorkspaceID)}, local},
		{"ineligible getter cannot mask the getter file", true, []*graph.Node{local, remote, getter, decoy}, remote},
		{"two actions in caller file", true, []*graph.Node{local, storeFactoryTestAction("app.ts::otherReset", local.FilePath, local.WorkspaceID)}, nil},
		{"caller has no file", true, []*graph.Node{local, storeFactoryTestAction("emptyPathReset", "", local.WorkspaceID)}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			call := storeFactoryTestCall("retired", 2)
			facts := []ParseFacts{storeFactoryTestSource(call)}
			if tc.caller {
				caller := storeFactoryTestCaller()
				if tc.name == "caller has no file" {
					caller.FilePath = ""
					facts = append(facts, producerProofFacts(caller))
				} else {
					facts[0].Nodes = []*graph.Node{caller}
				}
			}
			for _, n := range tc.inputs {
				facts = append(facts, producerProofFacts(n))
			}
			b, receipts := seedSyntheticCustody(t, facts...)
			stats, err := b.RebuildStoreFactory(t.Context(), receipts, storeFactoryTestRules())
			require.NoError(t, err)
			want := *call
			want.To = storeFactoryTestStub
			wantStats := RebuildStoreFactoryStats{Stubs: 1, Moves: 1}
			if tc.want != nil {
				want.To = tc.want.ID
				wantStats.Resolved = 1
			}
			require.Equal(t, wantStats, stats)
			old, next := pendingEdgeKey(call), pendingEdgeKey(&want)
			gone, err := b.pendingEdge(old)
			require.NoError(t, err)
			require.Nil(t, gone)
			saved, err := b.pendingEdge(next)
			require.NoError(t, err)
			require.Equal(t, &want, saved)
			custody, err := readRebuildEdgeCustody(b.pending, next)
			require.NoError(t, err)
			require.Equal(t, rebuildEdgeCustody{raw: old}, custody)
		})
	}
}

func TestRebuildStoreFactoryProofRefusalCountsOutputs(t *testing.T) {
	// The first old key has the last output key; scan order isn't First.
	first := storeFactoryTestCall("a-old", 2)
	second := storeFactoryTestCall("z-old", 3)
	second.Meta["store_action"] = "save"
	peer := storeFactoryTestCall("m-old", 4)
	peer.Meta["store_action"] = "reload"
	reset := storeFactoryTestAction("z-target", "store.ts", "")
	save := storeFactoryTestAction("a-target", "store.ts", "")
	save.Meta["store_member"] = "save"
	reload := storeFactoryTestAction("peer-target", "store.ts", "")
	reload.Meta["store_member"] = "reload"
	source := storeFactoryTestSource(first, second, peer)
	decls := ParseFacts{EffectiveLanguage: "typescript", File: graph.FileMetaRow{FilePath: "store.ts"},
		Nodes: []*graph.Node{reset, save, reload}}
	b, _ := seedSyntheticCustody(t, source, decls)
	before := rebuildFrameworkCustodyRows(t, b)
	// Only the peer action has receipt proof; no move may be applied.
	stats, err := b.RebuildStoreFactory(t.Context(), sqlCallsiteTestReceipts(source, producerProofFacts(reload)), storeFactoryTestRules())
	want := *second
	want.To = save.ID
	requireStoreFactoryProofRefusal(t, err, stats, 2, &want)
	require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
}

func TestRebuildStoreFactoryStationarySkipsReceipts(t *testing.T) {
	action := storeFactoryTestAction("store.ts::reset", "store.ts", "")
	stationary := storeFactoryTestCall(action.ID, 2)
	b, _ := seedSyntheticCustody(t, storeFactoryTestSource(stationary), producerProofFacts(action))
	before := rebuildFrameworkCustodyRows(t, b)
	stats, err := b.RebuildStoreFactory(t.Context(), unreadStoreFactoryReceipts(t), storeFactoryTestRules())
	require.NoError(t, err)
	require.Equal(t, RebuildStoreFactoryStats{Stubs: 1, Resolved: 1}, stats)
	require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
}

func requireStoreFactoryProofRefusal(t *testing.T, err error, stats RebuildStoreFactoryStats, count uint64, first *graph.Edge) {
	t.Helper()
	require.ErrorIs(t, err, ErrRebuildStoreFactory)
	var refused *RebuildUnknownProvenanceError
	require.ErrorAs(t, err, &refused)
	require.Equal(t, &RebuildUnknownProvenanceError{Count: count, First: graph.EdgeIdentityFor(first)}, refused)
	require.Zero(t, stats)
}

func unreadStoreFactoryReceipts(t *testing.T) iter.Seq2[ParseFacts, error] {
	return func(func(ParseFacts, error) bool) { t.Fatal("Store Factory unexpectedly read receipts") }
}

func storeFactoryTestCall(to string, line int) *graph.Edge {
	return &graph.Edge{From: "app.ts::caller", To: to, Kind: graph.EdgeCalls, FilePath: "app.ts", Line: line,
		Meta: map[string]any{"via": "store-factory", "store_binding": storeFactoryTestBinding, "store_action": storeFactoryTestMember}}
}

func storeFactoryTestSource(edges ...*graph.Edge) ParseFacts {
	return ParseFacts{EffectiveLanguage: "typescript", File: graph.FileMetaRow{FilePath: "app.ts"}, Edges: edges}
}

func storeFactoryTestAction(id, file, workspace string) *graph.Node {
	return &graph.Node{ID: id, Name: storeFactoryTestMember, Kind: graph.KindFunction, FilePath: file, Language: "typescript",
		WorkspaceID: workspace, Meta: map[string]any{"store_factory": storeFactoryTestBinding, "store_member": storeFactoryTestMember}}
}

func storeFactoryTestGetter(id, file string) *graph.Node {
	return &graph.Node{ID: id, Name: storeFactoryTestBinding, Kind: graph.KindVariable, FilePath: file, Language: "typescript",
		Meta: map[string]any{}}
}

func storeFactoryTestCaller() *graph.Node {
	return &graph.Node{ID: "app.ts::caller", Name: "caller", Kind: graph.KindMethod, FilePath: "app.ts", Language: "typescript",
		WorkspaceID: "workspace", Meta: map[string]any{}}
}

func storeFactoryTestRules() RebuildStoreFactoryRules {
	// The resolver tests cover real marker and payload rules. Keep these
	// callbacks small so this suite isolates the private store's behavior.
	return RebuildStoreFactoryRules{
		Action: func(n *graph.Node) (string, string) {
			binding, _ := n.Meta["store_factory"].(string)
			member, _ := n.Meta["store_member"].(string)
			return binding, member
		},
		Stub: func(e *graph.Edge) (string, string) {
			if e.Meta["via"] != "store-factory" {
				return "", ""
			}
			binding, _ := e.Meta["store_binding"].(string)
			member, _ := e.Meta["store_action"].(string)
			return binding, member
		},
		Getter: func(n *graph.Node, binding string) bool {
			_, action := n.Meta["store_factory"]
			return n.Name == binding && n.FilePath != "" && !action
		},
		Rebind: func(e *graph.Edge, member, target string, matched bool) bool {
			if !matched {
				target = "unresolved::*." + member
			}
			if target == e.To {
				return false
			}
			e.To = target
			return true
		},
	}
}

func TestRebuildStoreFactoryAndReduxThunkProofGuards(t *testing.T) {
	for _, redux := range []bool{false, true} {
		stage := "Store Factory"
		stageErr := ErrRebuildStoreFactory
		if redux {
			stage, stageErr = "Redux Thunk", ErrRebuildReduxThunk
		}
		t.Run(stage, func(t *testing.T) {
			for _, guard := range []string{
				"receipt owner path", "raw Call file", "Call receipt without language",
				"raw candidate file", "candidate receipt without language",
				"no receipt for anchor", "mismatched node key",
			} {
				t.Run(guard, func(t *testing.T) {
					action := storeFactoryTestAction("store.ts::reset", "store.ts", "")
					call := storeFactoryTestCall("retired", 2)
					if guard == "no receipt for anchor" {
						call.Meta["store_action"] = "removed"
					}
					if redux {
						call.Meta["via"] = "redux-thunk"
						call.Meta["thunk_dispatch"] = call.Meta["store_action"]
						action.Meta["redux_thunk"] = storeFactoryTestMember
					}
					source, decls := storeFactoryTestSource(call), producerProofFacts(action)
					b, _ := seedSyntheticCustody(t, source, decls)
					switch guard {
					case "receipt owner path":
						// Keep the raw edge and key unchanged. Only the enclosing receipt lies.
						source.File.FilePath = "wrong.ts"
					case "raw Call file":
						// The receipt agrees with its Call, but the raw key changed.
						raw := *call
						raw.FilePath = "wrong.ts"
						source.File.FilePath, source.Edges = raw.FilePath, []*graph.Edge{&raw}
					case "Call receipt without language":
						source.EffectiveLanguage = ""
					case "raw candidate file":
						// Node claims use ID, so this matches the claim, not its owner file.
						raw := *action
						raw.FilePath = "wrong.ts"
						decls.Nodes = []*graph.Node{&raw}
					case "candidate receipt without language":
						decls.EffectiveLanguage = ""
					case "no receipt for anchor":
						decls.Nodes = nil
					case "mismatched node key":
						val, err := encodeNode(action)
						require.NoError(t, err)
						require.NoError(t, b.pending.Set(pendingNodeKey("different ID"), val, pebble.NoSync))
					}
					before := rebuildFrameworkCustodyRows(t, b)
					receipts := sqlCallsiteTestReceipts(source, decls)
					var err error
					if redux {
						var stats RebuildReduxThunkStats
						stats, err = b.RebuildReduxThunk(t.Context(), receipts, reduxThunkStoreTestRules())
						require.Zero(t, stats)
					} else {
						var stats RebuildStoreFactoryStats
						stats, err = b.RebuildStoreFactory(t.Context(), receipts, storeFactoryTestRules())
						require.Zero(t, stats)
					}
					require.ErrorIs(t, err, stageErr)
					if guard == "mismatched node key" {
						require.ErrorIs(t, err, errInvalidRecord)
						require.ErrorContains(t, err, stage+" node identity")
					} else {
						want := *call
						want.To = action.ID
						if guard == "no receipt for anchor" {
							want.To = "unresolved::*.removed"
						}
						var refusal *RebuildUnknownProvenanceError
						require.ErrorAs(t, err, &refusal)
						require.Equal(t, &RebuildUnknownProvenanceError{Count: 1, First: graph.EdgeIdentityFor(&want)}, refusal)
					}
					require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
				})
			}
		})
	}
}

func TestRebuildReduxThunkCustodyGuards(t *testing.T) {
	action := storeFactoryTestAction("middle", "store.ts", "")
	action.Meta["redux_thunk"] = storeFactoryTestMember
	raw := storeFactoryTestCall(action.ID, 2)
	raw.Meta = map[string]any{"via": "redux-thunk", "thunk_dispatch": storeFactoryTestMember}
	unread := func(func(ParseFacts, error) bool) { t.Fatal("Redux Thunk unexpectedly read receipts") }
	for _, tc := range []struct{ name, alias string }{
		{"occupied scratch", ""},
		{"stationary ignores receipts", ""},
		{"stale stationary cannot claim raw key", "a-mover"},
		{"mover claims first", "a-mover"},
		{"stationary claims first", "z-mover"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, receipts := seedSyntheticCustody(t, storeFactoryTestSource(raw), producerProofFacts(action))
			if tc.name == "occupied scratch" {
				key := append(slices.Clone(rebuildReduxThunkPrefix), 'z')
				require.NoError(t, b.pending.Set(key, []byte("sentinel"), pebble.NoSync))
			}
			mover := *raw
			if tc.alias == "" {
				receipts = unread
			} else {
				mover.To = tc.alias
				require.NoError(t, b.ApplyLinkReindexes(t.Context(), []graph.EdgeReindex{{Edge: &mover, OldTo: raw.To}}))
				b.AddEdge(raw)
			}
			if tc.name == "stale stationary cannot claim raw key" {
				require.NoError(t, b.withWrites(func() error {
					return b.addEdgeWithCustody(raw, rebuildEdgeCustody{proof: rebuildProofReceiver})
				}))
				stale := *raw
				stale.Context = "new payload without new custody digest"
				val, err := encodeEdge(&stale)
				require.NoError(t, err)
				key := pendingEdgeKey(raw)
				require.NoError(t, b.pending.Set(key, val, pebble.NoSync))
				_, err = readRebuildEdgeCustody(b.pending, key)
				require.ErrorIs(t, err, ErrRebuildUnknownProvenance)
			}
			before := rebuildFrameworkCustodyRows(t, b)
			stats, err := b.RebuildReduxThunk(t.Context(), receipts, reduxThunkStoreTestRules())
			switch tc.name {
			case "occupied scratch":
				require.ErrorIs(t, err, ErrRebuildReduxThunk)
				require.ErrorIs(t, err, errInvalidRecord)
				require.ErrorContains(t, err, "Redux Thunk scratch range occupied")
				require.Zero(t, stats)
			case "stationary ignores receipts":
				require.NoError(t, err)
				require.Equal(t, RebuildReduxThunkStats{Stubs: 1, Resolved: 1}, stats)
			case "stale stationary cannot claim raw key":
				require.NoError(t, err)
				require.Equal(t, RebuildReduxThunkStats{Stubs: 2, Resolved: 2, Moves: 1}, stats)
				old := pendingEdgeKey(&mover)
				delete(before, string(old))
				delete(before, string(pendingReverseKey(&mover, old)))
				delete(before, string(rebuildLinkRecorderKey(old)))
			default:
				require.ErrorIs(t, err, ErrRebuildReduxThunk)
				var refusal *RebuildUnknownProvenanceError
				require.ErrorAs(t, err, &refusal)
				require.Equal(t, &RebuildUnknownProvenanceError{Count: 1, First: graph.EdgeIdentityFor(raw)}, refusal)
				require.Zero(t, stats)
			}
			require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
		})
	}
}

func reduxThunkStoreTestRules() RebuildReduxThunkRules {
	rules := storeFactoryTestRules()
	return RebuildReduxThunkRules{
		Thunk: func(n *graph.Node) string {
			name, _ := n.Meta["redux_thunk"].(string)
			return name
		},
		Action: rules.Action,
		Stub: func(e *graph.Edge) string {
			if e.Meta["via"] != "redux-thunk" {
				return ""
			}
			name, _ := e.Meta["thunk_dispatch"].(string)
			return name
		},
		Getter: rules.Getter,
		Rebind: rules.Rebind,
	}
}

func TestRebuildReduxThunkFileElection(t *testing.T) {
	local := storeFactoryTestAction("app.ts::reset", "app.ts", "workspace")
	remote := storeFactoryTestAction("store.ts::reset", "store.ts", "workspace")
	getter := storeFactoryTestGetter("store.ts::getter", remote.FilePath)
	decoy := storeFactoryTestGetter("peer.ts::getter", "peer.ts")
	decoy.Meta["store_factory"] = false
	for _, tc := range []struct {
		name   string
		nodes  []*graph.Node
		target string
	}{
		{"ineligible getter cannot mask the getter file", []*graph.Node{local, remote, getter, decoy}, remote.ID},
		{"two actions in getter file fall back to caller", []*graph.Node{local, remote, getter,
			storeFactoryTestAction("store.ts::otherReset", remote.FilePath, remote.WorkspaceID)}, local.ID},
		{"two actions in caller file", []*graph.Node{local, storeFactoryTestAction("app.ts::otherReset", local.FilePath, local.WorkspaceID)}, storeFactoryTestStub},
		{"caller has no file", []*graph.Node{local, storeFactoryTestAction("emptyPathReset", "", local.WorkspaceID)}, storeFactoryTestStub},
	} {
		t.Run(tc.name, func(t *testing.T) {
			caller := storeFactoryTestCaller()
			if tc.name == "caller has no file" {
				caller.FilePath = ""
			}
			call := storeFactoryTestCall("retired", 2)
			call.Meta["via"] = "redux-thunk"
			call.Meta["thunk_dispatch"] = storeFactoryTestMember
			anchor := storeFactoryTestAction("anchor.ts::other", "anchor.ts", "")
			anchor.Meta = map[string]any{"redux_thunk": "unrelated"}
			facts := []ParseFacts{storeFactoryTestSource(call), producerProofFacts(caller), producerProofFacts(anchor)}
			for _, n := range tc.nodes {
				facts = append(facts, producerProofFacts(n))
			}
			b, receipts := seedSyntheticCustody(t, facts...)
			stats, err := b.RebuildReduxThunk(t.Context(), receipts, reduxThunkStoreTestRules())
			require.NoError(t, err)
			wantStats := RebuildReduxThunkStats{Stubs: 1, Moves: 1}
			if tc.target != storeFactoryTestStub {
				wantStats.Resolved = 1
			}
			require.Equal(t, wantStats, stats)
			want := *call
			want.To = tc.target
			old, next := pendingEdgeKey(call), pendingEdgeKey(&want)
			gone, err := b.pendingEdge(old)
			require.NoError(t, err)
			require.Nil(t, gone)
			saved, err := b.pendingEdge(next)
			require.NoError(t, err)
			require.Equal(t, &want, saved)
			custody, err := readRebuildEdgeCustody(b.pending, next)
			require.NoError(t, err)
			require.Equal(t, rebuildEdgeCustody{raw: old}, custody)
		})
	}
}

func TestRebuildStoreFactoryAndReduxThunkAnchorProof(t *testing.T) {
	for _, redux := range []bool{false, true} {
		stage := "Store Factory"
		if redux {
			stage = "Redux Thunk"
		}
		t.Run(stage, func(t *testing.T) {
			for _, member := range []string{"still unrelated", "removed", "", "conflicting anchor receipts"} {
				t.Run("receipt role="+member, func(t *testing.T) {
					call := storeFactoryTestCall("retired", 2)
					call.Meta["store_action"] = "removed"
					action := storeFactoryTestAction("store.ts::reset", "store.ts", "")
					if redux {
						call.Meta["via"] = "redux-thunk"
						call.Meta["thunk_dispatch"] = "removed"
						action.Meta = map[string]any{"redux_thunk": storeFactoryTestMember}
					}
					source, decls := storeFactoryTestSource(call), producerProofFacts(action)
					b, _ := seedSyntheticCustody(t, source, decls)
					before := rebuildFrameworkCustodyRows(t, b)
					raw := *action
					if redux {
						raw.Meta = map[string]any{"redux_thunk": member}
					} else {
						raw.Meta = map[string]any{"store_factory": "otherStore", "store_member": member}
						if member == "removed" {
							raw.Meta["store_factory"] = storeFactoryTestBinding
						}
						if member == "" {
							delete(raw.Meta, "store_factory")
						}
					}
					if member == "conflicting anchor receipts" {
						raw.Meta = nil
						decls.Nodes = []*graph.Node{action, &raw}
					} else {
						decls.Nodes = []*graph.Node{&raw}
					}
					receipts := sqlCallsiteTestReceipts(source, decls)
					var moves uint64
					var err error
					if redux {
						var stats RebuildReduxThunkStats
						stats, err = b.RebuildReduxThunk(t.Context(), receipts, reduxThunkStoreTestRules())
						moves = stats.Moves
					} else {
						var stats RebuildStoreFactoryStats
						stats, err = b.RebuildStoreFactory(t.Context(), receipts, storeFactoryTestRules())
						moves = stats.Moves
					}
					want := *call
					want.To = "unresolved::*.removed"
					if member != "still unrelated" {
						var refusal *RebuildUnknownProvenanceError
						require.ErrorAs(t, err, &refusal)
						require.Equal(t, &RebuildUnknownProvenanceError{Count: 1, First: graph.EdgeIdentityFor(&want)}, refusal)
						require.Zero(t, moves)
						require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
						return
					}
					require.NoError(t, err)
					require.Equal(t, uint64(1), moves)
					old, next := pendingEdgeKey(call), pendingEdgeKey(&want)
					gone, err := b.pendingEdge(old)
					require.NoError(t, err)
					require.Nil(t, gone)
					saved, err := b.pendingEdge(next)
					require.NoError(t, err)
					require.Equal(t, &want, saved)
					custody, err := readRebuildEdgeCustody(b.pending, next)
					require.NoError(t, err)
					require.Equal(t, rebuildEdgeCustody{raw: old}, custody)
				})
			}
		})
	}
}

func TestRebuildStoreFactoryAndReduxThunkGetterCensus(t *testing.T) {
	for _, redux := range []bool{false, true} {
		stage, stageErr := "Store Factory", ErrRebuildStoreFactory
		if redux {
			stage, stageErr = "Redux Thunk", ErrRebuildReduxThunk
		}
		t.Run(stage, func(t *testing.T) {
			caller := storeFactoryTestCaller()
			winner := storeFactoryTestAction("app.ts::reset", caller.FilePath, caller.WorkspaceID)
			peer := storeFactoryTestAction("peer.ts::reset", "peer.ts", "foreign")
			call := storeFactoryTestCall(winner.ID, 2)
			if redux {
				call.Meta["via"] = "redux-thunk"
				call.Meta["thunk_dispatch"] = storeFactoryTestMember
				winner.Meta["redux_thunk"] = storeFactoryTestMember
				peer.Meta["redux_thunk"] = storeFactoryTestMember
			}
			getter := storeFactoryTestGetter("peer.ts::getter", peer.FilePath)
			getter.Meta["large"] = strings.Repeat("x", rebuildStoreFactoryRowBytes)
			b, _ := seedSyntheticCustody(t, producerProofFacts(caller), producerProofFacts(winner),
				producerProofFacts(peer), producerProofFacts(getter))
			check := func(stubs uint64, consulted bool) {
				t.Helper()
				before := rebuildFrameworkCustodyRows(t, b)
				receipts := func(func(ParseFacts, error) bool) { t.Fatal("unexpected receipt scan") }
				var err error
				if redux {
					var stats RebuildReduxThunkStats
					stats, err = b.RebuildReduxThunk(t.Context(), receipts, reduxThunkStoreTestRules())
					require.Equal(t, RebuildReduxThunkStats{Stubs: stubs, Resolved: stubs}, stats)
				} else {
					var stats RebuildStoreFactoryStats
					stats, err = b.RebuildStoreFactory(t.Context(), receipts, storeFactoryTestRules())
					require.Equal(t, RebuildStoreFactoryStats{Stubs: stubs, Resolved: stubs}, stats)
				}
				if consulted {
					require.ErrorIs(t, err, stageErr)
					require.ErrorIs(t, err, ErrRebuildLinkInputSize)
					require.ErrorContains(t, err, stage+" node row")
				} else {
					require.NoError(t, err)
				}
				require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
			}
			check(0, false) // no Call
			b.AddEdge(call)
			check(1, false) // one candidate after workspace filtering
			localPeer := *peer
			localPeer.WorkspaceID = caller.WorkspaceID
			b.AddNode(&localPeer)
			check(0, true) // the consulted getter hits the cap; refused stats are zero
		})
	}
}

func TestRebuildReduxThunkInputGuards(t *testing.T) {
	large := strings.Repeat("x", rebuildReduxThunkRowBytes)
	for _, tc := range []struct {
		name, message string
		cause         error
	}{
		{"candidate node cap", "Redux Thunk node row", ErrRebuildLinkInputSize},
		{"owned Call cap", "Redux Thunk edge row", ErrRebuildLinkInputSize},
		{"candidate without ID", "Redux Thunk candidate has no ID", errInvalidRecord},
	} {
		t.Run(tc.name, func(t *testing.T) {
			thunk := storeFactoryTestAction("store.ts::reset", "store.ts", "")
			thunk.Meta = map[string]any{"redux_thunk": storeFactoryTestMember}
			b, _ := seedSyntheticCustody(t, producerProofFacts(thunk))
			var key, val []byte
			var err error
			switch tc.name {
			case "candidate node cap":
				thunk.Meta["large"] = large
				key = pendingNodeKey(thunk.ID)
				val, err = encodeNode(thunk)
			case "owned Call cap":
				call := storeFactoryTestCall("retired", 2)
				call.Meta = map[string]any{"via": "redux-thunk", "thunk_dispatch": storeFactoryTestMember}
				call.Context = large
				key = pendingEdgeKey(call)
				val, err = encodeEdge(call)
			case "candidate without ID":
				thunk.ID = ""
				key = pendingNodeKey(thunk.ID)
				val, err = encodeNode(thunk)
			}
			require.NoError(t, err)
			require.NoError(t, b.pending.Set(key, val, pebble.NoSync))
			before := rebuildFrameworkCustodyRows(t, b)
			receipts := func(func(ParseFacts, error) bool) { t.Fatal("unexpected receipt scan") }
			stats, err := b.RebuildReduxThunk(t.Context(), receipts, reduxThunkStoreTestRules())
			require.ErrorIs(t, err, ErrRebuildReduxThunk)
			require.ErrorIs(t, err, tc.cause)
			require.ErrorContains(t, err, tc.message)
			require.Zero(t, stats)
			require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
		})
	}
}

func TestRebuildReduxThunkScratchReadGuards(t *testing.T) {
	for _, guard := range []string{"election row", "receipt claim", "deferred Call missing", "recorder has no old Call"} {
		t.Run(guard, func(t *testing.T) {
			b := newBuild(t, FullProfile)
			p := rebuildReduxThunkPass{
				rebuildCallMoves: rebuildCallMoves{
					b: b, prefix: rebuildReduxThunkPrefix, rowBytes: rebuildReduxThunkRowBytes, label: "Redux Thunk", inputRow: "edge row",
				},
				rules: reduxThunkStoreTestRules(),
			}
			call := storeFactoryTestCall("retired", 2)
			call.Meta["via"] = "redux-thunk"
			call.Meta["thunk_dispatch"] = storeFactoryTestMember
			key := pendingEdgeKey(call)
			var err error
			switch guard {
			case "election row":
				require.NoError(t, p.put(p.poolKey('t', 'i', storeFactoryTestMember), []byte{1}))
				_, err = p.elect(call, storeFactoryTestMember, storeFactoryTestBinding, true)
			case "receipt claim":
				require.NoError(t, p.put(append(p.column('r'), key...), []byte{0}))
				err = p.claim(nil, key)
			case "deferred Call missing":
				// One real Call makes plan read the deferred range. Its other
				// entry deliberately references a Call that is not in the store.
				b.AddEdge(call)
				require.NoError(t, p.put(p.poolKey('t', 'i', storeFactoryTestMember), []byte{0}))
				missing := *call
				missing.To = "before-retired"
				require.NoError(t, p.put(append(p.column('l'), pendingEdgeKey(&missing)...), nil))
				err = p.plan(t.Context())
			case "recorder has no old Call":
				require.NoError(t, p.claim(nil, key))
				err = p.record(t.Context(), storeFactoryTestSource(call))
			}
			require.ErrorIs(t, err, errInvalidRecord)
			require.ErrorContains(t, err, "Redux Thunk "+guard)
		})
	}
}

func TestRebuildReduxThunkSkipsActionCapWithoutThunk(t *testing.T) {
	action := storeFactoryTestAction("a.ts::reset", "a.ts", "")
	action.Meta["large"] = strings.Repeat("x", rebuildReduxThunkRowBytes)
	call := storeFactoryTestCall("retired", 2)
	call.Meta["via"] = "redux-thunk"
	call.Meta["thunk_dispatch"] = storeFactoryTestMember
	b, _ := seedSyntheticCustody(t, producerProofFacts(action), storeFactoryTestSource(call))
	receipts := func(func(ParseFacts, error) bool) { t.Fatal("unexpected receipt scan") }
	before := rebuildFrameworkCustodyRows(t, b)
	stats, err := b.RebuildReduxThunk(t.Context(), receipts, reduxThunkStoreTestRules())
	require.NoError(t, err)
	require.Zero(t, stats)
	require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))

	// The over-cap action sorts before an unrelated thunk, which still
	// activates the action index.
	thunk := storeFactoryTestAction("z.ts::unrelated", "z.ts", "")
	thunk.Meta = map[string]any{"redux_thunk": "unrelated"}
	b.AddNode(thunk)
	before = rebuildFrameworkCustodyRows(t, b)
	stats, err = b.RebuildReduxThunk(t.Context(), receipts, reduxThunkStoreTestRules())
	require.ErrorIs(t, err, ErrRebuildReduxThunk)
	require.ErrorIs(t, err, ErrRebuildLinkInputSize)
	require.ErrorContains(t, err, "Redux Thunk node row")
	require.Zero(t, stats)
	require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
}
