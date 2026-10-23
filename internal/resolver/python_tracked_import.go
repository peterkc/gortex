package resolver

import (
	"context"
	"iter"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// PythonPackageLookup exposes only indexed tops from declared tracked
// distributions. The indexer owns manifest I/O and consumer-local checks.
type PythonPackageLookup struct {
	HasTop    func(top string) bool
	Providers func(callerRepo, top string) map[string][]graph.FileNodeIdentity
}

// SetPythonPackageLookup defers manifest reads until a non-stdlib Python import
// needs a retry. A nil result means no declared tracked providers exist.
func (cr *CrossRepoResolver) SetPythonPackageLookup(build func() *PythonPackageLookup) {
	cr.pythonPackages = build
}

func trackedPythonImportSpec(e *graph.Edge) string {
	if e == nil || e.Kind != graph.EdgeImports || !isPythonSourcePath(e.FilePath) {
		return ""
	}
	var spec string
	switch {
	case strings.HasPrefix(e.To, "module::python:"):
		return ""
	case strings.HasPrefix(e.To, "unresolved::import::"):
		spec = strings.TrimPrefix(e.To, "unresolved::import::")
	case strings.HasPrefix(e.To, "external::"):
		spec = strings.TrimPrefix(e.To, "external::")
	case strings.HasPrefix(e.To, "module::pypi:"):
		spec, _ = e.Meta["python_import"].(string)
	}
	top, _, _ := strings.Cut(spec, ".")
	if !isDottedPythonModule(spec) || isPythonStdlibTop(top) {
		return ""
	}
	return spec
}

// Attributed targets retain the top even in a metadata-free edge projection.
// The complete dotted path must still be recovered from the full edge later.
func trackedPythonImportTop(e *graph.Edge) string {
	if e != nil && e.Kind == graph.EdgeImports && isPythonSourcePath(e.FilePath) && strings.HasPrefix(e.To, "module::pypi:") {
		top := strings.TrimPrefix(e.To, "module::pypi:")
		if isDottedPythonModule(top) && !isPythonStdlibTop(top) {
			return top
		}
		return ""
	}
	top, _, _ := strings.Cut(trackedPythonImportSpec(e), ".")
	return top
}

func (cr *CrossRepoResolver) trackedPythonImport(caller *graph.Node, spec string, lookup *PythonPackageLookup, nodes map[string]*graph.Node) *graph.Node {
	top, _, _ := strings.Cut(spec, ".")
	var files []graph.FileNodeIdentity
	providers := 0
	for _, candidates := range lookup.Providers(caller.RepoPrefix, top) {
		if len(candidates) == 0 || !cr.crossWorkspaceEligible(candidateWorkspaceID(caller), fileCandidateWorkspaceID(candidates[0]), spec) {
			continue
		}
		providers++
		files = candidates
	}
	if providers != 1 {
		return nil
	}
	var match *graph.Node
	for _, file := range files {
		base := strings.TrimSuffix(strings.TrimSuffix(file.FilePath, "/__init__.py"), ".py")
		stem := strings.TrimSuffix(base, top) + strings.ReplaceAll(spec, ".", "/")
		for _, suffix := range []string{".py", "/__init__.py"} {
			id := stem + suffix
			node, seen := nodes[id]
			if !seen {
				node = cr.cachedGetNode(id)
				nodes[id] = node
			}
			if node == nil || !pythonModuleFileMatches(node.FilePath, spec) {
				continue
			}
			if match != nil && match.ID != node.ID {
				return nil
			}
			match = node
		}
	}
	return match
}

// Retry only import/file edges; never reconsider working links. As in the npm
// bridge, close the light cursor before probing nodes or hydrating bounded pages.
func (cr *CrossRepoResolver) resolveTrackedPythonImports(ctx context.Context, edges iter.Seq[*graph.Edge], stats *CrossRepoStats) error {
	if cr.pythonPackages == nil {
		return ctx.Err()
	}
	var pending []*graph.Edge
	for e := range edges {
		if err := ctx.Err(); err != nil {
			return err
		}
		if trackedPythonImportTop(e) != "" {
			pending = append(pending, e)
		}
	}
	if len(pending) == 0 {
		return ctx.Err()
	}
	lookup := cr.pythonPackages()
	if lookup == nil {
		return ctx.Err()
	}
	eligible := pending[:0]
	for _, e := range pending {
		if lookup.HasTop(trackedPythonImportTop(e)) {
			eligible = append(eligible, e)
		}
	}
	nodes := map[string]*graph.Node{}
	for start := 0; start < len(eligible); start += resolvePendingPageRows {
		if err := ctx.Err(); err != nil {
			return err
		}
		page := eligible[start:min(start+resolvePendingPageRows, len(eligible))]
		sites := make([]graph.EdgeSite, 0, len(page))
		ids := make([]string, 0, len(page))
		seen := map[string]bool{}
		for _, e := range page {
			sites = append(sites, graph.EdgeSite{From: e.From, Line: e.Line, Kind: graph.EdgeImports})
			if !seen[e.From] {
				ids = append(ids, e.From)
				seen[e.From] = true
			}
		}
		full := cr.graph.GetEdgeCandidates(nil, sites)
		callers := cr.hotCachedNodesByIDs(ids)
		var batch []graph.EdgeReindex
		for _, projected := range page {
			caller := callers[projected.From]
			if caller == nil {
				continue
			}
			for _, e := range full.Site(projected.From, projected.Line, graph.EdgeImports) {
				if e.To != projected.To {
					continue
				}
				spec := trackedPythonImportSpec(e)
				if spec == "" {
					continue
				}
				match := cr.trackedPythonImport(caller, spec, lookup, nodes)
				if match == nil {
					continue
				}
				old := e.To
				e.To, e.CrossRepo = match.ID, true
				batch = append(batch, graph.EdgeReindex{Edge: e, OldTo: old})
				projected.To, projected.CrossRepo = e.To, e.CrossRepo
				stats.Resolved++
				stats.CrossRepoEdges++
				stats.ByRepo[match.RepoPrefix]++
			}
		}
		if len(batch) > 0 {
			cr.graph.ReindexEdges(batch)
		}
	}
	return ctx.Err()
}
