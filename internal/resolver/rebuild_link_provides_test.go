package resolver

import (
	"context"
	"fmt"
	"math"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func rebuildProvidesTestEdge(from, file, target, abstract string, line int) *graph.Edge {
	return &graph.Edge{From: from, FilePath: file, Line: line, To: target, Kind: graph.EdgeProvides,
		Meta: map[string]any{graph.MetaDIProvidesFor: abstract, graph.MetaDIBinding: graph.DIBindingUseClass}}
}

func TestRebuildProvidesCensusNative(t *testing.T) {
	ctx, build, _ := frontierFixture(t, 1, 0)
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
	ctx, build, _ := frontierFixture(t, 1, 0)
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
			ctx, build, _ := frontierFixture(t, 1, 0)
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

func TestRebuildProvidesOriginalPageRefusalEvenWhenUnselected(t *testing.T) {
	ctx, build, opts := frontierFixture(t, 1, 0)
	opts.Binary = filepath.Join(opts.ScratchParent, "must-not-start")
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
	checkFrontierScratch(t, opts.ScratchParent)
}

func TestRebuildProvidesProjectionRefusals(t *testing.T) {
	t.Run("retention limit", func(t *testing.T) {
		ctx, build, _ := frontierFixture(t, 1, 0)
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
		ctx, build, _ := frontierFixture(t, 1, 0)
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
		ctx, build, _ := frontierFixture(t, 1, 0)
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
		ctx, build, _ := frontierFixture(t, 1, 0)
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		idx, err := captureRebuildProvides(canceled, build.RebuildView())
		require.ErrorIs(t, err, store_pebble.ErrRebuildView)
		require.ErrorIs(t, err, context.Canceled)
		require.Nil(t, idx)
	})
}
