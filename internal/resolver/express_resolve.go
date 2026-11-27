package resolver

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// Express middleware/controller/service name-resolution. A route's named
// handler args — `app.get('/u', authMiddleware, UserController.list)` — are
// stamped by the JS/TS extractor as placeholder refs from the route's
// handler anchor (Meta["express_handler_ref"]). This pass binds them by the
// directory-convention heuristic: a middleware ident → a definition under
// /middleware/, an `XController.method` → the method on the XController class
// (preferring /controllers/), an `XService.method` → /services/, /helpers/.

// expressControllerDirs / expressServiceDirs / expressMiddlewareDirs are the
// conventional directories each handler shape prefers.
var (
	expressMiddlewareDirs = []string{"/middleware/", "/middlewares/"}
	expressControllerDirs = []string{"/controllers/", "/controller/"}
	expressServiceDirs    = []string{"/services/", "/service/", "/helpers/", "/utils/"}
)

// ResolveExpressHandlers binds express named-handler placeholder refs to
// their definitions by convention. Returns the number bound.
func ResolveExpressHandlers(g graph.Store) int {
	if g == nil {
		return 0
	}
	classMethods := expressClassMethodIndex(g)

	resolved := 0
	var reindex []graph.EdgeReindex
	for e := range g.EdgesByKind(graph.EdgeCalls) {
		cls, method, name, ok := expressHandlerKey(e)
		if !ok {
			continue
		}
		fromFile := ""
		if n := g.GetNode(e.From); n != nil {
			fromFile = n.FilePath
		}

		var targetID string
		if cls != "" {
			targetID = expressResolveMember(g, cls, method, fromFile, classMethods)
		} else if name != "" {
			targetID, _ = ResolveByConvention(g, name, "Middleware", expressMiddlewareDirs, fromFile)
		}
		oldTo := e.To
		if rebindExpressHandler(e, targetID) {
			reindex = append(reindex, graph.EdgeReindex{Edge: e, OldTo: oldTo})
			resolved++
		}
	}
	if len(reindex) > 0 {
		g.ReindexEdges(reindex)
	}
	return resolved
}

func expressHandlerKey(e *graph.Edge) (class, method, name string, ok bool) {
	if e == nil || !graph.IsUnresolvedTarget(e.To) {
		return "", "", "", false
	}
	if _, ok := e.Meta["express_handler_ref"]; !ok {
		return "", "", "", false
	}
	class, _ = e.Meta["express_ref_class"].(string)
	if class != "" {
		method, _ = e.Meta["express_ref_method"].(string)
	} else {
		name, _ = e.Meta["express_ref_name"].(string)
	}
	return class, method, name, true
}

func rebindExpressHandler(e *graph.Edge, target string) bool {
	if target == "" {
		return false
	}
	e.To = target
	e.Origin = graph.OriginASTInferred
	e.Confidence = 0.85
	e.ConfidenceLabel = graph.ConfidenceLabelFor(graph.EdgeCalls, 0.85)
	StampSynthesized(e, SynthExpressResolve)
	return true
}

func expressMemberDirs(class string) []string {
	if strings.HasSuffix(class, "Controller") {
		return expressControllerDirs
	}
	return expressServiceDirs
}

// expressResolveMember binds an XController.method / XService.method handler:
// it resolves the class by convention (preferring the dir its suffix implies)
// then returns the method node on that class.
func expressResolveMember(g graph.Store, cls, method, fromFile string, classMethods map[string]map[string][]*graph.Node) string {
	if method == "" {
		return ""
	}
	classID, _ := ResolveByConvention(g, cls, "", expressMemberDirs(cls), fromFile)
	className := cls
	if classID != "" {
		className = expressSimpleName(classID)
	}
	methods := classMethods[className]
	if methods == nil {
		return ""
	}
	cands := methods[method]
	if len(cands) == 1 {
		return cands[0].ID
	}
	// Multiple same-named methods across classes of this name: prefer the one
	// whose file matches the resolved class.
	if classID != "" {
		classFile := ""
		if cn := g.GetNode(classID); cn != nil {
			classFile = cn.FilePath
		}
		for _, m := range cands {
			if m.FilePath == classFile {
				return m.ID
			}
		}
	}
	return ""
}

// expressClassMethodIndex maps class simple-name → method-name → method nodes
// via the EdgeMemberOf edges.
func expressClassMethodIndex(g graph.Store) map[string]map[string][]*graph.Node {
	classOf := map[string]string{}
	for e := range graph.EdgesLightSeq(g, graph.EdgeMemberOf) {
		if class, ok := expressMemberOwner(e); ok {
			classOf[e.From] = class
		}
	}
	out := map[string]map[string][]*graph.Node{}
	for _, n := range nodesByKindsOrAll(g, graph.KindMethod, graph.KindFunction) {
		if n == nil {
			continue
		}
		cls := classOf[n.ID]
		if cls == "" {
			continue
		}
		if out[cls] == nil {
			out[cls] = map[string][]*graph.Node{}
		}
		out[cls][n.Name] = append(out[cls][n.Name], n)
	}
	return out
}

func expressMemberOwner(e *graph.Edge) (string, bool) {
	if e == nil || e.From == "" || e.To == "" {
		return "", false
	}
	return expressSimpleName(e.To), true
}

// expressSimpleName returns the last `::`-delimited segment of a node ID.
func expressSimpleName(id string) string {
	if i := strings.LastIndex(id, "::"); i >= 0 {
		return id[i+2:]
	}
	return id
}
