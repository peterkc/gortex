package indexer

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/resolver"
)

// Preserve importer-owned facts before their provider's files disappear.
func restubTrackedPythonRepoIncoming(g graph.Store, repo string) []string {
	if repo == "" {
		return nil
	}
	var files, ids []string
	flush := func() {
		incoming := g.GetInEdgesByNodeIDs(ids)
		evicted := make(map[string]struct{})
		for _, edges := range incoming {
			for _, edge := range edges {
				if edge != nil && graph.RepoPrefixOfID(edge.From) == repo {
					evicted[edge.From] = struct{}{}
				}
			}
		}
		files = appendUniqueSorted(files, restubTrackedPythonImportsFromView(g, evicted, incoming, true)...)
		ids = ids[:0]
	}
	for node := range graph.RepoNodeIdentitiesSeq(g, []string{repo}, graph.KindFile, graph.KindFunction, graph.KindMethod, graph.KindType) {
		file, _, _ := strings.Cut(node.ID, "::")
		if !strings.HasSuffix(file, ".py") {
			continue
		}
		ids = append(ids, node.ID)
		if len(ids) == 512 {
			flush()
		}
	}
	if len(ids) > 0 {
		flush()
	}
	return files
}

func restubTrackedPythonImportsFromView(g graph.Store, evicted map[string]struct{}, incoming map[string][]*graph.Edge, parkCalls bool) []string {
	consumers := make(map[string]struct{})
	var batch []graph.EdgeReindex
	for id, edges := range incoming {
		if _, changing := evicted[id]; !parkCalls && !changing {
			continue
		}
		for _, edge := range edges {
			if edge == nil || !edge.CrossRepo {
				continue
			}
			key := resolver.PythonImportStubMeta
			if edge.Kind == graph.EdgeCalls {
				key = resolver.PythonCallStubMeta
			} else if edge.Kind != graph.EdgeImports {
				continue
			}
			stub, _ := edge.Meta[key].(string)
			if _, gone := evicted[edge.From]; stub == "" || gone || edge.To == stub {
				continue
			}
			consumers[edge.FilePath] = struct{}{}
			// Save paths keep the existing carry/restub decision and its dataflow sites.
			if edge.Kind == graph.EdgeCalls && !parkCalls {
				continue
			}
			old := edge.To
			// A package edit can change a re-export without changing the old
			// definition. Reopen its dependent calls when the import is restubbed.
			if edge.Kind == graph.EdgeImports && strings.HasSuffix(old, "/__init__.py") {
				for row := range graph.EdgesInScopeSeq(g, nil, []string{edge.FilePath}, graph.EdgeCalls) {
					call := row.Edge
					if call == nil || !call.CrossRepo || call.Meta[resolver.PythonReExportFileMeta] != old {
						continue
					}
					original, _ := call.Meta[resolver.PythonCallStubMeta].(string)
					if original == "" {
						continue
					}
					previous := call.To
					graph.StashRestubProvenance(call)
					call.To, call.CrossRepo, call.ConfidenceLabel = original, false, ""
					delete(call.Meta, resolver.PythonReExportFileMeta)
					batch = append(batch, graph.EdgeReindex{Edge: call, OldTo: previous})
					// SQLite reads return separate objects. Update the prior view too,
					// so the generic carry cannot re-state the old call target.
					for _, prior := range incoming[previous] {
						if prior != nil && prior.From == call.From && prior.Kind == call.Kind && prior.Line == call.Line && prior.To == previous {
							prior.To, prior.CrossRepo, prior.ConfidenceLabel = call.To, false, ""
						}
					}
				}
			}
			if edge.Kind == graph.EdgeCalls {
				graph.StashRestubProvenance(edge)
				edge.ConfidenceLabel = ""
			}
			edge.To, edge.CrossRepo = stub, false
			batch = append(batch, graph.EdgeReindex{Edge: edge, OldTo: old})
		}
	}
	if len(batch) > 0 {
		g.ReindexEdges(batch)
	}
	return sortedStringKeys(consumers)
}

// Retry only parked imports claimed by this provider, or owned by this repo.
func (mi *MultiIndexer) pythonConsumerFilesForRepo(repo string) []string {
	lookup := mi.pythonPackageLookup()
	if lookup == nil {
		return nil
	}
	files := make(map[string]struct{})
	type candidate struct{ caller, top, file string }
	var candidates []candidate
	for edge := range graph.EdgesLightSeq(mi.graph, graph.EdgeImports) {
		if !strings.HasSuffix(edge.FilePath, ".py") {
			continue
		}
		top := ""
		for _, prefix := range []string{"module::pypi:", "external::", "unresolved::import::"} {
			if spec, ok := strings.CutPrefix(edge.To, prefix); ok {
				top, _, _ = strings.Cut(spec, ".")
				break
			}
		}
		candidates = append(candidates, candidate{graph.RepoPrefixOfID(edge.From), top, edge.FilePath})
	}
	// Providers may query the store; release the import cursor first.
	for _, item := range candidates {
		for provider := range lookup.Providers(item.caller, item.top) {
			if provider == repo || item.caller == repo {
				files[item.file] = struct{}{}
				break
			}
		}
	}
	return sortedStringKeys(files)
}

// Keep candidate importers before warmup has registered every repository.
// The existing cross-repo matcher still checks providers and declarations.
func pythonConsumerFilesForAddedFiles(g graph.Store, files []string) []string {
	if len(files) == 0 {
		return nil
	}
	tops := map[string]bool{}
	for _, file := range files {
		rel := strings.TrimPrefix(file, graph.RepoPrefixOfID(file)+"/")
		for _, path := range []string{rel, strings.TrimPrefix(rel, "src/")} {
			top, _, _ := strings.Cut(path, "/")
			tops[strings.TrimSuffix(top, ".py")] = true
		}
	}
	consumers := map[string]struct{}{}
	for edge := range graph.EdgesLightSeq(g, graph.EdgeImports) {
		if !strings.HasSuffix(edge.FilePath, ".py") {
			continue
		}
		for _, prefix := range []string{"module::pypi:", "external::", "unresolved::import::"} {
			if spec, ok := strings.CutPrefix(edge.To, prefix); ok {
				top, _, _ := strings.Cut(spec, ".")
				if tops[top] {
					consumers[edge.FilePath] = struct{}{}
				}
				break
			}
		}
	}
	return sortedStringKeys(consumers)
}

func (mi *MultiIndexer) pythonAddedFileConsumers(files []string) []string {
	if len(files) == 0 {
		return nil
	}
	// Warmup can publish a provider receipt before its configured consumers
	// are registered. Preserve that frontier until the workspace pass.
	warming := false
	if mi.configMgr != nil {
		configuredRepos := mi.configMgr.Global().Repos
		warming = len(configuredRepos) > 1 && len(mi.AllMetadata()) < len(configuredRepos)
	}
	if !warming && len(mi.AllMetadata()) < 2 {
		return nil
	}
	return pythonConsumerFilesForAddedFiles(mi.graph, files)
}
