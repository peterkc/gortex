package resolver

import (
	"context"
	"iter"
	"maps"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// resolveGoModuleImports preserves dependency contracts while lifting Go source
// imports onto a tracked package's representative file in a file frontier.
// counted records whether the ordinary frontier pass already counted an import
// as resolved (true) or unresolved (false), before this pass retargets it.
func (cr *CrossRepoResolver) resolveGoModuleImports(ctx context.Context, files []string, stats *CrossRepoStats, counted map[graph.EdgeIdentity]bool) error {
	if len(cr.goModules) == 0 || len(files) == 0 {
		return nil
	}
	flush := func(paths []string) {
		nodes := cr.graph.GetFileNodesByPaths(paths)
		outgoing := cr.graph.GetOutEdgesByNodeIDs(paths)
		var batch []graph.EdgeReindex
		for _, path := range paths {
			cr.rememberGoModuleImports(nodes[path])
			for _, e := range outgoing[path] {
				if e.Kind != graph.EdgeImports {
					continue
				}
				importPath := goModuleImportPath(e, nodes[path])
				if importPath == "" || !trackedGoImport(cr.goModules, importPath) {
					continue
				}
				source := cr.graph.GetNode(e.From)
				caller := ""
				if source != nil {
					caller = source.RepoPrefix
				}
				match := cr.goModuleImportCandidate(importPath, caller, candidateWorkspaceID(source))
				if match == nil || match.ID == e.To {
					continue
				}
				copy := *e
				copy.Meta = maps.Clone(e.Meta)
				copy.To = match.ID
				copy.CrossRepo = caller != "" && match.RepoPrefix != caller
				clearEdgeTerminal(&copy)
				batch = append(batch, graph.EdgeReindex{Edge: &copy, OldTo: e.To, RefreshIdentity: true, OldFilePath: e.FilePath, OldLine: e.Line})
				if len(batch) == externalCallMutationChunk {
					cr.applyGoModuleImportReindexes(batch, stats, counted)
					batch = nil
				}
			}
		}
		cr.applyGoModuleImportReindexes(batch, stats, counted)
	}
	unique := make(map[string]bool)
	var goFiles []string
	for _, path := range files {
		if strings.HasSuffix(path, ".go") && !unique[path] {
			unique[path] = true
			goFiles = append(goFiles, path)
		}
	}
	files = goFiles
	for start := 0; start < len(files); start += externalCallMutationChunk {
		if err := ctx.Err(); err != nil {
			return err
		}
		flush(files[start:min(start+externalCallMutationChunk, len(files))])
	}
	return ctx.Err()
}

// A dep contract has lost the package suffix. Recover it from the retained
// import statement, but never guess when several same-line imports fit.
func goModuleImportPath(e *graph.Edge, nodes []*graph.Node) string {
	target := graph.UnresolvedName(e.To)
	if target == "" {
		target = e.To
	}
	target = strings.TrimPrefix(target, externalCallPrefix)
	if path, ok := strings.CutPrefix(target, "import::"); ok {
		return path
	}
	if path, ok := strings.CutPrefix(target, "external::"); ok {
		return path
	}
	module, dep := strings.CutPrefix(target, "dep::")
	if !dep || strings.Contains(module, "::") {
		return ""
	}
	path := ""
	for _, n := range nodes {
		if n.Kind != graph.KindImport || n.Language != "go" || n.StartLine != e.Line {
			continue
		}
		candidate, _ := n.Meta["path"].(string)
		if candidate != module && !strings.HasPrefix(candidate, module+"/") {
			continue
		}
		if path != "" && path != candidate {
			return ""
		}
		path = candidate
	}
	return path
}

// Package identities are cached across resolver objects by module revision.
// A provider mutation invalidates only that provider's projection.
func (cr *CrossRepoResolver) goModuleImportCandidate(importPath, caller, workspace string) *graph.Node {
	state := cr.goModuleImportState()
	var candidates []*graph.Node
	for repo, module := range cr.goModules {
		if module == "" || (importPath != module && !strings.HasPrefix(importPath, module+"/")) {
			continue
		}
		packages, cached := state.packages[repo]
		if !cached {
			packages = make(map[string]graph.FileNodeIdentity)
			for file := range graph.FileNodeIdentitiesSeq(cr.graph, []string{repo}) {
				if !strings.HasSuffix(file.FilePath, ".go") || strings.HasSuffix(file.FilePath, "_test.go") {
					continue
				}
				dir := filePathDir(file.FilePath)
				if prior, ok := packages[dir]; !ok || file.ID < prior.ID {
					packages[dir] = file
				}
			}
			state.packages[repo] = packages
		}
		if file, ok := packages[repo+strings.TrimPrefix(importPath, module)]; ok {
			candidates = append(candidates, &graph.Node{ID: file.ID, Kind: graph.KindFile, FilePath: file.FilePath, RepoPrefix: file.RepoPrefix, WorkspaceID: file.WorkspaceID, Language: "go"})
		}
	}
	return goModuleCandidate(cr.goModules, importPath, candidates, caller, workspace, cr.crossWorkspaceLookup)
}

func (cr *CrossRepoResolver) applyGoModuleImportReindexes(batch []graph.EdgeReindex, stats *CrossRepoStats, counted map[graph.EdgeIdentity]bool) {
	if !cr.applyGoModuleReindexes(batch) {
		originals := make(map[graph.EdgeIdentity]*graph.Edge)
		for _, row := range batch {
			original := *row.Edge
			original.To = row.OldTo
			original.Meta = maps.Clone(row.Edge.Meta)
			originals[graph.EdgeIdentityFor(&original)] = &original
		}
		cr.queueGoModuleEdges(originals)
		cr.logger.Warn("Go module import batch dropped; queued for a later module pass")
		return
	}
	if stats == nil {
		return
	}
	for _, row := range batch {
		original := graph.EdgeIdentityFor(row.Edge)
		original.To = row.OldTo
		resolved, alreadyCounted := counted[original]
		if !alreadyCounted || !resolved {
			stats.Resolved++
		}
		if alreadyCounted && !resolved {
			stats.Unresolved--
		}
		if row.Edge.CrossRepo {
			stats.CrossRepoEdges++
			if target := cr.graph.GetNode(row.Edge.To); target != nil {
				stats.ByRepo[target.RepoPrefix]++
			}
		}
	}
}

// Filter identity-only rows before loading any file payload or adjacency.
func (cr *CrossRepoResolver) goModuleImportFileBatches(repos []string) iter.Seq[[]string] {
	return func(yield func([]string) bool) {
		seen := make(map[string]bool)
		var files []string
		for n := range graph.RepoNodeIdentitiesSeq(cr.graph, repos, graph.KindImport) {
			file, path, ok := strings.Cut(n.ID, "::import::")
			if !ok || !strings.HasSuffix(file, ".go") || !trackedGoImport(cr.goModules, path) || seen[file] {
				continue
			}
			seen[file] = true
			files = append(files, file)
			if len(files) == externalCallMutationChunk {
				if !yield(files) {
					return
				}
				files = nil
			}
		}
		if len(files) > 0 {
			yield(files)
		}
	}
}

// Import indexes share the store's resolve mutex, not the retry queue mutex.
// Retain statement IDs, not edges: consumer edits and removals are revalidated.
func (cr *CrossRepoResolver) goModuleImportState() *GoModuleDepRetry {
	if cr.goModuleRetry == nil {
		cr.goModuleRetry = &GoModuleDepRetry{}
	}
	state := cr.goModuleRetry
	if state.packageRevision != cr.goModuleRevision || !maps.Equal(state.packageModules, cr.goModules) {
		state.packageModules = maps.Clone(cr.goModules)
		state.packageRevision = cr.goModuleRevision
		state.packages = nil
		state.imports = nil
		state.importsSeeded = false
	}
	if state.packages == nil {
		state.packages = make(map[string]map[string]graph.FileNodeIdentity)
	}
	if state.imports == nil {
		state.imports = make(map[string]map[string]struct{})
	}
	return state
}

// An unchanged warm restart skips global resolution. Seed once, lazily, if
// the first operation is a provider edit; reuse the same seed in legacy retry.
// Callers hold the resolve mutex. Each candidate batch releases it afterward.
func (cr *CrossRepoResolver) resolveGoModuleImportSweep(ctx context.Context, stats *CrossRepoStats) error {
	state := cr.goModuleImportState()
	if state.importsSeeded {
		return nil
	}
	err := func() error {
		cr.mu.Unlock()
		// Reacquire on panic too, before the caller's deferred Unlock runs.
		defer cr.mu.Lock()
		for files := range cr.goModuleImportFileBatches(nil) {
			if err := ctx.Err(); err != nil {
				return err
			}
			func() {
				cr.mu.Lock()
				defer cr.mu.Unlock()
				_ = cr.resolveGoModuleImports(ctx, files, stats, nil)
			}()
		}
		return ctx.Err()
	}()
	if err == nil && state.packageRevision == cr.goModuleRevision && maps.Equal(state.packageModules, cr.goModules) {
		state.importsSeeded = true
	}
	return err
}

func (cr *CrossRepoResolver) rememberGoModuleImports(nodes []*graph.Node) {
	state := cr.goModuleImportState()
	for _, n := range nodes {
		if n.Kind != graph.KindImport || n.Language != "go" {
			continue
		}
		_, path, ok := strings.Cut(n.ID, "::import::")
		if !ok {
			continue
		}
		for repo, module := range cr.goModules {
			if module == "" || (path != module && !strings.HasPrefix(path, module+"/")) {
				continue
			}
			if state.imports[repo] == nil {
				state.imports[repo] = make(map[string]struct{})
			}
			state.imports[repo][n.ID] = struct{}{}
		}
	}
}

// Only retained statements registered for this provider are probed on edits.
// No global node scan is needed to reconstruct imports deleted by eviction.
func (cr *CrossRepoResolver) goModuleImportIDs(repo string) []string {
	state := cr.goModuleImportState()
	delete(state.packages, repo)
	_ = cr.resolveGoModuleImportSweep(context.Background(), nil)
	var ids []string
	for id := range state.imports[repo] {
		ids = append(ids, id)
	}
	return ids
}

func (cr *CrossRepoResolver) restoreGoModuleImports(repo string, stats *CrossRepoStats) {
	ids := cr.goModuleImportIDs(repo)
	for start := 0; start < len(ids); start += externalCallMutationChunk {
		cr.restoreGoModuleImportBatch(repo, ids[start:min(start+externalCallMutationChunk, len(ids))], stats)
	}
}

func (cr *CrossRepoResolver) restoreGoModuleImportBatch(repo string, ids []string, stats *CrossRepoStats) {
	state := cr.goModuleImportState()
	nodes := cr.graph.GetNodesByIDs(ids)
	paths := make(map[string]bool)
	var files []string
	for _, n := range nodes {
		if n != nil && n.Kind == graph.KindImport && n.Language == "go" && !paths[n.FilePath] {
			paths[n.FilePath] = true
			files = append(files, n.FilePath)
		}
	}
	outgoing := cr.graph.GetOutEdgesByNodeIDs(files)
	for _, id := range ids {
		n := nodes[id]
		if n == nil || n.Kind != graph.KindImport || n.Language != "go" {
			delete(state.imports[repo], id)
			continue
		}
		_, path, ok := strings.Cut(n.ID, "::import::")
		module := cr.goModules[repo]
		if !ok || module == "" || (path != module && !strings.HasPrefix(path, module+"/")) {
			delete(state.imports[repo], id)
			continue
		}
		present := false
		for _, e := range outgoing[n.FilePath] {
			if e.Kind == graph.EdgeImports && e.Line == n.StartLine {
				present = true
				break
			}
		}
		if !present {
			cr.graph.AddEdge(&graph.Edge{From: n.FilePath, To: "unresolved::import::" + path, Kind: graph.EdgeImports, FilePath: n.FilePath, Line: n.StartLine})
		}
	}
	_ = cr.resolveGoModuleImports(context.Background(), files, stats, nil)
}
