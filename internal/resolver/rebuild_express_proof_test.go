package resolver

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func rebuildExpressTestCall(caller *graph.Node, class, name string) *graph.Edge {
	e := &graph.Edge{
		From: caller.ID, To: "unresolved::" + name, Kind: graph.EdgeCalls, FilePath: caller.FilePath, Line: 2,
		Meta: map[string]any{"express_handler_ref": true},
	}
	if class == "" {
		e.Meta["express_ref_name"] = name
	} else {
		e.Meta["express_ref_class"], e.Meta["express_ref_method"] = class, name
	}
	return e
}

func TestRebuildExpressConventionProof(t *testing.T) {
	const lang = "javascript"
	for _, tc := range []struct {
		name, routeDir, firstDir, secondDir string
		change                              string
		want                                int
		refuse                              bool
	}{
		{name: "preferred tie by caller", routeDir: "src/middleware", firstDir: "src/middleware", secondDir: "other/middleware", want: 1},
		{name: "ordinary tie by caller", routeDir: "src/ordinary", firstDir: "src/ordinary", secondDir: "other/ordinary", want: 1},
		{name: "sole candidate without caller", routeDir: "src/routes", firstDir: "other/ordinary", change: "omit caller", want: 1},
		{name: "preferred without caller", routeDir: "src/routes", firstDir: "src/middleware", secondDir: "other/ordinary", change: "omit caller", want: 1},
		{name: "absent deciding caller", routeDir: "src/ordinary", firstDir: "src/ordinary", secondDir: "other/ordinary", change: "omit caller", want: 1, refuse: true},
		{name: "different deciding caller directory", routeDir: "src/ordinary", firstDir: "src/ordinary", secondDir: "other/ordinary", change: "caller moved", want: 1, refuse: true},
		{name: "receipt relocates lower tier into preferred tie", routeDir: "src/routes", firstDir: "src/middleware", secondDir: "other/ordinary", change: "second moved", want: 1, refuse: true},
		{name: "admission requires recording file", routeDir: "src/routes", firstDir: "src/middleware", change: "wrong recording file", want: 1, refuse: true},
		{name: "admission requires effective language", routeDir: "src/routes", firstDir: "src/middleware", change: "no language", want: 1, refuse: true},
		{name: "Call admission requires recording file", routeDir: "src/routes", firstDir: "src/middleware", change: "wrong Call recording file", want: 1, refuse: true},
		{name: "Call admission requires effective language", routeDir: "src/routes", firstDir: "src/middleware", change: "no Call language", want: 1, refuse: true},
		{name: "node language is not a handler role", routeDir: "src/routes", firstDir: "src/middleware", change: "node language", want: 1},
		{name: "duplicate declaration keeps winner", routeDir: "src/routes", firstDir: "src/middleware", change: "duplicate declaration", want: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			caller := rebuildEventNode(tc.routeDir+"/routes.js", "route", lang, "")
			first := rebuildEventNode(tc.firstDir+"/first.js", "auth", lang, "")
			call := rebuildExpressTestCall(caller, "", "authMiddleware")
			facts := []store_pebble.ParseFacts{
				rebuildEventFacts(caller.FilePath, lang, []*graph.Node{caller}, call),
				rebuildEventFacts(first.FilePath, lang, []*graph.Node{first}),
			}
			if tc.secondDir != "" {
				second := rebuildEventNode(tc.secondDir+"/second.js", "authMiddleware", lang, "")
				facts = append(facts, rebuildEventFacts(second.FilePath, lang, []*graph.Node{second}))
			}
			b, native := openRebuildEventStores(t, facts...)
			require.Equal(t, tc.want, ResolveExpressHandlers(native))
			want := rebuildEventSQLiteRows(native)
			require.Len(t, want.edges, 1)
			require.Equal(t, first.ID, want.edges[0].To)
			receipts := slices.Clone(facts)
			switch tc.change {
			case "omit caller":
				receipts[0].Nodes = nil
			case "caller moved":
				changed := *caller
				changed.FilePath = tc.secondDir + "/routes.js"
				receipts[0].Nodes = nil
				receipts = append(receipts, rebuildEventFacts(changed.FilePath, lang, []*graph.Node{&changed}))
			case "second moved":
				changed := *receipts[2].Nodes[0]
				changed.FilePath = "other/middleware/second.js"
				receipts[2] = rebuildEventFacts(changed.FilePath, "typescript", []*graph.Node{&changed})
			case "wrong recording file":
				receipts[1].File.FilePath = "wrong/first.js"
			case "no language":
				receipts[1].EffectiveLanguage = ""
			case "wrong Call recording file":
				receipts[0].File.FilePath = "wrong/routes.js"
			case "no Call language":
				receipts[0].EffectiveLanguage = ""
			case "node language":
				changed := *first
				changed.Language = "typescript"
				receipts[1].Nodes = []*graph.Node{&changed}
			case "duplicate declaration":
				changed := *first
				changed.FilePath = "other/middleware/first.js"
				// The last copy still wins. Receipt disagreement alone changes no edge.
				receipts = append(receipts, rebuildEventFacts(changed.FilePath, lang, []*graph.Node{&changed}))
			}
			before := rebuildEventSavedRows(t, b)
			stats, err := RunRebuildExpress(t.Context(), b, rebuildGRPCReceipts(receipts...), rebuildExpressTestSelection(t))
			if tc.refuse {
				require.ErrorIs(t, err, store_pebble.ErrRebuildExpress)
				require.ErrorIs(t, err, store_pebble.ErrRebuildUnknownProvenance)
				require.Zero(t, stats)
				require.Equal(t, before, rebuildEventSavedRows(t, b))
				return
			}
			require.NoError(t, err)
			require.Equal(t, store_pebble.RebuildExpressStats{Stubs: 1, Moves: 1}, stats)
			require.Equal(t, want, rebuildEventSavedRows(t, b))
		})
	}
}

func TestRebuildExpressMethodDecisionProof(t *testing.T) {
	const lang, classFile = "javascript", "src/controllers/wanted.js"
	caller := rebuildEventNode("src/routes/router.js", "route", lang, "")
	class := rebuildEventNode(classFile, "WantedController", lang, "")
	class.Kind = graph.KindType
	call := rebuildExpressTestCall(caller, class.Name, "list")
	outside := rebuildEventNode("a.js", "list", lang, "")
	first := rebuildEventNode(classFile, "WantedController.a", lang, "")
	last := rebuildEventNode(classFile, "WantedController.z", lang, "")
	for _, m := range []*graph.Node{first, last} {
		m.Kind, m.Name = graph.KindMethod, "list"
	}
	member := func(m *graph.Node) *graph.Edge {
		return &graph.Edge{From: m.ID, To: class.ID, Kind: graph.EdgeMemberOf, FilePath: m.FilePath, Line: 3}
	}
	for _, change := range []string{"exact", "unproved losing owner", "unproved winning owner", "singleton misses class witness"} {
		t.Run(change, func(t *testing.T) {
			decl := rebuildEventFacts(classFile, lang, []*graph.Node{class, last, first}, member(last), member(first))
			other := rebuildEventFacts(outside.FilePath, lang, []*graph.Node{outside}, member(outside))
			if change == "unproved losing owner" {
				other = rebuildEventFacts(outside.FilePath, lang, nil)
			}
			route := rebuildEventFacts(caller.FilePath, lang, []*graph.Node{caller}, call)
			b, native := openRebuildEventStores(t, route, decl, other)
			require.Equal(t, 1, ResolveExpressHandlers(native))
			want := rebuildEventSQLiteRows(native)
			var destination string
			for _, e := range want.edges {
				if e.Kind == graph.EdgeCalls {
					destination = e.To
				}
			}
			require.Equal(t, first.ID, destination)
			switch change {
			case "unproved losing owner":
				decl.Edges = decl.Edges[1:]
			case "unproved winning owner":
				decl.Edges = decl.Edges[:1]
			case "singleton misses class witness":
				decl.Nodes = []*graph.Node{first}
				other.Nodes = nil
			}
			before := rebuildEventSavedRows(t, b)
			_, err := RunRebuildExpress(t.Context(), b, rebuildGRPCReceipts(route, decl, other), rebuildExpressTestSelection(t))
			if change == "unproved winning owner" || change == "singleton misses class witness" {
				require.ErrorIs(t, err, store_pebble.ErrRebuildUnknownProvenance)
				require.Equal(t, before, rebuildEventSavedRows(t, b))
				return
			}
			require.NoError(t, err)
			require.Equal(t, want, rebuildEventSavedRows(t, b))
		})
	}
}

func TestRebuildExpressFirstCallReceipt(t *testing.T) {
	const file, lang = "src/routes.js", "javascript"
	caller := rebuildEventNode(file, "route", lang, "")
	method := rebuildEventNode(file, "B.list", lang, "")
	method.Kind, method.Name = graph.KindMethod, "list"
	member := &graph.Edge{From: method.ID, To: file + "::B", Kind: graph.EdgeMemberOf, FilePath: file, Line: 3}
	a, b := rebuildExpressTestCall(caller, "A", "list"), rebuildExpressTestCall(caller, "B", "list")
	untagged := *a
	untagged.Meta = nil
	for _, tc := range []struct {
		name        string
		first, last *graph.Edge
		refuse      bool
	}{
		{"private move disagrees", a, b, true},
		{"private miss disagrees", b, a, true},
		{"identical duplicates", b, b, false},
		{"first non-handler and last misses", &untagged, a, false},
		{"first non-handler and last binds", &untagged, b, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			facts := rebuildEventFacts(file, lang, []*graph.Node{caller, method}, member, tc.first, tc.last)
			build, native := openRebuildEventStores(t, facts)
			nativeBefore := rebuildEventSQLiteRows(native)
			require.Equal(t, nativeBefore.edges, rebuildEventSavedRows(t, build).edges)
			// Disagree at the private graph cut, not at parse insertion.
			build.AddEdge(tc.last)
			before := rebuildEventSavedRows(t, build)
			var firstClass, lastClass any
			for _, e := range nativeBefore.edges {
				if e.Kind == graph.EdgeCalls {
					firstClass = e.Meta["express_ref_class"]
				}
			}
			for _, e := range before.edges {
				if e.Kind == graph.EdgeCalls {
					lastClass = e.Meta["express_ref_class"]
				}
			}
			require.Equal(t, tc.first.Meta["express_ref_class"], firstClass)
			require.Equal(t, tc.last.Meta["express_ref_class"], lastClass)
			stats, err := RunRebuildExpress(t.Context(), build, rebuildGRPCReceipts(facts), rebuildExpressTestSelection(t))
			if tc.refuse {
				require.ErrorIs(t, err, store_pebble.ErrRebuildExpress)
				require.ErrorIs(t, err, store_pebble.ErrRebuildUnknownProvenance)
				require.Zero(t, stats)
				require.Equal(t, before, rebuildEventSavedRows(t, build))
				return
			}
			require.NoError(t, err)
			if tc.first == &untagged {
				require.Equal(t, store_pebble.RebuildExpressStats{Stubs: 1}, stats)
				require.Zero(t, ResolveExpressHandlers(native))
				require.Equal(t, nativeBefore, rebuildEventSQLiteRows(native))
				require.Equal(t, before, rebuildEventSavedRows(t, build))
				return
			}
			require.Equal(t, store_pebble.RebuildExpressStats{Stubs: 1, Moves: 1}, stats)
			require.Equal(t, 1, ResolveExpressHandlers(native))
			require.Equal(t, rebuildEventSQLiteRows(native), rebuildEventSavedRows(t, build))
		})
	}
}

func TestRebuildExpressCensus(t *testing.T) {
	const file, lang = "routes.js", "javascript"
	caller := rebuildEventNode(file, "route", lang, "")
	for _, tc := range []struct {
		name string
		kind graph.EdgeKind
		to   string
		tag  bool
		want bool
	}{
		{"tagged unresolved Call", graph.EdgeCalls, "unresolved::auth", true, true},
		{"tagged settled Call", graph.EdgeCalls, file + "::auth", true, false},
		{"untagged unresolved Call", graph.EdgeCalls, "unresolved::auth", false, false},
		{"tagged unresolved reference", graph.EdgeReferences, "unresolved::auth", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			call := rebuildExpressTestCall(caller, "", "auth")
			call.Kind, call.To = tc.kind, tc.to
			if !tc.tag {
				delete(call.Meta, "express_handler_ref")
			}
			build := openRebuildGRPCBuild(t, rebuildEventFacts(file, lang, []*graph.Node{caller}, call))
			frozen, err := FreezeRebuildFrameworkSelection(t.Context(), build.RebuildView(), rebuildExpressTestSelection(t))
			require.NoError(t, err)
			require.Equal(t, tc.want, frozen.allows(SynthExpressResolve))
		})
	}
}

func rebuildExpressTestSelection(t *testing.T) FrameworkSynthesizerSelection {
	t.Helper()
	selected, err := NewFrameworkSynthesizerSelection([]string{SynthExpressResolve})
	require.NoError(t, err)
	return selected
}
