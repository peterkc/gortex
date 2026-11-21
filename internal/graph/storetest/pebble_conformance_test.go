package storetest_test

import (
	"context"
	"iter"
	"maps"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/graph/storetest"
	"github.com/zzet/gortex/internal/storage/backend"
)

func TestPebbleStoreConformance(t *testing.T) {
	storetest.RunConformance(t, func(t *testing.T) graph.Store {
		b := conformanceBuild(t)
		t.Cleanup(func() {
			if t.Failed() || t.Skipped() {
				return
			}
			publishCheckpoint(t, b)
		})
		return &publishedPrefixStore{t: t, BuildStore: b}
	}, storetest.Semantics{
		Reads: storetest.ReadsDetached, EvictsEmptyRepoPrefix: true, AdjacencyLineOrdered: true,
		CanonicalIncomingTies: true, ConcurrentSnapshotReads: conformanceSnapshotReads,
	})
}

func conformanceBuild(t *testing.T) *store_pebble.BuildStore {
	t.Helper()
	b, err := backend.OpenPebbleBuild(filepath.Join(t.TempDir(), "graph"), store_pebble.FullProfile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := b.Close(); err != nil {
			t.Error(err)
		}
	})
	return b
}

func publishCheckpoint(t *testing.T, b *store_pebble.BuildStore) *store_pebble.Snapshot {
	t.Helper()
	if err := b.Publish(context.WithoutCancel(t.Context())); err != nil {
		t.Fatal(err)
	}
	s, err := b.OpenSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.ReadError(); err != nil {
			t.Error(err)
		}
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}

// Writes exercise the original BuildStore. Each distinct read prefix also
// replays those writes on a new BuildStore and publishes once. The adapter
// never serves or inspects unpublished rows and preserves the exact write
// order of fixtures that interleave mutations with assertions.
type publishedPrefixStore struct {
	*store_pebble.BuildStore
	t       *testing.T
	mu      sync.Mutex
	journal []func(*store_pebble.BuildStore)
	saved   *store_pebble.Snapshot
}

func (s *publishedPrefixStore) record(apply func(*store_pebble.BuildStore)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	apply(s.BuildStore)
	s.journal = append(s.journal, apply)
	s.saved = nil
}

func (s *publishedPrefixStore) checkpoint() *store_pebble.Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.saved == nil {
		replay := conformanceBuild(s.t)
		for _, apply := range s.journal {
			apply(replay)
		}
		s.saved = publishCheckpoint(s.t, replay)
	}
	return s.saved
}

func copyNode(n *graph.Node) *graph.Node {
	if n == nil {
		return nil
	}
	copy := *n
	copy.Meta = maps.Clone(n.Meta)
	return &copy
}

func copyEdge(e *graph.Edge) *graph.Edge {
	if e == nil {
		return nil
	}
	copy := *e
	copy.Meta = maps.Clone(e.Meta)
	return &copy
}

func (s *publishedPrefixStore) AddNode(n *graph.Node) {
	n = copyNode(n)
	s.record(func(b *store_pebble.BuildStore) { b.AddNode(n) })
}

func (s *publishedPrefixStore) AddEdge(e *graph.Edge) {
	e = copyEdge(e)
	s.record(func(b *store_pebble.BuildStore) { b.AddEdge(e) })
}

func (s *publishedPrefixStore) AddBatch(nodes []*graph.Node, edges []*graph.Edge) {
	nodes, edges = slices.Clone(nodes), slices.Clone(edges)
	for i, n := range nodes {
		nodes[i] = copyNode(n)
	}
	for i, e := range edges {
		edges[i] = copyEdge(e)
	}
	s.record(func(b *store_pebble.BuildStore) { b.AddBatch(nodes, edges) })
}

func (s *publishedPrefixStore) SetEdgeProvenance(e *graph.Edge, origin string) (changed bool) {
	before := copyEdge(e)
	s.record(func(b *store_pebble.BuildStore) {
		entry := copyEdge(before)
		ok := b.SetEdgeProvenance(entry, origin)
		if b == s.BuildStore {
			changed = ok
			if ok {
				e.Origin, e.Tier = entry.Origin, entry.Tier
			}
		}
	})
	return changed
}

func (s *publishedPrefixStore) SetEdgeProvenanceBatch(batch []graph.EdgeProvenanceUpdate) (changed int) {
	before := slices.Clone(batch)
	for i := range before {
		before[i].Edge = copyEdge(before[i].Edge)
	}
	s.record(func(b *store_pebble.BuildStore) {
		entries := slices.Clone(before)
		for i := range entries {
			entries[i].Edge = copyEdge(entries[i].Edge)
		}
		n := b.SetEdgeProvenanceBatch(entries)
		if b == s.BuildStore {
			changed = n
			for i, item := range entries {
				if item.Edge != nil {
					batch[i].Edge.Origin, batch[i].Edge.Tier = item.Edge.Origin, item.Edge.Tier
				}
			}
		}
	})
	return changed
}

func (s *publishedPrefixStore) ReindexEdge(e *graph.Edge, oldTo string) {
	e = copyEdge(e)
	s.record(func(b *store_pebble.BuildStore) { b.ReindexEdge(e, oldTo) })
}

func (s *publishedPrefixStore) ReindexEdges(batch []graph.EdgeReindex) {
	batch = slices.Clone(batch)
	for i := range batch {
		batch[i].Edge = copyEdge(batch[i].Edge)
	}
	s.record(func(b *store_pebble.BuildStore) { b.ReindexEdges(batch) })
}

func (s *publishedPrefixStore) RemoveEdge(from, to string, kind graph.EdgeKind) (removed bool) {
	s.record(func(b *store_pebble.BuildStore) {
		got := b.RemoveEdge(from, to, kind)
		if b == s.BuildStore {
			removed = got
		}
	})
	return removed
}

func (s *publishedPrefixStore) EvictFile(path string) (nodes, edges int) {
	s.record(func(b *store_pebble.BuildStore) {
		n, e := b.EvictFile(path)
		if b == s.BuildStore {
			nodes, edges = n, e
		}
	})
	return nodes, edges
}

func (s *publishedPrefixStore) EvictRepo(repo string) (nodes, edges int) {
	s.record(func(b *store_pebble.BuildStore) {
		n, e := b.EvictRepo(repo)
		if b == s.BuildStore {
			nodes, edges = n, e
		}
	})
	return nodes, edges
}

func (s *publishedPrefixStore) PersistEdgeAttributes(e *graph.Edge) {
	e = copyEdge(e)
	s.record(func(b *store_pebble.BuildStore) { b.PersistEdgeAttributes(e) })
}

func (s *publishedPrefixStore) PersistEdgeAttributesBatch(edges []*graph.Edge) {
	edges = slices.Clone(edges)
	for i := range edges {
		edges[i] = copyEdge(edges[i])
	}
	s.record(func(b *store_pebble.BuildStore) { b.PersistEdgeAttributesBatch(edges) })
}

func (s *publishedPrefixStore) BulkUpsertSymbolFTS(repo string, items []graph.SymbolFTSItem) (err error) {
	items = slices.Clone(items)
	s.record(func(b *store_pebble.BuildStore) {
		got := b.BulkUpsertSymbolFTS(repo, items)
		if b == s.BuildStore {
			err = got
		} else if got != nil {
			s.t.Fatal(got)
		}
	})
	return err
}

func (s *publishedPrefixStore) SearchSymbols(query string, limit int) ([]graph.SymbolHit, error) {
	return s.checkpoint().SearchSymbols(query, limit)
}

func (s *publishedPrefixStore) SearchSymbolBundles(query string, limit int) ([]graph.SymbolBundle, error) {
	return s.checkpoint().SearchSymbolBundles(query, limit)
}

func (s *publishedPrefixStore) AllEdgesLight(kinds ...graph.EdgeKind) []*graph.Edge {
	return s.checkpoint().AllEdgesLight(kinds...)
}

func (s *publishedPrefixStore) EdgesByKinds(kinds []graph.EdgeKind) iter.Seq[*graph.Edge] {
	return s.checkpoint().EdgesByKinds(kinds)
}

func (s *publishedPrefixStore) NodesByKinds(kinds []graph.NodeKind) []*graph.Node {
	return s.checkpoint().NodesByKinds(kinds)
}

func (s *publishedPrefixStore) NodeIDsByKinds(kinds []graph.NodeKind) []string {
	return s.checkpoint().NodeIDsByKinds(kinds)
}

func (s *publishedPrefixStore) EdgeKindCounts() map[graph.EdgeKind]int {
	return s.checkpoint().EdgeKindCounts()
}

func (s *publishedPrefixStore) InDegreeForNodes(ids []string) map[string]int {
	return s.checkpoint().InDegreeForNodes(ids)
}

func (s *publishedPrefixStore) BulkSetCloneShingles(repo string, rows map[string][]uint64) (err error) {
	rows = maps.Clone(rows)
	for id, values := range rows {
		rows[id] = slices.Clone(values)
	}
	s.record(func(b *store_pebble.BuildStore) {
		got := b.BulkSetCloneShingles(repo, rows)
		if b == s.BuildStore {
			err = got
		} else if got != nil {
			s.t.Fatal(got)
		}
	})
	return err
}

func (s *publishedPrefixStore) DeleteCloneShingles(ids []string) (err error) {
	ids = slices.Clone(ids)
	s.record(func(b *store_pebble.BuildStore) {
		got := b.DeleteCloneShingles(ids)
		if b == s.BuildStore {
			err = got
		} else if got != nil {
			s.t.Fatal(got)
		}
	})
	return err
}

func (s *publishedPrefixStore) LoadCloneShingles(repo string) (map[string][]uint64, error) {
	return s.checkpoint().LoadCloneShingles(repo)
}

func (s *publishedPrefixStore) EdgeAdjacencyForKinds(edges []graph.EdgeKind, nodes []graph.NodeKind) iter.Seq[[2]string] {
	return s.checkpoint().EdgeAdjacencyForKinds(edges, nodes)
}

func (s *publishedPrefixStore) InEdgeCountsByKind(kinds []graph.EdgeKind) map[string]int {
	return s.checkpoint().InEdgeCountsByKind(kinds)
}

func (s *publishedPrefixStore) NodeDegreeCounts(ids []string, kinds []graph.EdgeKind) []graph.NodeDegreeRow {
	return s.checkpoint().NodeDegreeCounts(ids, kinds)
}

func (s *publishedPrefixStore) NodeFanCounts(ids []string, in, out []graph.EdgeKind) []graph.NodeFanRow {
	return s.checkpoint().NodeFanCounts(ids, in, out)
}

func (s *publishedPrefixStore) NodeDegreeByKinds(kinds []graph.NodeKind, path string) []graph.NodeDegreeRow {
	return s.checkpoint().NodeDegreeByKinds(kinds, path)
}

func (s *publishedPrefixStore) NodesInFilesByKind(paths []string, kinds []graph.NodeKind) []*graph.Node {
	return s.checkpoint().NodesInFilesByKind(paths, kinds)
}

func (s *publishedPrefixStore) FileSymbolNamesByPaths(paths []string, kinds []graph.NodeKind) []graph.FileSymbolNameRow {
	return s.checkpoint().FileSymbolNamesByPaths(paths, kinds)
}

func (s *publishedPrefixStore) IfaceImplementsRows() []graph.IfaceImplementsRow {
	return s.checkpoint().IfaceImplementsRows()
}

func (s *publishedPrefixStore) MemberMethodsByType() map[string][]graph.MemberMethodInfo {
	return s.checkpoint().MemberMethodsByType()
}

func (s *publishedPrefixStore) StructuralParentEdges() []graph.StructuralParentEdgeRow {
	return s.checkpoint().StructuralParentEdges()
}

func (s *publishedPrefixStore) FileImporters(path string) []graph.FileImporterRow {
	return s.checkpoint().FileImporters(path)
}

func (s *publishedPrefixStore) FileImportCounts(scope []string) []graph.FileImportCountRow {
	return s.checkpoint().FileImportCounts(scope)
}

func (s *publishedPrefixStore) ReachableForwardByKinds(ids []string, kinds []graph.EdgeKind) map[string]bool {
	return s.checkpoint().ReachableForwardByKinds(ids, kinds)
}

func (s *publishedPrefixStore) ClassHierarchyTraverse(seed, dir string, kinds []graph.EdgeKind, depth int) []graph.ClassHierarchyRow {
	return s.checkpoint().ClassHierarchyTraverse(seed, dir, kinds, depth)
}

func (s *publishedPrefixStore) CommunityCrossingsByKind(kinds []graph.EdgeKind, comm map[string]string) map[string]int {
	return s.checkpoint().CommunityCrossingsByKind(kinds, comm)
}

func (s *publishedPrefixStore) CrossRepoCandidates(kinds []graph.EdgeKind) []graph.CrossRepoCandidateRow {
	return s.checkpoint().CrossRepoCandidates(kinds)
}

func (s *publishedPrefixStore) CrossRepoEdgeCounts() []graph.CrossRepoEdgeRow {
	return s.checkpoint().CrossRepoEdgeCounts()
}

func (s *publishedPrefixStore) BFS(ids []string, dir graph.Direction, kinds []graph.EdgeKind, depth, limit int) ([]graph.BFSHop, error) {
	return s.checkpoint().BFS(ids, dir, kinds, depth, limit)
}

func (s *publishedPrefixStore) DeadCodeCandidates(kinds []graph.NodeKind, allowed map[graph.NodeKind][]graph.EdgeKind) []*graph.Node {
	return s.checkpoint().DeadCodeCandidates(kinds, allowed)
}

func (s *publishedPrefixStore) ExtractCandidates(kinds []graph.EdgeKind, minLines, minCallers, minFanOut int, path string) []graph.ExtractCandidateRow {
	return s.checkpoint().ExtractCandidates(kinds, minLines, minCallers, minFanOut, path)
}

func (s *publishedPrefixStore) FileEditingContext(path string, kinds []graph.NodeKind) *graph.FileEditingContextResult {
	return s.checkpoint().FileEditingContext(path, kinds)
}

func (s *publishedPrefixStore) ScopedOutlineCounts(scope graph.NodeScope) (graph.OutlineCounts, error) {
	return s.checkpoint().ScopedOutlineCounts(scope)
}

func (s *publishedPrefixStore) ScopedOutlineCountsFiltered(scope graph.NodeScope, filter graph.OutlineEdgeFilter) (graph.OutlineCounts, error) {
	return s.checkpoint().ScopedOutlineCountsFiltered(scope, filter)
}

func (s *publishedPrefixStore) NodesByKindInRepos(kind graph.NodeKind, repos []string) iter.Seq[*graph.Node] {
	return s.checkpoint().NodesByKindInRepos(kind, repos)
}

func (s *publishedPrefixStore) DeadCodeCandidatesInRepos(kinds []graph.NodeKind, incoming map[graph.NodeKind][]graph.EdgeKind, repos []string) []*graph.Node {
	return s.checkpoint().DeadCodeCandidatesInRepos(kinds, incoming, repos)
}

func (s *publishedPrefixStore) DeadCodeCandidatesInReposFiltered(kinds []graph.NodeKind, incoming map[graph.NodeKind][]graph.EdgeKind, repos []string, filter graph.OutlineEdgeFilter) ([]*graph.Node, error) {
	return s.checkpoint().DeadCodeCandidatesInReposFiltered(kinds, incoming, repos, filter)
}

func (s *publishedPrefixStore) IfaceImplementsRowsInRepos(repos []string) []graph.IfaceImplementsRow {
	return s.checkpoint().IfaceImplementsRowsInRepos(repos)
}

func (s *publishedPrefixStore) IfaceImplementsRowsInReposFiltered(repos []string, filter graph.OutlineEdgeFilter) ([]graph.IfaceImplementsRow, error) {
	return s.checkpoint().IfaceImplementsRowsInReposFiltered(repos, filter)
}

func (s *publishedPrefixStore) ThrowerErrorSurface(pathPrefix string) []graph.ThrowerErrorRow {
	return s.checkpoint().ThrowerErrorSurface(pathPrefix)
}

func (s *publishedPrefixStore) AllNodesLight() []*graph.Node {
	return s.checkpoint().AllNodesLight()
}

func (s *publishedPrefixStore) ValueRefPlaceholderEdges() iter.Seq[*graph.Edge] {
	return s.checkpoint().ValueRefPlaceholderEdges()
}

func (s *publishedPrefixStore) FnValuePlaceholderEdges() iter.Seq[*graph.Edge] {
	return s.checkpoint().FnValuePlaceholderEdges()
}

func (s *publishedPrefixStore) GetNode(id string) *graph.Node { return s.checkpoint().GetNode(id) }

func (s *publishedPrefixStore) GetNodeByQualName(qualName string) *graph.Node {
	return s.checkpoint().GetNodeByQualName(qualName)
}

func (s *publishedPrefixStore) GetNodesByQualNames(qualNames []string) map[string][]*graph.Node {
	return s.checkpoint().GetNodesByQualNames(qualNames)
}

func (s *publishedPrefixStore) FindNodesByName(name string) []*graph.Node {
	return s.checkpoint().FindNodesByName(name)
}

func (s *publishedPrefixStore) FindNodesByNameInRepo(name, repoPrefix string) []*graph.Node {
	return s.checkpoint().FindNodesByNameInRepo(name, repoPrefix)
}

func (s *publishedPrefixStore) FindNodesByNameContaining(substr string, limit int) []*graph.Node {
	return s.checkpoint().FindNodesByNameContaining(substr, limit)
}

func (s *publishedPrefixStore) GetFileNodes(filePath string) []*graph.Node {
	return s.checkpoint().GetFileNodes(filePath)
}

func (s *publishedPrefixStore) GetFileNodesByPaths(filePaths []string) map[string][]*graph.Node {
	return s.checkpoint().GetFileNodesByPaths(filePaths)
}

func (s *publishedPrefixStore) GetRepoNodes(repoPrefix string) []*graph.Node {
	return s.checkpoint().GetRepoNodes(repoPrefix)
}

func (s *publishedPrefixStore) GetRepoNodesByLanguage(repoPrefix, language string) []*graph.Node {
	return s.checkpoint().GetRepoNodesByLanguage(repoPrefix, language)
}

func (s *publishedPrefixStore) GetNodesByLanguage(language string) []*graph.Node {
	return s.checkpoint().GetNodesByLanguage(language)
}

func (s *publishedPrefixStore) GetRepoNonContentNodes(repoPrefix string) []*graph.Node {
	return s.checkpoint().GetRepoNonContentNodes(repoPrefix)
}

func (s *publishedPrefixStore) GetOutEdges(nodeID string) []*graph.Edge {
	return s.checkpoint().GetOutEdges(nodeID)
}

func (s *publishedPrefixStore) GetInEdges(nodeID string) []*graph.Edge {
	return s.checkpoint().GetInEdges(nodeID)
}

func (s *publishedPrefixStore) GetInEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	return s.checkpoint().GetInEdgesByNodeIDs(ids)
}

func (s *publishedPrefixStore) GetOutEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	return s.checkpoint().GetOutEdgesByNodeIDs(ids)
}

func (s *publishedPrefixStore) GetEdgeCandidates(endpoints []graph.EdgeEndpoint, sites []graph.EdgeSite) graph.EdgeCandidateSet {
	return s.checkpoint().GetEdgeCandidates(endpoints, sites)
}

func (s *publishedPrefixStore) DistinctExternalTargets(kinds []graph.EdgeKind, fileSuffix string) []string {
	return s.checkpoint().DistinctExternalTargets(kinds, fileSuffix)
}

func (s *publishedPrefixStore) GetRepoEdges(repoPrefix string) []*graph.Edge {
	return s.checkpoint().GetRepoEdges(repoPrefix)
}

func (s *publishedPrefixStore) AllNodes() []*graph.Node { return s.checkpoint().AllNodes() }

func (s *publishedPrefixStore) AllEdges() []*graph.Edge { return s.checkpoint().AllEdges() }

func (s *publishedPrefixStore) EdgesByKind(kind graph.EdgeKind) iter.Seq[*graph.Edge] {
	return s.checkpoint().EdgesByKind(kind)
}

func (s *publishedPrefixStore) NodesByKind(kind graph.NodeKind) iter.Seq[*graph.Node] {
	return s.checkpoint().NodesByKind(kind)
}

func (s *publishedPrefixStore) EdgesWithUnresolvedTarget() iter.Seq[*graph.Edge] {
	return s.checkpoint().EdgesWithUnresolvedTarget()
}

func (s *publishedPrefixStore) GetNodesByIDs(ids []string) map[string]*graph.Node {
	return s.checkpoint().GetNodesByIDs(ids)
}

func (s *publishedPrefixStore) FindNodesByNames(names []string) map[string][]*graph.Node {
	return s.checkpoint().FindNodesByNames(names)
}

func (s *publishedPrefixStore) NodeCount() int { return s.checkpoint().NodeCount() }

func (s *publishedPrefixStore) EdgeCount() int { return s.checkpoint().EdgeCount() }

func (s *publishedPrefixStore) ProxyNodeCountAtLeast(limit int) bool {
	return s.checkpoint().ProxyNodeCountAtLeast(limit)
}

func (s *publishedPrefixStore) Stats() graph.GraphStats { return s.checkpoint().Stats() }

func (s *publishedPrefixStore) RepoStats() map[string]graph.GraphStats {
	return s.checkpoint().RepoStats()
}

func (s *publishedPrefixStore) RepoPrefixes() []string { return s.checkpoint().RepoPrefixes() }

func (s *publishedPrefixStore) EdgeIdentityRevisions() int {
	return s.checkpoint().EdgeIdentityRevisions()
}

func (s *publishedPrefixStore) VerifyEdgeIdentities() error {
	return s.checkpoint().VerifyEdgeIdentities()
}

func (s *publishedPrefixStore) RepoMemoryEstimate(repoPrefix string) graph.RepoMemoryEstimate {
	return s.checkpoint().RepoMemoryEstimate(repoPrefix)
}

func (s *publishedPrefixStore) AllRepoMemoryEstimates() map[string]graph.RepoMemoryEstimate {
	return s.checkpoint().AllRepoMemoryEstimates()
}

func conformanceSnapshotReads(t *testing.T) {
	b := conformanceBuild(t)
	b.AddNode(&graph.Node{ID: "seed", Name: "Seed", Kind: graph.KindFunction})
	old := publishCheckpoint(t, b)
	check := func() {
		n := old.GetNode("seed")
		if n == nil || n.Name != "Seed" || len(old.GetOutEdges("seed")) != 0 || old.NodeCount() != 1 || old.ReadError() != nil {
			t.Error("prior snapshot changed during build")
		}
		for n := range old.NodesByKind(graph.KindFunction) {
			if n.ID != "seed" {
				t.Errorf("old node = %q, want seed", n.ID)
			}
		}
	}
	var readers sync.WaitGroup
	reading := make(chan struct{}, 8)
	published := make(chan struct{})
	defer func() { close(published); readers.Wait() }()
	for range 8 {
		readers.Go(func() {
			check()
			reading <- struct{}{}
			for range 20 {
				check()
			}
			<-published
			check()
		})
	}
	for range 8 {
		<-reading
	}
	b.AddNode(&graph.Node{ID: "next", Name: "Next", Kind: graph.KindFunction})
	b.AddEdge(&graph.Edge{From: "seed", To: "next", Kind: graph.EdgeCalls})
	next := publishCheckpoint(t, b)
	if next.GetNode("next") == nil || next.NodeCount() != 2 || len(next.GetOutEdges("seed")) != 1 || next.ReadError() != nil {
		t.Fatal("fresh snapshot did not see the complete next publication")
	}
}
