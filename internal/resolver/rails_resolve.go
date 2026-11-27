package resolver

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// Rails receiver-constant name-resolution. A Ruby `Const.method` call loses
// its receiver in the base call graph (it becomes `unresolved::*.method`), so
// the Ruby extractor stamps the receiver constant as Meta["recv_const"]. This
// pass binds those residual calls to the directory-located Rails definition
// the constant names:
//
//   - `*Service` → /app/services/, `*Job` → /app/jobs/, `*Worker` → /app/workers/
//   - `*Helper`  → /app/helpers/ (a Ruby module)
//   - a bare PascalCase constant that is a detected ActiveRecord model →
//     /app/models/
//
// The class is resolved by directory convention (ResolveByConvention) and the
// call binds to the named method on it, or to the class itself when the method
// is inherited (ActiveRecord finders like `find`/`where` are not declared in
// the model). It is a fallback — an already-resolved call is never touched —
// and an ambiguous constant across two convention dirs is left unresolved.

var (
	railsServiceDirs = []string{"/app/services/"}
	railsJobDirs     = []string{"/app/jobs/"}
	railsWorkerDirs  = []string{"/app/workers/"}
	railsHelperDirs  = []string{"/app/helpers/"}
	railsModelDirs   = []string{"/app/models/"}
)

// ResolveRailsRefs binds residual unresolved Ruby `Const.method` calls to
// their Rails definitions by directory convention. Returns the count bound.
func ResolveRailsRefs(g graph.Store) int {
	return resolveRailsRefs(g, nil)
}

// resolveRailsRefs is the census-multiplexed form: cands, when non-nil,
// replaces the whole EdgeCalls decode with the shared walk's pre-matched
// recv_const candidates. The class/method node index keeps its own
// combined-kind scan: its first-wins name map depends on the multi-kind
// scan's row order, which per-kind snapshot slices cannot reproduce.
func resolveRailsRefs(g graph.Store, cands *frameworkPassCandidates) int {
	if g == nil {
		return 0
	}
	stream := g.EdgesByKind(graph.EdgeCalls)
	if cands != nil {
		if len(cands.calls) == 0 {
			// The convention indexes below are pure reads consumed only by
			// the loop; with no candidates the pass provably lands nothing.
			return 0
		}
		stream = frameworkEdgeSeq(cands.calls)
	}
	models := railsModelClassSet(g)
	methodsByClass := railsClassMethodIndex(g)
	resolved := 0
	var reindex []graph.EdgeReindex
	for e := range stream {
		recv, method := railsCallKey(e)
		if recv == "" {
			continue
		}
		fromFile := ""
		if n := g.GetNode(e.From); n != nil {
			fromFile = n.FilePath
		}
		if !railsRefSource(fromFile) {
			continue
		}
		isModel, dirs := railsRefDirs(recv)
		classID, _ := ResolveByConvention(g, recv, "", dirs, fromFile)
		if classID == "" {
			continue
		}
		if isModel && !models[classID] {
			continue // bare PascalCase that isn't an ActiveRecord model
		}
		target := classID
		if m := methodsByClass[classID][method]; m != "" {
			target = m
		}
		reindex = append(reindex, railsRefRebind(e, target))
		resolved++
	}
	if len(reindex) > 0 {
		g.ReindexEdges(reindex)
	}
	return resolved
}

func railsCallKey(e *graph.Edge) (receiver, method string) {
	if e == nil || !graph.IsUnresolvedTarget(e.To) {
		return "", ""
	}
	receiver, _ = e.Meta["recv_const"].(string)
	if receiver == "" {
		return "", ""
	}
	return receiver, strings.TrimPrefix(graph.UnresolvedName(e.To), "*.")
}

func railsRefSource(file string) bool {
	return strings.HasSuffix(file, ".rb")
}

func railsRefRebind(e *graph.Edge, target string) graph.EdgeReindex {
	oldTo := e.To
	e.To = target
	e.Origin = graph.OriginASTInferred
	e.Confidence = 0.8
	e.ConfidenceLabel = graph.ConfidenceLabelFor(graph.EdgeCalls, 0.8)
	StampSynthesized(e, SynthRailsResolve)
	return graph.EdgeReindex{Edge: e, OldTo: oldTo}
}

// railsRefDirs classifies a receiver constant into its convention dirs;
// isModel marks the bare-PascalCase (ActiveRecord model) case, which is
// additionally gated on the resolved class being a detected model.
func railsRefDirs(recv string) (isModel bool, dirs []string) {
	switch {
	case strings.HasSuffix(recv, "Service"):
		return false, railsServiceDirs
	case strings.HasSuffix(recv, "Job"):
		return false, railsJobDirs
	case strings.HasSuffix(recv, "Worker"):
		return false, railsWorkerDirs
	case strings.HasSuffix(recv, "Helper"):
		return false, railsHelperDirs
	}
	return true, railsModelDirs
}

// railsModelClassSet returns the set of class node IDs detected as
// ActiveRecord models — those with an outgoing EdgeModelsTable.
func railsModelClassSet(g graph.Store) map[string]bool {
	out := map[string]bool{}
	for e := range graph.EdgesLightSeq(g, graph.EdgeModelsTable) {
		if e != nil && e.From != "" {
			out[e.From] = true
		}
	}
	return out
}

// railsClassMethodIndex maps class/module node ID → method name → method node
// ID via the EdgeMemberOf edges.
func railsClassMethodIndex(g graph.Store) map[string]map[string]string {
	classOf := map[string]string{}
	for e := range graph.EdgesLightSeq(g, graph.EdgeMemberOf) {
		if class := railsMemberOwner(e); class != "" {
			classOf[e.From] = class
		}
	}
	out := map[string]map[string]string{}
	for _, n := range nodesByKindsOrAll(g, graph.KindMethod, graph.KindFunction) {
		if n == nil {
			continue
		}
		cls := classOf[n.ID]
		if cls == "" {
			continue
		}
		if out[cls] == nil {
			out[cls] = map[string]string{}
		}
		if _, ok := out[cls][n.Name]; !ok {
			out[cls][n.Name] = n.ID
		}
	}
	return out
}

func railsMemberOwner(e *graph.Edge) string {
	if e == nil || e.From == "" {
		return ""
	}
	return e.To
}
