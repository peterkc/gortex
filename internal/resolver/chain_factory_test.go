package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/zzet/gortex/internal/graph"
)

func fn(id, name, file, retType string) *graph.Node {
	n := &graph.Node{ID: id, Kind: graph.KindFunction, Name: name, FilePath: file}
	if retType != "" {
		n.Meta = map[string]any{"return_type": retType}
	}
	return n
}

func method(id, name, file, recv, retType string) *graph.Node {
	m := map[string]any{"receiver": recv}
	if retType != "" {
		m["return_type"] = retType
	}
	return &graph.Node{ID: id, Kind: graph.KindMethod, Name: name, FilePath: file, Meta: m}
}

// TestResolveFactoryChains_CrossFile pins that a fluent chain whose return types
// and methods live in different files resolves the final method.
func TestResolveFactoryChains_CrossFile(t *testing.T) {
	g := graph.New()
	g.AddNode(fn("a.go::New", "New", "a.go", "Builder"))
	g.AddNode(&graph.Node{ID: "a.go::Builder", Kind: graph.KindType, Name: "Builder", FilePath: "a.go"})
	g.AddNode(method("a.go::Builder.With", "With", "a.go", "Builder", "Builder"))
	g.AddNode(method("a.go::Builder.Build", "Build", "a.go", "Builder", "Widget"))
	g.AddNode(&graph.Node{ID: "b.go::Widget", Kind: graph.KindType, Name: "Widget", FilePath: "b.go"})
	g.AddNode(method("b.go::Widget.Run", "Run", "b.go", "Widget", ""))
	g.AddNode(fn("main.go::main", "main", "main.go", ""))
	e := &graph.Edge{
		From: "main.go::main", To: "unresolved::*.Run", Kind: graph.EdgeCalls,
		FilePath: "main.go", Meta: map[string]any{"receiver_expr": "New().With(x).Build()"},
	}
	g.AddEdge(e)

	assert.Equal(t, 1, ResolveFactoryChains(g))
	assert.Equal(t, "b.go::Widget.Run", e.To)
	assert.Equal(t, "factory_chain", e.Meta["via"])
	assert.Equal(t, graph.OriginASTInferred, e.Origin)
}

// TestResolveFactoryChains_Conformance pins that a factory returning an
// interface resolves the chained method to a unique concrete implementor, with
// the conformance flag set.
func TestResolveFactoryChains_Conformance(t *testing.T) {
	g := graph.New()
	g.AddNode(fn("a.go::factory", "factory", "a.go", "Iface"))
	g.AddNode(&graph.Node{ID: "a.go::Iface", Kind: graph.KindInterface, Name: "Iface", FilePath: "a.go"})
	g.AddNode(&graph.Node{ID: "b.go::Impl", Kind: graph.KindType, Name: "Impl", FilePath: "b.go"})
	g.AddNode(method("b.go::Impl.bar", "bar", "b.go", "Impl", ""))
	g.AddEdge(&graph.Edge{From: "b.go::Impl", To: "a.go::Iface", Kind: graph.EdgeImplements})
	g.AddNode(fn("m.go::run", "run", "m.go", ""))
	e := &graph.Edge{
		From: "m.go::run", To: "unresolved::*.bar", Kind: graph.EdgeCalls,
		FilePath: "m.go", Meta: map[string]any{"receiver_expr": "factory()"},
	}
	g.AddEdge(e)

	assert.Equal(t, 1, ResolveFactoryChains(g))
	assert.Equal(t, "b.go::Impl.bar", e.To, "chained method binds to the concrete implementor")
	assert.Equal(t, true, e.Meta["conformance_walked"])
}

// TestResolveFactoryChains_AmbiguousConformanceDropped pins that two
// implementors declaring the method drop the edge.
func TestResolveFactoryChains_AmbiguousConformanceDropped(t *testing.T) {
	g := graph.New()
	g.AddNode(fn("a.go::factory", "factory", "a.go", "Iface"))
	g.AddNode(&graph.Node{ID: "a.go::Iface", Kind: graph.KindInterface, Name: "Iface", FilePath: "a.go"})
	g.AddNode(&graph.Node{ID: "b.go::A", Kind: graph.KindType, Name: "A", FilePath: "b.go"})
	g.AddNode(method("b.go::A.bar", "bar", "b.go", "A", ""))
	g.AddNode(&graph.Node{ID: "c.go::B", Kind: graph.KindType, Name: "B", FilePath: "c.go"})
	g.AddNode(method("c.go::B.bar", "bar", "c.go", "B", ""))
	g.AddEdge(&graph.Edge{From: "b.go::A", To: "a.go::Iface", Kind: graph.EdgeImplements})
	g.AddEdge(&graph.Edge{From: "c.go::B", To: "a.go::Iface", Kind: graph.EdgeImplements})
	e := &graph.Edge{
		From: "m.go::run", To: "unresolved::*.bar", Kind: graph.EdgeCalls,
		FilePath: "m.go", Meta: map[string]any{"receiver_expr": "factory()"},
	}
	g.AddNode(fn("m.go::run", "run", "m.go", ""))
	g.AddEdge(e)

	assert.Equal(t, 0, ResolveFactoryChains(g), "ambiguous implementor dropped")
}

// TestResolveFactoryChains_DoesNotOverrideResolved pins that an already-resolved
// (e.g. LSP-confirmed) edge is never re-targeted.
func TestResolveFactoryChains_DoesNotOverrideResolved(t *testing.T) {
	g := graph.New()
	g.AddNode(fn("a.go::New", "New", "a.go", "Widget"))
	g.AddNode(&graph.Node{ID: "a.go::Widget", Kind: graph.KindType, Name: "Widget", FilePath: "a.go"})
	g.AddNode(method("a.go::Widget.Run", "Run", "a.go", "Widget", ""))
	g.AddNode(method("lsp.go::Other.Run", "Run", "lsp.go", "Other", ""))
	e := &graph.Edge{
		From: "m.go::main", To: "lsp.go::Other.Run", Kind: graph.EdgeCalls,
		Origin: graph.OriginLSPResolved, FilePath: "m.go",
		Meta: map[string]any{"receiver_expr": "New()"},
	}
	g.AddEdge(e)

	assert.Equal(t, 0, ResolveFactoryChains(g))
	assert.Equal(t, "lsp.go::Other.Run", e.To, "resolved edge is not overridden")
}

func TestResolveFactoryChains_DirectTargetMustBeMethod(t *testing.T) {
	g := graph.New()
	g.AddNode(fn("make.go::New", "New", "make.go", "Widget"))
	n := method("widget.go::Run", "Run", "widget.go", "Widget", "")
	n.Kind = graph.KindFunction
	g.AddNode(n)
	e := &graph.Edge{
		From: "main.go::main", To: "unresolved::*.Run", Kind: graph.EdgeCalls,
		FilePath: "main.go", Meta: map[string]any{"receiver_expr": "New()"},
	}
	g.AddEdge(e)

	assert.Zero(t, ResolveFactoryChains(g))
	assert.Equal(t, "unresolved::*.Run", e.To)
}
