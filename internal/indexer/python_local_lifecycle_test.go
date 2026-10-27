package indexer

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/search"
)

func TestPythonLocalRenameAndRestore(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, repos := range []string{"solo", "duo"} {
			t.Run(backend+"/"+repos, func(t *testing.T) {
				dir := t.TempDir()
				original := "def foo(): pass\nclass K:\n    def m(self): pass\n"
				writeWorkspaceFile(t, dir, "a.py", original)
				writeWorkspaceFile(t, dir, "b.py", "from a import foo, K\nimport a\ndef run():\n    foo()\n    a.foo()\n    k = K()\n    k.m()\n")
				entries := []config.RepoEntry{{Path: dir, Name: "solo", Workspace: "shared"}}
				if repos == "duo" {
					other := t.TempDir()
					writeWorkspaceFile(t, other, "z.py", "def foo(): pass\n")
					entries = append(entries, config.RepoEntry{Path: other, Name: "other", Workspace: "shared"})
				}
				cfgPath := filepath.Join(t.TempDir(), "config.yaml")
				gc := &config.GlobalConfig{Repos: entries}
				gc.SetConfigPath(cfgPath)
				require.NoError(t, gc.Save())
				cm, err := config.NewConfigManager(cfgPath)
				require.NoError(t, err)
				g := pythonLocalStore(t, backend)
				reg := newTestRegistry()
				reg.Register(languages.NewPythonExtractor())
				mi := NewMultiIndexer(g, reg, search.NewNull(), cm, zap.NewNop())
				t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
				_, err = mi.IndexAll()
				require.NoError(t, err)
				before := pythonLocalBindings(g)
				writeWorkspaceFile(t, dir, "a.py", "def foo2(): pass\nclass K:\n    def m2(self): pass\n")
				_, err = mi.IncrementalReindexRepo("solo", []string{filepath.Join(dir, "a.py")})
				require.NoError(t, err)
				writeWorkspaceFile(t, dir, "a.py", original)
				_, err = mi.IncrementalReindexRepo("solo", []string{filepath.Join(dir, "a.py")})
				require.NoError(t, err)
				require.Equal(t, before, pythonLocalBindings(g))
				cold := pythonLocalStore(t, backend)
				coldIndexer := NewMultiIndexer(cold, reg, search.NewNull(), cm, zap.NewNop())
				t.Cleanup(func() { require.NoError(t, coldIndexer.Close(context.Background())) })
				_, err = coldIndexer.IndexAll()
				require.NoError(t, err)
				require.Equal(t, pythonLocalBindings(cold), pythonLocalBindings(g))
			})
		}
	}
}

func pythonLocalStore(t *testing.T, backend string) graph.Store {
	t.Helper()
	if backend == "memory" {
		return graph.New()
	}
	disk, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, disk.Close()) })
	return disk
}

func pythonLocalBindings(g graph.Store) []string {
	var bindings []string
	for _, node := range g.GetFileNodes("solo/b.py") {
		for _, edge := range g.GetOutEdges(node.ID) {
			if edge.Kind == graph.EdgeImports || edge.Kind == graph.EdgeCalls {
				bindings = append(bindings, fmt.Sprintf("%s -%s-> %s cross=%v line=%d stub=%v", edge.From, edge.Kind, edge.To, edge.CrossRepo, edge.Line, edge.Meta["python_call_stub"]))
			}
		}
	}
	slices.Sort(bindings)
	return bindings
}
