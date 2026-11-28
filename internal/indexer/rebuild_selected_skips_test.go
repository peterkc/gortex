package indexer

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/resolver"
)

func TestRebuildSelectedSkipsProtectSvelteKitRawElection(t *testing.T) {
	for _, exact := range []bool{true, false} {
		t.Run(map[bool]string{true: "selected", false: "unfiltered control"}[exact], func(t *testing.T) {
			ctx := t.Context()
			b, err := store_pebble.OpenBuild(t.TempDir(), store_pebble.FullProfile)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, b.Close()) })
			const page, server = "routes/+page.ts", "routes/+page.server.ts"
			// A configured basename extractor can own this .ts filename
			// with a deferred effective language; the native role is by path.
			classifications := map[string]rebuildClassificationFact{server: {Language: "json", Size: 129, MaxFileSize: 128}}
			n := &graph.Node{ID: page + "::Page", Name: "Page", FilePath: page, Kind: graph.KindType, Language: "typescript", StartLine: 1, Meta: map[string]any{"component": true}}
			facts := store_pebble.ParseFacts{File: graph.FileMetaRow{FilePath: page, NodeCount: 1}, EffectiveLanguage: "typescript", Nodes: []*graph.Node{n}}
			require.NoError(t, b.AddParsedFile("", facts))
			require.NoError(t, b.AddRebuildHeldSkip(rebuildCheckedSkipNode(server, classifications[server])))
			require.NoError(t, b.IsolateRebuildLinks(ctx))
			selected, err := resolver.NewFrameworkSynthesizerSelection([]string{resolver.SynthSvelteKitLoad})
			require.NoError(t, err)
			receipts := func(yield func(store_pebble.ParseFacts, error) bool) { yield(facts, nil) }
			adds, err := resolver.RunRebuildSvelteKitLoad(ctx, b, receipts, selected, rebuildCheckedSkips(selectRebuildCheckedSkips(classifications, exact)))
			require.Zero(t, adds)
			if exact {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, store_pebble.ErrRebuildSvelteKitLoad)
			}
			require.Contains(t, classifications, server, "filter must not change checked owner inventory")
		})
	}
}
