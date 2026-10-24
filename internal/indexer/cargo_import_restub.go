package indexer

import (
	"maps"
	"slices"
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
		files = appendUniqueSorted(files, restubTrackedCargoImportsFromView(g, evicted, incoming)...)
		ids = ids[:0]
	}
	for node := range graph.RepoNodeIdentitiesSeq(g, []string{repo}, graph.KindFile) {
		if !strings.HasSuffix(node.ID, ".rs") {
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

func restubTrackedCargoImportsFromView(g graph.Store, evicted map[string]struct{}, incoming map[string][]*graph.Edge) []string {
	consumers := make(map[string]map[string]bool)
	var batch []graph.EdgeReindex
	for _, edges := range incoming {
		for _, edge := range edges {
			if edge == nil || edge.Kind != graph.EdgeImports {
				continue
			}
			stub, _ := edge.Meta[resolver.CargoImportStubMeta].(string)
			if _, gone := evicted[edge.From]; stub == "" || gone || edge.To == stub {
				continue
			}
			if consumers[edge.FilePath] == nil {
				consumers[edge.FilePath] = make(map[string]bool)
			}
			consumers[edge.FilePath][graph.RepoPrefixOfID(edge.To)] = true
			old := edge.To
			edge.To, edge.CrossRepo = stub, false
			batch = append(batch, graph.EdgeReindex{Edge: edge, OldTo: old})
		}
	}
	files := slices.Sorted(maps.Keys(consumers))
	if len(files) == 0 {
		return nil
	}
	var ids []string
	for _, nodes := range g.GetFileNodesByPaths(files) {
		for _, node := range nodes {
			if node != nil {
				ids = append(ids, node.ID)
			}
		}
	}
	for _, edges := range g.GetOutEdgesByNodeIDs(ids) {
		for _, edge := range edges {
			if edge == nil || edge.Kind != graph.EdgeCalls || !edge.CrossRepo || !consumers[edge.FilePath][graph.RepoPrefixOfID(edge.To)] {
				continue
			}
			if _, gone := evicted[edge.From]; gone {
				continue
			}
			old := edge.To
			_, name, symbol := strings.Cut(old, "::")
			if !symbol {
				continue
			}
			graph.StashRestubProvenance(edge)
			edge.To, edge.CrossRepo, edge.ConfidenceLabel = graph.UnresolvedMarker+name, false, ""
			batch = append(batch, graph.EdgeReindex{Edge: edge, OldTo: old})
		}
	}
	if len(batch) > 0 {
		g.ReindexEdges(batch)
	}
	return files
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
