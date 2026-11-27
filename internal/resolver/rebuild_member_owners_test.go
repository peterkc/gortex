package resolver

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func TestRebuildMemberOwnerSQLiteOrder(t *testing.T) {
	const methodFile, typeFile = "pkg/methods.go", "pkg/types.go"
	method := rebuildEventNode(methodFile, "Wanted.handle", "go", "")
	method.Kind, method.Name = graph.KindMethod, "handle"
	wanted := rebuildEventNode(typeFile, "Wanted", "go", "")
	other := rebuildEventNode(typeFile, "Other", "go", "")
	wanted.Kind, other.Kind = graph.KindType, graph.KindType
	repair := &graph.Edge{From: method.ID, To: methodFile + "::Wanted", Kind: graph.EdgeMemberOf, FilePath: methodFile, Line: 3}
	stationary := *repair
	stationary.To = other.ID
	for _, synth := range []string{SynthExpressResolve, SynthLaravelEvent} {
		t.Run(synth, func(t *testing.T) {
			selection, err := NewFrameworkSynthesizerSelection([]string{synth})
			require.NoError(t, err)
			const js, php = "src/routes.js", "app.php"
			caller := rebuildEventNode(js, "route", "javascript", "")
			call := &graph.Edge{
				From: caller.ID, To: "unresolved::handle", Kind: graph.EdgeCalls, FilePath: js, Line: 9,
				Meta: map[string]any{"express_handler_ref": true, "express_ref_class": "Wanted", "express_ref_method": "handle"},
			}
			facts := rebuildEventFacts(js, "javascript", []*graph.Node{caller}, call)
			if synth == SynthLaravelEvent {
				caller = rebuildEventNode(php, "publish", "php", "")
				provider := rebuildEventNode(php, "Provider", "php", "")
				provider.Kind = graph.KindType
				provider.Meta = map[string]any{"laravel_listen_map": "Fired=>Wanted"}
				// The native preflight needs a PHP handle. This one has no
				// owner or typed listener role, so it cannot win the election.
				witness := rebuildEventNode(php, "Unmapped.handle", "php", "")
				witness.Kind, witness.Name = graph.KindMethod, "handle"
				call = &graph.Edge{
					From: caller.ID, To: "unresolved::*.handle", Kind: graph.EdgeCalls, FilePath: php, Line: 9,
					Meta: map[string]any{"via": laravelEventVia, "laravel_event_type": "Fired"},
				}
				facts = rebuildEventFacts(php, "php", []*graph.Node{caller, provider, witness}, call)
			}
			for _, tc := range []struct {
				name         string
				first, last  *graph.Edge
				want         int
				unknownOrder bool
			}{
				{name: "repair first", first: repair, last: &stationary},
				{name: "repair last", first: &stationary, last: repair, want: 1},
				{name: "unmarked move", first: repair, last: &stationary, unknownOrder: true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					methodFacts := rebuildEventFacts(methodFile, "go", []*graph.Node{method}, tc.first, tc.last)
					typeFacts := rebuildEventFacts(typeFile, "go", []*graph.Node{wanted, other})
					b, native := openRebuildEventStores(t, facts, methodFacts, typeFacts)
					receipts := rebuildGRPCReceipts(facts, methodFacts, typeFacts)
					if tc.unknownOrder {
						moved := *repair
						moved.To = wanted.ID
						require.NoError(t, b.ApplyLinkReindexes(t.Context(), []graph.EdgeReindex{{Edge: &moved, OldTo: repair.To}}))
					} else {
						changed, err := native.RebindGoMethodReceivers("")
						require.NoError(t, err)
						require.Equal(t, 1, changed)
						changed, err = b.RebuildGoMethodReceivers(t.Context(), receipts)
						require.NoError(t, err)
						require.Equal(t, 1, changed)
						require.Equal(t, rebuildEventSQLiteRows(native), rebuildEventSavedRows(t, b))
					}
					frozen, err := FreezeRebuildFrameworkSelection(t.Context(), b.RebuildView(), selection)
					require.NoError(t, err)
					require.True(t, frozen.allows(synth))
					before := rebuildEventSavedRows(t, b)
					if synth == SynthExpressResolve {
						_, err = RunRebuildExpress(t.Context(), b, receipts, frozen)
					} else {
						_, err = RunRebuildLaravelEvent(t.Context(), b, receipts, frozen)
					}
					if tc.unknownOrder {
						require.ErrorIs(t, err, store_pebble.ErrRebuildUnknownProvenance)
						require.Equal(t, before, rebuildEventSavedRows(t, b))
						return
					}
					require.NoError(t, err)
					if synth == SynthExpressResolve {
						require.Equal(t, tc.want, ResolveExpressHandlers(native))
					} else {
						require.Equal(t, tc.want, ResolveLaravelEventCalls(native))
					}
					require.Equal(t, rebuildEventSQLiteRows(native), rebuildEventSavedRows(t, b))
				})
			}
		})
	}
}
