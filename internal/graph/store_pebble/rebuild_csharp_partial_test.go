package store_pebble

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func partialTestRules() RebuildCSharpPartialRules {
	return RebuildCSharpPartialRules{
		Group: func(n *graph.Node) (string, string, string) {
			if n.Kind != graph.KindType || n.Language != "csharp" {
				return "", "", ""
			}
			ns, _ := n.Meta["scope_ns"].(string)
			return n.RepoPrefix, ns, n.Name
		},
		Retarget: func(e *graph.Edge, _, canonical string) (graph.EdgeReindex, bool) {
			old := e.To
			e.To = canonical
			return graph.EdgeReindex{Edge: e, OldTo: old}, true
		},
		CopyFrom: func(e *graph.Edge, canonical string) *graph.Edge {
			ne := *e
			ne.From = canonical
			return &ne
		},
		Stamp: func(n *graph.Node, canonical string) bool {
			if n.Meta == nil {
				n.Meta = map[string]any{}
			}
			if n.ID == canonical {
				n.Meta["partial"] = true
			} else {
				n.Meta["partial_merged_into"] = canonical
			}
			return true
		},
	}
}

func TestRebuildCSharpPartialNoGroupByteIdentity(t *testing.T) {
	for _, lang := range []string{"go", "csharp"} {
		b := newBuild(t, FullProfile)
		for _, ns := range []string{"a", "b"} {
			b.AddNode(&graph.Node{ID: ns + "::T", Name: "T", Kind: graph.KindType, FilePath: ns, Language: lang,
				Meta: map[string]any{"scope_ns": ns}})
		}
		b.AddEdge(&graph.Edge{From: "a::T", To: "b::T", Kind: graph.EdgeExtends, FilePath: "a", Line: 1})
		before := rebuildFrameworkCustodyRows(t, b)
		require.NoError(t, b.RebuildCSharpPartial(t.Context(), func(func(ParseFacts, error) bool) {
			t.Fatal("a graph without a merge group must not replay receipts")
		}, partialTestRules()))
		require.Equal(t, before, rebuildFrameworkCustodyRows(t, b), "every downstream input row and custody byte is unchanged")
	}
}

func TestRebuildCSharpPartialRawCustody(t *testing.T) {
	const a, b = "A.cs", "B.cs"
	canonical := &graph.Node{ID: a + "::T", Kind: graph.KindType, Language: "csharp", FilePath: a, Name: "T"}
	fragment := &graph.Node{ID: b + "::T", Kind: graph.KindType, Language: "csharp", FilePath: b, Name: "T"}
	raw := &graph.Edge{From: fragment.ID, To: "unresolved::IFace", Kind: graph.EdgeImplements, FilePath: b, Line: 1}
	settled := *raw
	settled.To = "types.cs::IFace"
	folded := settled
	folded.From = canonical.ID
	for _, rawPresent := range []bool{true, false} {
		build := newBuild(t, FullProfile)
		build.AddBatch([]*graph.Node{canonical, fragment}, []*graph.Edge{raw})
		require.NoError(t, build.ApplyLinkReindexes(t.Context(), []graph.EdgeReindex{{Edge: &settled, OldTo: raw.To}}))
		before := rebuildFrameworkCustodyRows(t, build)
		receipts := func(yield func(ParseFacts, error) bool) {
			if !yield(ParseFacts{File: graph.FileMetaRow{FilePath: a}, EffectiveLanguage: "csharp", Nodes: []*graph.Node{canonical}}, nil) {
				return
			}
			edge := &folded // A receipt at the destination cannot prove the copy.
			if rawPresent {
				edge = raw
			}
			yield(ParseFacts{File: graph.FileMetaRow{FilePath: b}, EffectiveLanguage: "csharp", Nodes: []*graph.Node{fragment}, Edges: []*graph.Edge{edge}}, nil)
		}
		err := build.RebuildCSharpPartial(t.Context(), receipts, partialTestRules())
		if !rawPresent {
			require.ErrorIs(t, err, ErrRebuildCSharpPartial)
			require.ErrorIs(t, err, ErrRebuildUnknownProvenance)
			require.Equal(t, before, rebuildFrameworkCustodyRows(t, build))
			continue
		}
		require.NoError(t, err)
		saved, err := build.pendingEdge(pendingEdgeKey(&folded))
		require.NoError(t, err)
		require.Equal(t, &folded, saved)
		custody, err := readRebuildEdgeCustody(build.pending, pendingEdgeKey(saved))
		require.NoError(t, err)
		require.Equal(t, rebuildEdgeCustody{raw: pendingEdgeKey(raw)}, custody)
		from, err := build.csharpPartialReceiptSource(saved)
		require.NoError(t, err)
		require.Equal(t, fragment.ID, from)
	}
}
