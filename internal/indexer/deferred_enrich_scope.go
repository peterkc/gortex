package indexer

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"

	"github.com/zzet/gortex/internal/graph"
)

// markPendingEnrichFull records that the next deferred semantic pass must use
// the repository entry point. Full work dominates any queued file frontier.
func (idx *Indexer) markPendingEnrichFull() {
	idx.deferredEnrichMu.Lock()
	idx.deferredEnrichGeneration++
	idx.deferredEnrichFull = true
	idx.deferredEnrichFiles = nil
	idx.pendingEnrich.Store(true)
	idx.deferredEnrichMu.Unlock()
}

// markPendingEnrichFiles merges a known repo-scoped frontier. The complete set
// is dispatched to a batch-capable provider in one call; it is never expanded
// into an N+1 loop of per-file provider calls.
func (idx *Indexer) markPendingEnrichFiles(filePaths []string) {
	if len(filePaths) == 0 {
		return
	}

	idx.deferredEnrichMu.Lock()
	idx.deferredEnrichGeneration++
	if idx.pendingEnrich.Load() && !idx.deferredEnrichFull && len(idx.deferredEnrichFiles) == 0 {
		// A caller using the legacy atomic-only marker queued work whose scope is
		// unknown. Preserve it as a full pass instead of silently narrowing it.
		idx.deferredEnrichFull = true
	}
	if !idx.deferredEnrichFull {
		if idx.deferredEnrichFiles == nil {
			idx.deferredEnrichFiles = make(map[string]struct{}, len(filePaths))
		}
		for _, filePath := range filePaths {
			if filePath != "" {
				idx.deferredEnrichFiles[filePath] = struct{}{}
			}
		}
	}
	idx.pendingEnrich.Store(true)
	idx.deferredEnrichMu.Unlock()
}

// deferredEnrichScope snapshots pending work. An empty frontier is always
// treated as full for compatibility with older/direct pendingEnrich writers.
func (idx *Indexer) deferredEnrichScope() (filePaths []string, full bool, generation uint64) {
	idx.deferredEnrichMu.Lock()
	defer idx.deferredEnrichMu.Unlock()
	filePaths = make([]string, 0, len(idx.deferredEnrichFiles))
	for filePath := range idx.deferredEnrichFiles {
		filePaths = append(filePaths, filePath)
	}
	sort.Strings(filePaths)
	return filePaths,
		idx.deferredEnrichFull || len(filePaths) == 0,
		idx.deferredEnrichGeneration
}

// deferredEnrichFrontiers partitions one exact graph-file frontier by language
// with a single batched graph read. Deleted paths are absent and therefore need
// no provider work: their semantic nodes and edges were already evicted. The
// caller invokes each language provider once for the complete file batch.
func (idx *Indexer) deferredEnrichFrontiers(graphPaths []string) map[string][]string {
	nodesByFile := idx.graph.GetFileNodesByPaths(graphPaths)
	byLanguage := make(map[string][]string)
	for _, graphPath := range graphPaths {
		base := filepath.Base(graphPath)
		if base == "go.mod" || base == "go.work" {
			continue
		}
		for _, node := range nodesByFile[graphPath] {
			if node == nil || node.Kind != graph.KindFile || node.Language == "" {
				continue
			}
			byLanguage[node.Language] = append(byLanguage[node.Language], graphPath)
			break
		}
	}
	for language, paths := range byLanguage {
		byLanguage[language] = appendUniqueSorted(nil, paths...)
	}
	return byLanguage
}

// semanticDependencyFrontierForDeletedFiles captures the surviving source files
// whose resolvable references point at symbols about to be deleted. Every graph
// access is batched over the deletion set; the result is repo-local because a
// provider rooted at this indexer's checkout cannot safely enrich another repo.
func (idx *Indexer) semanticDependencyFrontierForDeletedFiles(relPaths []string) []string {
	if len(relPaths) == 0 {
		return nil
	}
	graphPaths := make([]string, 0, len(relPaths))
	for _, relPath := range relPaths {
		if relPath != "" {
			graphPaths = append(graphPaths, idx.prefixPath(relPath))
		}
	}
	graphPaths = appendUniqueSorted(nil, graphPaths...)
	nodesByFile := idx.graph.GetFileNodesByPaths(graphPaths)
	evictedIDs := make(map[string]struct{})
	var nodeIDs []string
	for _, graphPath := range graphPaths {
		for _, node := range nodesByFile[graphPath] {
			if node == nil || node.ID == "" {
				continue
			}
			if _, duplicate := evictedIDs[node.ID]; duplicate {
				continue
			}
			evictedIDs[node.ID] = struct{}{}
			nodeIDs = append(nodeIDs, node.ID)
		}
	}
	if len(nodeIDs) == 0 {
		return nil
	}

	incoming := idx.graph.GetInEdgesByNodeIDs(nodeIDs)
	sourceSet := make(map[string]struct{})
	for _, nodeID := range nodeIDs {
		for _, edge := range incoming[nodeID] {
			if edge == nil || !graph.IsResolvableRefEdge(edge.Kind) || graph.IsUnresolvedTarget(edge.To) {
				continue
			}
			if _, deleted := evictedIDs[edge.From]; !deleted {
				sourceSet[edge.From] = struct{}{}
			}
		}
	}
	if len(sourceSet) == 0 {
		return nil
	}
	sourceIDs := make([]string, 0, len(sourceSet))
	for id := range sourceSet {
		sourceIDs = append(sourceIDs, id)
	}
	sources := idx.graph.GetNodesByIDs(sourceIDs)
	var frontier []string
	for _, id := range sourceIDs {
		node := sources[id]
		if node == nil || node.FilePath == "" || node.RepoPrefix != idx.repoPrefix {
			continue
		}
		frontier = append(frontier, node.FilePath)
	}
	return appendUniqueSorted(nil, frontier...)
}

// pendingEnrichFrontier reads the DURABLE per-file deferral ledger: the graph
// paths whose KindFile node still carries graph.MetaReparsePendingEnrichment,
// which the incremental / watch paths stamp on a file they re-parsed while
// semantic enrichment was deferred (semantic.enrich_on_watch=false, or a
// deferred global-pass window). Unlike the in-memory deferredEnrichFiles set it
// lives on the node itself, so it survives a daemon restart — and unlike the
// whole-repo completion marker it needs neither a git sha nor a clean tree, so
// it is the ONLY evidence of outstanding enrichment for a repo that is not a
// git checkout or whose watch-indexed edits are still uncommitted.
//
// One repo-scoped, kind-filtered projection per call; the marker is a binary
// meta blob, so the key test is decoded backend-side rather than pushed into
// the query. Bounded by the repo's file count, not its symbol count.
func (idx *Indexer) pendingEnrichFrontier() []string {
	nodes := graph.ReadRepoNodesByKindsWithMetaKey(
		idx.graph, idx.repoPrefix, idx.workspaceID,
		[]graph.NodeKind{graph.KindFile}, graph.MetaReparsePendingEnrichment,
	)
	var frontier []string
	for _, node := range nodes {
		if node == nil || node.FilePath == "" {
			continue
		}
		// Mirror suppressionMayBeStale: only a truthy marker is pending. The
		// writer deletes the key when it clears, so a false value is unreachable
		// today — reading it defensively keeps the two consumers in agreement.
		if pending, _ := node.Meta[graph.MetaReparsePendingEnrichment].(bool); pending {
			frontier = append(frontier, node.FilePath)
		}
	}
	return appendUniqueSorted(nil, frontier...)
}

// dischargePendingEnrichFrontier clears the durable per-file deferral for the
// graph paths a deferred pass just covered. Without it the ledger only ever
// grows: nothing but a watch-time enrichment (which is off by construction on
// the deferral path) cleared the marker, so a resumed pass would re-arm the
// same files on every restart, and find_usages would keep riding every one of
// those files' suppressions with a re-verification-pending flag that no
// completed enrichment could ever retire.
func (idx *Indexer) dischargePendingEnrichFrontier(graphPaths []string) {
	if len(graphPaths) == 0 {
		return
	}
	discharged := make(map[string]bool, len(graphPaths))
	for _, graphPath := range graphPaths {
		if graphPath != "" {
			discharged[graphPath] = false
		}
	}
	idx.setReparsePendingEnrichments(discharged)
}

// clearPendingEnrich clears only the work represented by generation. If a
// watcher queued another change during enrichment, that newer work remains.
func (idx *Indexer) clearPendingEnrich(generation uint64) bool {
	idx.deferredEnrichMu.Lock()
	defer idx.deferredEnrichMu.Unlock()
	if idx.deferredEnrichGeneration != generation {
		return false
	}
	idx.deferredEnrichFiles = nil
	idx.deferredEnrichFull = false
	idx.pendingEnrich.Store(false)
	return true
}

// deferredEnrichOutcome is published by the pool's completion channel, then
// consumed under the repository mutation lane before contract finalization.
type deferredEnrichOutcome struct {
	generation uint64
	files      []string
	completed  []string
	full       bool
	sha        string
	dirty      bool
	err        error
}

type deferredEnrichOptions struct{ PrepareFileExports bool }

// finishDeferredFileEnrich requires repository mutation ownership. Keeping the
// generation check and durable discharge in that lane prevents a newer watcher
// payload from being erased between the check and the write.
func (idx *Indexer) finishDeferredFileEnrich(outcome deferredEnrichOutcome) error {
	if outcome.full {
		if !idx.clearPendingEnrich(outcome.generation) {
			return fmt.Errorf("%s: enrichment generation superseded", idx.repoPrefix)
		}
		// The repository owner excludes newer durable writes through discharge.
		idx.dischargePendingEnrichFrontier(idx.pendingEnrichFrontier())
		idx.semanticMgr.RecordRepoEnrichmentComplete(idx.graph, idx.repoPrefix, outcome.sha, outcome.dirty)
		return nil
	}
	if len(outcome.files) == 0 {
		return outcome.err
	}
	idx.deferredEnrichMu.Lock()
	defer idx.deferredEnrichMu.Unlock()
	if idx.deferredEnrichGeneration != outcome.generation {
		return errors.Join(outcome.err, fmt.Errorf("%s: enrichment generation superseded", idx.repoPrefix))
	}
	idx.dischargePendingEnrichFrontier(outcome.completed)
	for _, file := range outcome.completed {
		delete(idx.deferredEnrichFiles, file)
	}
	if len(idx.deferredEnrichFiles) == 0 {
		idx.deferredEnrichFiles = nil
		idx.deferredEnrichFull = false
		idx.pendingEnrich.Store(false)
	}
	return outcome.err
}
