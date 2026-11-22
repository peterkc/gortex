package indexer

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/resolver"
)

func TestRebuildImplementsReceiptBoundary(t *testing.T) {
	for _, tc := range []struct {
		name                            string
		moveMember, suppress, duplicate bool
		refuse, retained                bool
	}{
		{name: "deferred interface does not prevent supported match", retained: true},
		{name: "sole method membership in deferred receipt", moveMember: true, refuse: true},
		{name: "unproved preexisting pair at another site", suppress: true, refuse: true},
		{name: "independent supported method name", moveMember: true, duplicate: true, retained: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const file, plugin = "types.go", "other.custom"
			owner := &graph.Node{ID: file + "::T", Name: "T", Kind: graph.KindType, FilePath: file, StartLine: 3, Language: "go"}
			method := &graph.Node{ID: file + "::T.Run", Name: "Run", Kind: graph.KindMethod, FilePath: file, Language: "go"}
			iface := &graph.Node{ID: file + "::I", Name: "I", Kind: graph.KindInterface, FilePath: file, Language: "go", Meta: map[string]any{"methods": []string{"Run"}}}
			other := &graph.Node{ID: plugin + "::J", Name: "J", Kind: graph.KindInterface, FilePath: plugin, Language: "custom", Meta: map[string]any{"methods": []string{"Run"}}}
			member := &graph.Edge{From: method.ID, To: owner.ID, Kind: graph.EdgeMemberOf, FilePath: file, Line: 4}
			goFacts := store_pebble.ParseFacts{EffectiveLanguage: "go", File: graph.FileMetaRow{FilePath: file}, Nodes: []*graph.Node{owner, method, iface}}
			otherFacts := store_pebble.ParseFacts{EffectiveLanguage: "custom", File: graph.FileMetaRow{FilePath: plugin}, Nodes: []*graph.Node{other}}
			if tc.moveMember {
				otherFacts.Edges = append(otherFacts.Edges, member) // same row, different producer than path
			} else {
				goFacts.Edges = append(goFacts.Edges, member)
			}
			if tc.duplicate {
				independent := *method
				independent.ID += "@different"
				proof := *member
				proof.From, proof.Line = independent.ID, 6
				goFacts.Nodes = append(goFacts.Nodes, &independent)
				goFacts.Edges = append(goFacts.Edges, &proof)
			}
			if tc.suppress {
				otherFacts.Edges = append(otherFacts.Edges, &graph.Edge{From: owner.ID, To: iface.ID, Kind: graph.EdgeImplements,
					FilePath: plugin, Line: 29, Confidence: .5})
			}
			goFacts.File.NodeCount, otherFacts.File.NodeCount = len(goFacts.Nodes), len(otherFacts.Nodes)
			build, err := store_pebble.OpenBuild(filepath.Join(t.TempDir(), "working"), store_pebble.FullProfile)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, build.Close()) })
			require.NoError(t, build.AddParsedFile("", otherFacts))
			require.NoError(t, build.AddParsedFile("", goFacts))
			want := slices.Concat(goFacts.Edges, otherFacts.Edges)
			err = resolver.RunRebuildImplements(t.Context(), build, func(yield func(store_pebble.ParseFacts, error) bool) {
				if yield(otherFacts, nil) {
					yield(goFacts, nil)
				}
			})
			inferred := &graph.Edge{From: owner.ID, To: iface.ID, Kind: graph.EdgeImplements, FilePath: owner.FilePath,
				Line: owner.StartLine, Meta: map[string]any{"via": "method-set-inference"}}
			if tc.refuse {
				var refusal *store_pebble.RebuildUnknownProvenanceError
				require.ErrorAs(t, err, &refusal)
				require.Equal(t, uint64(1), refusal.Count)
				require.Equal(t, graph.EdgeIdentityFor(inferred), refusal.First)
			} else {
				require.NoError(t, err)
			}
			if tc.retained {
				want = append(want, inferred)
			}
			var rows []*graph.Edge
			require.NoError(t, build.RebuildView().VisitEdges(t.Context(), func(e *graph.Edge) error {
				rows = append(rows, e)
				return nil
			}))
			require.ElementsMatch(t, want, rows)
		})
	}
}
