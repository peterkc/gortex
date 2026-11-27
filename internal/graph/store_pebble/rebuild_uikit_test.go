package store_pebble

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildUIKitGuardsAndCleanup(t *testing.T) {
	const file = "App/Use.swift"
	source := &graph.Node{ID: "source", Kind: graph.KindFunction, FilePath: file, Language: "swift"}
	target := &graph.Node{ID: "target", Name: "DetailViewController", Kind: graph.KindType,
		FilePath: "App/ViewControllers/Detail.swift", Language: "swift"}
	site := &graph.Edge{From: source.ID, To: "unresolved::" + target.Name, Kind: graph.EdgeCalls, FilePath: file, Line: 1,
		Meta: map[string]any{"site": "parse"}}
	caller := producerProofFacts(source)
	caller.Edges = []*graph.Edge{site}

	for _, name := range []string{"nil_receipts", "incomplete_rules", "missing_freeze", "success", "refused"} {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			b, receipts := seedSyntheticCustody(t, caller, producerProofFacts(target))
			wantRows := rebuildFrameworkCustodyRows(t, b)
			rules := rebuildConventionTestRules()
			if name != "missing_freeze" {
				require.NoError(t, b.FreezeUIKitInputs(ctx, func(graph.FrameworkCensusEdge) bool { return true }))
			}
			switch name {
			case "nil_receipts":
				receipts = nil
			case "incomplete_rules":
				rules.Rebind = nil
			case "refused":
				// The live target still wins, but its declaration is missing from the receipts.
				receipts = func(yield func(ParseFacts, error) bool) { yield(caller, nil) }
			}

			moves, err := b.RebuildUIKit(ctx, receipts, rules)
			switch name {
			case "nil_receipts", "incomplete_rules":
				require.ErrorIs(t, err, ErrRebuildUIKit)
				require.ErrorIs(t, err, errInvalidRecord)
				require.ErrorContains(t, err, "inputs")
				require.Zero(t, moves)
				return
			case "missing_freeze":
				require.ErrorIs(t, err, ErrRebuildUIKit)
				require.ErrorIs(t, err, errInvalidRecord)
				require.ErrorContains(t, err, "census missing")
				require.Zero(t, moves)
			case "refused":
				require.ErrorIs(t, err, ErrRebuildUIKit)
				var refused *RebuildUnknownProvenanceError
				require.ErrorAs(t, err, &refused)
				require.Equal(t, &RebuildUnknownProvenanceError{Count: 1, First: graph.EdgeIdentityFor(site)}, refused)
				require.Zero(t, moves)
			case "success":
				require.NoError(t, err)
				require.EqualValues(t, 1, moves)
				bound := *site
				bound.To, bound.Confidence = target.ID, 0.9
				oldKey, newKey := pendingEdgeKey(site), pendingEdgeKey(&bound)
				value, err := encodeEdge(&bound)
				require.NoError(t, err)
				delete(wantRows, string(oldKey))
				delete(wantRows, string(pendingReverseKey(site, oldKey)))
				wantRows[string(newKey)] = value
				wantRows[string(pendingReverseKey(&bound, newKey))] = newKey
				wantRows[string(rebuildLinkRecorderKey(newKey))] = oldKey
			}
			// The whole pending keyspace must match: saved bytes, custody, and no gqF scratch.
			require.Equal(t, wantRows, rebuildFrameworkCustodyRows(t, b))
		})
	}
}
