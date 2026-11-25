package resolver

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser/languages"
)

// Given ordinary interface calls with competing in-repo Name methods, the
// main page must use direct imports (not transitive closure), retain the raw
// candidates if none is reachable, and leave the exact receiver tiers unfiltered.
func TestRebuildMainMemberDirectReachability(t *testing.T) {
	const (
		local    = "repo/drivers/local.go"
		settled  = "repo/clients/settled.go"
		raw      = "repo/clients/raw.go"
		empty    = "repo/clients/empty.go"
		imported = "repo/zzdep/impl.go"
		remote   = "repo/aaother/impl.go"
		depSpec  = "example.test/zzdep"
	)
	files := []struct{ path, source string }{
		{remote, `package aaother
type Remote struct{}
func (Remote) Name() string { return "remote" }
func (Remote) Only() string { return "only" }
`},
		{imported, `package zzdep
import _ "example.test/aaother"
type Imported struct{}
func (Imported) Name() string { return "imported" }
`},
		{local, `package drivers
import _ "example.test/zzdep"
type adapter interface { Name() string }
type ALocal struct{}
type BLocal struct{}
func (ALocal) Name() string { return "a" }
func (BLocal) Name() string { return "b" }
func Local(as []adapter) string { for _, a := range as { return a.Name() }; return "" }
`},
		{"repo/clients/types.go", `package clients
import "example.test/aaother"
type Remote = aaother.Remote
type adapter interface { Name() string }
type onlyAdapter interface { Only() string }
`},
		{settled, `package clients
import _ "example.test/zzdep"
func Settled(as []adapter) string { for _, a := range as { return a.Name() }; return "" }
func Exact(a Remote) string { return a.Name() }
func Deferred(as []adapter) string { for _, a := range as { return a.Name() }; return "" }
`},
		{raw, `package clients
import _ "example.test/zzdep"
func Raw(as []adapter) string { for _, a := range as { return a.Name() }; return "" }
`},
		{empty, `package clients
import _ "fmt"
func Empty(as []adapter) string { for _, a := range as { return a.Name() }; return "" }
func EmptyOnly(as []onlyAdapter) string { for _, a := range as { return a.Only() }; return "" }
`},
	}
	// Extract once. Keep all definition nodes but project only imports and
	// the raw wildcard calls: this test stops at main's page, before any tails.
	var nodes []*graph.Node
	var edges []*graph.Edge
	for _, file := range files {
		facts, err := languages.NewGoExtractor().Extract(file.path, []byte(file.source))
		require.NoError(t, err)
		t.Cleanup(facts.ReleaseTree)
		for _, n := range facts.Nodes {
			n.RepoPrefix = "repo"
			nodes = append(nodes, n)
		}
		for _, e := range facts.Edges {
			switch {
			case e.Kind == graph.EdgeImports:
				if file.path == settled || file.path == raw {
					require.Equal(t, "unresolved::import::"+depSpec, e.To)
					// Also exercise imports emitted by a non-file source node
					// in the caller file, not just by the file or its caller.
					e.From = file.path + "::import::" + depSpec
					if file.path == settled {
						e.To = imported
					}
				}
				edges = append(edges, e)
			case e.Kind == graph.EdgeCalls && (e.To == "unresolved::*.Name" || e.To == "unresolved::*.Only"):
				setEdgeTerminal(e, terminalReasonNoDefinition)
				edges = append(edges, e)
			}
		}
	}
	const importedName, remoteName = imported + "::Imported.Name", remote + "::Remote.Name"
	want := map[string]struct {
		to, receiver string
	}{
		local + "::Local":     {"unresolved::*.Name", ""},
		settled + "::Settled": {importedName, ""},
		raw + "::Raw":         {importedName, ""},
		empty + "::Empty":     {"unresolved::*.Name", ""},
		empty + "::EmptyOnly": {remote + "::Remote.Only", ""},
		settled + "::Exact":   {remoteName, "Remote"},
	}
	var selected, pending []*graph.Edge
	for _, edge := range edges {
		if graph.IsUnresolvedTarget(edge.To) {
			pending = append(pending, edge)
		}
		if tc, ok := want[edge.From]; ok && edge.Kind == graph.EdgeCalls {
			require.True(t, graph.IsUnresolvedTarget(edge.To))
			require.Equal(t, tc.receiver, edgeReceiverType(edge))
			selected = append(selected, edge)
		}
	}
	require.Len(t, selected, len(want))
	require.Len(t, edges, 13) // 6 imports, 7 raw calls (including Deferred).
	rebuildFnPointerSortRows(pending)
	rebuildFnPointerSortRows(selected)

	// Independent native Graph resolver. Its page reachability reads every
	// source node in the caller file. No post-main guard or live source access.
	g := graph.New()
	for _, n := range nodes {
		copy := *n
		copy.Meta = cloneFrameworkMeta(n.Meta)
		g.AddNode(&copy)
	}
	for _, e := range edges {
		g.AddEdge(cloneFrameworkEdge(e))
	}
	native := New(g)
	native.bulkMode = true
	require.NoError(t, native.warmLookupCache(selected))
	defer native.clearLookupCache()
	require.True(t, native.buildReachabilityIndexForPending(selected, native.nodeByID))
	defer native.clearReachabilityIndex()
	rawCandidates := native.cachedFindNodesByNameInRepoForEdge("Name", "repo", selected[0])
	require.Len(t, rawCandidates, 4)
	for _, n := range rawCandidates {
		require.Equal(t, graph.KindMethod, n.Kind)
		require.NotEqual(t, "adapter", nodeReceiverType(n), "the interface calls must not use an exact receiver tier")
	}
	var nativeMoves []graph.EdgeReindex
	for _, original := range selected {
		e := cloneFrameworkEdge(original)
		oldTo, changed := native.resolveEdge(e, &ResolveStats{})
		tc := want[e.From]
		require.Equal(t, tc.to, e.To, e.From)
		require.Empty(t, e.Meta["dispatch"], e.From)
		if tc.to == original.To {
			require.False(t, changed, e.From)
			continue
		}
		require.True(t, changed, e.From)
		clearEdgeTerminal(e)
		if tc.receiver == "Remote" {
			require.Equal(t, graph.OriginASTResolved, e.Origin)
			require.Equal(t, 0.85, e.Confidence)
		} else {
			require.Equal(t, graph.OriginASTInferred, e.Origin)
			require.Equal(t, 0.7, e.Confidence)
		}
		nativeMoves = append(nativeMoves, graph.EdgeReindex{Edge: e, OldTo: oldTo, OldKind: original.Kind})
	}
	g.ReindexEdges(nativeMoves)
	nativeRows := g.AllEdges()
	rebuildFnPointerSortRows(nativeRows)

	t.Run("selected page", func(t *testing.T) {
		ctx, build, opts := frontierFixture(t, 2, 0)
		opts.SpillBytes = 1
		build.AddBatch(nodes, edges)
		before := rebuildFnPointerSaved(t, build)
		require.ElementsMatch(t, edges, before)
		require.NoError(t, runRebuildLinkFrontier(ctx, build, opts, func(page []*graph.Edge) ([]*graph.Edge, error) {
			require.Equal(t, pending, page)
			var chosen []*graph.Edge
			for _, e := range page {
				if _, ok := want[e.From]; ok && e.Kind == graph.EdgeCalls {
					chosen = append(chosen, e)
				}
			}
			require.Equal(t, selected, chosen)
			return chosen, nil
		}, func(f *rebuildLinkFrontier, page []*graph.Edge) error {
			moves, err := reduceRebuildMainPage(ctx, f, page)
			if err != nil {
				return err
			}
			require.Equal(t, nativeMoves, moves)
			require.Equal(t, selected, page, "tentative reduction must not mutate the borrowed page")
			return build.ApplyLinkReindexes(ctx, moves)
		}))
		require.Equal(t, nativeRows, rebuildFnPointerSaved(t, build))
		checkFrontierScratch(t, opts.ScratchParent)
	})

	t.Run("unproved extension still refuses", func(t *testing.T) {
		ctx, build, opts := frontierFixture(t, 1, 0)
		const file, caller = "repo/z/caller.cs", "repo/z/caller.cs::Caller"
		bad := &graph.Edge{From: caller, To: "unresolved::*.NeedsEvidence", Kind: graph.EdgeCalls, FilePath: file}
		build.AddBatch(nodes, edges)
		build.AddBatch([]*graph.Node{
			{ID: caller, Name: "Caller", RepoPrefix: "repo", Language: "csharp", FilePath: file, Kind: graph.KindFunction},
			{ID: "extension", Name: "NeedsEvidence", RepoPrefix: "repo", Language: "csharp", FilePath: file, Kind: graph.KindMethod,
				Meta: map[string]any{"this_param_type": "I", "extension": true}},
			{ID: "local-distractor", Name: "NeedsEvidence", RepoPrefix: "repo", Language: "csharp", FilePath: file, Kind: graph.KindField},
		}, []*graph.Edge{bad})
		before := rebuildFnPointerSaved(t, build)
		err := runRebuildLinkFrontier(ctx, build, opts, func(page []*graph.Edge) ([]*graph.Edge, error) {
			var chosen []*graph.Edge
			for _, e := range page {
				if e.From == caller || (e.Kind == graph.EdgeCalls && want[e.From].to != "") {
					chosen = append(chosen, e)
				}
			}
			return chosen, nil
		}, func(f *rebuildLinkFrontier, page []*graph.Edge) error {
			require.Len(t, page, len(selected)+1)
			require.Equal(t, bad, page[len(page)-1])
			moves, err := reduceRebuildMainPage(ctx, f, page)
			require.Empty(t, moves)
			require.Equal(t, selected, page[:len(selected)])
			require.Equal(t, bad, page[len(page)-1])
			return err
		})
		require.ErrorIs(t, err, errRebuildMainRule)
		require.ErrorContains(t, err, "C# extension visibility")
		require.Equal(t, before, rebuildFnPointerSaved(t, build))
		checkFrontierScratch(t, opts.ScratchParent)
	})
}
