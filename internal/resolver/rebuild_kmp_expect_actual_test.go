package resolver

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/parser/languages"
)

func TestRebuildKMPExpectActualSQLiteOracle(t *testing.T) {
	// Real Kotlin extraction supplies both portable role markers. The other
	// languages deliberately carry synthetic markers: native matching is by
	// name and kind, not language, package, or a single elected declaration.
	common, expect := rebuildKMPExtract(t, "common.kt", "package common\nexpect fun platformName(): String\n", "expect")
	android, actual := rebuildKMPExtract(t, "android.kt", "package android\nactual fun platformName(): String {\n    return \"android\"\n}\n", "actual")
	goExpect := rebuildKMPNode("other.go", "go", "platformName", "expect", graph.KindFunction, 3)
	tsActual := rebuildKMPNode("other.ts", "typescript", "platformName", "actual", graph.KindFunction, 5)
	goMethod := rebuildKMPNode("other.go", "go", "update", "expect", graph.KindMethod, 7)
	tsMethod := rebuildKMPNode("other.ts", "typescript", "update", "actual", graph.KindMethod, 11)
	goType := rebuildKMPNode("other.go", "go", "Clock", "expect", graph.KindType, 13)
	tsType := rebuildKMPNode("other.ts", "typescript", "Clock", "actual", graph.KindType, 17)
	goInterface := rebuildKMPNode("other.go", "go", "Reader", "expect", graph.KindInterface, 19)
	tsInterface := rebuildKMPNode("other.ts", "typescript", "Reader", "actual", graph.KindInterface, 23)
	unmarked := rebuildKMPNode("negative.js", "javascript", "platformName", "", graph.KindFunction, 2)
	unmarked.Meta = nil
	badRole := rebuildKMPNode("negative.js", "javascript", "update", "Actual", graph.KindMethod, 4)
	badRoleType := rebuildKMPNode("negative.js", "javascript", "Clock", "", graph.KindType, 6)
	badRoleType.Meta["kmp_role"] = true
	// Both sides of unsupported/empty keys exist so accidentally admitting
	// either produces a row. A named opposite role of the wrong kind also exists.
	wrongKind := rebuildKMPNode("negative.js", "javascript", "OnlyType", "actual", graph.KindFunction, 8)
	stationary := &graph.Edge{
		From: tsActual.ID, To: goExpect.ID, Kind: graph.EdgeImplements, FilePath: tsActual.FilePath,
		Line: tsActual.StartLine, Confidence: 0.9, Origin: graph.OriginASTResolved,
		Meta: map[string]any{"fixture": "durable incumbent"},
	}
	facts := []store_pebble.ParseFacts{
		common, android,
		rebuildEventFacts("other.go", "go", []*graph.Node{
			goExpect, goMethod, goType, goInterface,
			rebuildKMPNode("other.go", "go", "OnlyType", "expect", graph.KindType, 25),
			rebuildKMPNode("other.go", "go", "value", "expect", graph.KindVariable, 27),
			rebuildKMPNode("other.go", "go", "", "expect", graph.KindFunction, 29),
		}),
		rebuildEventFacts("other.ts", "typescript", []*graph.Node{
			tsActual, tsMethod, tsType, tsInterface,
			rebuildKMPNode("other.ts", "typescript", "value", "actual", graph.KindVariable, 25),
			rebuildKMPNode("other.ts", "typescript", "", "actual", graph.KindFunction, 27),
		}, stationary),
		rebuildEventFacts("negative.js", "javascript", []*graph.Node{unmarked, badRole, badRoleType, wrongKind}),
	}
	for i := range facts {
		facts[i] = roundTripRebuildRNBridgeFacts(t, facts[i])
	}
	native := openRebuildTemporalOracle(t, facts...)
	ordered := slices.Clone(facts)
	slices.Reverse(ordered)
	private := openRebuildGRPCBuild(t, ordered...)
	before := rebuildEventSQLiteRows(native)
	require.Equal(t, before, rebuildEventSavedRows(t, private))
	require.Contains(t, before.edges, stationary)

	disabled, err := NewFrameworkSynthesizerSelection(nil)
	require.NoError(t, err)
	report := RunFrameworkSynthesizersWithSelection(native, disabled)
	require.NoError(t, report.Err)
	require.Zero(t, report.Total)
	stats, err := RunRebuildKMPExpectActual(t.Context(), private, rebuildGRPCReceipts(facts...), disabled, 1)
	require.NoError(t, err)
	require.Zero(t, stats)
	require.Equal(t, before, rebuildEventSQLiteRows(native))
	require.Equal(t, before, rebuildEventSavedRows(t, private))

	want := rebuildEventRows{nodes: before.nodes, edges: slices.Clone(before.edges)}
	// Seven pairs yield fourteen directions, with one already saved at its key.
	for _, pair := range [][2]*graph.Node{
		{expect, actual}, {expect, tsActual}, {goExpect, actual}, {goExpect, tsActual},
		{goMethod, tsMethod}, {goType, tsType}, {goInterface, tsInterface},
	} {
		if pair[0] != goExpect || pair[1] != tsActual {
			want.edges = append(want.edges, rebuildKMPExpectedEdge(pair[1], pair[0], graph.EdgeImplements))
		}
		want.edges = append(want.edges, rebuildKMPExpectedEdge(pair[0], pair[1], graph.EdgeReferences))
	}
	want.sort()
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthKMPExpectActual})
	require.NoError(t, err)
	for _, wantNew := range []uint64{13, 0} {
		report := RunFrameworkSynthesizersWithSelection(native, selection)
		require.NoError(t, report.Err)
		require.Equal(t, 7, report.Total) // Native counts pairs, even on replay.
		require.Equal(t, want, rebuildEventSQLiteRows(native))
		stats, err := RunRebuildKMPExpectActual(t.Context(), private, rebuildGRPCReceipts(facts...), selection, 1)
		require.NoError(t, err)
		require.Equal(t, wantNew, stats.Edges)
		if wantNew != 0 {
			require.Positive(t, stats.SpillBytes)
		}
		require.Equal(t, want, rebuildEventSavedRows(t, private))
	}
}

func TestRebuildKMPExpectActualRejectsUnprovedDeclaration(t *testing.T) {
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthKMPExpectActual})
	require.NoError(t, err)
	// An independent proved pair sorts before the missing proof. A durable
	// implements row for the later pair must survive; only its new reverse
	// direction needs proof. No part of either pair may be newly installed.
	firstExpect := rebuildKMPNode("a-common.kt", "kotlin", "Alpha", "expect", graph.KindFunction, 3)
	firstActual := rebuildKMPNode("a-platform.kt", "kotlin", "Alpha", "actual", graph.KindFunction, 7)
	expect := rebuildKMPNode("z-common.kt", "kotlin", "Zulu", "expect", graph.KindFunction, 11)
	actual := rebuildKMPNode("z-platform.kt", "kotlin", "Zulu", "actual", graph.KindFunction, 17)
	stationary := rebuildKMPExpectedEdge(actual, expect, graph.EdgeImplements)
	facts := []store_pebble.ParseFacts{
		rebuildEventFacts(firstExpect.FilePath, firstExpect.Language, []*graph.Node{firstExpect}),
		rebuildEventFacts(firstActual.FilePath, firstActual.Language, []*graph.Node{firstActual}),
		rebuildEventFacts(expect.FilePath, expect.Language, []*graph.Node{expect}),
		rebuildEventFacts(actual.FilePath, actual.Language, []*graph.Node{actual}, stationary),
	}
	for i := range facts {
		facts[i] = roundTripRebuildRNBridgeFacts(t, facts[i])
	}
	private := openRebuildGRPCBuild(t, facts...)
	native := openRebuildTemporalOracle(t, facts...)
	before := rebuildEventSavedRows(t, private)
	require.Equal(t, rebuildEventSQLiteRows(native), before)
	require.Equal(t, []*graph.Edge{stationary}, before.edges)
	report := RunFrameworkSynthesizersWithSelection(native, selection)
	require.NoError(t, report.Err)
	require.Equal(t, 2, report.Total)
	want := rebuildEventSQLiteRows(native)
	require.Len(t, want.edges, 4)
	missing := rebuildKMPExpectedEdge(expect, actual, graph.EdgeReferences)
	require.Contains(t, want.edges, missing)

	for _, mode := range []string{
		"missing receipt", "missing declaration", "missing role", "changed role", "changed role type",
		"changed receipt language", "changed receipt file", "changed declaration language", "changed declaration kind",
		"changed declaration name", "changed declaration line",
	} {
		t.Run(mode, func(t *testing.T) {
			proof := slices.Clone(facts)
			f := &proof[3]
			*f = roundTripRebuildRNBridgeFacts(t, *f)
			n := f.Nodes[0]
			switch mode {
			case "missing receipt":
				proof = proof[:3]
			case "missing declaration":
				f.Nodes = nil
				f.File.NodeCount = 0
			case "missing role":
				delete(n.Meta, "kmp_role")
			case "changed role":
				n.Meta["kmp_role"] = "expect"
			case "changed role type":
				n.Meta["kmp_role"] = true
			case "changed receipt language":
				f.EffectiveLanguage = "java"
			case "changed receipt file":
				f.File.FilePath = "other/z-platform.kt"
			case "changed declaration language":
				n.Language = "java"
				f.EffectiveLanguage = n.Language
			case "changed declaration kind":
				n.Kind = graph.KindMethod
			case "changed declaration name":
				n.Name = "Other"
			case "changed declaration line":
				n.StartLine++
			}
			if mode != "missing receipt" {
				*f = roundTripRebuildRNBridgeFacts(t, *f)
			}
			stats, err := RunRebuildKMPExpectActual(t.Context(), private, rebuildGRPCReceipts(proof...), selection, 1)
			require.ErrorIs(t, err, store_pebble.ErrRebuildKMPExpectActual)
			require.ErrorIs(t, err, store_pebble.ErrRebuildUnknownProvenance)
			var refused *store_pebble.RebuildUnknownProvenanceError
			require.ErrorAs(t, err, &refused)
			require.Equal(t, uint64(1), refused.Count)
			require.Equal(t, graph.EdgeIdentityFor(missing), refused.First)
			require.Zero(t, stats)
			require.Equal(t, before, rebuildEventSavedRows(t, private))
		})
	}
	// Replaying with intact receipts on the same root also proves the refusal
	// path did not leave scratch state that blocks a subsequent valid run.
	stats, err := RunRebuildKMPExpectActual(t.Context(), private, rebuildGRPCReceipts(facts...), selection, 1)
	require.NoError(t, err)
	require.Equal(t, uint64(3), stats.Edges)
	require.Positive(t, stats.SpillBytes)
	require.Equal(t, want, rebuildEventSavedRows(t, private))
}

func TestRebuildKMPExpectActualCountsDistinctUnprovedOutputs(t *testing.T) {
	alphaExpect := rebuildKMPNode("y-common.kt", "kotlin", "Alpha", "expect", graph.KindFunction, 3)
	alphaActual := rebuildKMPNode("z-platform.kt", "kotlin", "Alpha", "actual", graph.KindFunction, 7)
	zuluExpect := rebuildKMPNode("a-common.kt", "kotlin", "Zulu", "expect", graph.KindFunction, 11)
	zuluActual := rebuildKMPNode("b-platform.kt", "kotlin", "Zulu", "actual", graph.KindFunction, 17)
	facts := []store_pebble.ParseFacts{
		rebuildEventFacts(alphaExpect.FilePath, alphaExpect.Language, []*graph.Node{alphaExpect}),
		rebuildEventFacts(alphaActual.FilePath, alphaActual.Language, []*graph.Node{alphaActual}),
		rebuildEventFacts(zuluExpect.FilePath, zuluExpect.Language, []*graph.Node{zuluExpect}),
		rebuildEventFacts(zuluActual.FilePath, zuluActual.Language, []*graph.Node{zuluActual}),
	}
	private := openRebuildGRPCBuild(t, facts...)
	before := rebuildEventSavedRows(t, private)
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthKMPExpectActual})
	require.NoError(t, err)
	conflict := *zuluActual
	conflict.StartLine++
	bad := rebuildEventFacts(conflict.FilePath, conflict.Language, []*graph.Node{&conflict})
	// Neither Alpha endpoint is proved. A valid and a conflicting Zulu
	// receipt must refuse both directions, regardless of receipt order.
	// Zulu's reverse row is visited last but its source ID sorts first.
	first := rebuildKMPExpectedEdge(zuluExpect, zuluActual, graph.EdgeReferences)
	for _, proof := range [][]store_pebble.ParseFacts{
		{facts[2], facts[3], bad},
		{bad, facts[3], facts[2]},
	} {
		stats, err := RunRebuildKMPExpectActual(t.Context(), private, rebuildGRPCReceipts(proof...), selection, 1)
		require.ErrorIs(t, err, store_pebble.ErrRebuildKMPExpectActual)
		var refused *store_pebble.RebuildUnknownProvenanceError
		require.ErrorAs(t, err, &refused)
		require.Equal(t, &store_pebble.RebuildUnknownProvenanceError{Count: 4, First: graph.EdgeIdentityFor(first)}, refused)
		require.Zero(t, stats)
		require.Equal(t, before, rebuildEventSavedRows(t, private))
	}
	stats, err := RunRebuildKMPExpectActual(t.Context(), private, rebuildGRPCReceipts(facts...), selection, 1)
	require.NoError(t, err)
	require.EqualValues(t, 4, stats.Edges)
}

func TestRebuildKMPExpectActualNeedsProvedKotlinMarker(t *testing.T) {
	expect := rebuildKMPNode("common.go", "go", "clock", "expect", graph.KindFunction, 3)
	actual := rebuildKMPNode("platform.ts", "typescript", "clock", "actual", graph.KindFunction, 7)
	marker := rebuildKMPNode("support.kt", "kotlin", "Support", "", graph.KindType, 1)
	facts := []store_pebble.ParseFacts{
		rebuildEventFacts(expect.FilePath, expect.Language, []*graph.Node{expect}),
		rebuildEventFacts(actual.FilePath, actual.Language, []*graph.Node{actual}),
		rebuildEventFacts(marker.FilePath, marker.Language, []*graph.Node{marker}),
	}
	for i := range facts {
		facts[i] = roundTripRebuildRNBridgeFacts(t, facts[i])
	}
	private := openRebuildGRPCBuild(t, facts...)
	native := openRebuildTemporalOracle(t, facts...)
	before := rebuildEventSavedRows(t, private)
	require.Equal(t, rebuildEventSQLiteRows(native), before)
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthKMPExpectActual})
	require.NoError(t, err)
	frozen, err := FreezeRebuildFrameworkSelection(t.Context(), private.RebuildView(), selection)
	require.NoError(t, err)
	require.True(t, frozen.allows(SynthKMPExpectActual))

	decoy := *marker
	decoy.Language = "java"
	claim := rebuildEventFacts(decoy.FilePath, decoy.Language, []*graph.Node{&decoy})
	claim = roundTripRebuildRNBridgeFacts(t, claim)
	for _, proof := range [][]store_pebble.ParseFacts{
		facts[:2],
		{facts[0], facts[1], facts[2], claim},
		{claim, facts[2], facts[0], facts[1]},
	} {
		stats, err := RunRebuildKMPExpectActual(t.Context(), private, rebuildGRPCReceipts(proof...), frozen, 1)
		require.ErrorIs(t, err, store_pebble.ErrRebuildUnknownProvenance)
		var refused *store_pebble.RebuildUnknownProvenanceError
		require.ErrorAs(t, err, &refused)
		require.EqualValues(t, 2, refused.Count)
		first := rebuildKMPExpectedEdge(expect, actual, graph.EdgeReferences)
		require.Equal(t, graph.EdgeIdentityFor(first), refused.First)
		require.Zero(t, stats)
		require.Equal(t, before, rebuildEventSavedRows(t, private))
	}

	report := RunFrameworkSynthesizersWithSelection(native, selection)
	require.NoError(t, report.Err)
	require.Equal(t, 1, report.Total)
	stats, err := RunRebuildKMPExpectActual(t.Context(), private, rebuildGRPCReceipts(facts...), frozen, 1)
	require.NoError(t, err)
	require.EqualValues(t, 2, stats.Edges)
	want := rebuildEventSQLiteRows(native)
	require.Equal(t, want, rebuildEventSavedRows(t, private))

	// With both directions installed, missing marker proof cannot affect output.
	stats, err = RunRebuildKMPExpectActual(t.Context(), private, rebuildGRPCReceipts(facts[:2]...), frozen, 1)
	require.NoError(t, err)
	require.Zero(t, stats.Edges)
	require.Equal(t, want, rebuildEventSavedRows(t, private))
}

func TestRebuildKMPExpectActualSQLiteSpillAtMiB(t *testing.T) {
	const pairs = 12
	expect := rebuildKMPNode("common.kt", "kotlin", "Clock", "expect", graph.KindType, 2)
	facts := []store_pebble.ParseFacts{rebuildEventFacts(expect.FilePath, expect.Language, []*graph.Node{expect})}
	for i := range pairs {
		actual := rebuildKMPNode(fmt.Sprintf("platform%d.kt", i), "kotlin", "Clock", "actual", graph.KindType, 2)
		// Distinct IDs in both the proposal keys and values cross 1 MiB.
		actual.ID += strings.Repeat("x", 32<<10)
		facts = append(facts, rebuildEventFacts(actual.FilePath, actual.Language, []*graph.Node{actual}))
	}
	private := openRebuildGRPCBuild(t, facts...)
	native := openRebuildTemporalOracle(t, facts...)
	before := rebuildEventSavedRows(t, private)
	require.Equal(t, before, rebuildEventSQLiteRows(native))
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthKMPExpectActual})
	require.NoError(t, err)
	report := RunFrameworkSynthesizersWithSelection(native, selection)
	require.NoError(t, report.Err)
	require.Equal(t, pairs, report.Total)
	stats, err := RunRebuildKMPExpectActual(t.Context(), private, rebuildGRPCReceipts(facts...), selection, 1<<20)
	require.NoError(t, err)
	require.EqualValues(t, 2*pairs, stats.Edges)
	require.Positive(t, stats.SpillBytes)
	require.Equal(t, rebuildEventSQLiteRows(native), rebuildEventSavedRows(t, private))
}

func rebuildKMPExtract(t *testing.T, file, source, role string) (store_pebble.ParseFacts, *graph.Node) {
	t.Helper()
	parsed, err := languages.NewKotlinExtractor().Extract(file, []byte(source))
	require.NoError(t, err)
	t.Cleanup(parsed.ReleaseTree)
	var declaration *graph.Node
	for _, n := range parsed.Nodes {
		if n.Name == "platformName" && n.Kind == graph.KindFunction {
			require.Nil(t, declaration)
			declaration = n
		}
	}
	require.NotNil(t, declaration)
	require.Equal(t, role, declaration.Meta["kmp_role"])
	return rebuildEventFacts(file, "kotlin", parsed.Nodes, parsed.Edges...), declaration
}

func rebuildKMPNode(file, language, name, role string, kind graph.NodeKind, line int) *graph.Node {
	return &graph.Node{
		ID: file + "::" + name, Name: name, Kind: kind, Language: language, FilePath: file,
		StartLine: line, EndLine: line + 1, Meta: map[string]any{"kmp_role": role},
	}
}

func rebuildKMPExpectedEdge(from, to *graph.Node, kind graph.EdgeKind) *graph.Edge {
	return &graph.Edge{
		From:            from.ID,
		To:              to.ID,
		Kind:            kind,
		FilePath:        from.FilePath,
		Line:            from.StartLine,
		Confidence:      0.8,
		ConfidenceLabel: graph.ConfidenceLabelFor(kind, 0.8),
		Origin:          graph.OriginASTInferred,
		Meta: map[string]any{
			"via":             "kmp.expect-actual",
			MetaSynthesizedBy: SynthKMPExpectActual,
			MetaProvenance:    ProvenanceHeuristic,
		},
	}
}
