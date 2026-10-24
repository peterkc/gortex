package indexer

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/resolver"
)

// Preserve importer-owned facts before their provider's files disappear.
func restubTrackedCargoRepoIncoming(g graph.Store, repo string) []string {
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
		files = appendUniqueSorted(files, restubTrackedCargoImportsFromView(g, evicted, incoming, true)...)
		ids = ids[:0]
	}
	for node := range graph.RepoNodeIdentitiesSeq(g, []string{repo}, graph.KindFile, graph.KindFunction, graph.KindMethod) {
		file, _, _ := strings.Cut(node.ID, "::")
		if !strings.HasSuffix(file, ".rs") {
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

func restubTrackedCargoImportsFromView(g graph.Store, evicted map[string]struct{}, incoming map[string][]*graph.Edge, parkCalls bool) []string {
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
			key := resolver.CargoImportStubMeta
			if edge.Kind == graph.EdgeCalls {
				key = resolver.CargoCallStubMeta
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
func (mi *MultiIndexer) cargoConsumerFilesForRepo(repo string) []string {
	lookup := mi.cargoCrateLookup()
	if lookup == nil {
		return nil
	}
	files := make(map[string]struct{})
	for edge := range graph.EdgesLightSeq(mi.graph, graph.EdgeImports) {
		spec, external := strings.CutPrefix(edge.To, "external::")
		if !external || !strings.HasSuffix(edge.FilePath, ".rs") {
			continue
		}
		name, _, _ := strings.Cut(spec, "/")
		for _, crate := range lookup(edge.FilePath, name) {
			if graph.RepoPrefixOfID(crate.Root) == repo || graph.RepoPrefixOfID(edge.From) == repo {
				files[edge.FilePath] = struct{}{}
				break
			}
		}
	}
	return sortedStringKeys(files)
}
