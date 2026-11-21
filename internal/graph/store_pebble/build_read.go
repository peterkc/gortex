package store_pebble

import (
	"errors"
	"iter"
	"sync"

	"github.com/zzet/gortex/internal/graph"
)

// ErrUnpublishedBuild signals that this handle cannot serve a pending read.
// OpenSnapshot returns a separate handle after the first publish.
var ErrUnpublishedBuild = errors.New("pebble build has no readable snapshot; use the published snapshot")

// graph.Store's legacy read methods cannot return errors. Keep the first
// refusal separate from mutation failure so a read attempt cannot poison a
// valid build. Consumers must open a Snapshot to read published rows.
func (b *BuildStore) refuseRead() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.readErr == nil {
		b.readErr = ErrUnpublishedBuild
	}
}

func (b *BuildStore) ResolveMutex() *sync.Mutex { return &b.resolveMu }

func (b *BuildStore) GetNode(id string) *graph.Node {
	b.refuseRead()
	return nil
}

func (b *BuildStore) GetNodeByQualName(qualName string) *graph.Node {
	b.refuseRead()
	return nil
}

func (b *BuildStore) GetNodesByQualNames(qualNames []string) map[string][]*graph.Node {
	b.refuseRead()
	return nil
}

func (b *BuildStore) FindNodesByName(name string) []*graph.Node {
	b.refuseRead()
	return nil
}

func (b *BuildStore) FindNodesByNameInRepo(name, repoPrefix string) []*graph.Node {
	b.refuseRead()
	return nil
}

func (b *BuildStore) FindNodesByNameContaining(substr string, limit int) []*graph.Node {
	b.refuseRead()
	return nil
}

func (b *BuildStore) GetFileNodes(filePath string) []*graph.Node {
	b.refuseRead()
	return nil
}

func (b *BuildStore) GetFileNodesByPaths(filePaths []string) map[string][]*graph.Node {
	b.refuseRead()
	return nil
}

func (b *BuildStore) GetRepoNodes(repoPrefix string) []*graph.Node {
	b.refuseRead()
	return nil
}

func (b *BuildStore) GetRepoNodesByLanguage(repoPrefix, language string) []*graph.Node {
	b.refuseRead()
	return nil
}

func (b *BuildStore) GetNodesByLanguage(language string) []*graph.Node {
	b.refuseRead()
	return nil
}

func (b *BuildStore) GetRepoNonContentNodes(repoPrefix string) []*graph.Node {
	b.refuseRead()
	return nil
}

func (b *BuildStore) GetOutEdges(nodeID string) []*graph.Edge {
	b.refuseRead()
	return nil
}

func (b *BuildStore) GetInEdges(nodeID string) []*graph.Edge {
	b.refuseRead()
	return nil
}

func (b *BuildStore) GetInEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	b.refuseRead()
	return nil
}

func (b *BuildStore) GetOutEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	b.refuseRead()
	return nil
}

func (b *BuildStore) GetEdgeCandidates(endpoints []graph.EdgeEndpoint, sites []graph.EdgeSite) graph.EdgeCandidateSet {
	b.refuseRead()
	return graph.EdgeCandidateSet{}
}

func (b *BuildStore) DistinctExternalTargets(kinds []graph.EdgeKind, fileSuffix string) []string {
	b.refuseRead()
	return nil
}

func (b *BuildStore) GetRepoEdges(repoPrefix string) []*graph.Edge {
	b.refuseRead()
	return nil
}

func (b *BuildStore) AllNodes() []*graph.Node {
	b.refuseRead()
	return nil
}

func (b *BuildStore) AllEdges() []*graph.Edge {
	b.refuseRead()
	return nil
}

func (b *BuildStore) EdgesByKind(kind graph.EdgeKind) iter.Seq[*graph.Edge] {
	b.refuseRead()
	return func(func(*graph.Edge) bool) {}
}

func (b *BuildStore) NodesByKind(kind graph.NodeKind) iter.Seq[*graph.Node] {
	b.refuseRead()
	return func(func(*graph.Node) bool) {}
}

func (b *BuildStore) EdgesWithUnresolvedTarget() iter.Seq[*graph.Edge] {
	b.refuseRead()
	return func(func(*graph.Edge) bool) {}
}

func (b *BuildStore) GetNodesByIDs(ids []string) map[string]*graph.Node {
	b.refuseRead()
	return nil
}

func (b *BuildStore) FindNodesByNames(names []string) map[string][]*graph.Node {
	b.refuseRead()
	return nil
}

func (b *BuildStore) NodeCount() int {
	b.refuseRead()
	return 0
}

func (b *BuildStore) EdgeCount() int {
	b.refuseRead()
	return 0
}

func (b *BuildStore) ProxyNodeCountAtLeast(limit int) bool {
	b.refuseRead()
	return false
}

func (b *BuildStore) Stats() graph.GraphStats {
	b.refuseRead()
	return graph.GraphStats{}
}

func (b *BuildStore) RepoStats() map[string]graph.GraphStats {
	b.refuseRead()
	return nil
}

func (b *BuildStore) RepoPrefixes() []string {
	b.refuseRead()
	return nil
}

func (b *BuildStore) EdgeIdentityRevisions() int {
	b.refuseRead()
	return 0
}

func (b *BuildStore) VerifyEdgeIdentities() error {
	b.refuseRead()
	return ErrUnpublishedBuild
}

func (b *BuildStore) RepoMemoryEstimate(repoPrefix string) graph.RepoMemoryEstimate {
	b.refuseRead()
	return graph.RepoMemoryEstimate{}
}

func (b *BuildStore) AllRepoMemoryEstimates() map[string]graph.RepoMemoryEstimate {
	b.refuseRead()
	return nil
}
