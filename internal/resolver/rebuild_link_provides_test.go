package resolver

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/parser/languages"
)

func rebuildProvidesTestEdge(from, file, target, abstract string, line int) *graph.Edge {
	return &graph.Edge{From: from, FilePath: file, Line: line, To: target, Kind: graph.EdgeProvides,
		Meta: map[string]any{graph.MetaDIProvidesFor: abstract, graph.MetaDIBinding: graph.DIBindingUseClass}}
}

func TestRebuildProvidesCensusNative(t *testing.T) {
	ctx, build, _ := frontierFixture(t, 0)
	nodes := []*graph.Node{
		{ID: "ts", Language: "typescript", RepoPrefix: "repo", FilePath: "module.ts"},
		{ID: "php", Language: "php", RepoPrefix: "other", FilePath: "module.php"},
	}
	edges := []*graph.Edge{
		rebuildProvidesTestEdge("ts", "module.ts", "repo::file.ts::Email", "Notifier", 1),
		rebuildProvidesTestEdge("php", "module.php", "other::unresolved::Sms", "Notifier", 1),
		rebuildProvidesTestEdge("php", "module.php", "Pager", "Notifier", 2),
		rebuildProvidesTestEdge("missing", "other.php", "unresolved::Global", "Notifier", 3),
		rebuildProvidesTestEdge("ts", "module.ts", "unresolved::Email", "Notifier", 4),
		rebuildProvidesTestEdge("ts", "module.ts", "repo::", "EmptyTargetSuffix", 5),
	}
	wrongBinding := rebuildProvidesTestEdge("php", "module.php", "WrongBinding", "Notifier", 6)
	wrongBinding.Meta[graph.MetaDIBinding] = "useValue"
	wrongAbstract := rebuildProvidesTestEdge("php", "module.php", "WrongAbstract", "Notifier", 7)
	wrongAbstract.Meta[graph.MetaDIProvidesFor] = 42
	wrongKind := rebuildProvidesTestEdge("ts", "module.ts", "WrongKind", "Notifier", 8)
	wrongKind.Kind = graph.EdgeConsumes
	emptyAbstract := rebuildProvidesTestEdge("php", "module.php", "EmptyAbstract", "", 9)
	edges = append(edges, wrongBinding, wrongAbstract, wrongKind, emptyAbstract)
	build.AddBatch(nodes, edges)
	g := graph.New()
	g.AddBatch(nodes, edges)
	native := New(g)
	native.buildProvidesForIndex()
	want := map[string]map[string]struct{}{
		"Notifier":          {"Email": {}, "Sms": {}, "Pager": {}, "Global": {}},
		"EmptyTargetSuffix": {"": {}},
	}
	require.Equal(t, want, native.providesForIdx)
	got, err := captureRebuildProvides(ctx, build.RebuildView())
	require.NoError(t, err)
	require.Equal(t, want, got)
}

// Exercise the native first-use timing independently of the stationarity
// guard. Private commits before preparation are visible; tentative moves on
// the triggering page and all subsequent commits are not, even if the page
// is empty after selection.
func TestRebuildProvidesFirstUse(t *testing.T) {
	ctx, build, _ := frontierFixture(t, 0)
	before := rebuildProvidesTestEdge("a", "first.ts", "unresolved::Before", "First", 1)
	samePage := rebuildProvidesTestEdge("b", "page.ts", "unresolved::Current", "Second", 1)
	trigger := &graph.Edge{From: "z", Kind: graph.EdgeCalls, To: "unresolved::*.Run", FilePath: "call.ts", Meta: map[string]any{"receiver_type": "First"}}
	build.AddBatch(nil, []*graph.Edge{before, samePage, trigger})
	g := graph.New()
	g.AddBatch(nil, []*graph.Edge{cloneFrameworkEdge(before), cloneFrameworkEdge(samePage), cloneFrameworkEdge(trigger)})
	native := New(g)
	indexes := newResolveAllPassIndexes(native)
	defer indexes.close()
	f := rebuildLinkFrontier{view: build.RebuildView()}
	prepare := func(page []*graph.Edge) {
		t.Helper()
		if pendingNeedsProvidesIndex(page) {
			indexes.ensureProvides(nil)
		}
		require.NoError(t, prepareRebuildProvides(ctx, &f, page))
		require.Equal(t, native.providesForIdx, f.providesFor)
	}
	move := func(old *graph.Edge, target string) {
		t.Helper()
		e := *old
		e.To = target
		change := graph.EdgeReindex{Edge: &e, OldTo: old.To, OldKind: old.Kind}
		require.NoError(t, build.ApplyLinkReindexes(ctx, []graph.EdgeReindex{change}))
		native.graph.ReindexEdges([]graph.EdgeReindex{{Edge: cloneFrameworkEdge(&e), OldTo: old.To, OldKind: old.Kind}})
	}
	prepare([]*graph.Edge{before, {To: "unresolved::*.Untyped", Meta: map[string]any{}}})
	require.Nil(t, f.providesFor)
	move(before, "file.ts::Earlier")
	prepare([]*graph.Edge{samePage, trigger})
	want := map[string]map[string]struct{}{"First": {"Earlier": {}}, "Second": {"Current": {}}}
	require.Equal(t, want, f.providesFor)
	move(samePage, "file.ts::Later")
	prepare([]*graph.Edge{trigger})
	require.Equal(t, want, f.providesFor)
}

func TestRebuildProvidesStationarity(t *testing.T) {
	for _, tc := range []struct {
		name, lang, targetID string
		sibling              bool
		wantRefusal          bool
	}{
		{"typescript-stable", "typescript", "defs.ts::Impl", false, false},
		{"php-stable", "php", "defs.php::Impl", false, false},
		{"php-changes-spelling", "php", "defs.php::Owner.Impl", false, true},
		{"php-competing-non-class", "php", "defs.php::Impl", true, true},
		{"php-no-candidate", "php", "", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, build, _ := frontierFixture(t, 0)
			provider := rebuildProvidesTestEdge("module", "module."+tc.lang, "unresolved::Impl", "Abstract", 1)
			build.AddBatch([]*graph.Node{{ID: provider.From, Language: tc.lang, RepoPrefix: "repo", FilePath: provider.FilePath}}, []*graph.Edge{provider})
			// All-repo scan must not admit the unrelated repository's hit.
			build.AddNode(&graph.Node{ID: "outside::NotImpl", Name: "Impl", RepoPrefix: "outside", Language: tc.lang})
			if tc.targetID != "" {
				build.AddNode(&graph.Node{ID: tc.targetID, Name: "Impl", RepoPrefix: "repo", Language: tc.lang})
			}
			if tc.sibling {
				sibling := *provider
				sibling.To = "defs.php::Impl"
				sibling.Meta = nil // could win and erase the moving pair
				build.AddEdge(&sibling)
			}
			err := checkRebuildProvidesStationarity(ctx, build.RebuildView())
			if tc.wantRefusal {
				require.ErrorIs(t, err, errRebuildMainRule)
				require.ErrorContains(t, err, "useClass stationarity")
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// The frozen Laravel bind emits two distinct token targets at one recording
// site. Resolve both through the native dispatcher, then apply those actual
// reindexes to the private build and compare the complete census on both sides.
func TestRebuildProvidesStationarityLaravel(t *testing.T) {
	ctx, build, _ := frontierFixture(t, 0)
	const providerPath = "app/Providers/AppServiceProvider.php"
	const abstractPath = "app/Repositories/UserRepository.php"
	const concretePath = "app/Repositories/EloquentUserRepository.php"
	const fixture = "../../bench/fixtures/di/laravel"
	g := graph.New()
	var providers []*graph.Edge
	for _, path := range []string{providerPath, abstractPath, concretePath} {
		src, err := os.ReadFile(filepath.Join(fixture, path))
		require.NoError(t, err)
		result, err := languages.NewPHPExtractor().Extract(path, src)
		require.NoError(t, err)
		for _, n := range result.Nodes {
			n.RepoPrefix = "repo"
		}
		build.AddBatch(result.Nodes, nil)
		g.AddBatch(result.Nodes, nil)
		for _, e := range result.Edges {
			if e.Kind == graph.EdgeProvides && e.Meta[graph.MetaDIOrigin] == "bind" {
				providers = append(providers, e)
			}
		}
	}
	require.Len(t, providers, 2)
	wantOld := map[string]string{
		"unresolved::EloquentUserRepository": graph.DIBindingUseClass,
		"unresolved::UserRepository":         "bind",
	}
	gotOld := make(map[string]string)
	for _, e := range providers {
		require.Equal(t, providerPath+"::AppServiceProvider", e.From)
		require.Equal(t, providerPath, e.FilePath)
		require.Equal(t, 21, e.Line)
		switch e.To {
		case "unresolved::EloquentUserRepository":
			require.Equal(t, "UserRepository", e.Meta[graph.MetaDIProvidesFor])
		case "unresolved::UserRepository":
			require.Equal(t, "UserRepository", e.Meta[graph.MetaDIToken])
		}
		binding, ok := e.Meta[graph.MetaDIBinding].(string)
		require.True(t, ok)
		gotOld[e.To] = binding
		g.AddEdge(cloneFrameworkEdge(e))
		build.AddEdge(e)
	}
	require.Equal(t, wantOld, gotOld)
	require.NoError(t, checkRebuildProvidesStationarity(ctx, build.RebuildView()))

	native := New(g)
	want := map[string]map[string]struct{}{"UserRepository": {"EloquentUserRepository": {}}}
	native.buildProvidesForIndex()
	require.Equal(t, want, native.providesForIdx)
	before, err := captureRebuildProvides(ctx, build.RebuildView())
	require.NoError(t, err)
	require.Equal(t, native.providesForIdx, before)

	// Complete old-key order for the two rows at this same site is To order.
	slices.SortFunc(providers, func(a, b *graph.Edge) int { return strings.Compare(a.To, b.To) })
	wantTargets := map[string]string{
		"unresolved::EloquentUserRepository": concretePath + "::EloquentUserRepository",
		"unresolved::UserRepository":         abstractPath + "::UserRepository",
	}
	var moves []graph.EdgeReindex
	var stats ResolveStats
	for _, e := range providers {
		resolved := cloneFrameworkEdge(e)
		oldTo, changed := native.resolveEdge(resolved, &stats)
		require.True(t, changed)
		require.Equal(t, e.To, oldTo)
		require.Equal(t, wantTargets[oldTo], resolved.To)
		require.Equal(t, graph.EdgeProvides, resolved.Kind)
		moves = append(moves, graph.EdgeReindex{Edge: resolved, OldTo: oldTo, OldKind: e.Kind})
	}
	require.Equal(t, 2, stats.Resolved)
	require.NoError(t, build.ApplyLinkReindexes(ctx, moves))
	g.ReindexEdges(moves)

	wantResolved := []string{concretePath + "::EloquentUserRepository", abstractPath + "::UserRepository"}
	var nativeTargets, privateTargets []string
	for e := range g.EdgesByKind(graph.EdgeProvides) {
		nativeTargets = append(nativeTargets, e.To)
	}
	require.NoError(t, build.RebuildView().VisitEdges(ctx, func(e *graph.Edge) error {
		if e.Kind == graph.EdgeProvides {
			privateTargets = append(privateTargets, e.To)
		}
		return nil
	}))
	require.ElementsMatch(t, wantResolved, nativeTargets)
	require.ElementsMatch(t, wantResolved, privateTargets)
	native.buildProvidesForIndex()
	after, err := captureRebuildProvides(ctx, build.RebuildView())
	require.NoError(t, err)
	require.Equal(t, before, native.providesForIdx)
	require.Equal(t, before, after)
}

func TestRebuildProvidesStationarityDestinations(t *testing.T) {
	for _, tc := range []struct {
		name            string
		classTarget     string
		ordinaryTargets []string
		otherSite       string
		wantRefusal     bool
	}{
		{name: "pending-class-disjoint-bound-ordinary", classTarget: "unresolved::Impl", ordinaryTargets: []string{"defs.php::Other"}},
		{name: "bound-class-disjoint-pending-ordinary", classTarget: "defs.php::Impl", ordinaryTargets: []string{"unresolved::Other"}},
		{name: "bound-class-competing-pending-ordinary", classTarget: "defs.php::Impl", ordinaryTargets: []string{"unresolved::Impl"}, wantRefusal: true},
		{name: "pending-class-competing-qualified-ordinary", classTarget: "unresolved::Impl", ordinaryTargets: []string{"repo::unresolved::Impl"}, wantRefusal: true},
		{name: "qualified-class-competing-pending-ordinary", classTarget: "repo::unresolved::Impl", ordinaryTargets: []string{"unresolved::Impl"}, wantRefusal: true},
		{name: "ordinary-only-overlap", classTarget: "unresolved::Impl", ordinaryTargets: []string{"unresolved::Other", "repo::unresolved::Other"}},
		{name: "unsupported-sibling-dispatch", classTarget: "defs.php::Impl", ordinaryTargets: []string{"unresolved::*.Impl"}, wantRefusal: true},
		{name: "different-from", classTarget: "unresolved::Impl", ordinaryTargets: []string{"defs.php::Impl"}, otherSite: "from"},
		{name: "different-file", classTarget: "unresolved::Impl", ordinaryTargets: []string{"defs.php::Impl"}, otherSite: "file"},
		{name: "different-line", classTarget: "unresolved::Impl", ordinaryTargets: []string{"defs.php::Impl"}, otherSite: "line"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, build, _ := frontierFixture(t, 0)
			provider := rebuildProvidesTestEdge("module", "module.php", tc.classTarget, "Abstract", 1)
			build.AddBatch([]*graph.Node{
				{ID: provider.From, Language: "php", RepoPrefix: "repo", FilePath: provider.FilePath},
				{ID: "other-module", Language: "php", RepoPrefix: "repo", FilePath: provider.FilePath},
				{ID: "defs.php::Impl", Name: "Impl", Language: "php", RepoPrefix: "repo", FilePath: "defs.php"},
				{ID: "defs.php::Other", Name: "Other", Language: "php", RepoPrefix: "repo", FilePath: "defs.php"},
			}, []*graph.Edge{provider})
			for _, target := range tc.ordinaryTargets {
				sibling := *provider
				sibling.To = target
				sibling.Meta = map[string]any{graph.MetaDIBinding: "bind"}
				switch tc.otherSite {
				case "from":
					sibling.From = "other-module"
				case "file":
					sibling.FilePath = "other.php"
				case "line":
					sibling.Line++
				}
				build.AddEdge(&sibling)
			}
			err := checkRebuildProvidesStationarity(ctx, build.RebuildView())
			if tc.wantRefusal {
				require.ErrorIs(t, err, errRebuildMainRule)
				require.ErrorContains(t, err, "useClass stationarity")
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestRebuildProvidesOriginalPageRefusalEvenWhenUnselected(t *testing.T) {
	ctx, build, opts := frontierFixture(t, 0)
	lease := opts.ScratchParent
	opts.ScratchParent = filepath.Join(lease, "must-not-start")
	provider := rebuildProvidesTestEdge("a", "module.php", "unresolved::Impl", "Abstract", 1)
	member := &graph.Edge{From: "z", Kind: graph.EdgeCalls, To: "unresolved::*.Run", FilePath: "call.php", Meta: map[string]any{"receiver_type": "Abstract"}}
	build.AddBatch([]*graph.Node{
		{ID: "a", Language: "php", RepoPrefix: "repo", FilePath: provider.FilePath},
		{ID: "other.php::Owner.Impl", Name: "Impl", Language: "php", RepoPrefix: "repo"},
	}, []*graph.Edge{provider, member})
	want := rebuildFnPointerSaved(t, build)
	selectCalls := 0
	err := RunRebuildMain(ctx, build, opts, RebuildMainInputs{
		SelectCallers: func([]*graph.Edge) ([]*graph.Edge, error) {
			selectCalls++
			return nil, nil
		},
	})
	require.ErrorIs(t, err, errRebuildMainRule)
	require.ErrorContains(t, err, "useClass stationarity")
	require.Zero(t, selectCalls, "original typed page must prepare before receipt selection")
	require.Equal(t, want, rebuildFnPointerSaved(t, build))
	checkFrontierScratch(t, lease)
}

func TestRebuildProvidesProjectionRefusals(t *testing.T) {
	t.Run("retention limit", func(t *testing.T) {
		ctx, build, _ := frontierFixture(t, 0)
		edges := make([]*graph.Edge, 0, rebuildProvidesRows+1)
		for i := range rebuildProvidesRows + 1 {
			edges = append(edges, rebuildProvidesTestEdge("module", "module.ts", fmt.Sprintf("file.ts::Impl%d", i), "Abstract", i+1))
		}
		build.AddBatch(nil, edges)
		idx, err := captureRebuildProvides(ctx, build.RebuildView())
		require.ErrorIs(t, err, store_pebble.ErrRebuildLinkInputSize)
		require.Nil(t, idx)
		require.ErrorIs(t, checkRebuildProvidesStationarity(ctx, build.RebuildView()), store_pebble.ErrRebuildLinkInputSize)
	})
	t.Run("candidate visit limit", func(t *testing.T) {
		ctx, build, _ := frontierFixture(t, 0)
		nodes := []*graph.Node{{ID: "module", Language: "typescript", RepoPrefix: "repo"}}
		var edges []*graph.Edge
		for i := range 129 {
			nodes = append(nodes, &graph.Node{ID: fmt.Sprintf("defs%d.ts::Impl", i), Name: "Impl", RepoPrefix: "repo", Language: "typescript"})
			edges = append(edges, rebuildProvidesTestEdge("module", "module.ts", "unresolved::Impl", "Abstract", i+1))
		}
		build.AddBatch(nodes, edges)
		err := checkRebuildProvidesStationarity(ctx, build.RebuildView())
		require.ErrorIs(t, err, store_pebble.ErrRebuildLinkInputSize)
		require.ErrorContains(t, err, "candidate rows")
	})
	t.Run("latched invalid record", func(t *testing.T) {
		ctx, build, _ := frontierFixture(t, 0)
		valid := rebuildProvidesTestEdge("module", "module.ts", "Impl", "Abstract", 1)
		build.AddEdge(valid)
		invalid := *valid
		invalid.Confidence = math.NaN()
		build.AddEdge(&invalid)
		idx, err := captureRebuildProvides(ctx, build.RebuildView())
		require.ErrorIs(t, err, store_pebble.ErrRebuildView)
		require.ErrorContains(t, err, "non-finite edge confidence")
		require.Nil(t, idx)
	})
	t.Run("canceled read", func(t *testing.T) {
		ctx, build, _ := frontierFixture(t, 0)
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		idx, err := captureRebuildProvides(canceled, build.RebuildView())
		require.ErrorIs(t, err, store_pebble.ErrRebuildView)
		require.ErrorIs(t, err, context.Canceled)
		require.Nil(t, idx)
	})
}
