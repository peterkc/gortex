package store_pebble

import (
	"fmt"
	"strings"
	"testing"

	"github.com/cockroachdb/pebble/v2"
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
			key, value := "partial_merged_into", any(canonical)
			if n.ID == canonical {
				key, value = "partial", true
			}
			if n.Meta[key] == value {
				return false
			}
			if n.Meta == nil {
				n.Meta = map[string]any{}
			}
			n.Meta[key] = value
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
		replays := 0
		receipts := func(func(ParseFacts, error) bool) { replays++ }
		require.NoError(t, b.RebuildCSharpPartial(t.Context(), receipts, partialTestRules()))
		require.Equal(t, 1, replays, "the receipts could form a group that the live graph lacks")
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
	}
}

func partialTestFacts(lang string, n *graph.Node, edges ...*graph.Edge) ParseFacts {
	return ParseFacts{File: graph.FileMetaRow{FilePath: n.FilePath}, EffectiveLanguage: lang, Nodes: []*graph.Node{n}, Edges: edges}
}

func partialTestReceipts(facts ...ParseFacts) func(func(ParseFacts, error) bool) {
	return func(yield func(ParseFacts, error) bool) {
		for _, f := range facts {
			if !yield(f, nil) {
				return
			}
		}
	}
}

func partialTestType(id, file, ns string) *graph.Node {
	return &graph.Node{ID: id, Name: "T", Kind: graph.KindType, Language: "csharp", FilePath: file, Meta: map[string]any{"scope_ns": ns}}
}

func TestRebuildCSharpPartialReceiptCohorts(t *testing.T) {
	canonical := partialTestType("b", "B.cs", "ns")
	fragment := partialTestType("c", "C.cs", "ns")
	entrant := partialTestType("a", "A.cs", "other")
	moved := partialTestType(entrant.ID, entrant.FilePath, "ns")
	for _, active := range []bool{false, true} {
		b := newBuild(t, FullProfile)
		b.AddBatch([]*graph.Node{canonical, entrant}, nil)
		facts := []ParseFacts{partialTestFacts("csharp", canonical), partialTestFacts("razor", moved)}
		if active {
			b.AddNode(fragment)
			facts = append(facts, partialTestFacts("csharp", fragment))
		}
		before := rebuildFrameworkCustodyRows(t, b)
		err := b.RebuildCSharpPartial(t.Context(), partialTestReceipts(facts...), partialTestRules())
		var refused *RebuildUnknownProvenanceError
		require.ErrorAs(t, err, &refused)
		want := uint64(2)
		if active {
			want++
		}
		require.Equal(t, want, refused.Count)
		require.Equal(t, graph.EdgeIdentity{From: entrant.ID, Kind: "node"}, refused.First)
		require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
	}
}

func TestRebuildCSharpPartialReceiptOnlyGroup(t *testing.T) {
	canonical := partialTestType("a", "A.cs", "ns")
	fragment := partialTestType("b", "B.cs", "ns")
	for _, tc := range []struct {
		name string
		lang string
	}{
		{"no live candidate", "go"},
		{"one live candidate", "csharp"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBuild(t, FullProfile)
			first, second := *canonical, *fragment
			first.Language, second.Language = tc.lang, "go"
			b.AddBatch([]*graph.Node{&first, &second}, nil)
			before := rebuildFrameworkCustodyRows(t, b)
			receipts := partialTestReceipts(partialTestFacts("csharp", canonical), partialTestFacts("razor", fragment))
			err := b.RebuildCSharpPartial(t.Context(), receipts, partialTestRules())
			var refused *RebuildUnknownProvenanceError
			require.ErrorAs(t, err, &refused)
			require.EqualValues(t, 2, refused.Count)
			require.Equal(t, graph.EdgeIdentity{From: canonical.ID, Kind: "node"}, refused.First)
			require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
		})
	}
}

func TestRebuildCSharpPartialReceiptAdmission(t *testing.T) {
	canonical := partialTestType("a", "A.cs", "ns")
	fragment := partialTestType("b", "B.cs", "ns")
	for _, name := range []string{"no language", "other file", "unknown ID", "role cleared"} {
		t.Run(name, func(t *testing.T) {
			b := newBuild(t, FullProfile)
			live := *fragment
			live.Kind = graph.KindInterface
			b.AddNode(canonical)
			if name != "unknown ID" {
				b.AddNode(&live)
			}
			before := rebuildFrameworkCustodyRows(t, b)
			facts := partialTestFacts("razor", fragment)
			if name == "no language" {
				facts.EffectiveLanguage = ""
			}
			if name == "other file" {
				facts.File.FilePath = "Other.cs"
			}
			receipts := []ParseFacts{partialTestFacts("csharp", canonical), facts}
			if name == "role cleared" {
				receipts = append(receipts, partialTestFacts("csharp", &live))
			}
			require.NoError(t, b.RebuildCSharpPartial(t.Context(), partialTestReceipts(receipts...), partialTestRules()))
			require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
		})
	}
}

func TestRebuildCSharpPartialSettledAndSuppressed(t *testing.T) {
	canonical := partialTestType("a", "A.cs", "ns")
	fragment := partialTestType("b", "B.cs", "ns")
	canonical.Meta["partial"] = true
	fragment.Meta["partial_merged_into"] = canonical.ID
	occupied := &graph.Edge{From: canonical.ID, To: "iface", Kind: graph.EdgeImplements, FilePath: canonical.FilePath, Line: 1}
	duplicate := &graph.Edge{From: fragment.ID, To: occupied.To, Kind: occupied.Kind, FilePath: fragment.FilePath, Line: 2}
	for _, exact := range []bool{false, true} {
		b := newBuild(t, FullProfile)
		b.AddBatch([]*graph.Node{canonical, fragment}, []*graph.Edge{occupied, duplicate})
		cFacts := partialTestFacts("csharp", canonical)
		fFacts := partialTestFacts("razor", fragment)
		if exact {
			cFacts.Edges = []*graph.Edge{occupied}
		} else {
			fFacts.Edges = []*graph.Edge{duplicate}
		}
		before := rebuildFrameworkCustodyRows(t, b)
		err := b.RebuildCSharpPartial(t.Context(), partialTestReceipts(cFacts, fFacts), partialTestRules())
		if !exact {
			var refused *RebuildUnknownProvenanceError
			require.ErrorAs(t, err, &refused)
			require.Equal(t, graph.EdgeIdentityFor(occupied), refused.First)
			require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
			continue
		}
		require.NoError(t, err)
		out, err := b.pendingEdge(pendingEdgeKey(duplicate))
		require.NoError(t, err)
		require.Nil(t, out, "an already-suppressed copy does not need a receipt position")
		before = rebuildFrameworkCustodyRows(t, b)
		require.NoError(t, b.RebuildCSharpPartial(t.Context(), nil, partialTestRules()))
		require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
	}
}

func TestRebuildCSharpPartialIncidentRowsAndClaims(t *testing.T) {
	canonical := partialTestType("a", "A.cs", "ns")
	fragment := partialTestType("b", "B.cs", "ns")
	call := &graph.Edge{From: "caller", To: fragment.ID, Kind: graph.EdgeReferences, FilePath: "Use.cs", Line: 1}
	other := *call
	other.Line++
	for _, tc := range []string{"unrelated row", "incident row", "duplicate claim"} {
		t.Run(tc, func(t *testing.T) {
			b := newBuild(t, FullProfile)
			b.AddBatch([]*graph.Node{canonical, fragment}, []*graph.Edge{call})
			input := &graph.Node{ID: call.From, FilePath: call.FilePath, Kind: graph.KindFunction}
			facts := partialTestReceipts(partialTestFacts("csharp", canonical), partialTestFacts("razor", fragment), partialTestFacts("csharp", input, call))
			if tc == "duplicate claim" {
				err := b.putEdgeWithCustody(&other, rebuildEdgeCustody{raw: pendingEdgeKey(call)})
				require.NoError(t, err)
			} else {
				large := other
				if tc == "unrelated row" {
					large.To = "unrelated"
				}
				large.Meta = map[string]any{"large": strings.Repeat("x", rebuildPartialRowLimit)}
				b.AddEdge(&large)
			}
			before := rebuildFrameworkCustodyRows(t, b)
			err := b.RebuildCSharpPartial(t.Context(), facts, partialTestRules())
			switch tc {
			case "unrelated row":
				require.NoError(t, err)
				want := *call
				want.To = canonical.ID
				out, err := b.pendingEdge(pendingEdgeKey(&want))
				require.NoError(t, err)
				require.Equal(t, &want, out)
			case "incident row":
				require.ErrorIs(t, err, ErrRebuildLinkInputSize)
				require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
			case "duplicate claim":
				var refused *RebuildUnknownProvenanceError
				require.ErrorAs(t, err, &refused)
				require.EqualValues(t, 2, refused.Count)
				require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
			}
		})
	}
}

func TestRebuildCSharpPartialAttributionBoundary(t *testing.T) {
	canonical := partialTestType("a", "A.cs", "ns")
	fragment := partialTestType("b", "B.cs", "ns")
	later := partialTestType("c", "C.cs", "ns")
	use := &graph.Node{ID: "use", FilePath: "Use.cs", Kind: graph.KindFunction}
	var incoming []*graph.Edge
	for i := 1; i <= 2048; i++ {
		incoming = append(incoming, &graph.Edge{From: use.ID, To: fragment.ID, Kind: graph.EdgeReferences,
			FilePath: use.FilePath, Line: i, Meta: map[string]any{"payload": fmt.Sprint(i)}})
	}
	b := newBuild(t, FullProfile)
	last := *incoming[len(incoming)-1]
	last.To = later.ID
	last.Meta = map[string]any{"payload": "loser"}
	b.AddBatch([]*graph.Node{canonical, fragment, later, use}, append(incoming, &last))
	// The last fragment offers the same destination as move 2048. Its
	// earlier receipt cannot supersede the first fragment's payload.
	receipts := partialTestReceipts(partialTestFacts("csharp", canonical), partialTestFacts("csharp", fragment),
		partialTestFacts("csharp", later), partialTestFacts("csharp", use, append([]*graph.Edge{&last}, incoming...)...))
	require.NoError(t, b.RebuildCSharpPartial(t.Context(), receipts, partialTestRules()))
	for _, e := range incoming {
		want := *e
		want.To = canonical.ID
		got, err := b.pendingEdge(pendingEdgeKey(&want))
		require.NoError(t, err)
		require.Equal(t, &want, got)
		old, err := b.pendingEdge(pendingEdgeKey(e))
		require.NoError(t, err)
		require.Nil(t, old)
	}
	old, err := b.pendingEdge(pendingEdgeKey(&last))
	require.NoError(t, err)
	require.Nil(t, old)
}

func TestRebuildCSharpPartialStaleSourceNotConsulted(t *testing.T) {
	b := newBuild(t, FullProfile)
	owner := partialTestType("owner", "Owner.cs", "ns")
	iface := &graph.Node{ID: "iface", Name: "Iface", FilePath: "Iface.cs", Kind: graph.KindInterface, Language: "csharp"}
	arc := &graph.Edge{From: owner.ID, To: iface.ID, Kind: graph.EdgeImplements, FilePath: owner.FilePath, Line: 2}
	b.AddBatch([]*graph.Node{owner, iface}, nil)
	require.NoError(t, b.putEdgeWithCustody(arc, rebuildEdgeCustody{proof: rebuildProofUntrackedMove}))
	changed := *arc
	changed.Origin = graph.OriginSpeculative
	key := pendingEdgeKey(arc)
	val, err := encodeEdge(&changed)
	require.NoError(t, err)
	require.NoError(t, b.pending.Set(key, val, pebble.NoSync))
	_, err = readRebuildEdgeCustody(b.pending, key)
	require.ErrorIs(t, err, ErrRebuildUnknownProvenance)
	before := rebuildFrameworkCustodyRows(t, b)
	receipts := partialTestReceipts()
	methods := func(map[string]any) []string { return nil }
	infer := func(*graph.Node, string) *graph.Edge { return arc }
	origin := func(string) string { return "" }
	emit := func(*graph.Node, string, string) *graph.Edge { return nil }
	require.NoError(t, b.RebuildImplements(t.Context(), receipts, methods, infer))
	require.NoError(t, b.RebuildOverrides(t.Context(), receipts, origin, emit, infer))
	require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
}
