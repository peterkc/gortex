package indexer

import (
	"context"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/search"
)

// pythonFallbackRefreshTransform changes extra.py's bytes on exactly its second
// application: the probe read sees call 1, takePreparedRefresh sees call 2
// (mismatch, so the stage falls back), and the fallback indexFile sees call 3.
type pythonFallbackRefreshTransform struct{ calls *atomic.Int32 }

func (pythonFallbackRefreshTransform) name() string          { return "python-fallback-refresh" }
func (pythonFallbackRefreshTransform) matches(p string) bool { return strings.HasSuffix(p, "extra.py") }
func (pythonFallbackRefreshTransform) asLanguage() string    { return "" }
func (f pythonFallbackRefreshTransform) apply(p string, src []byte) ([]byte, error) {
	if f.calls.Add(1) == 2 {
		return append(append([]byte(nil), src...), []byte("\n# concurrent write\n")...), nil
	}
	return src, nil
}

func TestTrackedPythonNewModuleThroughFallback(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			provider, consumer := t.TempDir(), t.TempDir()
			writeWorkspaceFile(t, provider, "pyproject.toml", "[project]\nname = 'pillow'\n")
			writeWorkspaceFile(t, provider, "src/PIL/__init__.py", "")
			writeWorkspaceFile(t, provider, "src/PIL/image.py", pythonLifecycleProvider)
			writeWorkspaceFile(t, provider, "src/PIL/util.py", "def helper(): pass\n")
			writeWorkspaceFile(t, consumer, "pyproject.toml", "[project]\ndependencies = ['pillow']\n")
			writeWorkspaceFile(t, consumer, "app.py", pythonLifecycleConsumer+"\nfrom PIL.extra import later\ndef after(): return later()\n")
			entries := []config.RepoEntry{{Path: consumer, Name: "consumer", Workspace: "shared"}, {Path: provider, Name: "provider", Workspace: "shared"}}
			cfgPath := filepath.Join(t.TempDir(), "config.yaml")
			gc := &config.GlobalConfig{Repos: entries}
			gc.SetConfigPath(cfgPath)
			require.NoError(t, gc.Save())
			cm, err := config.NewConfigManager(cfgPath)
			require.NoError(t, err)
			reg := newTestRegistry()
			reg.Register(languages.NewPythonExtractor())
			g := pythonLocalStore(t, backend)
			mi := NewMultiIndexer(g, reg, search.NewNull(), cm, zap.NewNop())
			t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
			_, err = mi.IndexAll()
			require.NoError(t, err)

			idx := mi.GetIndexer("provider")
			require.NotNil(t, idx)
			var calls atomic.Int32
			idx.transforms.transforms = append(idx.transforms.transforms, pythonFallbackRefreshTransform{calls: &calls})
			writeWorkspaceFile(t, provider, "src/PIL/extra.py", "def later(): pass\n")
			_, err = mi.IncrementalReindexRepo("provider", []string{filepath.Join(provider, "src/PIL/extra.py")})
			require.NoError(t, err)
			require.GreaterOrEqual(t, calls.Load(), int32(3), "fallback path must run")
			require.NotNil(t, g.GetNode("provider/src/PIL/extra.py"))

			cold := pythonLocalStore(t, backend)
			coldIndexer := NewMultiIndexer(cold, reg, search.NewNull(), cm, zap.NewNop())
			t.Cleanup(func() { require.NoError(t, coldIndexer.Close(context.Background())) })
			_, err = coldIndexer.IndexAll()
			require.NoError(t, err)
			require.Equal(t, pythonLifecycleBindings(cold, ""), pythonLifecycleBindings(g, ""))
		})
	}
}
