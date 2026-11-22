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

func TestRebuildOverridesCheckedImplementationParent(t *testing.T) {
	facts, _ := rebuildOverrideOrderFixture()
	base := *facts.Nodes[0]
	base.Kind, base.Meta = graph.KindInterface, map[string]any{"methods": []string{"Run"}}
	facts.Nodes[0], facts.Edges = &base, facts.Edges[1:]
	build, err := store_pebble.OpenBuild(filepath.Join(t.TempDir(), "working"), store_pebble.FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, build.Close()) })
	require.NoError(t, build.AddParsedFile("", facts))
	receipts := func(yield func(store_pebble.ParseFacts, error) bool) { yield(facts, nil) }
	require.NoError(t, resolver.RunRebuildImplements(t.Context(), build, receipts))
	require.NoError(t, resolver.RunRebuildOverrides(t.Context(), build, receipts))
	want := slices.Concat(facts.Edges, []*graph.Edge{
		{From: overrideFixtureChild, To: overrideFixtureBase, Kind: graph.EdgeImplements, FilePath: overrideFixturePath,
			Line: 6, Meta: map[string]any{"via": "method-set-inference"}},
		{From: overrideFixtureChild + ".Run", To: overrideFixtureBase + ".Run", Kind: graph.EdgeOverrides, FilePath: overrideFixturePath,
			Line: 7, Confidence: 1, ConfidenceLabel: "EXTRACTED", Origin: graph.OriginASTInferred},
	})
	require.Equal(t, rebuildOverrideRowsByKey(t, want), readRebuildOverrideProofRows(t, build))
}

func TestRebuildOverridesRejectsUnprovedParent(t *testing.T) {
	facts, parent := rebuildOverrideOrderFixture()
	// A supported recording path does not prove who emitted the parent row.
	plugin := store_pebble.ParseFacts{EffectiveLanguage: "custom", File: graph.FileMetaRow{FilePath: "probe.custom"},
		Edges: []*graph.Edge{facts.Edges[0]}}
	facts.Edges = facts.Edges[1:]
	build, err := store_pebble.OpenBuild(filepath.Join(t.TempDir(), "working"), store_pebble.FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, build.Close()) })
	require.NoError(t, build.AddParsedFile("", plugin))
	require.NoError(t, build.AddParsedFile("", facts))
	require.NoError(t, build.ApplyLinkReindexes(t.Context(), []graph.EdgeReindex{{Edge: parent, OldTo: plugin.Edges[0].To}}))
	want := slices.Concat(facts.Edges, []*graph.Edge{parent})
	err = resolver.RunRebuildOverrides(t.Context(), build, func(yield func(store_pebble.ParseFacts, error) bool) {
		if yield(plugin, nil) {
			yield(facts, nil)
		}
	})
	var refusal *store_pebble.RebuildUnknownProvenanceError
	require.ErrorAs(t, err, &refusal)
	require.Equal(t, uint64(1), refusal.Count)
	require.Equal(t, graph.EdgeIdentity{From: overrideFixtureChild + ".Run", To: overrideFixtureBase + ".Run",
		Kind: graph.EdgeOverrides, FilePath: overrideFixturePath, Line: 7}, refusal.First)
	require.Equal(t, rebuildOverrideRowsByKey(t, want), readRebuildOverrideProofRows(t, build))
}

func readRebuildOverrideProofRows(t *testing.T, build *store_pebble.BuildStore) map[graph.EdgeIdentity]*graph.Edge {
	t.Helper()
	var edges []*graph.Edge
	require.NoError(t, build.RebuildView().VisitEdges(t.Context(), func(e *graph.Edge) error {
		edges = append(edges, e)
		return nil
	}))
	return rebuildOverrideRowsByKey(t, edges)
}

func TestRebuildOverridesRejectsDeferredWinningMember(t *testing.T) {
	facts, parent := rebuildOverrideOrderFixture()
	method := *facts.Nodes[3]
	method.ID += "_deferred"
	method.FilePath, method.Language = "probe.custom", "custom"
	membership := &graph.Edge{From: method.ID, To: overrideFixtureChild, Kind: graph.EdgeMemberOf,
		FilePath: method.FilePath, Line: 9}
	plugin := store_pebble.ParseFacts{EffectiveLanguage: method.Language,
		File:  graph.FileMetaRow{FilePath: method.FilePath, NodeCount: 1},
		Nodes: []*graph.Node{&method}, Edges: []*graph.Edge{membership}}
	build, err := store_pebble.OpenBuild(filepath.Join(t.TempDir(), "working"), store_pebble.FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, build.Close()) })
	require.NoError(t, build.AddParsedFile("", plugin))
	require.NoError(t, build.AddParsedFile("", facts))
	require.NoError(t, build.ApplyLinkReindexes(t.Context(), []graph.EdgeReindex{{Edge: parent, OldTo: facts.Edges[0].To}}))
	err = resolver.RunRebuildOverrides(t.Context(), build, func(yield func(store_pebble.ParseFacts, error) bool) {
		if yield(plugin, nil) {
			yield(facts, nil)
		}
	})
	var refusal *store_pebble.RebuildUnknownProvenanceError
	require.ErrorAs(t, err, &refusal)
	require.Equal(t, uint64(1), refusal.Count)
	require.Equal(t, method.ID, refusal.First.From)
	want := slices.Concat(facts.Edges[1:], plugin.Edges, []*graph.Edge{parent})
	require.Equal(t, rebuildOverrideRowsByKey(t, want), readRebuildOverrideProofRows(t, build))
}
