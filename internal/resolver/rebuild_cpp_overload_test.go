package resolver

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func rebuildCppTestFn(id, params, shapes string, required int) *graph.Node {
	n := cppFn(id, params, shapes, required)
	n.QualName, n.RepoPrefix, n.Language, n.FilePath = id, "repo", "cpp", "lib/overloads.cpp"
	return n
}

func TestRebuildCppOverloadNativeRows(t *testing.T) {
	const callerFile = "src/caller.cpp"
	caller := &graph.Node{ID: "caller", Name: "caller", RepoPrefix: "repo", Language: "cpp", FilePath: callerFile,
		Kind: graph.KindFunction, Meta: map[string]any{MetaScopeNamespace: "Local"}}
	nodes := []*graph.Node{caller}
	var edges []*graph.Edge
	var wantIDs []string
	add := func(name, hints, want string, group ...*graph.Node) *graph.Edge {
		for _, n := range group {
			n.Name = name
			nodes = append(nodes, n)
		}
		e := &graph.Edge{From: caller.ID, To: "unresolved::" + name, Kind: graph.EdgeCalls, FilePath: callerFile,
			Line: len(edges) + 1, Meta: map[string]any{MetaScopeArgTypes: hints, "payload": "kept"}}
		setEdgeTerminal(e, terminalReasonNoDefinition)
		edges, wantIDs = append(edges, e), append(wantIDs, want)
		return e
	}
	fn := rebuildCppTestFn
	// Native candidate order is by language then ID, not insertion order.
	for _, order := range []struct{ name, left, right, best string }{
		{"DominantLast", "a-last", "b-last", "z-last"},
		{"DominantFirst", "z-first", "b-first", "a-first"},
	} {
		first := fn(order.left, "int,double", "v,v", 2)
		first.Meta[MetaScopeNamespace] = "Local"
		middle := fn(order.right, "double,int", "v,v", 2)
		middle.FilePath = callerFile
		best := fn(order.best, "int,int", "v,v", 2)
		best.Kind = graph.KindMethod
		add(order.name, "const char,char", best.ID, first, middle, best).Kind = graph.EdgeReads
	}
	incomparable := fn("z-incomparable", "double,int", "v,v", 2)
	incomparable.Meta[MetaScopeNamespace] = "Local"
	add("Incomparable", "char,char", incomparable.ID, fn("a-incomparable", "char,double", "v,v", 2), incomparable)

	equal := fn("z-equal", "Thing,Other", "v,v", 2)
	equal.Meta[MetaScopeNamespace] = "Assoc"
	add("Equal", "Assoc::Thing,?", equal.ID, fn("a-equal", "Thing,int", "v,v", 2), equal)

	nonviable := fn("z-nonviable", "double", "v", 1)
	nonviable.Meta[MetaScopeNamespace] = "Assoc"
	add("AllNonviable", "Assoc::Thing", nonviable.ID, fn("a-nonviable", "int", "v", 1), nonviable)
	add("OneViable", "int", "z-value", fn("a-pointer", "int", "p", 1), fn("z-value", "int", "v", 1))
	add("SoleArity", "Assoc::Thing", "z-sole-arity", fn("a-zero-arity", "", "", 0), fn("z-sole-arity", "int", "v", 1))

	zeroMatch := fn("z-zero-match", "double", "v", 1)
	zeroMatch.Meta[MetaScopeNamespace] = "Local"
	add("ZeroArityMatches", "", zeroMatch.ID, fn("a-zero-match", "int", "v", 1), zeroMatch)
	noHints := fn("z-no-hints", "double", "v", 0)
	noHints.Meta[MetaScopeNamespace] = "Local"
	add("NoHints", "", noHints.ID, fn("a-no-hints", "", "", 0), noHints)
	ellipsis := fn("a-ellipsis", "int", "v", 1)
	ellipsis.Meta["cpp_variadic"] = "1"
	add("Variadic", "int,char", "z-default", ellipsis, fn("z-default", "int,int,double", "v,v,v", 2))

	best := fn("z-filtered", "int,int", "v,v", 2)
	best.Language = "" // native C-family lookup also includes language-neutral rows
	lower := fn("a-filtered", "double,double", "v,v", 2)
	lower.Meta[MetaScopeNamespace] = "Local"
	barrel := fn("b-barrel", "char,char", "v,v", 2)
	barrel.Meta["reexport"] = true
	attributed := fn("repo::stdlib::C::Filtered", "char,char", "v,v", 2)
	plain := fn("c-not-generic", "char,char", "v,v", 2)
	foreign := fn("d-other-repo", "char,char", "v,v", 2)
	foreign.RepoPrefix = "other"
	otherLang := fn("e-other-language", "char,char", "v,v", 2)
	otherLang.Language = "go"
	for _, n := range []*graph.Node{best, lower, barrel, attributed, foreign, otherLang} {
		n.Meta["type_params"] = "T"
	}
	add("Filtered", "char,char", best.ID, best, lower, barrel, attributed, plain, foreign, otherLang).Meta[graph.MetaGenericInstantiation] = true

	ctx, b, opts := frontierFixture(t, 2, 0)
	opts.SpillBytes = 1 // the real scoped candidate page, with a forced sort spill
	b.AddBatch(nodes, edges)
	slices.Reverse(nodes)
	native := rebuildImportNativeRows(t, nodes, edges, nil)
	require.Len(t, native, len(edges))
	for _, e := range native {
		require.Equal(t, wantIDs[e.Line-1], e.To, "line %d", e.Line)
		require.Equal(t, graph.OriginASTResolved, e.Origin)
		require.Equal(t, 0.92, e.Confidence)
		require.Equal(t, "scope", e.Meta["resolution"])
		require.Equal(t, "kept", e.Meta["payload"])
		require.False(t, edgeTerminalFlag(e))
		if e.Line <= 2 {
			require.Equal(t, graph.EdgeReferences, e.Kind)
		}
	}
	require.NoError(t, runRebuildMain(ctx, b, opts, false, RebuildMainInputs{}))
	require.Equal(t, native, rebuildFnPointerSaved(t, b))
	checkFrontierScratch(t, opts.ScratchParent)
}

func TestRebuildCppOverloadReplayPageError(t *testing.T) {
	ctx, b, opts := frontierFixture(t, 1, 0)
	best := rebuildCppTestFn("best", "int", "v", 1)
	worse := rebuildCppTestFn("worse", "double", "v", 1)
	e := &graph.Edge{From: "caller", To: "unresolved::process", Kind: graph.EdgeCalls,
		Meta: map[string]any{MetaScopeArgTypes: "char"}}
	b.AddBatch([]*graph.Node{best, worse}, []*graph.Edge{e})
	before := rebuildFnPointerSaved(t, b)
	_, langs := resolverCompatibleLanguages("cpp")
	req := store_pebble.LinkRequest{Kind: store_pebble.LinkName, Key: "process", RepoPrefix: "repo", Languages: langs}
	fault := errors.New("candidate replay canceled after the first page")
	err := runRebuildLinkFrontier(ctx, b, opts, nil, func(f *rebuildLinkFrontier, page []*graph.Edge) error {
		require.Len(t, page, 1)
		state := rebuildCppOverload{argHints: scopePositionalArgHints(e.Meta)}
		stats, err := f.candidates(ctx, []store_pebble.LinkRequest{req}, func(_ int, n *graph.Node) error {
			if rebuildFunctionCandidateEligible(e, n) {
				state.consider(n)
			}
			return nil
		})
		require.NoError(t, err)
		require.Equal(t, 2, stats.ResultRows)
		replay, cancel := context.WithCancelCause(ctx)
		cancel(fault)
		pick, err := state.pick(replay, f, req, e)
		require.Empty(t, pick)
		return err
	})
	require.ErrorIs(t, err, store_pebble.ErrRebuildView)
	require.ErrorIs(t, err, store_pebble.ErrRebuildLink)
	require.ErrorIs(t, err, fault)
	require.Equal(t, before, rebuildFnPointerSaved(t, b))
	checkFrontierScratch(t, opts.ScratchParent)
}
