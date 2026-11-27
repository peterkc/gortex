package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func mediatrHandler(g *graph.Graph, id, file, reqType, kind string) {
	g.AddNode(&graph.Node{ID: id, Kind: graph.KindMethod, Name: "Handle", FilePath: file, Language: "csharp",
		Meta: map[string]any{"mediatr_request_type": reqType, "mediatr_kind": kind}})
}

func mediatrSend(g *graph.Graph, fromID, file, reqType, kind string) {
	if g.GetNode(fromID) == nil {
		g.AddNode(&graph.Node{ID: fromID, Kind: graph.KindMethod, Name: lastSeg(fromID), FilePath: file, Language: "csharp"})
	}
	g.AddEdge(&graph.Edge{From: fromID, To: "unresolved::*.Handle", Kind: graph.EdgeCalls, FilePath: file,
		Meta: map[string]any{"via": mediatrVia, "mediatr_request_type": reqType, "mediatr_kind": kind}})
}

func synthMediatREdge(g graph.Store, from, to string) *graph.Edge {
	for e := range g.EdgesByKind(graph.EdgeCalls) {
		if e == nil || e.From != from || e.To != to || e.Meta == nil {
			continue
		}
		if by, _ := e.Meta[MetaSynthesizedBy].(string); by == SynthMediatR {
			return e
		}
	}
	return nil
}

func TestResolveMediatRCalls_SendBindsSingleHandler(t *testing.T) {
	g := graph.New()
	mediatrHandler(g, "App.cs::CreateOrderHandler.Handle", "App.cs", "CreateOrder", "request")
	mediatrSend(g, "App.cs::Controller.Place", "App.cs", "CreateOrder", "request")

	n := ResolveMediatRCalls(g)
	require.Equal(t, 1, n)
	e := synthMediatREdge(g, "App.cs::Controller.Place", "App.cs::CreateOrderHandler.Handle")
	require.NotNil(t, e)
	assert.Equal(t, ConfidenceTyped, e.Confidence)
	assert.Equal(t, ProvenanceFramework, e.Meta[MetaProvenance])
}

func TestResolveMediatRCalls_PublishFansOut(t *testing.T) {
	g := graph.New()
	mediatrHandler(g, "App.cs::EmailHandler.Handle", "App.cs", "OrderPlaced", "notification")
	mediatrHandler(g, "App.cs::SmsHandler.Handle", "App.cs", "OrderPlaced", "notification")
	mediatrSend(g, "App.cs::Controller.Place", "App.cs", "OrderPlaced", "notification")

	n := ResolveMediatRCalls(g)
	require.Equal(t, 2, n, "a notification fans out to every handler")
	assert.NotNil(t, synthMediatREdge(g, "App.cs::Controller.Place", "App.cs::EmailHandler.Handle"))
	assert.NotNil(t, synthMediatREdge(g, "App.cs::Controller.Place", "App.cs::SmsHandler.Handle"))
}

func TestResolveMediatRCalls_RequestKindIsolated(t *testing.T) {
	// A Send (request) must not bind to a same-typed notification handler.
	g := graph.New()
	mediatrHandler(g, "App.cs::NotifHandler.Handle", "App.cs", "Thing", "notification")
	mediatrSend(g, "App.cs::C.M", "App.cs", "Thing", "request")

	assert.Equal(t, 0, ResolveMediatRCalls(g))
	assert.Nil(t, synthMediatREdge(g, "App.cs::C.M", "App.cs::NotifHandler.Handle"))
}

func TestResolveMediatRCalls_UnknownRequestStaysPlaceholder(t *testing.T) {
	g := graph.New()
	mediatrHandler(g, "App.cs::H.Handle", "App.cs", "Known", "request")
	mediatrSend(g, "App.cs::C.M", "App.cs", "Ghost", "request")

	assert.Equal(t, 0, ResolveMediatRCalls(g))
}

func TestResolveMediatRCalls_InvalidHandlerKindDoesNotActivate(t *testing.T) {
	const file = "App.cs"
	for _, kind := range []string{"", "unexpected"} {
		g := graph.New()
		mediatrHandler(g, "handler", file, "Message", kind)
		call := &graph.Edge{From: "caller", To: "settled", Kind: graph.EdgeCalls, FilePath: file,
			Meta: map[string]any{"via": mediatrVia, "mediatr_kind": "request", "mediatr_request_type": "Message"}}
		g.AddEdge(call)
		want := *call
		require.Zero(t, ResolveMediatRCalls(g))
		var got []*graph.Edge
		for e := range g.EdgesByKind(graph.EdgeCalls) {
			got = append(got, e)
		}
		require.Equal(t, []*graph.Edge{&want}, got)
	}
}
