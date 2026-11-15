package graph

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuiltinStubNodesParsesBothStubIDForms(t *testing.T) {
	edges := []*Edge{
		// Repo-prefixed form.
		{From: "web/src/a.ts::fn", To: "web::builtin::ts::array::push", Kind: EdgeCalls},
		// Solo-repo form: StubID with an empty repo prefix elides the
		// leading segment, so the ID starts at "builtin::".
		{From: "src/b.ts::fn", To: "builtin::ts::string::split", Kind: EdgeCalls},
		// Duplicate target within the batch — one node only.
		{From: "web/src/c.ts::fn", To: "web::builtin::ts::array::push", Kind: EdgeCalls},
		// Non-builtin targets are ignored.
		{From: "web/src/a.ts::fn", To: "web/src/d.ts::helper", Kind: EdgeCalls},
		{From: "web/src/a.ts::fn", To: "web::ext::lodash::map", Kind: EdgeCalls},
		// Malformed: category+name segments missing.
		{From: "web/src/a.ts::fn", To: "web::builtin::ts", Kind: EdgeCalls},
		nil,
	}

	stubs := BuiltinStubNodes(edges)
	require.Len(t, stubs, 2)

	byID := map[string]*Node{}
	for _, n := range stubs {
		byID[n.ID] = n
	}

	prefixed := byID["web::builtin::ts::array::push"]
	require.NotNil(t, prefixed)
	assert.Equal(t, KindBuiltin, prefixed.Kind)
	assert.Equal(t, "push", prefixed.Name)
	assert.Equal(t, "ts", prefixed.Language)
	assert.Equal(t, "web", prefixed.RepoPrefix)
	assert.Equal(t, true, prefixed.Meta["builtin"])
	assert.Equal(t, "array", prefixed.Meta["builtin_kind"])

	solo := byID["builtin::ts::string::split"]
	require.NotNil(t, solo)
	assert.Equal(t, KindBuiltin, solo.Kind)
	assert.Equal(t, "split", solo.Name)
	assert.Equal(t, "ts", solo.Language)
	assert.Equal(t, "", solo.RepoPrefix, "solo-repo stubs carry the unprefixed convention")
	assert.Equal(t, "string", solo.Meta["builtin_kind"])
}

// The in-memory funnel: AddBatch must materialize the stub node behind a
// builtin edge target, and a warm re-index of the same edge must not churn
// the node store with a duplicate upsert.
func TestGraphAddBatchMaterializesBuiltinStubs(t *testing.T) {
	g := New()
	caller := &Node{ID: "web/src/a.ts::fn", Kind: KindFunction, Name: "fn", FilePath: "web/src/a.ts", RepoPrefix: "web"}
	edge := &Edge{From: caller.ID, To: "web::builtin::ts::array::push", Kind: EdgeCalls, FilePath: "web/src/a.ts", Line: 3}

	g.AddBatch([]*Node{caller}, []*Edge{edge})

	stub := g.GetNode("web::builtin::ts::array::push")
	require.NotNil(t, stub, "builtin edge target must exist as a node after AddBatch")
	assert.Equal(t, KindBuiltin, stub.Kind)
	assert.Equal(t, "push", stub.Name)

	countAfterFirst := g.NodeCount()
	g.AddBatch(nil, []*Edge{{From: caller.ID, To: "web::builtin::ts::array::push", Kind: EdgeCalls, FilePath: "web/src/a.ts", Line: 9}})
	assert.Equal(t, countAfterFirst, g.NodeCount(), "re-adding an edge to a seen stub must not create nodes")
	require.NotNil(t, g.GetNode("web::builtin::ts::array::push"))
}

// Given function-shaped builtin IDs with no category segment, in any language,
// when the write funnel materializes them, then builtin_kind is "func", the
// value the Go attribution pass stores; category IDs keep their category.
func TestBuiltinStubNodesNamesFunctionBuiltins(t *testing.T) {
	stubs := BuiltinStubNodes([]*Edge{
		{From: "pkg/a.go::Run", To: "repo::builtin::go::append", Kind: EdgeCalls},
		{From: "pkg/a.go::Run", To: "builtin::go::len", Kind: EdgeCalls},
		{From: "pkg/a.py::run", To: "builtin::py::len", Kind: EdgeCalls},
		{From: "pkg/a.go::Run", To: "builtin::go::type::string", Kind: EdgeTypedAs},
		{From: "pkg/a.go::Run", To: "builtin::go::const::nil", Kind: EdgeReferences},
	})
	kinds := map[string]any{}
	for _, n := range stubs {
		kinds[n.ID] = n.Meta["builtin_kind"]
	}
	assert.Equal(t, map[string]any{
		"repo::builtin::go::append": "func",
		"builtin::go::len":          "func",
		"builtin::py::len":          "func",
		"builtin::go::type::string": "type",
		"builtin::go::const::nil":   "const",
	}, kinds)
}
