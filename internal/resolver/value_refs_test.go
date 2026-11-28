package resolver

import (
	"iter"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func valueRefCandidate(g graph.Store, from, name, file string, line int) {
	g.AddEdge(&graph.Edge{
		From: from, To: "unresolved::valueref::" + name, Kind: graph.EdgeReads,
		FilePath: file, Line: line, Origin: graph.OriginSpeculative,
		Meta: map[string]any{"via": valueRefCandidateVia, "name": name},
	})
}

func readsEdge(g graph.Store, from, to string) *graph.Edge {
	for _, e := range g.GetInEdges(to) {
		if e.From == from && e.Kind == graph.EdgeReads && e.Meta != nil {
			if v, _ := e.Meta["via"].(string); v == valueRefVia {
				return e
			}
		}
	}
	return nil
}

// TestValueRefConstReaderImpactRadius is the C2 named test: a function that
// reads a distinctive same-file constant gains a tiered EdgeReads to it, so the
// reader appears in the constant's impact radius (incoming non-Defines/MemberOf
// edges) — which blast-radius analysis walks.
func TestValueRefConstReaderImpactRadius(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{
		ID: "cfg.go::MAX_RETRIES", Kind: graph.KindConstant, Name: "MAX_RETRIES",
		FilePath: "cfg.go", StartLine: 3, Language: "go",
	})
	g.AddNode(&graph.Node{
		ID: "cfg.go::DoWork", Kind: graph.KindFunction, Name: "DoWork",
		FilePath: "cfg.go", StartLine: 10, Language: "go",
	})
	g.AddNode(&graph.Node{
		ID: "cfg.go::lower", Kind: graph.KindConstant, Name: "ab",
		FilePath: "cfg.go", StartLine: 4, Language: "go",
	})
	valueRefCandidate(g, "cfg.go::DoWork", "MAX_RETRIES", "cfg.go", 12)
	// A short / non-distinctive name must NOT bind.
	valueRefCandidate(g, "cfg.go::DoWork", "ab", "cfg.go", 13)

	n := ResolveValueRefs(g)
	assert.Equal(t, 1, n, "only the distinctive constant read binds")

	e := readsEdge(g, "cfg.go::DoWork", "cfg.go::MAX_RETRIES")
	require.NotNil(t, e, "reader should gain a value-ref EdgeReads to the constant")
	assert.Equal(t, graph.OriginASTResolved, e.Origin, "the read must ride a provenance tier")

	// Impact-radius property: the reader is among the constant's incoming
	// (non-Defines/MemberOf) edges, which fillImpactLive walks.
	var inRadius bool
	for _, in := range g.GetInEdges("cfg.go::MAX_RETRIES") {
		if in.From == "cfg.go::DoWork" && in.Kind != graph.EdgeDefines && in.Kind != graph.EdgeMemberOf {
			inRadius = true
		}
	}
	assert.True(t, inRadius, "DoWork must appear in MAX_RETRIES' impact radius")
}

// TestValueRefShadowAndSelfPruned confirms a same-file parameter shadows the
// constant (no bind) and a constant never reads itself.
func TestValueRefShadowAndSelfPruned(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{
		ID: "f.go::TIMEOUT", Kind: graph.KindConstant, Name: "TIMEOUT",
		FilePath: "f.go", StartLine: 2, Language: "go",
	})
	g.AddNode(&graph.Node{
		ID: "f.go::Run", Kind: graph.KindFunction, Name: "Run", FilePath: "f.go", StartLine: 5, Language: "go",
	})
	g.AddNode(&graph.Node{
		ID: "f.go::Run.TIMEOUT", Kind: graph.KindParam, Name: "TIMEOUT", FilePath: "f.go", StartLine: 5, Language: "go",
	})
	valueRefCandidate(g, "f.go::Run", "TIMEOUT", "f.go", 6)     // shadowed by the param
	valueRefCandidate(g, "f.go::TIMEOUT", "TIMEOUT", "f.go", 2) // self-read

	assert.Equal(t, 0, ResolveValueRefs(g), "shadowed and self reads must be pruned")
}

// TestValueRefInnerLocalShadowPruned pins the declarator-census shadow gate: an
// inner-scope local (`let TIMEOUT` / `TIMEOUT := …`) materialised as a KindLocal
// shadows the file-scope constant of the same name, so a candidate read inside
// that scope must NOT be bound to the constant (it might read the local). A
// second file with no shadowing local still binds, proving the gate is
// shadow-specific, not a blanket drop.
func TestValueRefInnerLocalShadowPruned(t *testing.T) {
	g := graph.New()
	// File a.go: file-scope const shadowed by an inner-scope local of the
	// same name → the read must stay unbound.
	g.AddNode(&graph.Node{
		ID: "a.go::RETRY_LIMIT", Kind: graph.KindConstant, Name: "RETRY_LIMIT",
		FilePath: "a.go", StartLine: 2, Language: "go",
	})
	g.AddNode(&graph.Node{
		ID: "a.go::Run", Kind: graph.KindFunction, Name: "Run", FilePath: "a.go", StartLine: 5, Language: "go",
	})
	g.AddNode(&graph.Node{
		ID: "a.go::Run#RETRY_LIMIT", Kind: graph.KindLocal, Name: "RETRY_LIMIT", FilePath: "a.go", StartLine: 6, Language: "go",
	})
	valueRefCandidate(g, "a.go::Run", "RETRY_LIMIT", "a.go", 7) // reads the inner local, not the const

	// File b.go: same constant shape but no shadowing local → binds.
	g.AddNode(&graph.Node{
		ID: "b.go::RETRY_LIMIT", Kind: graph.KindConstant, Name: "RETRY_LIMIT",
		FilePath: "b.go", StartLine: 2, Language: "go",
	})
	g.AddNode(&graph.Node{
		ID: "b.go::Go", Kind: graph.KindFunction, Name: "Go", FilePath: "b.go", StartLine: 5, Language: "go",
	})
	valueRefCandidate(g, "b.go::Go", "RETRY_LIMIT", "b.go", 6) // binds to the const

	assert.Equal(t, 1, ResolveValueRefs(g), "only the un-shadowed read should bind")
	require.NotNil(t, readsEdge(g, "b.go::Go", "b.go::RETRY_LIMIT"), "un-shadowed read must bind to the constant")
	assert.Nil(t, readsEdge(g, "a.go::Run", "a.go::RETRY_LIMIT"), "inner-local-shadowed read must stay unbound")
}

// TestValueRefReaderScopeSpecific pins the recall recovery: a constant read by
// function A binds even when an unrelated function B in the same file declares a
// same-named local, while a read inside B (which itself rebinds the name) is
// dropped — reader-scope specificity the old file-wide census lacked.
func TestValueRefReaderScopeSpecific(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "c.go::LIMIT_X", Kind: graph.KindConstant, Name: "LIMIT_X", FilePath: "c.go", StartLine: 2, Language: "go"})
	g.AddNode(&graph.Node{ID: "c.go::A", Kind: graph.KindFunction, Name: "A", FilePath: "c.go", StartLine: 5, Language: "go"})
	g.AddNode(&graph.Node{ID: "c.go::B", Kind: graph.KindFunction, Name: "B", FilePath: "c.go", StartLine: 10, Language: "go"})
	g.AddNode(&graph.Node{ID: "c.go::B#LIMIT_X", Kind: graph.KindLocal, Name: "LIMIT_X", FilePath: "c.go", StartLine: 11, Language: "go"})
	valueRefCandidate(g, "c.go::A", "LIMIT_X", "c.go", 6)  // A has no local → binds
	valueRefCandidate(g, "c.go::B", "LIMIT_X", "c.go", 12) // B rebinds locally → dropped

	assert.Equal(t, 1, ResolveValueRefs(g))
	require.NotNil(t, readsEdge(g, "c.go::A", "c.go::LIMIT_X"), "A's read binds despite B's unrelated local")
	assert.Nil(t, readsEdge(g, "c.go::B", "c.go::LIMIT_X"), "B's read is shadowed by its own local")
}

// TestValueRefConditionalDef pins that a name with two file-scope declarators (a
// try/except / #[cfg] conditional def) binds the read to the nearest preceding
// declarator and stamps conditional_def.
func TestValueRefConditionalDef(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "cfg.py::API_URL@3", Kind: graph.KindVariable, Name: "API_URL", FilePath: "cfg.py", StartLine: 3, Language: "python"})
	g.AddNode(&graph.Node{ID: "cfg.py::API_URL@6", Kind: graph.KindVariable, Name: "API_URL", FilePath: "cfg.py", StartLine: 6, Language: "python"})
	g.AddNode(&graph.Node{ID: "cfg.py::use", Kind: graph.KindFunction, Name: "use", FilePath: "cfg.py", StartLine: 10, Language: "python"})
	valueRefCandidate(g, "cfg.py::use", "API_URL", "cfg.py", 12)

	assert.Equal(t, 1, ResolveValueRefs(g))
	e := readsEdge(g, "cfg.py::use", "cfg.py::API_URL@6")
	require.NotNil(t, e, "binds to the nearest preceding conditional declarator")
	assert.Equal(t, true, e.Meta["conditional_def"])
}

type valueRefProjectionTestStore struct {
	graph.Store
	candidates        []*graph.Edge
	scanCalls         int
	edgesByKindCalls  int
	reindexOldTargets []string
}

func (s *valueRefProjectionTestStore) ValueRefPlaceholderEdges() iter.Seq[*graph.Edge] {
	s.scanCalls++
	return func(yield func(*graph.Edge) bool) {
		for _, edge := range s.candidates {
			if !yield(edge) {
				return
			}
		}
	}
}

func (s *valueRefProjectionTestStore) EdgesByKind(kind graph.EdgeKind) iter.Seq[*graph.Edge] {
	s.edgesByKindCalls++
	return s.Store.EdgesByKind(kind)
}

func (s *valueRefProjectionTestStore) ReindexEdges(batch []graph.EdgeReindex) {
	for _, mutation := range batch {
		s.reindexOldTargets = append(s.reindexOldTargets, mutation.OldTo)
	}
	s.Store.ReindexEdges(batch)
}

type valueRefProjectionFixture struct {
	graph               *graph.Graph
	first, second       *graph.Edge
	staleVia, wrongKind *graph.Edge
}

func newValueRefProjectionFixture() valueRefProjectionFixture {
	g := graph.New()
	for _, node := range []*graph.Node{
		{ID: "cfg.go::ALPHA_LIMIT", Kind: graph.KindConstant, Name: "ALPHA_LIMIT", FilePath: "cfg.go", StartLine: 2, Language: "go"},
		{ID: "cfg.go::BETA_LIMIT", Kind: graph.KindConstant, Name: "BETA_LIMIT", FilePath: "cfg.go", StartLine: 3, Language: "go"},
		{ID: "cfg.go::First", Kind: graph.KindFunction, Name: "First", FilePath: "cfg.go", StartLine: 10, Language: "go"},
		{ID: "cfg.go::Second", Kind: graph.KindFunction, Name: "Second", FilePath: "cfg.go", StartLine: 20, Language: "go"},
		{ID: "cfg.go::Stale", Kind: graph.KindFunction, Name: "Stale", FilePath: "cfg.go", StartLine: 30, Language: "go"},
		{ID: "cfg.go::WrongKind", Kind: graph.KindFunction, Name: "WrongKind", FilePath: "cfg.go", StartLine: 40, Language: "go"},
	} {
		g.AddNode(node)
	}
	first := &graph.Edge{
		From: "cfg.go::First", To: "unresolved::valueref::ALPHA_LIMIT", Kind: graph.EdgeReads,
		FilePath: "cfg.go", Line: 12, Origin: graph.OriginSpeculative,
		Meta: map[string]any{"via": valueRefCandidateVia, "name": "ALPHA_LIMIT"},
	}
	second := &graph.Edge{
		From: "cfg.go::Second", To: "unresolved::valueref::BETA_LIMIT", Kind: graph.EdgeReads,
		FilePath: "cfg.go", Line: 22, Origin: graph.OriginSpeculative,
		Meta: map[string]any{"via": valueRefCandidateVia, "name": "BETA_LIMIT"},
	}
	staleVia := &graph.Edge{
		From: "cfg.go::Stale", To: "unresolved::valueref::ALPHA_LIMIT", Kind: graph.EdgeReads,
		FilePath: "cfg.go", Line: 32, Origin: graph.OriginSpeculative,
		Meta: map[string]any{"via": valueRefVia, "name": "ALPHA_LIMIT"},
	}
	wrongKind := &graph.Edge{
		From: "cfg.go::WrongKind", To: "unresolved::valueref::ALPHA_LIMIT", Kind: graph.EdgeCalls,
		FilePath: "cfg.go", Line: 42, Origin: graph.OriginSpeculative,
		Meta: map[string]any{"via": valueRefCandidateVia, "name": "ALPHA_LIMIT"},
	}
	for _, edge := range []*graph.Edge{first, second, staleVia, wrongKind} {
		g.AddEdge(edge)
	}
	return valueRefProjectionFixture{
		graph: g, first: first, second: second, staleVia: staleVia, wrongKind: wrongKind,
	}
}

// TestValueRefProjectionParityStaleAndOrder proves the optional candidate scan
// is semantics-identical to the EdgeReads fallback, stale/wrong-kind rows are
// revalidated away, and the framework adapter preserves the scanner's order.
func TestValueRefProjectionParityStaleAndOrder(t *testing.T) {
	fallback := newValueRefProjectionFixture()
	require.Equal(t, 2, ResolveValueRefs(fallback.graph))

	projectedFixture := newValueRefProjectionFixture()
	projected := &valueRefProjectionTestStore{
		Store: projectedFixture.graph,
		// Deliberately reverse the live candidates and interleave stale rows.
		candidates: []*graph.Edge{
			projectedFixture.wrongKind,
			projectedFixture.staleVia,
			projectedFixture.second,
			projectedFixture.first,
		},
	}
	batchStore := newFrameworkEdgeBatchStore(projected)
	require.Equal(t, 2, ResolveValueRefs(batchStore))
	assert.Equal(t, 1, projected.scanCalls)
	assert.Zero(t, projected.edgesByKindCalls, "projected path must not decode the whole EdgeReads kind")
	assert.Equal(t, []string{
		"unresolved::valueref::BETA_LIMIT",
		"unresolved::valueref::ALPHA_LIMIT",
	}, projected.reindexOldTargets, "projection and framework adapter must preserve candidate order")

	for _, store := range []graph.Store{fallback.graph, projectedFixture.graph} {
		require.NotNil(t, readsEdge(store, "cfg.go::First", "cfg.go::ALPHA_LIMIT"))
		require.NotNil(t, readsEdge(store, "cfg.go::Second", "cfg.go::BETA_LIMIT"))
	}
	assert.Equal(t, "unresolved::valueref::ALPHA_LIMIT", projectedFixture.staleVia.To)
	assert.Equal(t, valueRefVia, projectedFixture.staleVia.Meta["via"])
	assert.Equal(t, "unresolved::valueref::ALPHA_LIMIT", projectedFixture.wrongKind.To)
	assert.Equal(t, graph.EdgeCalls, projectedFixture.wrongKind.Kind)
}

func TestValueRefEmptyReaderDoesNotShadow(t *testing.T) {
	// A separator at byte zero does not make the local a child of an empty ID.
	local := &graph.Node{ID: "#LIMIT_KEY", Kind: graph.KindLocal, Name: "LIMIT_KEY"}
	require.False(t, valueRefShadowsReader("", local.ID))
	require.False(t, valueRefReaderShadowed("", []*graph.Node{local}))
}

func TestValueRefDeclarationElectionRetainsEncounterOrder(t *testing.T) {
	decls := func(yield func(string, int) bool) {
		if yield("z", 5) && yield("a", 12) {
			yield("b", 5)
		}
	}
	for _, tc := range []struct {
		line int
		want string
	}{
		{2, "z"}, {5, "b"}, {10, "b"}, {12, "a"},
	} {
		id, conditional := electValueRefDeclaration(tc.line, decls)
		require.Equal(t, tc.want, id)
		require.True(t, conditional)
	}
	id, conditional := electValueRefDeclaration(5, func(yield func(string, int) bool) {
		yield("only", 9)
	})
	require.Equal(t, "only", id)
	require.False(t, conditional)
	id, conditional = electValueRefDeclaration(5, func(func(string, int) bool) {})
	require.Empty(t, id)
	require.False(t, conditional)
}
