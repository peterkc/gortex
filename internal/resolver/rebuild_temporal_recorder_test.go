package resolver

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func TestRebuildTemporalFollowsInstalledCallRecorder(t *testing.T) {
	const (
		wrapperFile = "wf/wrapper.go"
		callerFile  = "wf/use.go"
		wrapper     = wrapperFile + "::run"
		caller      = callerFile + "::Use"
		activity    = wrapperFile + "::Charge"
	)
	wrapperFacts := store_pebble.ParseFacts{
		EffectiveLanguage: "go", File: graph.FileMetaRow{FilePath: wrapperFile, NodeCount: 3},
		Nodes: []*graph.Node{
			{ID: wrapper, Name: "run", Kind: graph.KindFunction, Language: "go", FilePath: wrapperFile},
			{ID: wrapper + "#param:name", Name: "name", Kind: graph.KindParam, Language: "go", FilePath: wrapperFile,
				Meta: map[string]any{"position": 0}},
			{ID: activity, Name: "Charge", Kind: graph.KindFunction, Language: "go", FilePath: wrapperFile,
				Meta: map[string]any{"signature": "func Charge(ctx context.Context) error"}},
		},
		Edges: []*graph.Edge{rebuildTemporalOracleStub(wrapperFile, wrapper, "name")},
	}
	wrapperFacts.Edges[0].Meta["temporal_name_param"] = "name"
	call := &graph.Edge{From: caller, To: "unresolved::run", Kind: graph.EdgeCalls, FilePath: callerFile, Line: 3,
		Meta: map[string]any{"callee": "run", "arg_names": []string{"Charge"}}}
	callerFacts := store_pebble.ParseFacts{
		EffectiveLanguage: "go", File: graph.FileMetaRow{FilePath: callerFile, NodeCount: 1},
		Nodes: []*graph.Node{{ID: caller, Name: "Use", Kind: graph.KindFunction, Language: "go", FilePath: callerFile}},
		Edges: []*graph.Edge{call},
	}
	landed := *call
	landed.To = wrapper
	move := graph.EdgeReindex{Edge: &landed, OldTo: call.To}
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthTemporalStub})
	require.NoError(t, err)
	native := openRebuildTemporalOracle(t, wrapperFacts, callerFacts)
	native.ReindexEdges([]graph.EdgeReindex{move})
	require.NoError(t, RunFrameworkSynthesizersWithSelection(native, selection).Err)
	require.Len(t, native.GetOutEdges(caller), 2)

	for _, withRecorder := range []bool{true, false} {
		name := "missing original key cannot use same-site decoy"
		if withRecorder {
			name = "main winner is the wrapper recorder"
		}
		t.Run(name, func(t *testing.T) {
			b, err := store_pebble.OpenBuild(filepath.Join(t.TempDir(), "working"), store_pebble.FullProfile)
			require.NoError(t, err)
			t.Cleanup(func() {
				require.NoError(t, b.Close())
			})
			require.NoError(t, b.AddParsedFile("", wrapperFacts))
			require.NoError(t, b.AddParsedFile("", callerFacts))
			receipt := callerFacts
			if withRecorder {
				require.NoError(t, b.ApplyLinkReindexes(t.Context(), []graph.EdgeReindex{move}))
			} else {
				b.ReindexEdges([]graph.EdgeReindex{move})
				decoy := *call
				decoy.To = "unresolved::other::run"
				receipt.Edges = []*graph.Edge{&decoy}
			}
			before := rebuildGRPCSavedCalls(t, b)
			stats, err := RunRebuildTemporal(t.Context(), b, rebuildGRPCReceipts(wrapperFacts, receipt), selection, 1)
			if !withRecorder {
				var refused *store_pebble.RebuildUnknownProvenanceError
				require.ErrorAs(t, err, &refused)
				require.Equal(t, uint64(1), refused.Count)
				require.Equal(t, graph.EdgeIdentity{From: caller, To: activity, Kind: graph.EdgeCalls, FilePath: callerFile, Line: call.Line}, refused.First)
				require.Zero(t, stats)
				require.Equal(t, before, rebuildGRPCSavedCalls(t, b))
				return
			}
			require.NoError(t, err)
			require.Equal(t, uint64(1), stats.Wrappers)
			want := make(map[graph.EdgeIdentity]*graph.Edge)
			for e := range native.EdgesByKind(graph.EdgeCalls) {
				want[graph.EdgeIdentityFor(e)] = e
			}
			got := make(map[graph.EdgeIdentity]*graph.Edge)
			for _, e := range rebuildGRPCSavedCalls(t, b) {
				got[graph.EdgeIdentityFor(e)] = e
			}
			require.Equal(t, want, got)
		})
	}
}
