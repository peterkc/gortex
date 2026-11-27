package resolver

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/parser/languages"
)

// Given native Go dataflow edges whose source is a placeholder, compare each
// original position, including competing moves to the same destination key.
func TestRebuildMainDetachedMemberNative(t *testing.T) {
	for _, tc := range []struct{ name, dep string }{
		{"nested-import-hit", "internal/zdep/dep.go"},
		{"top-level-import-miss", "zdep/dep.go"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checkRebuildMainDetachedMemberNative(t, tc.dep)
		})
	}
}

func checkRebuildMainDetachedMemberNative(t *testing.T, dep string) {
	t.Helper()
	const (
		client = "client/main.go"
		remote = "aother/remote.go"
	)
	// Native only puts nested directories in the last-component index.
	// The unresolved example.test/zdep import reaches internal/zdep but
	// not the top-level zdep directory (which can only match exactly).
	importedDir := "internal/zdep"
	reachable := dep == importedDir+"/dep.go"
	var nodes []*graph.Node
	var pending, imports []*graph.Edge
	for _, file := range []struct{ path, source string }{
		{client, `package client
import _ "example.test/zdep"
type Slot interface { Dispatch(int) }
type A struct{}
type B struct{}
func (A) Dispatch(int) {}
func (B) Dispatch(int) {}
func Record(d, v any) {
	d.Receive(v.Payload)
	d.Receive(v.Payload)
	d.Ambiguous(v.Payload)
	d.Dispatch(v.Payload)
	d.Local = v.Payload
}
`},
		{remote, `package aother
type Other struct{}
func (Other) Receive(int) {}
`},
		{dep, `package zdep
type Imported struct{}
func (Imported) Receive(int) {}
func (Imported) Ambiguous(int) {}
type Second struct{}
func (Second) Ambiguous(int) {}
`},
	} {
		facts, err := languages.NewGoExtractor().Extract(file.path, []byte(file.source))
		require.NoError(t, err)
		t.Cleanup(facts.ReleaseTree)
		nodes = append(nodes, facts.Nodes...)
		for _, edge := range facts.Edges {
			switch {
			case edge.Kind == graph.EdgeImports && edge.To == "unresolved::import::example.test/zdep":
				imports = append(imports, edge)
			case (edge.Kind == graph.EdgeArgOf || edge.Kind == graph.EdgeValueFlow) &&
				edge.From == "unresolved::*.Payload" && strings.HasPrefix(edge.To, "unresolved::*."):
				if edge.To == "unresolved::*.Dispatch" {
					// Typed control: force the interface lookup after the two
					// local methods survive reachability. Ordinary raw Go
					// dataflow edges need not carry a receiver hint.
					edge.Meta["receiver_type"] = "Slot"
				}
				pending = append(pending, edge)
			}
		}
	}
	require.Len(t, imports, 1)
	require.Len(t, pending, 5)
	duplicate := cloneFrameworkEdge(pending[0])
	require.Equal(t, "unresolved::*.Receive", duplicate.To)
	duplicate.To = "repo::unresolved::*.Receive"
	duplicate.Meta["duplicate_payload"] = "second"
	pending = append(pending, duplicate)
	rebuildFnPointerSortRows(pending)

	// A missing From gives native the empty repository and no language cut.
	// A same-file foreign method must not win. A reachable non-Go candidate
	// in the empty repository must remain eligible for the locality branch.
	nodes = append(nodes,
		&graph.Node{ID: "foreign", Name: "Receive", Kind: graph.KindMethod, RepoPrefix: "repo", Language: "go", FilePath: client},
		&graph.Node{ID: "local-web", Name: "Local", Kind: graph.KindMethod, Language: "typescript", FilePath: "client/local.ts"},
	)
	saved := append(append([]*graph.Edge(nil), imports...), pending...)
	g := graph.New()
	g.AddBatch(nodes, nil)
	for _, edge := range saved {
		g.AddEdge(cloneFrameworkEdge(edge))
	}
	native := New(g)
	native.bulkMode = true
	require.NoError(t, native.warmLookupCache(pending))
	defer native.clearLookupCache()
	require.True(t, native.buildReachabilityIndexForPending(pending, native.nodeByID))
	defer native.clearReachabilityIndex()
	wantDirs := map[string]struct{}{"client": {}}
	if reachable {
		wantDirs[importedDir] = struct{}{}
	}
	require.Equal(t, wantDirs, native.reachableDirsByFile[client])
	var want []graph.EdgeReindex
	for _, original := range pending {
		require.Nil(t, g.GetNode(original.From))
		edge := cloneFrameworkEdge(original)
		oldTo, moved := native.resolveEdge(edge, &ResolveStats{})
		switch graph.UnresolvedName(oldTo) {
		case "*.Receive":
			scope, langs := native.resolverNameScopeForEdge(original, native.callerRepoPrefix(original))
			require.Equal(t, "", scope.repo)
			require.Empty(t, langs)
			candidates := native.cachedFindNodesByNameInRepoForEdge("Receive", scope.repo, original)
			require.Len(t, candidates, 2)
			require.Equal(t, remote+"::Other.Receive", candidates[0].ID)
			require.Equal(t, dep+"::Imported.Receive", candidates[1].ID)
			if reachable {
				require.True(t, moved)
				require.Equal(t, dep+"::Imported.Receive", edge.To)
			} else {
				require.False(t, moved)
				require.Equal(t, original, edge)
			}
		case "*.Ambiguous":
			require.False(t, moved)
			require.Equal(t, original, edge)
		case "*.Dispatch":
			require.Equal(t, client+"::A.Dispatch", edge.To)
			require.Equal(t, "interface", edge.Meta["dispatch"])
		case "*.Local":
			require.Equal(t, "local-web", edge.To)
		default:
			t.Fatalf("unexpected fixture target %q", oldTo)
		}
		if moved {
			clearEdgeTerminal(edge)
			want = append(want, graph.EdgeReindex{Edge: edge, OldTo: oldTo, OldKind: original.Kind})
		}
	}
	if reachable {
		require.Len(t, want, 5)
	} else {
		require.Len(t, want, 2)
	}
	g.ReindexEdges(want)
	wantSaved := g.AllEdges()
	rebuildFnPointerSortRows(wantSaved)

	for _, arm := range []string{"batch", "repeat", "scalar"} {
		t.Run(arm, func(t *testing.T) {
			ctx, build, opts := frontierFixture(t, 0)
			build.AddBatch(nodes, saved)
			var got []graph.EdgeReindex
			refusals := map[string]int{"direct-import": 0, "locality": 0, "typed-control": 0}
			selectMembers := func(page []*graph.Edge) ([]*graph.Edge, error) {
				var selected []*graph.Edge
				for _, edge := range page {
					if edge.Kind != graph.EdgeImports {
						selected = append(selected, edge)
					}
				}
				return selected, nil
			}
			require.NoError(t, runRebuildLinkFrontier(ctx, build, opts, selectMembers, func(f *rebuildLinkFrontier, page []*graph.Edge) error {
				require.Equal(t, pending, page)
				if err := prepareRebuildProvides(ctx, f, page); err != nil {
					return err
				}
				if arm != "scalar" {
					var err error
					got, err = reduceRebuildMainPage(ctx, f, page)
					return err
				}
				for _, original := range page {
					edge := cloneFrameworkEdge(original)
					name := strings.TrimPrefix(graph.UnresolvedName(edge.To), "*.")
					if err := resolveRebuildMember(ctx, f, edge, name); err != nil {
						switch {
						case errors.Is(err, errRebuildMainRule) && strings.Contains(err.Error(), "native page direct-import reachability"):
							refusals["direct-import"]++
						case errors.Is(err, errRebuildMainRule) && strings.Contains(err.Error(), "method locality or interface dispatch fallback"):
							refusals["locality"]++
						case errors.Is(err, errRebuildMainRule) && name == "Dispatch" && strings.Contains(err.Error(), "native first-use useClass bindings"):
							refusals["typed-control"]++
						default:
							return err
						}
						continue // test census only
					}
					if edge.To != original.To {
						clearEdgeTerminal(edge)
						got = append(got, graph.EdgeReindex{Edge: edge, OldTo: original.To, OldKind: original.Kind})
					}
				}
				for _, tc := range []struct {
					from, file, name, reason string
					kind                     graph.EdgeKind
				}{
					{page[0].From, client, "Receive", "native page direct-import reachability", graph.EdgeCalls},
					{"missing-source", client, "Receive", "native page direct-import reachability", graph.EdgeArgOf},
					{page[0].From, "", "Local", "method locality or interface dispatch fallback", graph.EdgeArgOf},
				} {
					edge := &graph.Edge{From: tc.from, To: "unresolved::*." + tc.name, Kind: tc.kind, FilePath: tc.file,
						Origin: graph.OriginASTInferred, Meta: map[string]any{"arg_position": 0}}
					before := cloneFrameworkEdge(edge)
					err := resolveRebuildMember(ctx, f, edge, tc.name)
					require.ErrorIs(t, err, errRebuildMainRule)
					require.ErrorContains(t, err, tc.reason)
					require.Equal(t, before, edge)
				}
				return nil
			}))
			t.Logf("fixture positions=%d native_moves=%d product_moves=%d refusals=%v", len(pending), len(want), len(got), refusals)
			require.Zero(t, refusals["direct-import"]+refusals["locality"]+refusals["typed-control"])
			require.Equal(t, want, got)
			require.Equal(t, pending, rebuildDetachedPending(t, build))
			require.NoError(t, build.ApplyLinkReindexes(ctx, got))
			require.Equal(t, wantSaved, rebuildFnPointerSaved(t, build))
			checkFrontierScratch(t, opts.ScratchParent)
		})
	}
}

func rebuildDetachedPending(t *testing.T, build *store_pebble.BuildStore) []*graph.Edge {
	t.Helper()
	var pending []*graph.Edge
	for _, edge := range rebuildFnPointerSaved(t, build) {
		if edge.Kind == graph.EdgeArgOf || edge.Kind == graph.EdgeValueFlow {
			pending = append(pending, edge)
		}
	}
	return pending
}

// Given a prepared first position and a failed frozen input read on the
// second, neither position can be applied and the private head stays unoffered.
func TestRebuildMainDetachedMemberReadRefusal(t *testing.T) {
	ctx, build, opts := frontierFixture(t, 0)
	const first, later = "client/first.go", "web/later.ts"
	build.AddBatch([]*graph.Node{
		{ID: "method", Kind: graph.KindMethod, Name: "Receive", Language: "go", FilePath: first},
		{ID: later, Kind: graph.KindFile, Name: "later.ts", Language: "typescript", FilePath: later},
	}, []*graph.Edge{
		{From: "unresolved::A", To: "unresolved::*.Receive", Kind: graph.EdgeArgOf, FilePath: first},
		{From: "unresolved::Z", To: "unresolved::*.Receive", Kind: graph.EdgeArgOf, FilePath: later},
		{From: later, To: "unresolved::import::some-package", Kind: graph.EdgeImports, FilePath: later},
	})
	before := rebuildFnPointerSaved(t, build)
	attempted := false
	selectMembers := func(page []*graph.Edge) ([]*graph.Edge, error) {
		var selected []*graph.Edge
		for _, edge := range page {
			if edge.Kind == graph.EdgeArgOf {
				selected = append(selected, edge)
			}
		}
		return selected, nil
	}
	err := runRebuildLinkFrontier(ctx, build, opts, selectMembers, func(f *rebuildLinkFrontier, page []*graph.Edge) error {
		require.Len(t, page, 2)
		f.imports.externalNpmDependency = func(file, spec string) (bool, error) {
			attempted = true
			require.Equal(t, later, file)
			require.Equal(t, "some-package", spec)
			require.Contains(t, f.pageReach, first)
			return false, os.ErrClosed
		}
		moves, err := reduceRebuildMainPage(ctx, f, page)
		require.Nil(t, moves)
		return err
	})
	require.True(t, attempted)
	require.ErrorIs(t, err, store_pebble.ErrRebuildView)
	require.ErrorIs(t, err, os.ErrClosed)
	require.Equal(t, before, rebuildFnPointerSaved(t, build))
	_, err = build.OpenSnapshot()
	require.ErrorIs(t, err, store_pebble.ErrUnpublishedBuild)
	checkFrontierScratch(t, opts.ScratchParent)
}
