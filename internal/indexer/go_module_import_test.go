package indexer

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/search"
)

func TestMultiIndexer_GoModuleImport(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, mode := range []string{"track", "reverse", "all", "sub_track", "sub_reverse"} {
			t.Run(backend+"/"+mode, func(t *testing.T) {
				t.Setenv("HOME", t.TempDir())
				consumerRoot, provider := t.TempDir(), t.TempDir()
				consumer, consumerFile := consumerRoot, "main.go"
				if mode == "sub_track" || mode == "sub_reverse" {
					consumer = filepath.Join(consumerRoot, "svc")
					consumerFile = "svc/main.go"
					require.NoError(t, os.MkdirAll(consumer, 0o755))
				}
				writeFile(t, filepath.Join(consumer, "go.mod"), "module example.com/consumer\nrequire example.com/lib v1.0.0\n")
				writeFile(t, filepath.Join(consumer, "main.go"), `package main
import "example.com/lib"
func main() { lib.F() }
`)
				writeFile(t, filepath.Join(provider, "go.mod"), "module example.com/lib\n")
				writeFile(t, filepath.Join(provider, "lib.go"), "package lib\nfunc F() {}\n")
				var g graph.Store = graph.New()
				if backend == "sqlite" {
					s, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
					require.NoError(t, err)
					t.Cleanup(func() { require.NoError(t, s.Close()) })
					g = s
				}
				cm := newTestConfigManager(t)
				mi := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), cm, zap.NewNop())
				t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
				entries := []config.RepoEntry{
					{Path: consumerRoot, Name: "consumer", Workspace: "shared"},
					{Path: provider, Name: "provider", Workspace: "shared"},
				}
				if mode == "all" {
					cm.Global().Repos = entries
					_, err := mi.IndexAll()
					require.NoError(t, err)
				} else {
					if mode == "reverse" || mode == "sub_reverse" {
						entries[0], entries[1] = entries[1], entries[0]
					}
					for _, entry := range entries {
						_, err := mi.TrackRepoCtx(context.Background(), entry)
						require.NoError(t, err)
					}
				}
				mi.RunGlobalResolve()
				var imports []*graph.Edge
				for _, e := range g.GetOutEdges("consumer/" + consumerFile) {
					if e.Kind == graph.EdgeImports {
						imports = append(imports, e)
					}
				}
				require.Len(t, imports, 1)
				t.Logf("import target after track + global resolve: %s", imports[0].To)
				assert.Equal(t, "provider/lib.go", imports[0].To)
				assert.True(t, imports[0].CrossRepo)
				contractID := "dep::example.com/lib"
				if mode == "all" {
					contractID = "dep::provider::lib"
				}
				if consumerFile == "main.go" {
					require.NotNil(t, g.GetNode(contractID), "keep the go.mod dependency contract")
				}
				var calls []string
				for _, e := range g.GetOutEdges("consumer/" + consumerFile + "::main") {
					if e.Kind == graph.EdgeCalls {
						calls = append(calls, e.To)
					}
				}
				assert.Equal(t, []string{"provider/lib.go::F"}, calls)
				for _, mode := range []string{"file", "repo"} {
					writeFile(t, filepath.Join(consumer, "main.go"), "// edited\npackage main\nimport \"example.com/lib\"\nfunc main() { lib.F() }\n")
					switch mode {
					case "file":
						require.NoError(t, mi.GetIndexer("consumer").IndexFile(filepath.Join(consumer, "main.go")))
					case "repo":
						_, err := mi.IndexRepo("consumer")
						require.NoError(t, err)
					}
					var targets []string
					for _, e := range g.GetOutEdges("consumer/" + consumerFile) {
						if e.Kind == graph.EdgeImports {
							targets = append(targets, e.To)
							assert.True(t, e.CrossRepo)
						}
					}
					assert.Equal(t, []string{"provider/lib.go"}, targets, mode)
					mi.RunGlobalResolve()
					var afterGlobal []string
					for _, e := range g.GetOutEdges("consumer/" + consumerFile) {
						if e.Kind == graph.EdgeImports {
							afterGlobal = append(afterGlobal, e.To)
						}
					}
					assert.Equal(t, []string{"provider/lib.go"}, afterGlobal, mode+" global")
				}
				writeFile(t, filepath.Join(provider, "lib.go"), "package lib\nfunc F() { /* edited */ }\n")
				require.NoError(t, mi.GetIndexer("provider").IndexFile(filepath.Join(provider, "lib.go")))
				for _, phase := range []string{"provider_file", "provider_repo"} {
					if phase == "provider_repo" {
						_, err := mi.IndexRepo("provider")
						require.NoError(t, err)
					}
					var targets []string
					for _, e := range g.GetOutEdges("consumer/" + consumerFile) {
						if e.Kind == graph.EdgeImports {
							targets = append(targets, e.To)
						}
					}
					assert.Equal(t, []string{"provider/lib.go"}, targets, phase)
				}
			})
		}
	}
}
