package resolver

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func TestRebuildValueRefsSQLiteOracle(t *testing.T) {
	const file, name, lang = "value.go", "LIMIT_KEY", "go"
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthValueRefName})
	require.NoError(t, err)
	for _, mode := range []string{"nearest declaration", "shadow", "bound terminal retained", "receipt entrant", "shared raw claim", "joint omissions", "unrelated large local", "large declaration"} {
		t.Run(mode, func(t *testing.T) {
			reader := rebuildEventNode(file, "read", lang, "")
			first := rebuildEventNode(file, "a-constant", lang, "")
			first.Kind, first.Name, first.StartLine = graph.KindConstant, name, 9
			last := rebuildEventNode(file, "z-constant", lang, "")
			last.Kind, last.Name, last.StartLine = graph.KindVariable, name, 9
			shadow := rebuildEventNode(file, "read#limit", lang, "")
			shadow.Kind, shadow.Name = graph.KindLocal, name
			use := &graph.Edge{
				From: reader.ID, To: "unresolved::valueref::" + name, Kind: graph.EdgeReads, FilePath: file, Line: 12,
				Meta: map[string]any{"via": valueRefCandidateVia, "name": name},
			}
			nodes := []*graph.Node{reader, last, first}
			var alternative *graph.Node
			switch mode {
			case "bound terminal retained":
				use.To = last.ID
				use.Meta[metaResolveTerminal] = true
				use.Meta[metaResolveTerminalReason] = "fixture failure"
			case "shadow", "joint omissions":
				nodes = append(nodes, shadow)
			case "receipt entrant":
				alternative = rebuildEventNode(file, "candidate", lang, "")
				alternative.Name, alternative.StartLine = name, 11
				nodes = append(nodes, alternative)
			case "unrelated large local":
				other := rebuildEventNode(file, "other#limit", lang, "")
				other.Kind, other.Name = graph.KindLocal, name
				other.Meta = map[string]any{"large": strings.Repeat("x", 16<<20)}
				nodes = append(nodes, other)
			case "large declaration":
				last.Meta = map[string]any{"large": strings.Repeat("x", 16<<20)}
			}
			facts := rebuildEventFacts(file, lang, nodes, use)
			raw := facts
			switch mode {
			case "receipt entrant":
				changed := *alternative
				changed.Kind = graph.KindConstant
				raw.Nodes = append(nodes[:3:3], &changed)
			case "joint omissions":
				raw.Nodes = []*graph.Node{reader}
			}
			raw.File.NodeCount = len(raw.Nodes)
			b, native := openRebuildEventStores(t, facts)
			if mode == "shared raw claim" {
				mover := *use
				mover.To = "unresolved::valueref::moved"
				require.NoError(t, b.ApplyLinkReindexes(t.Context(), []graph.EdgeReindex{{Edge: &mover, OldTo: use.To}}))
				stationary := *use
				stationary.Meta = nil
				b.AddEdge(&stationary)
			}
			before := rebuildEventSavedRows(t, b)
			frozen, err := FreezeRebuildFrameworkSelection(t.Context(), b.RebuildView(), selection)
			require.NoError(t, err)
			require.True(t, frozen.allows(SynthValueRefName))
			count, err := RunRebuildValueRefs(t.Context(), b, rebuildGRPCReceipts(raw), frozen)
			switch mode {
			case "nearest declaration", "unrelated large local":
				require.NoError(t, err)
				require.EqualValues(t, 1, count)
				require.Equal(t, 1, ResolveValueRefs(native))
				want := rebuildEventSQLiteRows(native)
				require.Len(t, want.edges, 1)
				require.Equal(t, last.ID, want.edges[0].To)
				require.Equal(t, true, want.edges[0].Meta["conditional_def"])
				require.Equal(t, want, rebuildEventSavedRows(t, b))
			case "shadow", "bound terminal retained":
				require.NoError(t, err)
				require.Zero(t, count)
				require.Zero(t, ResolveValueRefs(native))
				require.Equal(t, before, rebuildEventSQLiteRows(native))
				require.Equal(t, before, rebuildEventSavedRows(t, b))
				if mode == "bound terminal retained" {
					require.Len(t, before.edges, 1)
					require.Equal(t, true, before.edges[0].Meta[metaResolveTerminal])
					require.Equal(t, "fixture failure", before.edges[0].Meta[metaResolveTerminalReason])
				}
			default:
				require.ErrorIs(t, err, store_pebble.ErrRebuildValueRefs)
				if mode == "large declaration" {
					require.ErrorIs(t, err, store_pebble.ErrRebuildLinkInputSize)
				} else {
					require.ErrorIs(t, err, store_pebble.ErrRebuildUnknownProvenance)
				}
				require.Zero(t, count)
				require.Equal(t, before, rebuildEventSavedRows(t, b))
				if mode == "joint omissions" {
					require.Zero(t, ResolveValueRefs(native))
					_, omitted := openRebuildEventStores(t, raw)
					require.Zero(t, ResolveValueRefs(omitted))
					one := rebuildEventFacts(file, lang, []*graph.Node{reader, first}, use)
					_, partial := openRebuildEventStores(t, one)
					require.Equal(t, 1, ResolveValueRefs(partial))
				}
				if mode == "receipt entrant" {
					require.Equal(t, 1, ResolveValueRefs(native))
					_, receipts := openRebuildEventStores(t, raw)
					require.Equal(t, 1, ResolveValueRefs(receipts))
					require.Equal(t, last.ID, rebuildEventSQLiteRows(native).edges[0].To)
					require.Equal(t, alternative.ID, rebuildEventSQLiteRows(receipts).edges[0].To)
				}
			}
		})
	}
}

func TestRebuildValueRefsDeclarationElection(t *testing.T) {
	const file, name, lang = "election.go", "LIMIT_KEY", "go"
	reader := rebuildEventNode(file, "read", lang, "")
	first := rebuildEventNode(file, "a-constant", lang, "")
	first.Kind, first.Name, first.StartLine = graph.KindConstant, name, 12
	middle := rebuildEventNode(file, "m-constant", lang, "")
	middle.Kind, middle.Name, middle.StartLine = graph.KindConstant, name, 5
	last := rebuildEventNode(file, "z-constant", lang, "")
	last.Kind, last.Name, last.StartLine = graph.KindVariable, name, 5
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthValueRefName})
	require.NoError(t, err)
	for _, tc := range []struct {
		name         string
		declarations []*graph.Node
		line         int
		want         string
		conditional  bool
	}{
		{"single future declaration", []*graph.Node{first}, 2, first.ID, false},
		{"before all declarations", []*graph.Node{last, first, middle}, 2, middle.ID, true},
		{"same line", []*graph.Node{last, first, middle}, 5, last.ID, true},
		{"between declarations", []*graph.Node{last, first, middle}, 10, last.ID, true},
		{"after all declarations", []*graph.Node{last, first, middle}, 20, first.ID, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			use := &graph.Edge{
				From: reader.ID, To: "unresolved::valueref::" + name, Kind: graph.EdgeReads, FilePath: file, Line: tc.line,
				Meta: map[string]any{"via": valueRefCandidateVia, "name": name},
			}
			// Native and rebuild both scan nodes by ascending ID, not receipt order.
			facts := rebuildEventFacts(file, lang, append([]*graph.Node{reader}, tc.declarations...), use)
			b, native := openRebuildEventStores(t, facts)
			frozen, err := FreezeRebuildFrameworkSelection(t.Context(), b.RebuildView(), selection)
			require.NoError(t, err)
			require.True(t, frozen.allows(SynthValueRefName))
			count, err := RunRebuildValueRefs(t.Context(), b, rebuildGRPCReceipts(facts), frozen)
			require.NoError(t, err)
			require.EqualValues(t, 1, count)
			require.Equal(t, 1, ResolveValueRefs(native))
			want := rebuildEventSQLiteRows(native)
			require.Len(t, want.edges, 1)
			require.Equal(t, tc.want, want.edges[0].To)
			if tc.conditional {
				require.Equal(t, true, want.edges[0].Meta["conditional_def"])
			} else {
				require.NotContains(t, want.edges[0].Meta, "conditional_def")
			}
			require.Equal(t, want, rebuildEventSavedRows(t, b))
			// The installed bound read no longer belongs to this cold turn.
			count, err = RunRebuildValueRefs(t.Context(), b, rebuildGRPCReceipts(facts), frozen)
			require.NoError(t, err)
			require.Zero(t, count)
			require.Zero(t, ResolveValueRefs(native))
			require.Equal(t, want, rebuildEventSavedRows(t, b))
		})
	}
}

func TestRebuildValueRefsNoMoveGuards(t *testing.T) {
	const file, name, lang = "value.go", "LIMIT_KEY", "go"
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthValueRefName})
	require.NoError(t, err)
	for _, mode := range []string{"empty file", "empty name", "wrong via", "wrong kind", "no declaration", "non-distinctive declaration", "generated reader", "self read", "selection disabled"} {
		t.Run(mode, func(t *testing.T) {
			reader := rebuildEventNode(file, "read", lang, "")
			decl := rebuildEventNode(file, name, lang, "")
			decl.Kind, decl.StartLine = graph.KindConstant, 2
			use := &graph.Edge{
				From: reader.ID, To: "unresolved::valueref::" + name, Kind: graph.EdgeReads, FilePath: file, Line: 12,
				Meta: map[string]any{"via": valueRefCandidateVia, "name": name},
			}
			nodes := []*graph.Node{reader, decl}
			selected := selection
			switch mode {
			case "empty file":
				use.FilePath = ""
			case "empty name":
				delete(use.Meta, "name")
			case "wrong via":
				use.Meta["via"] = valueRefVia
			case "wrong kind":
				use.Kind = graph.EdgeReferences
			case "no declaration":
				nodes = nodes[:1]
			case "non-distinctive declaration":
				decl.Name = "lower"
				use.To = "unresolved::valueref::lower"
				use.Meta["name"] = decl.Name
			case "generated reader":
				reader.Meta = map[string]any{"generated": true}
			case "self read":
				use.From = decl.ID
			case "selection disabled":
				selected = FrameworkSynthesizerSelection{configured: true}
			}
			facts := rebuildEventFacts(file, lang, nodes, use)
			b, native := openRebuildEventStores(t, facts)
			before := rebuildEventSavedRows(t, b)
			require.Len(t, before.edges, 1)
			frozen, err := FreezeRebuildFrameworkSelection(t.Context(), b.RebuildView(), selected)
			require.NoError(t, err)
			require.Equal(t, mode != "selection disabled", frozen.allows(SynthValueRefName))
			count, err := RunRebuildValueRefs(t.Context(), b, rebuildGRPCReceipts(facts), frozen)
			require.NoError(t, err)
			require.Zero(t, count)
			require.Equal(t, before, rebuildEventSavedRows(t, b))
			if mode != "selection disabled" {
				require.Zero(t, ResolveValueRefs(native))
				require.Equal(t, before, rebuildEventSQLiteRows(native))
			}
		})
	}
}

func TestRebuildValueRefsReceiptFilters(t *testing.T) {
	const file, name, lang = "value.go", "LIMIT_KEY", "go"
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthValueRefName})
	require.NoError(t, err)
	for _, mode := range []string{"edge language", "edge file", "edge kind", "edge name", "edge via", "node language", "node file", "generated reader omitted", "unknown custody", "first receipt wins"} {
		t.Run(mode, func(t *testing.T) {
			reader := rebuildEventNode(file, "read", lang, "")
			decl := rebuildEventNode(file, name, lang, "")
			decl.Kind, decl.StartLine = graph.KindConstant, 2
			use := &graph.Edge{
				From: reader.ID, To: "unresolved::valueref::" + name, Kind: graph.EdgeReads, FilePath: file, Line: 12,
				Meta: map[string]any{"via": valueRefCandidateVia, "name": name},
			}
			facts := rebuildEventFacts(file, lang, []*graph.Node{reader, decl}, use)
			b, native := openRebuildEventStores(t, facts)
			nodes, edges := facts, facts
			nodes.Edges = nil
			edges.Nodes, edges.File.NodeCount = nil, 0
			switch mode {
			case "edge language":
				edges.EffectiveLanguage = ""
			case "edge file":
				edges.File.FilePath = "wrong.go"
			case "edge kind":
				bad := *use
				bad.Kind = graph.EdgeReferences
				edges.Edges = []*graph.Edge{&bad}
			case "edge name":
				bad := *use
				bad.Meta = map[string]any{"via": valueRefCandidateVia, "name": "OTHER_KEY"}
				edges.Edges = []*graph.Edge{&bad}
			case "edge via":
				bad := *use
				bad.Meta = map[string]any{"via": valueRefVia, "name": name}
				edges.Edges = []*graph.Edge{&bad}
			case "node language":
				nodes.EffectiveLanguage = ""
			case "node file":
				nodes.File.FilePath = "wrong.go"
			case "generated reader omitted":
				generated := *reader
				generated.Meta = map[string]any{"generated": true}
				b.AddNode(&generated)
				nodes.Nodes = []*graph.Node{decl}
				nodes.File.NodeCount = len(nodes.Nodes)
			case "unknown custody":
				moved := *use
				moved.To = "unresolved::valueref::moved"
				// The generic move carries no raw lineage, even if the replay
				// contains a matching identity at the destination.
				b.ReindexEdges([]graph.EdgeReindex{{Edge: &moved, OldTo: use.To}})
				edges.Edges = []*graph.Edge{&moved}
			case "first receipt wins":
				bad := *use
				bad.Meta = map[string]any{"via": valueRefVia, "name": name}
				edges.Edges = []*graph.Edge{use, &bad}
			}
			before := rebuildEventSavedRows(t, b)
			require.Len(t, before.edges, 1)
			frozen, err := FreezeRebuildFrameworkSelection(t.Context(), b.RebuildView(), selection)
			require.NoError(t, err)
			require.True(t, frozen.allows(SynthValueRefName))
			count, err := RunRebuildValueRefs(t.Context(), b, rebuildGRPCReceipts(edges, nodes), frozen)
			if mode == "first receipt wins" {
				require.NoError(t, err)
				require.EqualValues(t, 1, count)
				require.Equal(t, 1, ResolveValueRefs(native))
				require.Equal(t, rebuildEventSQLiteRows(native), rebuildEventSavedRows(t, b))
				return
			}
			require.ErrorIs(t, err, store_pebble.ErrRebuildValueRefs)
			var refusal *store_pebble.RebuildUnknownProvenanceError
			require.ErrorAs(t, err, &refusal)
			require.Equal(t, &store_pebble.RebuildUnknownProvenanceError{Count: 1, First: graph.EdgeIdentityFor(before.edges[0])}, refusal)
			require.Zero(t, count)
			require.Equal(t, before, rebuildEventSavedRows(t, b))
		})
	}
}
