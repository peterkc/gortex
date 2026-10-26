package indexer

import (
	"context"
	"sort"
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
			if edge.Kind == graph.EdgeImports {
				delete(edge.Meta, resolver.CargoImportModuleMeta)
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

// Retry parked imports; new Rust files can also retarget marked provider bindings.
func (mi *MultiIndexer) cargoConsumerFilesForRepo(repo string, retargetBound bool) []string {
	return mi.cargoConsumerFilesForRepos([]string{repo}, retargetBound)
}

func (mi *MultiIndexer) cargoConsumerFilesForRepos(repos []string, retargetBound bool) []string {
	lookup := mi.cargoCrateLookup()
	if lookup == nil || len(repos) == 0 {
		return nil
	}
	return mi.cargoConsumerFilesForLookup(repos, retargetBound, lookup)
}

func (mi *MultiIndexer) cargoAddedFileConsumers(repo string) []string {
	snapshot := mi.cargoManifestSnapshot()
	if snapshot.hasUnknownManifest() {
		// Missing evidence must widen recovery, never discard a possible importer.
		files := map[string]struct{}{}
		for edge := range graph.EdgesLightSeq(mi.graph, graph.EdgeImports) {
			if strings.HasSuffix(edge.FilePath, ".rs") {
				files[edge.FilePath] = struct{}{}
			}
		}
		return sortedStringKeys(files)
	}
	if !snapshot.cargoDeclared {
		return nil
	}
	return mi.cargoConsumerFilesForLookup([]string{repo}, true, snapshot.cargo)
}

func (mi *MultiIndexer) cargoConsumerFilesForLookup(repos []string, retargetBound bool, lookup resolver.CargoCrateLookup) []string {
	wanted := make(map[string]bool, len(repos))
	for _, repo := range repos {
		wanted[repo] = true
	}
	files := make(map[string]struct{})
	var sites []graph.EdgeSite
	for edge := range graph.EdgesLightSeq(mi.graph, graph.EdgeImports) {
		if !strings.HasSuffix(edge.FilePath, ".rs") {
			continue
		}
		spec, external := strings.CutPrefix(edge.To, "external::")
		if !external {
			spec, external = strings.CutPrefix(edge.To, "unresolved::import::")
		}
		if !external {
			if retargetBound && edge.CrossRepo && wanted[graph.RepoPrefixOfID(edge.To)] {
				sites = append(sites, graph.EdgeSite{From: edge.From, Line: edge.Line, Kind: graph.EdgeImports})
			}
			continue
		}
		name, _, _ := strings.Cut(spec, "/")
		for _, crate := range lookup(edge.FilePath, name) {
			if wanted[graph.RepoPrefixOfID(crate.Root)] || wanted[graph.RepoPrefixOfID(edge.From)] {
				files[edge.FilePath] = struct{}{}
				break
			}
		}
	}
	// Close the light cursor before hydrating only marked-provider candidates.
	for start := 0; start < len(sites); start += 512 {
		page := sites[start:min(start+512, len(sites))]
		full := mi.graph.GetEdgeCandidates(nil, page)
		for _, site := range page {
			for _, edge := range full.Site(site.From, site.Line, site.Kind) {
				stub, _ := edge.Meta[resolver.CargoImportStubMeta].(string)
				if stub != "" && edge.CrossRepo && wanted[graph.RepoPrefixOfID(edge.To)] && strings.HasSuffix(edge.FilePath, ".rs") {
					files[edge.FilePath] = struct{}{}
				}
			}
		}
	}
	return sortedStringKeys(files)
}

// Consume warm receipts only after all repositories are registered.
func (mi *MultiIndexer) runWarmCargoResolve(ctx context.Context, files []string, full bool) error {
	mi.mu.RLock()
	pending := map[string]*IndexResult{}
	var repos []string
	for repo, meta := range mi.repos {
		if meta != nil && meta.cargoRetarget != nil {
			pending[repo] = meta.cargoRetarget
			repos = append(repos, repo)
		}
	}
	mi.mu.RUnlock()
	sort.Strings(repos)
	var err error
	if full {
		err = mi.runCrossRepoResolveContext(ctx, false, repos...)
	} else {
		if len(repos) > 0 {
			files = appendUniqueSorted(files, mi.cargoConsumerFilesForRepos(repos, true)...)
		}
		if err = context.Cause(ctx); err == nil {
			mi.runCrossRepoResolveFiles(files, repos...)
			err = context.Cause(ctx)
		}
	}
	if err == nil {
		mi.mu.Lock()
		for repo, receipt := range pending {
			if meta := mi.repos[repo]; meta != nil && meta.cargoRetarget == receipt {
				meta.cargoRetarget = nil
			}
		}
		mi.mu.Unlock()
	}
	return err
}
