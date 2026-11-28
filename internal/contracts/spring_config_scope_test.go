package contracts

import (
	"errors"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestBindSpringConfigIsolatesRepositoriesRootsAndIDs(t *testing.T) {
	const rel = "src/main/resources/application.yml"
	rootA, rootB := t.TempDir(), t.TempDir()
	writeSpringConfigFile(t, rootA, rel, "shared:\n  value: a\nalpha:\n  only: yes\n")
	writeSpringConfigFile(t, rootB, rel, "shared:\n  value: b\nbeta:\n  only: yes\n")

	g := graph.New()
	pathA, pathB := "repo-a/"+rel, "repo-b/"+rel
	readerA, readerB := "repo-a::Reader", "repo-b::Reader"
	g.AddBatch([]*graph.Node{
		{ID: pathA, Kind: graph.KindFile, FilePath: pathA, RepoPrefix: "repo-a", WorkspaceID: "ws"},
		{ID: pathB, Kind: graph.KindFile, FilePath: pathB, RepoPrefix: "repo-b", WorkspaceID: "ws"},
		{ID: readerA, Kind: graph.KindType, FilePath: "repo-a/Reader.java", StartLine: 3, RepoPrefix: "repo-a", WorkspaceID: "ws", Meta: map[string]any{"spring_config_keys": []string{"shared.value"}}},
		{ID: readerB, Kind: graph.KindType, FilePath: "repo-b/Reader.java", StartLine: 4, RepoPrefix: "repo-b", WorkspaceID: "ws", Meta: map[string]any{"spring_config_keys": []string{"shared.value"}}},
	}, nil)

	scopeA := SpringConfigScope{RepoPrefix: "repo-a", RepoRoot: rootA, WorkspaceID: "ws"}
	scopeB := SpringConfigScope{RepoPrefix: "repo-b", RepoRoot: rootB, WorkspaceID: "ws"}
	if got, err := BindSpringConfig(g, scopeA); err != nil || got == 0 {
		t.Fatalf("repo-a binding added nothing (err=%v)", err)
	}
	if got, err := BindSpringConfig(g, scopeB); err != nil || got == 0 {
		t.Fatalf("repo-b binding added nothing (err=%v)", err)
	}

	sharedA := scopeA.KeyID("shared.value")
	sharedB := scopeB.KeyID("shared.value")
	if sharedA == sharedB {
		t.Fatalf("multi-repo config IDs collided: %q", sharedA)
	}
	for id, repo := range map[string]string{sharedA: "repo-a", sharedB: "repo-b"} {
		node := g.GetNode(id)
		if node == nil || node.RepoPrefix != repo || node.WorkspaceID != "ws" {
			t.Fatalf("node %q = %#v, want repo=%q workspace=ws", id, node, repo)
		}
	}
	if g.GetNode(scopeA.KeyID("alpha.only")) == nil {
		t.Fatal("repo-a root was not used to read alpha.only")
	}
	if g.GetNode(scopeA.KeyID("beta.only")) != nil {
		t.Fatal("repo-a pass read repo-b root")
	}
	if g.GetNode(scopeB.KeyID("beta.only")) == nil {
		t.Fatal("repo-b root was not used to read beta.only")
	}
	if !hasReadsConfigEdge(g, readerA, sharedA) || hasReadsConfigEdge(g, readerA, sharedB) {
		t.Fatalf("repo-a reader crossed repository boundary: %v", g.GetOutEdges(readerA))
	}
	if !hasReadsConfigEdge(g, readerB, sharedB) || hasReadsConfigEdge(g, readerB, sharedA) {
		t.Fatalf("repo-b reader crossed repository boundary: %v", g.GetOutEdges(readerB))
	}
	if got, err := BindSpringConfig(g, scopeA); err != nil || got != 0 {
		t.Fatalf("warm repo-a replay added %d rows, want 0 (err=%v)", got, err)
	}
	if got, err := BindSpringConfig(g, scopeB); err != nil || got != 0 {
		t.Fatalf("warm repo-b replay added %d rows, want 0 (err=%v)", got, err)
	}
}

func TestBindSpringConfigMigratesScopedLegacyID(t *testing.T) {
	const rel = "src/main/resources/application.yml"
	root := t.TempDir()
	writeSpringConfigFile(t, root, rel, "app:\n  value: hidden\n")
	scope := SpringConfigScope{RepoPrefix: "repo-a", RepoRoot: root, WorkspaceID: "ws"}
	filePath := "repo-a/" + rel
	readerID := "repo-a::Reader"
	legacyID := springConfigKeyID("app.value")

	g := graph.New()
	g.AddBatch([]*graph.Node{
		{ID: filePath, Kind: graph.KindFile, FilePath: filePath, RepoPrefix: "repo-a", WorkspaceID: "ws"},
		{ID: readerID, Kind: graph.KindType, FilePath: "repo-a/Reader.java", StartLine: 8, RepoPrefix: "repo-a", WorkspaceID: "ws", Meta: map[string]any{"spring_config_keys": []string{"app.value"}}},
		{ID: legacyID, Kind: graph.KindConfigKey, Name: "app.value", FilePath: filePath, Meta: map[string]any{"source": "spring", "value_redacted": true}},
	}, []*graph.Edge{{From: readerID, To: legacyID, Kind: graph.EdgeReadsConfig, FilePath: "repo-a/Reader.java", Line: 8}})

	if got, err := BindSpringConfig(g, scope); err != nil || got == 0 {
		t.Fatalf("migration added no qualified rows (err=%v)", err)
	}
	desiredID := scope.KeyID("app.value")
	if g.GetNode(legacyID) != nil {
		t.Fatalf("legacy unqualified node %q survived migration", legacyID)
	}
	if g.GetNode(desiredID) == nil || !hasReadsConfigEdge(g, readerID, desiredID) {
		t.Fatalf("qualified replacement missing: node=%#v edges=%v", g.GetNode(desiredID), g.GetOutEdges(readerID))
	}
	if hasReadsConfigEdge(g, readerID, legacyID) {
		t.Fatal("legacy reads_config edge survived migration")
	}
}

func TestBindSpringConfigUsesConstantBatchOperations(t *testing.T) {
	const count = 160
	const rel = "src/main/resources/application.yml"
	root := t.TempDir()
	var src strings.Builder
	base := graph.New()
	nodes := []*graph.Node{{ID: "repo/" + rel, Kind: graph.KindFile, FilePath: "repo/" + rel, RepoPrefix: "repo", WorkspaceID: "ws"}}
	for i := 0; i < count; i++ {
		key := fmt.Sprintf("key%d", i)
		fmt.Fprintf(&src, "%s: value\n", key)
		nodes = append(nodes, &graph.Node{
			ID: fmt.Sprintf("repo::Reader%d", i), Kind: graph.KindField,
			FilePath: "repo/Reader.java", StartLine: i + 1,
			RepoPrefix: "repo", WorkspaceID: "ws",
			Meta: map[string]any{"spring_config_keys": []string{key}},
		})
	}
	writeSpringConfigFile(t, root, rel, src.String())
	base.AddBatch(nodes, nil)
	counting := &springCountingStore{Store: base}
	scope := SpringConfigScope{RepoPrefix: "repo", RepoRoot: root, WorkspaceID: "ws"}

	if got, err := BindSpringConfig(counting, scope); err != nil || got != count*2 {
		t.Fatalf("cold binding added %d rows, want %d (err=%v)", got, count*2, err)
	}
	if counting.fileProjections != 1 || counting.readerProjections != 1 || counting.nodeBatches != 1 ||
		counting.edgeCandidateBatches != 1 || counting.outEdgeBatches != 1 || counting.addBatches != 1 {
		t.Fatalf("cold operations = %+v, want one of each batched operation", counting)
	}
	if counting.globalScans != 0 || counting.pointReads != 0 || counting.pointWrites != 0 {
		t.Fatalf("cold binding used global/point operations: %+v", counting)
	}
	if got, err := BindSpringConfig(counting, scope); err != nil || got != 0 {
		t.Fatalf("warm binding added %d rows, want 0 (err=%v)", got, err)
	}
	if counting.fileProjections != 2 || counting.readerProjections != 2 || counting.nodeBatches != 2 ||
		counting.edgeCandidateBatches != 2 || counting.outEdgeBatches != 2 || counting.addBatches != 1 {
		t.Fatalf("warm operations = %+v, want constant reads and no second write", counting)
	}
}

func writeSpringConfigFile(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func hasReadsConfigEdge(g graph.Store, from, to string) bool {
	for _, edge := range g.GetOutEdges(from) {
		if edge != nil && edge.Kind == graph.EdgeReadsConfig && edge.To == to {
			return true
		}
	}
	return false
}

type springCountingStore struct {
	graph.Store
	fileProjections      int
	readerProjections    int
	nodeBatches          int
	edgeCandidateBatches int
	outEdgeBatches       int
	addBatches           int
	globalScans          int
	pointReads           int
	pointWrites          int
}

func (s *springCountingStore) RepoFilePaths(repoPrefix, workspaceID string, languages, extensions []string) []string {
	s.fileProjections++
	return s.Store.(graph.RepoFilePathReader).RepoFilePaths(repoPrefix, workspaceID, languages, extensions)
}

func (s *springCountingStore) RepoNodesByKindsWithMetaKey(repoPrefix, workspaceID string, kinds []graph.NodeKind, metaKey string) []*graph.Node {
	s.readerProjections++
	return s.Store.(graph.RepoMetaNodeReader).RepoNodesByKindsWithMetaKey(repoPrefix, workspaceID, kinds, metaKey)
}

func (s *springCountingStore) GetNodesByIDs(ids []string) map[string]*graph.Node {
	s.nodeBatches++
	return s.Store.GetNodesByIDs(ids)
}

func (s *springCountingStore) GetEdgeCandidates(endpoints []graph.EdgeEndpoint, sites []graph.EdgeSite) graph.EdgeCandidateSet {
	s.edgeCandidateBatches++
	return s.Store.GetEdgeCandidates(endpoints, sites)
}

func (s *springCountingStore) GetOutEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	s.outEdgeBatches++
	return s.Store.GetOutEdgesByNodeIDs(ids)
}

func (s *springCountingStore) AddBatch(nodes []*graph.Node, edges []*graph.Edge) {
	s.addBatches++
	s.Store.AddBatch(nodes, edges)
}

func (s *springCountingStore) GetNode(id string) *graph.Node {
	s.pointReads++
	return s.Store.GetNode(id)
}

func (s *springCountingStore) AddNode(node *graph.Node) {
	s.pointWrites++
	s.Store.AddNode(node)
}

func (s *springCountingStore) AddEdge(edge *graph.Edge) {
	s.pointWrites++
	s.Store.AddEdge(edge)
}

func (s *springCountingStore) NodesByKind(kind graph.NodeKind) iter.Seq[*graph.Node] {
	s.globalScans++
	return s.Store.NodesByKind(kind)
}

type springExactRemovalOnlyStore struct {
	graph.Store
	remover  graph.ExactEdgeBatchRemover
	removals int
}

func (s *springExactRemovalOnlyStore) RemoveEdgesExact(edges []*graph.Edge) int {
	s.removals++
	return s.remover.RemoveEdgesExact(edges)
}

type springConfigEvictionOnlyStore struct {
	graph.Store
	evicter   graph.ConfigNodeBatchEvicter
	evictions int
}

func (s *springConfigEvictionOnlyStore) EvictConfigNodesByIDs(ids []string) (int, int) {
	s.evictions++
	return s.evicter.EvictConfigNodesByIDs(ids)
}

func TestBindSpringConfigRefusesIncompleteLegacyMigration(t *testing.T) {
	for _, missing := range []string{"config node eviction", "exact edge removal"} {
		t.Run(missing, func(t *testing.T) {
			// Given legacy rows and one missing cleanup capability, when binding
			// runs, then it refuses before either cleanup or replacement mutation.
			root := t.TempDir()
			writeSpringConfigFile(t, root, "application.yml", "app.value: hidden\n")
			scope := SpringConfigScope{RepoPrefix: "repo", RepoRoot: root, WorkspaceID: "ws"}
			base := graph.New()
			legacyID := springConfigKeyID("app.value")
			base.AddBatch([]*graph.Node{
				{ID: "repo/application.yml", Kind: graph.KindFile, FilePath: "repo/application.yml", RepoPrefix: "repo", WorkspaceID: "ws"},
				{ID: "repo::Reader", Kind: graph.KindType, FilePath: "repo/Reader.java", RepoPrefix: "repo", WorkspaceID: "ws", Meta: map[string]any{"spring_config_keys": []string{"app.value"}}},
				{ID: legacyID, Kind: graph.KindConfigKey, FilePath: "repo/application.yml", Meta: map[string]any{"source": "spring"}},
			}, []*graph.Edge{{From: "repo::Reader", To: legacyID, Kind: graph.EdgeReadsConfig, FilePath: "repo/Reader.java"}})
			counting := &springCountingStore{Store: base}
			exact := &springExactRemovalOnlyStore{Store: counting, remover: base}
			evicter := &springConfigEvictionOnlyStore{Store: counting, evicter: base}
			var store graph.Store
			if missing == "config node eviction" {
				store = exact
			} else {
				store = evicter
			}

			got, err := BindSpringConfig(store, scope)
			if !errors.Is(err, errors.ErrUnsupported) || got != 0 {
				t.Errorf("binding = %d, %v, want zero and ErrUnsupported", got, err)
			}
			if counting.addBatches != 0 || exact.removals != 0 || evicter.evictions != 0 {
				t.Fatalf("partial migration: adds=%d removals=%d evictions=%d", counting.addBatches, exact.removals, evicter.evictions)
			}
			if base.GetNode(legacyID) == nil || !hasReadsConfigEdge(base, "repo::Reader", legacyID) ||
				base.GetNode(scope.KeyID("app.value")) != nil {
				t.Fatal("refused migration changed the legacy graph")
			}
		})
	}
}
