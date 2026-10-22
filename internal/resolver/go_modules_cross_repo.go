package resolver

import (
	"context"
	"maps"
	"strings"
	"sync"

	"github.com/zzet/gortex/internal/graph"
)

func (cr *CrossRepoResolver) SetGoModules(modules map[string]string) {
	cr.goModules = maps.Clone(modules)
}

func (cr *CrossRepoResolver) resolveGoExtern(e *graph.Edge, spec string, stats *CrossRepoStats) {
	sep := strings.LastIndex(spec, "::")
	if sep < 0 || !strings.HasSuffix(e.FilePath, ".go") {
		stats.Unresolved++
		return
	}
	match := goModuleCandidate(cr.goModules, spec[:sep], cr.cachedFindNodesByName(spec[sep+2:]), cr.callerRepoPrefix(e), cr.callerWorkspaceID(e), cr.crossWorkspaceLookup)
	if match == nil {
		if isStdlibLike(spec[:sep]) {
			e.To = graph.StubID(cr.callerRepoPrefix(e), graph.StubKindStdlib, spec[:sep], spec[sep+2:])
		} else {
			e.To = "dep::" + spec
		}
		stats.Unresolved++
		return
	}
	bindGoModuleEdge(e, match, cr.callerRepoPrefix(e))
	stats.Resolved++
	if e.CrossRepo {
		stats.CrossRepoEdges++
		stats.ByRepo[match.RepoPrefix]++
	}
}

// GoModuleDepRetry remembers the last completed legacy-terminal sweep for a
// store's module/config revision. MultiIndexer shares it across pass objects.
type GoModuleDepRetry struct {
	mu       sync.Mutex
	modules  map[string]string
	revision uint64
	complete bool
	pending  map[graph.EdgeIdentity]*graph.Edge
}

func (cr *CrossRepoResolver) SetGoModuleDepRetry(state *GoModuleDepRetry, revision uint64) {
	cr.goModuleRetry, cr.goModuleRevision = state, revision
}

// retryGoModuleDeps visits only external terminals, filters by tracked module
// identity, and bounds mutation batches. Symbol lookup is memoized per batch.
func (cr *CrossRepoResolver) retryGoModuleDeps(ctx context.Context, stats *CrossRepoStats) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(cr.goModules) == 0 {
		return nil
	}
	if cr.goModuleRetry == nil {
		cr.goModuleRetry = &GoModuleDepRetry{}
	}
	state := cr.goModuleRetry
	state.mu.Lock()
	pending := state.pending
	state.pending = nil
	state.mu.Unlock()
	var deferred []*graph.Edge
	for _, edge := range pending {
		deferred = append(deferred, edge)
		if len(deferred) == externalCallMutationChunk {
			cr.resolveGoModuleEdgesLocked(deferred, stats)
			deferred = deferred[:0]
		}
	}
	cr.resolveGoModuleEdgesLocked(deferred, stats)
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.complete && state.revision == cr.goModuleRevision && maps.Equal(state.modules, cr.goModules) {
		return nil
	}
	var batch []graph.EdgeReindex
	originals := make(map[graph.EdgeIdentity]*graph.Edge)
	names := make(map[string][]*graph.Node)
	imports := make(map[string]bool)
	flush := func() {
		if len(batch) > 0 {
			if !cr.applyGoModuleReindexes(batch) {
				cr.logger.Warn("Go module terminal batch dropped; queued for a later module pass")
				if state.pending == nil {
					state.pending = make(map[graph.EdgeIdentity]*graph.Edge)
				}
				maps.Copy(state.pending, originals)
			}
			batch = nil
			clear(originals)
		}
		clear(names)
		clear(imports)
	}
	collect := externalCallCandidateEdges(cr.graph)
	collect(func(row externalCallCandidate) bool {
		if ctx.Err() != nil {
			return false
		}
		e := row.edge
		if resolutionExempt(e) || !strings.HasSuffix(e.FilePath, ".go") {
			return true
		}
		spec := goModuleEdgeSpec(e)
		sep := strings.LastIndex(spec, "::")
		if sep < 0 {
			return true
		}
		importPath := spec[:sep]
		tracked, known := imports[importPath]
		if !known {
			tracked = trackedGoImport(cr.goModules, importPath)
			imports[importPath] = tracked
		}
		if len(imports) >= externalCallMutationChunk {
			flush()
		}
		if !tracked {
			return true
		}
		name := spec[sep+2:]
		candidates, ok := names[name]
		if !ok {
			candidates = cr.graph.FindNodesByName(name)
			names[name] = candidates
		}
		source := row.source
		if source == nil {
			source = cr.graph.GetNode(e.From)
		}
		caller := ""
		if source != nil {
			caller = source.RepoPrefix
		}
		match := goModuleCandidate(cr.goModules, spec[:sep], candidates, caller, candidateWorkspaceID(source), cr.crossWorkspaceLookup)
		if match != nil {
			original := *e
			original.Meta = maps.Clone(e.Meta)
			originals[graph.EdgeIdentityFor(e)] = &original
			oldTo := e.To
			bindGoModuleEdge(e, match, caller)
			delete(e.Meta, "external_call")
			delete(e.Meta, "go_extern_target")
			batch = append(batch, graph.EdgeReindex{Edge: e, OldTo: oldTo})
			stats.Resolved++
			if e.CrossRepo {
				stats.CrossRepoEdges++
				stats.ByRepo[match.RepoPrefix]++
			}
		}
		if len(batch) >= externalCallMutationChunk || len(names) >= externalCallMutationChunk {
			flush()
		}
		return true
	})
	flush()
	if err := ctx.Err(); err != nil {
		return err
	}
	state.modules, state.revision, state.complete = maps.Clone(cr.goModules), cr.goModuleRevision, true
	return nil
}

// goModuleIncomingEdges probes only package-qualified placeholders owned by the
// changed Go definitions. Bare-name frontier keys cannot find these imports.
func (cr *CrossRepoResolver) goModuleIncomingEdges(nodesByFile map[string][]*graph.Node, definitionFiles []string) []*graph.Edge {
	if len(cr.goModules) == 0 {
		return nil
	}
	keys := make(map[string]struct{})
	providers := make(map[string][]*graph.Node)
	synthetic := make(map[string]struct{})
	for _, file := range definitionFiles {
		for _, n := range nodesByFile[file] {
			if n == nil || n.Language != "go" || n.Name == "" || (n.Kind != graph.KindFunction && n.Kind != graph.KindType && n.Kind != graph.KindInterface) {
				continue
			}
			module := cr.goModules[n.RepoPrefix]
			dir := filePathDir(n.FilePath)
			if module == "" || (dir != n.RepoPrefix && !strings.HasPrefix(dir, n.RepoPrefix+"/")) {
				continue
			}
			spec := module + strings.TrimPrefix(dir, n.RepoPrefix) + "::" + n.Name
			providers[spec] = append(providers[spec], n)
			keys[graph.UnresolvedMarker+"extern::"+spec] = struct{}{}
			keys["dep::"+spec] = struct{}{}
			synthetic[externalCallPrefix+"dep::"+module+strings.TrimPrefix(dir, n.RepoPrefix)] = struct{}{}
		}
	}
	var ids []string
	for key := range keys {
		ids = append(ids, key)
	}
	for key := range synthetic {
		ids = append(ids, key)
	}
	var edges []*graph.Edge
	sources := make(map[string]*graph.Node)
	for _, incoming := range cr.graph.GetInEdgesByNodeIDs(ids) {
		for _, e := range incoming {
			if strings.HasPrefix(e.To, externalCallPrefix) {
				target, _ := e.Meta["go_extern_target"].(string)
				if _, wanted := keys[target]; !wanted {
					continue
				}
			}
			spec := goModuleEdgeSpec(e)
			sep := strings.LastIndex(spec, "::")
			if sep < 0 {
				continue
			}
			source, known := sources[e.From]
			if !known {
				source = cr.cachedGetNode(e.From)
				sources[e.From] = source
			}
			for _, provider := range providers[spec] {
				if crossWorkspaceEligible(cr.crossWorkspaceLookup, candidateWorkspaceID(source), candidateWorkspaceID(provider), spec[:sep]) {
					edges = append(edges, e)
					break
				}
			}
		}
	}
	return edges
}

// applyGoModuleReindexes never publishes derived work for a dropped batch.
func (cr *CrossRepoResolver) applyGoModuleReindexes(batch []graph.EdgeReindex) bool {
	if len(batch) == 0 {
		return true
	}
	if writer, ok := cr.graph.(interface {
		ReindexEdgesApplied([]graph.EdgeReindex) bool
	}); ok {
		if !writer.ReindexEdgesApplied(batch) {
			return false
		}
	} else {
		cr.graph.ReindexEdges(batch)
	}
	DetectCrossRepoEdgesForReindexes(cr.graph, batch)
	for _, row := range batch {
		cr.noteRetargetedCall(row.Edge)
	}
	return true
}

// ResolveDeferredGoModuleEdges needs no directory, dependency or whole-repo frontier.
// Each bounded batch releases the shared resolve lock before the next one starts.
func (cr *CrossRepoResolver) ResolveDeferredGoModuleEdges(edges []*graph.Edge) error {
	for start := 0; start < len(edges); start += externalCallMutationChunk {
		end := min(start+externalCallMutationChunk, len(edges))
		cr.mu.Lock()
		cr.resolveGoModuleEdgesLocked(edges[start:end], nil)
		cr.mu.Unlock()
	}
	return nil
}

func goModuleEdgeSpec(e *graph.Edge) string {
	target := e.To
	if strings.HasPrefix(target, externalCallPrefix) {
		target, _ = e.Meta["go_extern_target"].(string)
	}
	switch {
	case strings.HasPrefix(graph.UnresolvedName(target), "extern::"):
		return strings.TrimPrefix(graph.UnresolvedName(target), "extern::")
	case strings.HasPrefix(target, "dep::"):
		return strings.TrimPrefix(target, "dep::")
	case graph.IsStdlibStub(target):
		return graph.StubRest(target)
	default:
		return ""
	}
}

func (cr *CrossRepoResolver) resolveGoModuleEdgesLocked(edges []*graph.Edge, stats *CrossRepoStats) {
	var batch []graph.EdgeReindex
	names := make(map[[2]string][]*graph.Node)
	sources := make(map[string]*graph.Node)
	targetRepos := make(map[string]string)
	originals := make(map[graph.EdgeIdentity]*graph.Edge)
	for _, e := range edges {
		if e == nil || resolutionExempt(e) || !strings.HasSuffix(e.FilePath, ".go") {
			continue
		}
		spec := goModuleEdgeSpec(e)
		sep := strings.LastIndex(spec, "::")
		if sep < 0 {
			continue
		}
		source, known := sources[e.From]
		if !known {
			source = cr.cachedGetNode(e.From)
			sources[e.From] = source
		}
		caller := ""
		if source != nil {
			caller = source.RepoPrefix
		}
		key := [2]string{spec[sep+2:], caller}
		candidates, known := names[key]
		if !known {
			candidates = cr.cachedFindNodesByName(key[0])
			names[key] = candidates
		}
		match := goModuleCandidate(cr.goModules, spec[:sep], candidates, caller, candidateWorkspaceID(source), cr.crossWorkspaceLookup)
		copy := *e
		copy.Meta = maps.Clone(e.Meta)
		if match != nil {
			targetRepos[match.ID] = match.RepoPrefix
			bindGoModuleEdge(&copy, match, caller)
		} else {
			if !graph.IsUnresolvedTarget(e.To) {
				continue
			}
			if isStdlibLike(spec[:sep]) {
				copy.To = graph.StubID(caller, graph.StubKindStdlib, spec[:sep], spec[sep+2:])
			} else {
				copy.To = "dep::" + spec
			}
		}
		if copy.To == e.To {
			continue
		}
		original := *e
		original.Meta = maps.Clone(e.Meta)
		originals[graph.EdgeIdentityFor(e)] = &original
		delete(copy.Meta, "external_call")
		delete(copy.Meta, "go_extern_target")
		batch = append(batch, graph.EdgeReindex{Edge: &copy, OldTo: e.To, RefreshIdentity: true, OldFilePath: e.FilePath, OldLine: e.Line})
	}
	batch = cr.filterLiveReindex(batch)
	if !cr.applyGoModuleReindexes(batch) {
		cr.logger.Warn("Go module rebind batch dropped; queued for a later module pass")
		if cr.goModuleRetry == nil {
			cr.goModuleRetry = &GoModuleDepRetry{}
		}
		cr.goModuleRetry.mu.Lock()
		if cr.goModuleRetry.pending == nil {
			cr.goModuleRetry.pending = make(map[graph.EdgeIdentity]*graph.Edge)
		}
		maps.Copy(cr.goModuleRetry.pending, originals)
		cr.goModuleRetry.mu.Unlock()
		return
	}
	if stats == nil {
		return
	}
	for _, row := range batch {
		if isSyntheticResolveTarget(row.Edge.To) {
			stats.Unresolved++
			continue
		}
		stats.Resolved++
		if row.Edge.CrossRepo {
			stats.CrossRepoEdges++
			stats.ByRepo[targetRepos[row.Edge.To]]++
		}
	}
}

// RetryGoModuleDeps is the revision-keyed migration used after publishing a module.
func (cr *CrossRepoResolver) RetryGoModuleDeps(ctx context.Context) error {
	cr.mu.Lock()
	defer cr.mu.Unlock()
	return cr.retryGoModuleDeps(ctx, &CrossRepoStats{ByRepo: make(map[string]int)})
}

// ResolveGoModuleIncomingForRepo probes exact targets for the provider's definitions.
// Reindexing has no changed-symbol receipt, so stream only its definition IDs and
// hydrate at most one batch; never load the repository's nodes or edges as a set.
func (cr *CrossRepoResolver) ResolveGoModuleIncomingForRepo(repo string) error {
	if cr.goModules[repo] == "" {
		return nil
	}
	var ids []string
	flush := func() error {
		nodes := cr.graph.GetNodesByIDs(ids)
		byFile := make(map[string][]*graph.Node)
		var files []string
		for _, n := range nodes {
			if n == nil {
				continue
			}
			if _, exists := byFile[n.FilePath]; !exists {
				files = append(files, n.FilePath)
			}
			byFile[n.FilePath] = append(byFile[n.FilePath], n)
		}
		edges := cr.goModuleIncomingEdges(byFile, files)
		ids = ids[:0]
		return cr.ResolveDeferredGoModuleEdges(edges)
	}
	for n := range graph.RepoNodeIdentitiesSeq(cr.graph, []string{repo}, graph.KindFunction, graph.KindType, graph.KindInterface) {
		ids = append(ids, n.ID)
		if len(ids) == externalCallMutationChunk {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	if len(ids) > 0 {
		return flush()
	}
	return nil
}
