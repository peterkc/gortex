package store_pebble_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/resolver"
)

func TestRebuildTemporalReducerStampsWrapperCustody(t *testing.T) {
	const (
		file     = "wf.go"
		wrapper  = file + "::run"
		caller   = file + "::Use"
		activity = file + "::Charge"
	)
	seed := &graph.Edge{
		From: wrapper, To: "unresolved::temporal::activity::name", Kind: graph.EdgeCalls, FilePath: file, Line: 10,
		Meta: map[string]any{
			"via":                 "temporal.stub",
			"temporal_kind":       "activity",
			"temporal_name":       "name",
			"temporal_name_param": "name",
		},
	}
	trigger := &graph.Edge{
		From: caller, To: wrapper, Kind: graph.EdgeCalls, FilePath: file, Line: 20,
		Meta: map[string]any{"callee": "run", "arg_names": []string{"Charge"}},
	}
	facts := store_pebble.ParseFacts{
		EffectiveLanguage: "go", File: graph.FileMetaRow{FilePath: file, NodeCount: 4},
		Nodes: []*graph.Node{
			{ID: caller, Name: "Use", Kind: graph.KindFunction, Language: "go", FilePath: file},
			{ID: wrapper, Name: "run", Kind: graph.KindFunction, Language: "go", FilePath: file},
			{ID: wrapper + "#param:name", Name: "name", Kind: graph.KindParam, Language: "go", FilePath: file,
				Meta: map[string]any{"position": 0}},
			{ID: activity, Name: "Charge", Kind: graph.KindFunction, Language: "go", FilePath: file,
				Meta: map[string]any{"signature": "func Charge(ctx context.Context) error"}},
		},
		Edges: []*graph.Edge{seed, trigger},
	}
	b, err := store_pebble.OpenBuild(filepath.Join(t.TempDir(), "working"), store_pebble.FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	require.NoError(t, b.AddParsedFile("", facts))
	selection, err := resolver.NewFrameworkSynthesizerSelection([]string{resolver.SynthTemporalStub})
	require.NoError(t, err)
	receipts := func(yield func(store_pebble.ParseFacts, error) bool) { yield(facts, nil) }
	stats, err := resolver.RunRebuildTemporal(t.Context(), b, receipts, selection, 1)
	require.NoError(t, err)
	require.EqualValues(t, 1, stats.Wrappers)
	require.EqualValues(t, 1, stats.Moves)
	output := &graph.Edge{From: caller, To: activity, Kind: graph.EdgeCalls, FilePath: file, Line: trigger.Line}
	store_pebble.RequireRebuildTemporalSyntheticCustody(t, b, output)
}
