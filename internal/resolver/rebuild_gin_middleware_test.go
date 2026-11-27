package resolver

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func rebuildGinSelection(t *testing.T, build *store_pebble.BuildStore) FrameworkSynthesizerSelection {
	t.Helper()
	selected, err := NewFrameworkSynthesizerSelection([]string{SynthGinMiddleware})
	require.NoError(t, err)
	frozen, err := FreezeRebuildFrameworkSelection(t.Context(), build.RebuildView(), selected)
	require.NoError(t, err)
	return frozen
}

func TestRebuildGinMiddlewareSQLiteOracle(t *testing.T) {
	dispatcher := &graph.Node{ID: "dispatch", Name: "Handle", Kind: graph.KindMethod, Language: "go", FilePath: "router.go", StartLine: 4,
		RepoPrefix: "vendor", WorkspaceID: "one", Meta: map[string]any{"gin_dispatcher": true}}
	first := &graph.Node{ID: "first", Name: "Handle", Kind: graph.KindFunction, Language: "go", FilePath: "router.go", WorkspaceID: "one"}
	otherLanguage := &graph.Node{ID: "foreign", Name: "Handle", Kind: graph.KindMethod, Language: "python", FilePath: "handler.py",
		RepoPrefix: "app", WorkspaceID: "one"}
	wrongWorkspace := &graph.Node{ID: "wrong", Name: "Handle", Kind: graph.KindFunction, Language: "go", FilePath: "registrar.go",
		RepoPrefix: "vendor", WorkspaceID: "two", Meta: map[string]any{"gin_handlers": []string{"Handle", "Handle", "Missing"}}}
	incumbent := &graph.Edge{From: dispatcher.ID, To: first.ID, Kind: graph.EdgeCalls, FilePath: dispatcher.FilePath,
		Line: dispatcher.StartLine, Origin: graph.OriginASTResolved, Meta: map[string]any{"kept": true}}
	facts := []store_pebble.ParseFacts{
		rebuildEventFacts(dispatcher.FilePath, "go", []*graph.Node{dispatcher, first}, incumbent),
		rebuildEventFacts(otherLanguage.FilePath, "python", []*graph.Node{otherLanguage}),
		rebuildEventFacts(wrongWorkspace.FilePath, "go", []*graph.Node{wrongWorkspace}),
	}
	build, native := openRebuildEventStores(t, facts...)
	before := rebuildEventSavedRows(t, build)
	selection := rebuildGinSelection(t, build)
	// The occupied target needs no proof when dropping it changes neither the
	// saved keys nor the threshold decision. The registrar is in another workspace.
	facts[0] = rebuildGinOmit(facts[0], first)
	for range 2 {
		require.Equal(t, 2, ResolveGinMiddlewareCalls(native))
		offers, err := RunRebuildGinMiddleware(t.Context(), build, rebuildGRPCReceipts(facts...), selection)
		require.NoError(t, err)
		require.Equal(t, uint64(2), offers)
		saved := rebuildEventSavedRows(t, build)
		require.Equal(t, before.nodes, saved.nodes)
		require.Equal(t, rebuildEventSQLiteRows(native).edges, saved.edges)
		require.ElementsMatch(t, []*graph.Edge{incumbent, ginMiddlewareEdge(dispatcher, otherLanguage)}, saved.edges)
	}
}

func TestRebuildGinMiddlewareSQLiteThreshold(t *testing.T) {
	const file = "threshold.go"
	callable := func(id, name, ws string) *graph.Node {
		return &graph.Node{ID: id, Name: name, Kind: graph.KindFunction, Language: "go", FilePath: file, WorkspaceID: ws, StartLine: 4}
	}
	d, late := callable("a-dispatch", "dispatch", "one"), callable("z-dispatch", "dispatch", "two")
	d.Meta = map[string]any{"gin_dispatcher": true, "gin_handlers": []any{"B", "A", true}}
	late.Meta = map[string]any{"gin_dispatcher": true}
	all := []*graph.Node{late, d}
	var first, skipped *graph.Node
	for i := ginFanoutCap; i >= 0; i-- {
		h := callable(fmt.Sprintf("a-%04d", i), "A", "one")
		all = append(all, h)
		switch i {
		case 0:
			first = h
		case ginFanoutCap:
			skipped = h
		}
	}
	for _, ws := range []string{"one", "two"} {
		all = append(all, callable(ws+"-b-2", "B", ws), callable(ws+"-b-1", "B", ws))
	}
	incumbent := ginMiddlewareEdge(d, first)
	incumbent.Meta = map[string]any{"kept": true}
	facts := rebuildEventFacts(file, "go", all, incumbent)
	for _, omitted := range [][]*graph.Node{{skipped}, {first}, {first, skipped}} {
		build, native := openRebuildEventStores(t, facts)
		before := rebuildEventSavedRows(t, build)
		receipt := facts
		for _, omit := range omitted {
			receipt = rebuildGinOmit(receipt, omit)
		}
		offers, err := RunRebuildGinMiddleware(t.Context(), build, rebuildGRPCReceipts(receipt), rebuildGinSelection(t, build))
		if omitted[0] == first {
			// Losing an occupied offer changes which later unoccupied row the threshold admits.
			require.ErrorIs(t, err, store_pebble.ErrRebuildGinMiddleware)
			var refused *store_pebble.RebuildUnknownProvenanceError
			require.ErrorAs(t, err, &refused)
			require.Equal(t, graph.EdgeIdentityFor(ginMiddlewareEdge(d, skipped)), refused.First)
			require.EqualValues(t, 1, refused.Count)
			require.Zero(t, offers)
			require.Equal(t, before, rebuildEventSavedRows(t, build))
			continue
		}
		require.NoError(t, err)
		require.Equal(t, uint64(514), offers)
		require.Equal(t, 514, ResolveGinMiddlewareCalls(native))
		saved := rebuildEventSavedRows(t, build)
		require.Equal(t, rebuildEventSQLiteRows(native).edges, saved.edges)
		require.Len(t, saved.edges, 514)
		require.Contains(t, saved.edges, incumbent)
		require.Contains(t, saved.edges, ginMiddlewareEdge(d, callable("one-b-1", "B", "one")))
		require.Contains(t, saved.edges, ginMiddlewareEdge(late, callable("two-b-1", "B", "two")))
		require.NotContains(t, saved.edges, ginMiddlewareEdge(d, skipped))
	}
}

func rebuildGinOmit(facts store_pebble.ParseFacts, omit *graph.Node) store_pebble.ParseFacts {
	var nodes []*graph.Node
	for _, n := range facts.Nodes {
		if n != omit {
			nodes = append(nodes, n)
		}
	}
	return rebuildEventFacts(facts.File.FilePath, facts.EffectiveLanguage, nodes, facts.Edges...)
}

func TestRebuildGinMiddlewareOutputProof(t *testing.T) {
	const file = "proof.go"
	d := &graph.Node{ID: "dispatch", Kind: graph.KindMethod, Language: "go", FilePath: file, StartLine: 2, Meta: map[string]any{"gin_dispatcher": true}}
	h := &graph.Node{ID: "handler", Name: "Handle", Kind: graph.KindFunction, Language: "go", FilePath: file}
	r := &graph.Node{ID: "register", Kind: graph.KindFunction, Language: "go", FilePath: file, Meta: map[string]any{"gin_handlers": []string{"Handle"}}}
	facts := rebuildEventFacts(file, "go", []*graph.Node{d, h, r})
	for _, omit := range []*graph.Node{d, h, r} {
		build := openRebuildGRPCBuild(t, facts)
		before := rebuildEventSavedRows(t, build)
		offers, err := RunRebuildGinMiddleware(t.Context(), build, rebuildGRPCReceipts(rebuildGinOmit(facts, omit)), rebuildGinSelection(t, build))
		require.Zero(t, offers)
		require.ErrorIs(t, err, store_pebble.ErrRebuildGinMiddleware)
		var refused *store_pebble.RebuildUnknownProvenanceError
		require.ErrorAs(t, err, &refused)
		require.Equal(t, graph.EdgeIdentityFor(ginMiddlewareEdge(d, h)), refused.First)
		require.EqualValues(t, 1, refused.Count)
		require.Equal(t, before, rebuildEventSavedRows(t, build))
	}
}

func TestRebuildGinMiddlewareFrozenSelection(t *testing.T) {
	build := openRebuildGRPCBuild(t)
	frozen := rebuildGinSelection(t, build)
	require.False(t, frozen.allows(SynthGinMiddleware))
	build.AddNode(&graph.Node{ID: "g", Kind: graph.KindFunction, Language: "go", FilePath: "x.go"})
	require.True(t, rebuildGinSelection(t, build).allows(SynthGinMiddleware))
	offers, err := RunRebuildGinMiddleware(t.Context(), build, nil, frozen)
	require.NoError(t, err)
	require.Zero(t, offers)
}

func TestRebuildGinMiddlewareReceiptRoles(t *testing.T) {
	const file = "gin.go"
	d := &graph.Node{ID: "dispatch", Kind: graph.KindMethod, Language: "go", FilePath: file, StartLine: 2, Meta: map[string]any{"gin_dispatcher": true}}
	h := &graph.Node{ID: "handler", Name: "Handle", Kind: graph.KindFunction, Language: "go", FilePath: file}
	r := &graph.Node{ID: "register", Kind: graph.KindFunction, Language: "go", FilePath: file, Meta: map[string]any{"gin_handlers": []string{"Handle"}}}
	for _, tc := range []struct {
		name        string
		role        *graph.Node
		change      func(*graph.Node)
		receiptOnly bool
	}{
		{"dispatcher flag", d, func(n *graph.Node) { n.Meta = nil }, true},
		{"registered name", r, func(n *graph.Node) { n.Meta = map[string]any{"gin_handlers": []string{"Other"}} }, true},
		{"target name", h, func(n *graph.Node) { n.Name = "Other" }, true},
		{"target kind", h, func(n *graph.Node) { n.Kind = graph.KindVariable }, true},
		{"dispatcher workspace", d, func(n *graph.Node) { n.WorkspaceID = "other" }, false},
		{"target workspace", h, func(n *graph.Node) { n.WorkspaceID = "other" }, false},
		{"dispatcher line", d, func(n *graph.Node) { n.StartLine++ }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := *tc.role
			tc.change(&changed)
			original := rebuildEventFacts(file, "go", []*graph.Node{d, h, r})
			nodes := []*graph.Node{d, h, r}
			for i, n := range nodes {
				if n.ID == changed.ID {
					nodes[i] = &changed
				}
			}
			alternate := rebuildEventFacts(file, "go", nodes)
			cases := [][2]store_pebble.ParseFacts{{original, alternate}}
			if tc.receiptOnly {
				cases = append(cases, [2]store_pebble.ParseFacts{alternate, original})
			}
			for _, pair := range cases {
				build := openRebuildGRPCBuild(t, pair[0])
				before := rebuildEventSavedRows(t, build)
				offers, err := RunRebuildGinMiddleware(t.Context(), build, rebuildGRPCReceipts(pair[1]), rebuildGinSelection(t, build))
				require.Zero(t, offers)
				require.ErrorIs(t, err, store_pebble.ErrRebuildGinMiddleware)
				var refused *store_pebble.RebuildUnknownProvenanceError
				require.ErrorAs(t, err, &refused)
				want := uint64(1)
				if tc.name == "dispatcher line" {
					want = 2
				}
				require.Equal(t, want, refused.Count)
				require.Equal(t, graph.EdgeIdentityFor(ginMiddlewareEdge(d, h)), refused.First)
				require.Equal(t, before, rebuildEventSavedRows(t, build))
			}
		})
	}
}

func TestRebuildGinMiddlewareReceiptWinner(t *testing.T) {
	const file = "gin.go"
	d := &graph.Node{ID: "dispatch", Kind: graph.KindMethod, Language: "go", FilePath: file, Meta: map[string]any{"gin_dispatcher": true}}
	h := &graph.Node{ID: "handler", Name: "Handle", Kind: graph.KindFunction, Language: "go", FilePath: file}
	r := &graph.Node{ID: "register", Kind: graph.KindFunction, Language: "go", FilePath: file, Meta: map[string]any{"gin_handlers": []string{"Handle"}}}
	changed := *h
	changed.Name = "Other"
	facts := rebuildEventFacts(file, "go", []*graph.Node{d, h, r})
	for _, last := range []*graph.Node{h, &changed} {
		first := &changed
		want := 1
		if last == &changed {
			first = h
			want = 0
		}
		firstFacts := rebuildEventFacts(file, "go", []*graph.Node{first})
		finalFacts := rebuildEventFacts(file, "go", []*graph.Node{last, last})
		build, native := openRebuildEventStores(t, facts)
		require.NoError(t, native.AddBatchChecked(firstFacts.Nodes, nil))
		require.NoError(t, native.AddBatchChecked(finalFacts.Nodes, nil))
		require.Equal(t, want, ResolveGinMiddlewareCalls(native))
		offers, err := RunRebuildGinMiddleware(t.Context(), build, rebuildGRPCReceipts(facts, firstFacts, finalFacts), rebuildGinSelection(t, build))
		if want == 0 {
			require.ErrorIs(t, err, store_pebble.ErrRebuildUnknownProvenance)
			require.Empty(t, rebuildEventSavedRows(t, build).edges)
			continue
		}
		require.NoError(t, err)
		require.EqualValues(t, want, offers)
		require.Equal(t, rebuildEventSQLiteRows(native).edges, rebuildEventSavedRows(t, build).edges)
	}
}

func TestRebuildGinMiddlewareGoExternalWitness(t *testing.T) {
	const file = "router.go"
	d := &graph.Node{ID: "dispatch", Kind: graph.KindMethod, Language: "python", FilePath: "dispatch.py",
		Meta: map[string]any{"gin_dispatcher": true, "gin_handlers": []string{"New"}}}
	goNode := &graph.Node{ID: "go", Name: "routes", Kind: graph.KindFunction, Language: "go", FilePath: file}
	const target = "external::github.com/gin-contrib/cors::New"
	call := &graph.Edge{From: goNode.ID, To: target, Kind: graph.EdgeCalls, FilePath: file}
	goFacts := rebuildEventFacts(file, "go", []*graph.Node{goNode}, call)
	dispatchFacts := rebuildEventFacts(d.FilePath, d.Language, []*graph.Node{d})
	build, native := openRebuildEventStores(t, goFacts, dispatchFacts)
	require.NoError(t, runRebuildGoExternalCalls(t.Context(), build))
	New(native).attributeGoExternalCalls()
	require.Equal(t, 1, ResolveGinMiddlewareCalls(native))
	before := rebuildEventSavedRows(t, build)
	selection := rebuildGinSelection(t, build)
	require.True(t, selection.allows(SynthGinMiddleware))

	badLanguage := *goNode
	badLanguage.Language = "golang"
	badFile := *goNode
	badFile.FilePath = "renamed.go"
	for _, receipt := range []store_pebble.ParseFacts{
		rebuildEventFacts(file, "", []*graph.Node{goNode}, call),
		rebuildEventFacts(file, "go", []*graph.Node{&badLanguage}, call),
		rebuildEventFacts(file, "go", []*graph.Node{&badFile}, call),
		rebuildEventFacts(badFile.FilePath, "go", []*graph.Node{&badFile}, call),
	} {
		offers, err := RunRebuildGinMiddleware(t.Context(), build, rebuildGRPCReceipts(receipt, dispatchFacts), selection)
		require.Zero(t, offers)
		require.ErrorIs(t, err, store_pebble.ErrRebuildUnknownProvenance)
		require.Equal(t, before, rebuildEventSavedRows(t, build))
	}
	offers, err := RunRebuildGinMiddleware(t.Context(), build, rebuildGRPCReceipts(goFacts, dispatchFacts), selection)
	require.NoError(t, err)
	require.EqualValues(t, 1, offers)
	require.Equal(t, rebuildEventSQLiteRows(native).edges, rebuildEventSavedRows(t, build).edges)
}

func TestRebuildGinMiddlewareIgnoresUnusedHandlerFields(t *testing.T) {
	const file = "gin.go"
	d := &graph.Node{ID: "dispatch", Kind: graph.KindMethod, Language: "go", FilePath: file,
		Meta: map[string]any{"gin_dispatcher": true, "gin_handlers": []string{"Handle"}}}
	h := &graph.Node{ID: "handler", Name: "Handle", Kind: graph.KindFunction, Language: "go", FilePath: file}
	facts := rebuildEventFacts(file, "go", []*graph.Node{d, h})
	build, native := openRebuildEventStores(t, facts)
	moved := *h
	moved.Language = "python"
	moved.FilePath = "elsewhere.py"
	moved.Kind = graph.KindMethod
	moved.StartLine = 90
	receipts := rebuildGRPCReceipts(rebuildGinOmit(facts, h), rebuildEventFacts(moved.FilePath, moved.Language, []*graph.Node{&moved}))
	offers, err := RunRebuildGinMiddleware(t.Context(), build, receipts, rebuildGinSelection(t, build))
	require.NoError(t, err)
	require.EqualValues(t, 1, offers)
	require.Equal(t, 1, ResolveGinMiddlewareCalls(native))
	require.Equal(t, rebuildEventSQLiteRows(native).edges, rebuildEventSavedRows(t, build).edges)
}
