package store_pebble

import (
	"crypto/sha256"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildGoExternalPrivateProducerStamp(t *testing.T) {
	ctx := t.Context()
	caller := &graph.Node{ID: "caller", Kind: graph.KindFunction, Name: "Call", Language: "go", FilePath: "caller.go"}
	module := &graph.Node{ID: "module", Kind: graph.KindModule, Name: "cors", Language: "go"}
	symbol := &graph.Node{ID: "external::example.test/cors::New", Kind: graph.KindFunction, Name: "New", Language: "go"}
	call := &graph.Edge{From: caller.ID, To: symbol.ID, Kind: graph.EdgeCalls, FilePath: caller.FilePath}
	member := &graph.Edge{From: symbol.ID, To: module.ID, Kind: graph.EdgeMemberOf, Origin: graph.OriginASTResolved}

	b := newBuild(t, FullProfile)
	b.AddBatch([]*graph.Node{caller}, []*graph.Edge{call})
	control := newBuild(t, FullProfile)
	control.AddBatch([]*graph.Node{caller, module, symbol}, []*graph.Edge{call})
	require.NoError(t, control.addEdgeWithCustody(member, rebuildEdgeCustody{proof: rebuildProofUntrackedMove}))
	want := rebuildFrameworkCustodyRows(t, control)
	desired := func(e *graph.Edge) (*graph.Node, *graph.Node, error) {
		if graph.EdgeIdentityFor(e) == graph.EdgeIdentityFor(call) {
			return module, symbol, nil
		}
		return nil, nil, nil
	}
	apply := func(store *BuildStore) {
		t.Helper()
		err := store.RebuildGoExternalCalls(ctx, desired, func(any) int { return 0 })
		require.NoError(t, err)
	}
	apply(b)
	apply(control)
	require.Equal(t, want, rebuildFrameworkCustodyRows(t, control), "an exact incumbent is not stamped")
	live := pendingNodeKey(symbol.ID)
	after := rebuildFrameworkCustodyRows(t, b)
	digest := sha256.Sum256(want[string(live)])
	stamp := string(rebuildGoExternalSymbolStampKey(live))
	require.Equal(t, digest[:], after[stamp])
	delete(after, stamp)
	require.Equal(t, want, after, "only the private producer witness differs")

	proved, err := rebuildGoExternalSymbolStamped(b.pending, live)
	require.NoError(t, err)
	require.True(t, proved)
	beforeRepeat := rebuildFrameworkCustodyRows(t, b)
	apply(b)
	require.Equal(t, beforeRepeat, rebuildFrameworkCustodyRows(t, b))

	changed := *symbol
	changed.Name = "Other"
	b.AddNode(&changed)
	apply(b)
	proved, err = rebuildGoExternalSymbolStamped(b.pending, live)
	require.NoError(t, err)
	require.False(t, proved, "a replacement cannot borrow the old witness")
}
