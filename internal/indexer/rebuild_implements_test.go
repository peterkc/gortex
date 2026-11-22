package indexer

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/resolver"
)

func TestRebuildImplementsNativeAllSupported(t *testing.T) {
	ctx := t.Context()
	facts, nodes, input := rebuildImplementsFixture()
	want := slices.Concat(input, []*graph.Edge{{
		From: "full.ts::Full", To: "contract.ts::Runner", Kind: graph.EdgeImplements,
		FilePath: "full.ts", Line: 10, Origin: "",
		Meta: map[string]any{"via": "method-set-inference"},
	}})

	native, err := store_sqlite.Open(filepath.Join(t.TempDir(), "native.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, native.Close()) })
	require.NoError(t, native.AddBatchChecked(nodes, input))
	require.ElementsMatch(t, input, native.AllEdges(), "native settled input")
	require.Equal(t, 1, resolver.New(native).InferImplements(), "only the unlinked full match emits")
	nativeRows := native.AllEdges()
	require.ElementsMatch(t, want, nativeRows, "native full persisted rows")

	build, err := store_pebble.OpenBuild(filepath.Join(t.TempDir(), "working"), store_pebble.FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, build.Close()) })
	build.AddBatch(nodes, input)
	view := build.RebuildView()
	read := func() []*graph.Edge {
		t.Helper()
		var rows []*graph.Edge
		require.NoError(t, view.VisitEdges(ctx, func(e *graph.Edge) error {
			rows = append(rows, e)
			return nil
		}))
		return rows
	}
	require.ElementsMatch(t, input, read(), "private settled input")
	require.NoError(t, view.VisitLinkPendingPages(ctx, 1, 1<<20, func(rows []*graph.Edge) error {
		t.Fatalf("settled input has a pending resolver page: %v", rows)
		return nil
	}))
	receipts := func(yield func(store_pebble.ParseFacts, error) bool) {
		for _, receipt := range facts {
			if !yield(receipt, nil) {
				return
			}
		}
	}
	require.NoError(t, resolver.RunRebuildImplements(ctx, build, receipts))
	require.ElementsMatch(t, nativeRows, read(), "private full persisted rows, including both pre-existing sites")
}

// The fixture starts after resolution. It supplies matching pre-link receipts
// but does not show that a parser or the full build worker produces these rows.
func rebuildImplementsFixture() (facts []store_pebble.ParseFacts, nodes []*graph.Node, settled []*graph.Edge) {
	const tsFile = "contract.ts"
	const ifaceID = tsFile + "::Runner"
	for _, tc := range []struct {
		path, language, name string
		methods              []string
	}{
		{tsFile, "typescript", "Declared", []string{"Run", "Stop"}},
		{"full.ts", "typescript", "Full", []string{"Run", "Stop", "Reset"}},
		{"partial.go", "go", "Partial", []string{"Run"}},
		{"partial.js", "javascript", "Partial", []string{"Stop"}},
	} {
		const declarationLine = 10
		ownerID := tc.path + "::" + tc.name
		receipt := store_pebble.ParseFacts{
			File:              graph.FileMetaRow{FilePath: tc.path},
			EffectiveLanguage: tc.language,
			Nodes: []*graph.Node{
				{ID: ownerID, Name: tc.name, Kind: graph.KindType, FilePath: tc.path,
					Language: tc.language, StartLine: declarationLine, EndLine: 30},
			},
		}
		for i, name := range tc.methods {
			id, line := ownerID+"."+name, 20+i
			receipt.Nodes = append(receipt.Nodes, &graph.Node{
				ID: id, Name: name, Kind: graph.KindMethod, FilePath: tc.path,
				Language: tc.language, StartLine: line, EndLine: line,
			})
			receipt.Edges = append(receipt.Edges, &graph.Edge{
				From: id, To: ownerID, Kind: graph.EdgeMemberOf, FilePath: tc.path, Line: line,
			})
		}
		settled = append(settled, receipt.Edges...)
		if tc.path == tsFile {
			receipt.Nodes = append(receipt.Nodes, &graph.Node{
				ID: ifaceID, Name: "Runner", Kind: graph.KindInterface, FilePath: tsFile,
				Language: "typescript", StartLine: 2, EndLine: 5,
				Meta: map[string]any{"methods": []string{"Run", "Stop"}, "type_flavor": "interface"},
			})
			// Neither site has the declaration key that inference would insert.
			for i, tag := range []string{"first site", "second site"} {
				raw := &graph.Edge{
					From: ownerID, To: "unresolved::Runner", Kind: graph.EdgeImplements,
					FilePath: tsFile, Line: 12 + i, Origin: graph.OriginASTResolved,
					Meta: map[string]any{"ref_context": graph.RefContextInherit, "fixture_tag": tag},
				}
				receipt.Edges = append(receipt.Edges, raw)
				resolved := *raw
				resolved.To = ifaceID
				settled = append(settled, &resolved)
			}
		}
		receipt.File.NodeCount = len(receipt.Nodes)
		facts = append(facts, receipt)
		nodes = append(nodes, receipt.Nodes...)
	}
	return facts, nodes, settled
}
