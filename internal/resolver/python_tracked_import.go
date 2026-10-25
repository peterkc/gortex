package resolver

import (
	"context"
	"iter"
	"slices"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// PythonImportStubMeta preserves the import's exact target before a tracked bind.
const PythonImportStubMeta = "python_import_stub"

// PythonCallStubMeta retains the exact function, method or module-qualified call target.
const PythonCallStubMeta = "python_call_stub"

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

// SQLite's projection scanner closes each page cursor before yielding, so the
// retry may hydrate and reindex edges without retaining the complete light scan.
// Other stores drain the light scan first: EdgesLightSeq can hold a live cursor
// that must be closed before the retry reads or writes the graph.
func (cr *CrossRepoResolver) trackedPythonImportEdges() iter.Seq[*graph.Edge] {
	if scanner, ok := cr.graph.(interface {
		ScanTestEdgeProjections([]graph.EdgeKind, int, func([]graph.TestEdgeProjection) bool)
	}); ok {
		return func(yield func(*graph.Edge) bool) {
			scanner.ScanTestEdgeProjections([]graph.EdgeKind{graph.EdgeImports}, resolvePendingPageRows, func(page []graph.TestEdgeProjection) bool {
				for _, e := range page {
					if !yield(&graph.Edge{From: e.From, To: e.To, Kind: e.Kind, FilePath: e.FilePath, Line: e.Line}) {
						return false
					}
				}
				return true
			})
		}
	}
	var edges []*graph.Edge
	for e := range graph.EdgesLightSeq(cr.graph, graph.EdgeImports) {
		if trackedPythonImportTop(e) != "" {
			edges = append(edges, e)
		}
	}
	return slices.Values(edges)
}

// Retry only import/file edges; never reconsider working links. Full passes
// stream reopened calls later; scoped passes collect them for their pending list.
func (cr *CrossRepoResolver) resolveTrackedPythonImports(ctx context.Context, edges iter.Seq[*graph.Edge], stats *CrossRepoStats, captureCalls bool) (reopened []*graph.Edge, err error) {
	if cr.pythonPackages == nil {
		return reopened, ctx.Err()
	}
	next, stop := iter.Pull(edges)
	defer stop()
	var lookup *PythonPackageLookup
	for {
		page := make([]*graph.Edge, 0, resolvePendingPageRows)
		for len(page) < resolvePendingPageRows {
			if err := ctx.Err(); err != nil {
				return reopened, err
			}
			e, ok := next()
			if !ok {
				break
			}
			top := trackedPythonImportTop(e)
			if top == "" {
				continue
			}
			if lookup == nil {
				lookup = cr.pythonPackages()
				if lookup == nil {
					return reopened, ctx.Err()
				}
			}
			if lookup.HasTop(top) {
				page = append(page, e)
			}
		}
		if err := ctx.Err(); err != nil {
			return reopened, err
		}
		if len(page) == 0 {
			return reopened, nil
		}
		nodes := map[string]*graph.Node{}
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
				if e.Meta == nil {
					e.Meta = make(map[string]any)
				}
				e.Meta[PythonImportStubMeta], e.Meta["python_import"] = old, spec
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
			calls := cr.restubCallsForLinkedPythonImports(batch)
			if captureCalls {
				reopened = append(reopened, calls...)
			}
		}
	}
}

// Reopen only synthesized calls backed by an import this bridge just linked.
func (cr *CrossRepoResolver) restubCallsForLinkedPythonImports(changes []graph.EdgeReindex) []*graph.Edge {
	specs := make(map[string][]string)
	var files []string
	for _, change := range changes {
		spec, _ := change.Edge.Meta["python_import"].(string)
		if stub, _ := change.Edge.Meta[PythonImportStubMeta].(string); stub == "" || spec == "" || !change.Edge.CrossRepo {
			continue
		}
		file := change.Edge.FilePath
		if _, seen := specs[file]; !seen {
			files = append(files, file)
		}
		specs[file] = append(specs[file], spec)
	}
	if len(files) == 0 {
		return nil
	}
	var reopened []*graph.Edge
	var batch []graph.EdgeReindex
	for row := range graph.EdgesInScopeSeq(cr.graph, nil, files, graph.EdgeCalls) {
		call := row.Edge
		if call == nil {
			continue
		}
		for _, spec := range specs[call.FilePath] {
			original, _ := call.Meta[PythonCallStubMeta].(string)
			// Cold master resolution leaves dep:: terminals until externalization;
			// incremental tracking has already synthesized external-call:: nodes.
			callTarget := call.To
			if ecosystem, module, ok := parseExternalCallTarget(callTarget); ok && (ecosystem == "dep" || ecosystem == "stdlib") {
				callTarget = externalCallNodeID("dep", module)
			}
			callTarget = strings.Replace(callTarget, externalCallPrefix+"stdlib::", externalCallPrefix+"dep::", 1)
			target := externalCallPrefix + "dep::" + spec
			if original == "" || (callTarget != target && !strings.HasPrefix(callTarget, target+".")) {
				continue
			}
			old := call.To
			graph.StashRestubProvenance(call)
			delete(call.Meta, "external_call")
			call.To, call.CrossRepo, call.ConfidenceLabel = original, false, ""
			reopened = append(reopened, call)
			batch = append(batch, graph.EdgeReindex{Edge: call, OldTo: old})
			break
		}
	}
	if len(batch) > 0 {
		cr.graph.ReindexEdges(batch)
	}
	return reopened
}

// Qualified Python calls use the exact linked import, not repo-wide reachability.
func (cr *CrossRepoResolver) linkedPythonCallCandidate(e *graph.Edge, node *graph.Node, module string) bool {
	for _, imp := range cr.bridgeImportsByFile[cr.callerFileID(e)] {
		spec, _ := imp.Meta["python_import"].(string)
		if imp.Meta[PythonImportStubMeta] == nil || spec == "" || (module != spec && !strings.HasPrefix(module, spec+".")) {
			continue
		}
		if node.FilePath == imp.To && cr.crossWorkspaceEligible(cr.callerWorkspaceID(e), candidateWorkspaceID(node), spec) {
			return true
		}
	}
	return false
}

func (cr *CrossRepoResolver) resolvePythonImportedMethod(e *graph.Edge, target string) bool {
	if e.Kind != graph.EdgeCalls || !isPythonSourcePath(e.FilePath) || !strings.HasPrefix(target, "*.") {
		return false
	}
	receiver := edgeReceiverType(e)
	if receiver == "" {
		return false
	}
	for _, node := range cr.graph.FindNodesByNameInRepo(receiver, cr.callerRepoPrefix(e)) {
		if node.Kind == graph.KindType && node.RepoPrefix == graph.RepoPrefixOfID(e.From) {
			return false
		}
	}
	candidates := make(map[string][]*graph.Node)
	name := strings.TrimPrefix(target, "*.")
	for _, node := range cr.bridgeCandidates(e, map[string]bool{name: true}, false)[name] {
		candidates[node.FilePath] = append(candidates[node.FilePath], node)
	}
	var match *graph.Node
	for _, imp := range cr.bridgeImportsByFile[cr.callerFileID(e)] {
		spec, _ := imp.Meta["python_import"].(string)
		if imp.Meta[PythonImportStubMeta] == nil || !pythonModuleFileMatches(imp.To, spec) {
			continue
		}
		bindings, _ := imp.Meta["python_bindings"].(map[string]any)
		imported, _ := bindings[receiver].(string)
		if imported == "" {
			continue
		}
		for _, candidate := range candidates[imp.To] {
			if candidate.Kind != graph.KindMethod || candidate.FilePath != imp.To || nodeReceiverType(candidate) != imported || !cr.crossWorkspaceEligible(cr.callerWorkspaceID(e), candidateWorkspaceID(candidate), spec) {
				continue
			}
			if match != nil && match.ID != candidate.ID {
				return false
			}
			match = candidate
		}
	}
	if match == nil {
		return false
	}
	e.To, e.CrossRepo = match.ID, true
	e.Confidence, e.Origin = 0.85, graph.OriginASTResolved
	return true
}
