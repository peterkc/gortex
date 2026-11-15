package contracts_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestBindSpringConfigGraphSQLiteParity(t *testing.T) {
	factories := map[string]func(*testing.T) graph.Store{
		"graph": func(*testing.T) graph.Store { return graph.New() },
		"sqlite": func(t *testing.T) graph.Store {
			store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			return store
		},
	}
	for name, factory := range factories {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			const rel = "src/main/resources/application.yml"
			writeScopedConfigFile(t, root, rel, "app:\n  value: secret\n")
			scope := contracts.SpringConfigScope{RepoPrefix: "repo", RepoRoot: root, WorkspaceID: "ws"}
			store := factory(t)
			store.AddBatch([]*graph.Node{
				{ID: "repo/" + rel, Kind: graph.KindFile, FilePath: "repo/" + rel, RepoPrefix: "repo", WorkspaceID: "ws"},
				{ID: "repo::Reader", Kind: graph.KindType, FilePath: "repo/Reader.java", StartLine: 7, RepoPrefix: "repo", WorkspaceID: "ws", Meta: map[string]any{"spring_config_keys": []string{"app.value", "app.value"}}},
				{ID: "cfg::spring::app.value", Kind: graph.KindConfigKey, FilePath: "repo/" + rel, Meta: map[string]any{"source": "spring"}},
			}, []*graph.Edge{{From: "repo::Reader", To: "cfg::spring::app.value", Kind: graph.EdgeReadsConfig, FilePath: "repo/Reader.java", Line: 7}})

			// Given supported cleanup and duplicate hints, when legacy rows migrate
			// and binding repeats, then only the scoped rows remain, without duplicates.
			if got, err := contracts.BindSpringConfig(store, scope); err != nil || got != 2 {
				t.Fatalf("cold BindSpringConfig = %d, want 2 (err=%v)", got, err)
			}
			if store.GetNode("cfg::spring::app.value") != nil || hasScopedReadsConfigEdge(store, "repo::Reader", "cfg::spring::app.value") {
				t.Fatal("legacy rows survived supported migration")
			}
			const desired = "cfg::spring::ws::repo::app.value"
			node := store.GetNode(desired)
			if node == nil || node.RepoPrefix != "repo" || node.WorkspaceID != "ws" || node.FilePath != "repo/"+rel {
				t.Fatalf("config node = %#v", node)
			}
			if !hasScopedReadsConfigEdge(store, "repo::Reader", desired) {
				t.Fatalf("reads_config edge to %q missing: %#v", desired, store.GetOutEdges("repo::Reader"))
			}
			if got, err := contracts.BindSpringConfig(store, scope); err != nil || got != 0 {
				t.Fatalf("warm BindSpringConfig = %d, want 0 (err=%v)", got, err)
			}
		})
	}
}

func writeScopedConfigFile(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func hasScopedReadsConfigEdge(store graph.Store, from, to string) bool {
	for _, edge := range store.GetOutEdges(from) {
		if edge != nil && edge.Kind == graph.EdgeReadsConfig && edge.To == to {
			return true
		}
	}
	return false
}
