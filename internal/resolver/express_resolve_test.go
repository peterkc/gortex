package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func expressAnchor(g *graph.Graph, id, file string) {
	g.AddNode(&graph.Node{ID: id, Kind: graph.KindFunction, Name: "route handler", FilePath: file, Language: "javascript",
		Meta: map[string]any{"express_handler": true}})
}

func expressRef(g *graph.Graph, from, file, to string, meta map[string]any) {
	meta["express_handler_ref"] = true
	g.AddEdge(&graph.Edge{From: from, To: to, Kind: graph.EdgeCalls, FilePath: file, Meta: meta})
}

func expressClassMethod(g *graph.Graph, classID, methodID, file, class, method string) {
	g.AddNode(&graph.Node{ID: classID, Kind: graph.KindType, Name: class, FilePath: file})
	g.AddNode(&graph.Node{ID: methodID, Kind: graph.KindMethod, Name: method, FilePath: file})
	g.AddEdge(&graph.Edge{From: methodID, To: classID, Kind: graph.EdgeMemberOf})
}

func synthExpressEdge(g graph.Store, from, to string) *graph.Edge {
	for e := range g.EdgesByKind(graph.EdgeCalls) {
		if e == nil || e.From != from || e.To != to || e.Meta == nil {
			continue
		}
		if by, _ := e.Meta[MetaSynthesizedBy].(string); by == SynthExpressResolve {
			return e
		}
	}
	return nil
}

func TestResolveExpressHandlers_FixtureTree(t *testing.T) {
	g := graph.New()
	const anchor = "src/routes/users.js::express-handler@1"
	expressAnchor(g, anchor, "src/routes/users.js")

	// /middleware/auth.js exports `auth`; a decoy auth lives in /util/.
	convNode(g, "src/middleware/auth.js::auth", "src/middleware/auth.js", "auth")
	convNode(g, "src/util/auth.js::auth", "src/util/auth.js", "auth")
	expressClassMethod(g, "src/controllers/UserController.js::UserController", "src/controllers/UserController.js::UserController.list", "src/controllers/UserController.js", "UserController", "list")
	expressClassMethod(g, "src/services/UserService.js::UserService", "src/services/UserService.js::UserService.create", "src/services/UserService.js", "UserService", "create")

	expressRef(g, anchor, "src/routes/users.js", "unresolved::authMiddleware", map[string]any{"express_ref_name": "authMiddleware"})
	expressRef(g, anchor, "src/routes/users.js", "unresolved::list", map[string]any{"express_ref_class": "UserController", "express_ref_method": "list"})
	expressRef(g, anchor, "src/routes/users.js", "unresolved::create", map[string]any{"express_ref_class": "UserService", "express_ref_method": "create"})

	n := ResolveExpressHandlers(g)
	require.Equal(t, 3, n)

	// Middleware: suffix-stripped + /middleware/ preference beats the decoy.
	mw := synthExpressEdge(g, anchor, "src/middleware/auth.js::auth")
	require.NotNil(t, mw, "authMiddleware binds to /middleware/auth.js")
	assert.Nil(t, synthExpressEdge(g, anchor, "src/util/auth.js::auth"))
	// Controller + service methods.
	assert.NotNil(t, synthExpressEdge(g, anchor, "src/controllers/UserController.js::UserController.list"))
	assert.NotNil(t, synthExpressEdge(g, anchor, "src/services/UserService.js::UserService.create"))
}

func TestResolveExpressHandlers_UnresolvableLeftAlone(t *testing.T) {
	g := graph.New()
	const anchor = "r.js::express-handler@1"
	expressAnchor(g, anchor, "r.js")
	// No middleware definition anywhere.
	expressRef(g, anchor, "r.js", "unresolved::ghostMiddleware", map[string]any{"express_ref_name": "ghostMiddleware"})

	assert.Equal(t, 0, ResolveExpressHandlers(g))
	assert.Nil(t, synthExpressEdge(g, anchor, "unresolved::ghostMiddleware"))
}

func TestExpressMemberDirs(t *testing.T) {
	assert.Equal(t, []string{"/controllers/", "/controller/"}, expressMemberDirs("UserController"))
	assert.Equal(t, []string{"/services/", "/service/", "/helpers/", "/utils/"}, expressMemberDirs("UserService"))
	assert.Equal(t, []string{"/services/", "/service/", "/helpers/", "/utils/"}, expressMemberDirs("User"))
}
