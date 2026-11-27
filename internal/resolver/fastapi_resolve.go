package resolver

import (
	"iter"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// FastAPI dependency / router directory-convention fallback. The Python
// extractor stamps a `Depends(get_db)` argument as a `via=fastapi.Depends`
// call placeholder and an `include_router(api_router)` argument as a
// `via=fastapi.router` reference placeholder. When the standard import/
// reference resolver already bound the target (the precise path) those
// placeholders are no longer unresolved and this pass leaves them alone. Only
// the residual unresolved ones are bound by directory convention —
// dependencies under /dependencies/ /deps/ /core/, routers under /routers/
// /api/ /routes/ /endpoints/ — so recall improves without regressing the
// precise path and without double-binding.

var (
	fastapiDepDirs    = []string{"/dependencies/", "/deps/", "/core/"}
	fastapiRouterDirs = []string{"/routers/", "/api/", "/routes/", "/endpoints/"}
)

// ResolveFastAPIDeps binds residual unresolved FastAPI dependency / router
// references to their definitions by directory convention. Returns the count
// bound.
func ResolveFastAPIDeps(g graph.Store) int {
	return resolveFastAPIDeps(g, nil)
}

// resolveFastAPIDeps is the census-multiplexed form: cands, when non-nil,
// replaces both whole-stream decodes with the shared walk's pre-matched
// candidates.
func resolveFastAPIDeps(g graph.Store, cands *frameworkPassCandidates) int {
	if g == nil {
		return 0
	}
	callsStream := g.EdgesByKind(graph.EdgeCalls)
	refsStream := g.EdgesByKind(graph.EdgeReferences)
	if cands != nil {
		callsStream = frameworkEdgeSeq(cands.calls)
		refsStream = frameworkEdgeSeq(cands.refs)
	}
	resolved := 0
	var reindex []graph.EdgeReindex
	for _, stream := range []iter.Seq[*graph.Edge]{callsStream, refsStream} {
		for e := range stream {
			if e == nil {
				continue
			}
			via, _ := e.Meta["via"].(string)
			name, preferDirs := fastapiResolveShape(e.To, via)
			if preferDirs == nil {
				continue
			}
			fromFile := ""
			if n := g.GetNode(e.From); n != nil {
				fromFile = n.FilePath
			}
			if !fastapiResolveSource(fromFile) {
				continue
			}
			targetID, conf := ResolveByConvention(g, name, "", preferDirs, fromFile)
			if targetID == "" {
				continue
			}
			reindex = append(reindex, fastapiResolveRebind(e, targetID, conf))
			resolved++
		}
	}
	if len(reindex) > 0 {
		g.ReindexEdges(reindex)
	}
	return resolved
}

func fastapiConventionDirs(via string) []string {
	switch via {
	case "fastapi.Depends":
		return fastapiDepDirs
	case "fastapi.router":
		return fastapiRouterDirs
	default:
		return nil
	}
}

func fastapiResolveShape(target, via string) (string, []string) {
	if !graph.IsUnresolvedTarget(target) {
		return "", nil
	}
	dirs := fastapiConventionDirs(via)
	if dirs == nil {
		return "", nil
	}
	name := graph.UnresolvedName(target)
	if strings.ContainsRune(name, '.') {
		return "", nil // member expressions are left to the import resolver
	}
	return name, dirs
}

func fastapiResolveSource(path string) bool {
	return strings.HasSuffix(path, ".py")
}

func fastapiResolveRebind(e *graph.Edge, targetID string, conf float64) graph.EdgeReindex {
	oldTo := e.To
	e.To = targetID
	e.Origin = graph.OriginASTInferred
	e.Confidence = conf
	e.ConfidenceLabel = graph.ConfidenceLabelFor(e.Kind, conf)
	StampSynthesized(e, SynthFastAPIResolve)
	return graph.EdgeReindex{Edge: e, OldTo: oldTo}
}
