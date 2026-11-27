package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func synthUIKitEdge(g graph.Store, kind graph.EdgeKind, from, to string) *graph.Edge {
	for e := range g.EdgesByKind(kind) {
		if e == nil || e.From != from || e.To != to || e.Meta == nil {
			continue
		}
		if by, _ := e.Meta[MetaSynthesizedBy].(string); by == SynthUIKitResolve {
			return e
		}
	}
	return nil
}

func TestResolveUIKitRefs_CellAndControllerAndDelegate(t *testing.T) {
	g := graph.New()
	const vc = "App/ViewControllers/FeedViewController.swift::FeedViewController"
	g.AddNode(&graph.Node{ID: vc, Kind: graph.KindType, Name: "FeedViewController", FilePath: "App/ViewControllers/FeedViewController.swift", Language: "swift"})

	convNode(g, "App/Cells/PostCell.swift::PostCell", "App/Cells/PostCell.swift", "PostCell")
	convNode(g, "App/ViewControllers/DetailViewController.swift::DetailViewController", "App/ViewControllers/DetailViewController.swift", "DetailViewController")
	// A delegate co-located with the controller (same-dir tier).
	convNode(g, "App/ViewControllers/FeedDelegate.swift::FeedDelegate", "App/ViewControllers/FeedDelegate.swift", "FeedDelegate")

	g.AddEdge(&graph.Edge{From: vc, To: "unresolved::PostCell", Kind: graph.EdgeInstantiates, FilePath: "App/ViewControllers/FeedViewController.swift"})
	g.AddEdge(&graph.Edge{From: vc, To: "unresolved::DetailViewController", Kind: graph.EdgeReferences, FilePath: "App/ViewControllers/FeedViewController.swift"})
	g.AddEdge(&graph.Edge{From: vc, To: "unresolved::FeedDelegate", Kind: graph.EdgeInstantiates, FilePath: "App/ViewControllers/FeedViewController.swift"})

	require.Equal(t, 3, ResolveUIKitRefs(g))
	assert.NotNil(t, synthUIKitEdge(g, graph.EdgeInstantiates, vc, "App/Cells/PostCell.swift::PostCell"),
		"*Cell binds to /Cells/")
	assert.NotNil(t, synthUIKitEdge(g, graph.EdgeReferences, vc, "App/ViewControllers/DetailViewController.swift::DetailViewController"),
		"*ViewController binds to /ViewControllers/")
	assert.NotNil(t, synthUIKitEdge(g, graph.EdgeInstantiates, vc, "App/ViewControllers/FeedDelegate.swift::FeedDelegate"),
		"*Delegate binds via the caller's own directory")
}

func TestResolveUIKitRefs_AmbiguousCellLeftAlone(t *testing.T) {
	g := graph.New()
	const vc = "App/V.swift::V"
	g.AddNode(&graph.Node{ID: vc, Kind: graph.KindType, Name: "V", FilePath: "App/V.swift", Language: "swift"})
	convNode(g, "a/PostCell.swift::PostCell", "a/PostCell.swift", "PostCell")
	convNode(g, "b/PostCell.swift::PostCell", "b/PostCell.swift", "PostCell")
	g.AddEdge(&graph.Edge{From: vc, To: "unresolved::PostCell", Kind: graph.EdgeInstantiates, FilePath: "App/V.swift"})

	require.Equal(t, 0, ResolveUIKitRefs(g))
}

func TestResolveUIKitRefs_NonAppleLeftAlone(t *testing.T) {
	g := graph.New()
	const goFn = "pkg/svc.go::Use"
	g.AddNode(&graph.Node{ID: goFn, Kind: graph.KindFunction, Name: "Use", FilePath: "pkg/svc.go", Language: "go"})
	convNode(g, "App/Cells/PostCell.swift::PostCell", "App/Cells/PostCell.swift", "PostCell")
	g.AddEdge(&graph.Edge{From: goFn, To: "unresolved::PostCell", Kind: graph.EdgeInstantiates, FilePath: "pkg/svc.go"})

	require.Equal(t, 0, ResolveUIKitRefs(g))
}

type uikitReindexOrderStore struct {
	graph.Store
	kinds []graph.EdgeKind
}

func (s *uikitReindexOrderStore) FindEdgesByIdentities(
	identities []graph.EdgeIdentity,
) map[graph.EdgeIdentity]*graph.Edge {
	return s.Store.(graph.EdgeIdentityBatchFinder).FindEdgesByIdentities(identities)
}

func (s *uikitReindexOrderStore) ReindexEdges(reindexes []graph.EdgeReindex) {
	for _, reindex := range reindexes {
		if reindex.Edge != nil {
			s.kinds = append(s.kinds, reindex.Edge.Kind)
		}
	}
	s.Store.ReindexEdges(reindexes)
}

func newUIKitFourKindGraph() *graph.Graph {
	g := graph.New()
	const source = "App/Controllers/Use.swift::Use"
	g.AddNode(&graph.Node{ID: source, Kind: graph.KindFunction, Name: "Use", FilePath: "App/Controllers/Use.swift", Language: "swift"})
	convNode(g, "App/ViewControllers/AlphaViewController.swift::AlphaViewController", "App/ViewControllers/AlphaViewController.swift", "AlphaViewController")
	convNode(g, "App/Cells/BetaCell.swift::BetaCell", "App/Cells/BetaCell.swift", "BetaCell")
	convNode(g, "App/Delegates/GammaDelegate.swift::GammaDelegate", "App/Delegates/GammaDelegate.swift", "GammaDelegate")
	convNode(g, "App/DataSources/DeltaDataSource.swift::DeltaDataSource", "App/DataSources/DeltaDataSource.swift", "DeltaDataSource")
	g.AddBatch(nil, []*graph.Edge{
		{From: source, To: "unresolved::AlphaViewController", Kind: graph.EdgeInstantiates, FilePath: "App/Controllers/Use.swift", Line: 1},
		{From: source, To: "unresolved::BetaCell", Kind: graph.EdgeReferences, FilePath: "App/Controllers/Use.swift", Line: 2},
		{From: source, To: "unresolved::GammaDelegate", Kind: graph.EdgeTypedAs, FilePath: "App/Controllers/Use.swift", Line: 3},
		{From: source, To: "unresolved::DeltaDataSource", Kind: graph.EdgeCalls, FilePath: "App/Controllers/Use.swift", Line: 4},
	})
	return g
}

func uikitSharedCandidates(g graph.Store) (*frameworkStreamCandidates, *frameworkPassCandidates) {
	streams := newFrameworkStreamCandidates(
		g,
		map[string]int{"apple": 1},
		map[string]int{SynthUIKitResolve: 1},
	)
	collectFrameworkEdgeCensus(g, streams)
	return streams, streams.passStreams(g, SynthUIKitResolve)
}

func TestResolveUIKitRefs_SharedCandidatesMatchLegacyAndPreserveKindOrder(t *testing.T) {
	legacy := newUIKitFourKindGraph()
	require.Equal(t, 4, ResolveUIKitRefs(legacy))

	sharedGraph := newUIKitFourKindGraph()
	streams, bundle := uikitSharedCandidates(sharedGraph)
	require.NotNil(t, bundle)
	require.Len(t, bundle.refs, 1)
	require.Len(t, bundle.calls, 1)

	ordered := &uikitReindexOrderStore{Store: sharedGraph}
	require.Equal(t, 4, resolveUIKitRefs(ordered, bundle))
	assert.Equal(t, []graph.EdgeKind{
		graph.EdgeInstantiates,
		graph.EdgeReferences,
		graph.EdgeTypedAs,
		graph.EdgeCalls,
	}, ordered.kinds)

	const source = "App/Controllers/Use.swift::Use"
	for _, tc := range []struct {
		kind   graph.EdgeKind
		target string
	}{
		{graph.EdgeInstantiates, "App/ViewControllers/AlphaViewController.swift::AlphaViewController"},
		{graph.EdgeReferences, "App/Cells/BetaCell.swift::BetaCell"},
		{graph.EdgeTypedAs, "App/Delegates/GammaDelegate.swift::GammaDelegate"},
		{graph.EdgeCalls, "App/DataSources/DeltaDataSource.swift::DeltaDataSource"},
	} {
		assert.NotNil(t, synthUIKitEdge(legacy, tc.kind, source, tc.target))
		assert.NotNil(t, synthUIKitEdge(sharedGraph, tc.kind, source, tc.target))
	}
	streams.releasePass(SynthUIKitResolve, bundle)
}

func TestResolveUIKitRefs_SharedCandidatesDropRetargetedEdge(t *testing.T) {
	g := graph.New()
	const source = "App/Controllers/Use.swift::Use"
	const stableTarget = "App/Existing.swift::Existing"
	g.AddNode(&graph.Node{ID: source, Kind: graph.KindFunction, Name: "Use", FilePath: "App/Controllers/Use.swift", Language: "swift"})
	g.AddNode(&graph.Node{ID: stableTarget, Kind: graph.KindFunction, Name: "Existing", FilePath: "App/Existing.swift", Language: "swift"})
	convNode(g, "App/DataSources/DeltaDataSource.swift::DeltaDataSource", "App/DataSources/DeltaDataSource.swift", "DeltaDataSource")
	call := &graph.Edge{From: source, To: "unresolved::DeltaDataSource", Kind: graph.EdgeCalls, FilePath: "App/Controllers/Use.swift", Line: 7}
	g.AddEdge(call)

	streams := newFrameworkStreamCandidates(
		g,
		map[string]int{"apple": 1},
		map[string]int{SynthUIKitResolve: 1},
	)
	collectFrameworkEdgeCensus(g, streams)
	require.Len(t, streams.perPass[SynthUIKitResolve].calls, 1, "census must retain the pre-pass identity")

	live := *call
	live.To = stableTarget
	g.ReindexEdges([]graph.EdgeReindex{{Edge: &live, OldTo: call.To}})
	bundle := streams.passStreams(g, SynthUIKitResolve)
	require.NotNil(t, bundle)
	require.Empty(t, bundle.calls, "exact refetch must drop the stale unresolved identity")
	require.Equal(t, 0, resolveUIKitRefs(g, bundle))
	assert.Nil(t, synthUIKitEdge(g, graph.EdgeCalls, source, "App/DataSources/DeltaDataSource.swift::DeltaDataSource"))
	streams.releasePass(SynthUIKitResolve, bundle)
}

func TestResolveUIKitRefs_SeesSwiftUIResiduals(t *testing.T) {
	g := graph.New()
	const (
		sourceFile = "App/Sources/Use.swift"
		source     = sourceFile + "::Use"
		modelFile  = "App/Models/ModelViewController.swift"
		model      = modelFile + "::ModelViewController"
		vcFile     = "App/ViewControllers/ScreenViewController.swift"
		vc         = vcFile + "::ScreenViewController"
	)
	g.AddNode(&graph.Node{ID: source, Kind: graph.KindFunction, Name: "Use", FilePath: sourceFile, Language: "swift"})
	convNode(g, model, modelFile, "ModelViewController")
	convNode(g, vc, vcFile, "ScreenViewController")
	g.AddBatch(nil, []*graph.Edge{
		{From: source, To: "unresolved::ModelViewController", Kind: graph.EdgeReferences, FilePath: sourceFile, Line: 1},
		{From: source, To: "unresolved::ScreenViewController", Kind: graph.EdgeCalls, FilePath: sourceFile, Line: 2},
	})
	streams := newFrameworkStreamCandidates(g, map[string]int{"apple": 1}, map[string]int{SynthUIKitResolve: 1})
	collectFrameworkEdgeCensus(g, streams)
	require.Equal(t, 1, ResolveSwiftUIRefs(g))
	bundle := streams.passStreams(g, SynthUIKitResolve)
	require.NotNil(t, bundle)
	defer streams.releasePass(SynthUIKitResolve, bundle)
	require.Empty(t, bundle.refs, "SwiftUI consumed the frozen Reference")
	require.Len(t, bundle.calls, 1)
	require.Equal(t, 1, resolveUIKitRefs(g, bundle))
	assert.NotNil(t, synthUIKitEdge(g, graph.EdgeCalls, source, vc))
	assert.Nil(t, synthUIKitEdge(g, graph.EdgeReferences, source, model))
	refs := g.GetOutEdges(source)
	require.Len(t, refs, 2)
	for _, e := range refs {
		if e.Kind == graph.EdgeReferences {
			require.Equal(t, model, e.To)
			require.Equal(t, SynthSwiftUIResolve, e.Meta[MetaSynthesizedBy])
		}
	}
}
