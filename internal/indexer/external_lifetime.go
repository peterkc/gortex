package indexer

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// captureStdlibCandidates reads only old file destinations and recorded edges,
// before eviction erases them. Module parents come from attributed symbols, not
// a scan of the repository's external identities.
func (idx *Indexer) captureStdlibCandidates(paths []string, outByNode map[string][]*graph.Edge) []string {
	var ids []string
	collect := func(edge *graph.Edge) {
		if edge == nil {
			return
		}
		for _, id := range []string{edge.From, edge.To} {
			if strings.HasPrefix(id, idx.repoPrefix+"::stdlib::") || strings.HasPrefix(id, idx.repoPrefix+"::module::go:") {
				ids = append(ids, id)
			}
		}
	}
	for _, edges := range outByNode {
		for _, edge := range edges {
			collect(edge)
		}
	}
	if reader, ok := graph.RecordedEdgesOf(idx.graph); ok {
		for _, edge := range reader.RecordedEdgesAt(paths) {
			collect(edge)
		}
	}
	ids = appendUniqueSorted(nil, ids...)
	if len(ids) == 0 {
		return nil
	}
	nodes := idx.graph.GetNodesByIDs(ids)
	var candidates, modules []string
	for _, id := range ids {
		node := nodes[id]
		if !ownedStdlibNode(node, idx.repoPrefix) {
			continue
		}
		candidates = append(candidates, id)
		if node.Kind != graph.KindModule {
			modules = append(modules, stdlibModuleID(node))
		}
	}
	modules = appendUniqueSorted(nil, modules...)
	if len(modules) > 0 {
		parents := idx.graph.GetNodesByIDs(modules)
		for _, id := range modules {
			if ownedStdlibNode(parents[id], idx.repoPrefix) {
				candidates = append(candidates, id)
			}
		}
	}
	return appendUniqueSorted(nil, candidates...)
}

// capturePathStdlibCandidates uses the deletion engine's existing file chunk
// bound. The returned frontier spans chunks, but no graph projection does.
func (idx *Indexer) capturePathStdlibCandidates(paths []string) []string {
	var candidates []string
	for start := 0; start < len(paths); start += deletedBatchFiles {
		chunk := paths[start:min(start+deletedBatchFiles, len(paths))]
		var ids []string
		for _, nodes := range idx.graph.GetFileNodesByPaths(chunk) {
			for _, node := range nodes {
				if node != nil {
					ids = append(ids, node.ID)
				}
			}
		}
		old := idx.graph.GetOutEdgesByNodeIDs(ids)
		candidates = appendUniqueSorted(candidates, idx.captureStdlibCandidates(chunk, old)...)
	}
	return candidates
}

func stdlibModuleID(node *graph.Node) string {
	path, _ := node.Meta["module_path"].(string)
	return graph.StubID(node.RepoPrefix, graph.StubKindModule, "go:"+path)
}

func ownedStdlibNode(node *graph.Node, repoPrefix string) bool {
	if node == nil || repoPrefix == "" || node.RepoPrefix != repoPrefix || node.FilePath != "" || node.Language != "go" {
		return false
	}
	if node.Kind == graph.KindModule {
		path, _ := node.Meta["import_path"].(string)
		return path != "" && node.Meta["role"] == "stdlib" && node.Meta["ecosystem"] == "go" &&
			node.ID == graph.StubID(repoPrefix, graph.StubKindModule, "go:"+path)
	}
	path, _ := node.Meta["module_path"].(string)
	return node.Kind == graph.KindFunction && path != "" && node.Meta["external"] == true && node.Meta["module_role"] == "stdlib" &&
		node.Name != "" && node.ID == graph.StubID(repoPrefix, graph.StubKindStdlib, path, node.Name)
}

// settleStdlibCandidates retains candidates until the complete deferred resolver
// tail. A synchronous caller has already resolved its replacement graph.
func (idx *Indexer) settleStdlibCandidates(ids, paths []string, batch *reparsePendingEnrichmentBatch) {
	if len(ids) == 0 {
		return
	}
	if batch != nil && batch.deferResolverCatchup {
		batch.stdlibCandidates = appendUniqueSorted(batch.stdlibCandidates, ids...)
		batch.stdlibCandidatePaths = appendUniqueSorted(batch.stdlibCandidatePaths, paths...)
		return
	}
	idx.retireStdlibCandidates(ids, paths)
}

func (idx *Indexer) retireDeferredStdlibCandidates(batch *reparsePendingEnrichmentBatch) (int, int) {
	if batch == nil {
		return 0, 0
	}
	ids, paths := batch.stdlibCandidates, batch.stdlibCandidatePaths
	batch.stdlibCandidates, batch.stdlibCandidatePaths = nil, nil
	return idx.retireStdlibCandidates(ids, paths)
}

// retireStdlibCandidates ignores only the attributed symbol's synthetic
// membership. Every other incoming or outgoing edge protects the identity.
// Modules are considered after their doomed members, so an import or a surviving
// member keeps its module. Unsupported stores retain candidates unchanged.
func (idx *Indexer) retireStdlibCandidates(ids, paths []string) (int, int) {
	evicter, ok := idx.graph.(graph.PathlessNodeBatchEvicter)
	if !ok || len(ids) == 0 {
		return 0, 0
	}
	// A per-file delta can still restore unchanged files' prior rows. Decide
	// only after that restatement, or its endpoint filter loses live references.
	if idx.inertReparsed != nil {
		idx.deferredStdlibCandidates = appendUniqueSorted(idx.deferredStdlibCandidates, ids...)
		idx.deferredStdlibCandidatePaths = appendUniqueSorted(idx.deferredStdlibCandidatePaths, paths...)
		return 0, 0
	}
	ids = appendUniqueSorted(nil, ids...)
	nodes := idx.graph.GetNodesByIDs(ids)
	// References emitted by the replacement files prove liveness without
	// loading a shared symbol's complete repository-wide adjacency.
	keep := make(map[string]bool)
	if reader, ok := graph.RecordedEdgesOf(idx.graph); ok && len(paths) > 0 {
		for _, edge := range reader.RecordedEdgesAt(paths) {
			if edge != nil {
				keep[edge.From], keep[edge.To] = true, true
			}
		}
	}
	for _, id := range ids {
		node := nodes[id]
		if keep[id] && ownedStdlibNode(node, idx.repoPrefix) && node.Kind != graph.KindModule {
			keep[stdlibModuleID(node)] = true
		}
	}
	var candidates []string
	for _, id := range ids {
		if !keep[id] && ownedStdlibNode(nodes[id], idx.repoPrefix) {
			candidates = append(candidates, id)
		}
	}
	if len(candidates) == 0 {
		return 0, 0
	}
	in := idx.graph.GetInEdgesByNodeIDs(candidates)
	out := idx.graph.GetOutEdgesByNodeIDs(candidates)
	doomed := make(map[string]bool)
	var symbols, modules []string
	for _, id := range candidates {
		node := nodes[id]
		if node.Kind == graph.KindModule || len(in[id]) > 0 {
			continue
		}
		live := false
		for _, edge := range out[id] {
			if edge == nil || edge.Kind != graph.EdgeMemberOf || edge.FilePath != "" || edge.To != stdlibModuleID(node) || edge.Origin != graph.OriginASTResolved {
				live = true
				break
			}
		}
		if !live {
			doomed[id] = true
			symbols = append(symbols, id)
		}
	}
	for _, id := range candidates {
		if nodes[id].Kind != graph.KindModule || len(out[id]) > 0 {
			continue
		}
		live := false
		for _, edge := range in[id] {
			if edge == nil || !doomed[edge.From] || edge.Kind != graph.EdgeMemberOf || edge.FilePath != "" || edge.Origin != graph.OriginASTResolved {
				live = true
				break
			}
		}
		if !live {
			modules = append(modules, id)
		}
	}
	remove := append(symbols, modules...)
	if len(remove) == 0 {
		return 0, 0
	}
	n, e := evicter.EvictPathlessNodesByIDs(remove)
	// The existing evicter owns counters and receipts, but not symbol FTS.
	// Remove sidecars only for identities actually absent after its effect.
	remaining := idx.graph.GetNodesByIDs(remove)
	var removed []string
	for _, id := range remove {
		if remaining[id] == nil {
			removed = append(removed, id)
			idx.removeFromSearch(nodes[id])
		}
	}
	idx.deleteSymbolFTS(removed)
	return n, e
}
