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
	if len(files) == 0 {
		return nil
	}
	cr.goModuleImportState()
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
				if importPath == "" || !trackedGoImport(cr.goModulePaths, importPath) && e.Meta["go_import"] == nil {
					continue
				}
				source := cr.graph.GetNode(e.From)
				caller := ""
				if source != nil {
					caller = source.RepoPrefix
				}
				match := cr.goModuleImportCandidate(importPath, caller, candidateWorkspaceID(source))
				if match != nil && match.ID == e.To || match == nil && e.Meta["go_import"] == nil {
					continue
				}
				copy := *e
				copy.Meta = maps.Clone(e.Meta)
				if copy.Meta == nil {
					copy.Meta = make(map[string]any)
				}
				if copy.Meta["go_import"] == nil {
					copy.Meta["go_import"] = importPath
					copy.Meta["go_import_fallback"] = e.To
				}
				if match != nil {
					copy.To = match.ID
					copy.CrossRepo = caller != "" && match.RepoPrefix != caller
				} else {
					copy.To, _ = copy.Meta["go_import_fallback"].(string)
					if copy.To == "" || graph.IsUnresolvedTarget(copy.To) {
						copy.To = "external::" + importPath
					}
					copy.CrossRepo, copy.Confidence, copy.ConfidenceLabel, copy.Origin, copy.Tier = false, 0, "", "", ""
				}
				if copy.To == e.To {
					continue
				}
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
	if path, ok := e.Meta["go_import"].(string); ok && path != "" {
		return path
	}
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
	state := cr.goModuleRetry
	var candidates []*graph.Node
	module := cr.goModulePaths.longest(importPath)
	for _, repo := range cr.goModuleRepos[module] {
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
	if _, replaced := goModuleReplacementFor(cr.goModuleReplacements, caller, importPath); replaced {
		return nil
	}
	return goModuleCandidate(cr.goModules, importPath, candidates, caller, workspace, cr.crossWorkspaceLookup)
}

func (cr *CrossRepoResolver) applyGoModuleImportReindexes(batch []graph.EdgeReindex, stats *CrossRepoStats, counted map[graph.EdgeIdentity]bool) {
	batch = cr.filterLiveReindex(batch)
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
func (cr *CrossRepoResolver) goModuleImportFileBatches(ctx context.Context, repos []string) iter.Seq2[[]string, error] {
	return func(yield func([]string, error) bool) {
		seen := make(map[string]bool)
		var files []string
		for n := range graph.RepoNodeIdentitiesSeq(cr.graph, repos, graph.KindImport) {
			if err := ctx.Err(); err != nil {
				yield(nil, err)
				return
			}
			file, path, ok := strings.Cut(n.ID, "::import::")
			if !ok || !strings.HasSuffix(file, ".go") || !trackedGoImport(cr.goModulePaths, path) || seen[file] {
				continue
			}
			if module := cr.goModules[n.RepoPrefix]; module != "" && (path == module || strings.HasPrefix(path, module+"/")) {
				continue
			}
			seen[file] = true
			files = append(files, file)
			if len(files) == externalCallMutationChunk {
				if !yield(files, nil) {
					return
				}
				files = nil
			}
		}
		if len(files) > 0 {
			yield(files, nil)
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
	if state.packageRevision != cr.goModuleRevision {
		state.packageRevision = cr.goModuleRevision
		state.packages = nil
	}
	if !maps.Equal(state.packageModules, cr.goModules) {
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

// Seed the save-time registry once, without hydrating files or mutating edges.
// A workspace-sized identity scan must not consume a provider's recovery budget.
func (cr *CrossRepoResolver) seedGoModuleImportRegistry(ctx context.Context) error {
	cr.mu.Lock()
	defer cr.mu.Unlock()
	state := cr.goModuleImportState()
	if state.importsSeeded || len(cr.goModules) == 0 {
		return ctx.Err()
	}
	for n := range graph.RepoNodeIdentitiesSeq(cr.graph, nil, graph.KindImport) {
		if err := ctx.Err(); err != nil {
			return err
		}
		file, path, ok := strings.Cut(n.ID, "::import::")
		if !ok || !strings.HasSuffix(file, ".go") || !trackedGoImport(cr.goModulePaths, path) {
			continue
		}
		if module := cr.goModules[n.RepoPrefix]; module != "" && (path == module || strings.HasPrefix(path, module+"/")) {
			continue
		}
		cr.rememberGoModuleImports([]*graph.Node{{ID: n.ID, RepoPrefix: n.RepoPrefix, Kind: graph.KindImport, Language: "go"}})
	}
	state.importsSeeded = ctx.Err() == nil
	return ctx.Err()
}

// An unchanged warm restart skips global resolution. Public entry points seed
// once before taking the pass lock; each candidate batch takes its own lock.
func (cr *CrossRepoResolver) resolveGoModuleImportSweep(ctx context.Context, stats *CrossRepoStats) error {
	cr.mu.Lock()
	state := cr.goModuleImportState()
	seeded := state.importsSeeded
	cr.mu.Unlock()
	if seeded || len(cr.goModules) == 0 {
		return ctx.Err()
	}
	for files, err := range cr.goModuleImportFileBatches(ctx, nil) {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := func() error {
			cr.mu.Lock()
			defer cr.mu.Unlock()
			return cr.resolveGoModuleImports(ctx, files, stats, nil)
		}(); err != nil {
			return err
		}
	}
	cr.mu.Lock()
	defer cr.mu.Unlock()
	if ctx.Err() == nil && state.packageRevision == cr.goModuleRevision && maps.Equal(state.packageModules, cr.goModules) {
		state.importsSeeded = true
	}
	return ctx.Err()
}

func (cr *CrossRepoResolver) rememberGoModuleImports(nodes []*graph.Node) {
	state := cr.goModuleRetry
	for _, n := range nodes {
		if n.Kind != graph.KindImport || n.Language != "go" {
			continue
		}
		_, path, ok := strings.Cut(n.ID, "::import::")
		if !ok {
			continue
		}
		for _, repo := range cr.goModuleRepos[cr.goModulePaths.longest(path)] {
			if repo == n.RepoPrefix {
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
	var ids []string
	for id := range state.imports[repo] {
		ids = append(ids, id)
	}
	return ids
}

func (cr *CrossRepoResolver) restoreGoModuleImports(ctx context.Context, repo string, stats *CrossRepoStats) error {
	if len(cr.graph.RepoPrefixes()) < 2 {
		return ctx.Err()
	}
	ids := cr.goModuleImportIDs(repo)
	for start := 0; start < len(ids); start += externalCallMutationChunk {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := cr.restoreGoModuleImportBatch(ctx, repo, ids[start:min(start+externalCallMutationChunk, len(ids))], stats); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func (cr *CrossRepoResolver) restoreGoModuleImportBatch(ctx context.Context, repo string, ids []string, stats *CrossRepoStats) error {
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
	var fileNodes map[string][]*graph.Node
	for _, id := range ids {
		n := nodes[id]
		if n == nil || n.Kind != graph.KindImport || n.Language != "go" {
			delete(state.imports[repo], id)
			continue
		}
		_, path, ok := strings.Cut(n.ID, "::import::")
		module := cr.goModules[repo]
		if n.RepoPrefix == repo || !ok || module == "" || (path != module && !strings.HasPrefix(path, module+"/")) {
			delete(state.imports[repo], id)
			continue
		}
		present := false
		for _, e := range outgoing[n.FilePath] {
			if e.Kind != graph.EdgeImports || e.Line != n.StartLine {
				continue
			}
			target := graph.UnresolvedName(e.To)
			if target == "" {
				target = e.To
			}
			if strings.HasPrefix(strings.TrimPrefix(target, externalCallPrefix), "dep::") && fileNodes == nil {
				fileNodes = cr.graph.GetFileNodesByPaths(files)
			}
			if goModuleImportPath(e, fileNodes[n.FilePath]) == path {
				present = true
				break
			}
			replacement, _ := goModuleReplacementFor(cr.goModuleReplacements, n.RepoPrefix, path)
			if replacement.Directory != "" && graph.RepoPrefixOfID(e.To) == n.RepoPrefix &&
				strings.HasSuffix(e.To, ".go") && filePathDir(e.To) == replacement.Directory {
				present = true
				break
			}
			// A longer module may own the bound package, even when this
			// statement is also registered under the edited provider.
			targetRepo := graph.RepoPrefixOfID(e.To)
			targetModule := cr.goModules[targetRepo]
			if strings.HasSuffix(e.To, ".go") && targetModule != "" &&
				(path == targetModule || strings.HasPrefix(path, targetModule+"/")) &&
				filePathDir(e.To) == targetRepo+strings.TrimPrefix(path, targetModule) {
				present = true
				break
			}
		}
		if !present {
			cr.graph.AddEdge(&graph.Edge{From: n.FilePath, To: "unresolved::import::" + path, Kind: graph.EdgeImports, FilePath: n.FilePath, Line: n.StartLine})
		}
	}
	return cr.resolveGoModuleImports(ctx, files, stats, nil)
}
