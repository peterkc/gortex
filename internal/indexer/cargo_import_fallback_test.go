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

// cargoFallbackRefreshTransform changes extra.rs's bytes on exactly its second
// application: the probe read sees call 1, takePreparedRefresh sees call 2
// (mismatch, so the stage falls back), and the fallback indexFile sees call 3.
type cargoFallbackRefreshTransform struct{ calls *atomic.Int32 }

func (cargoFallbackRefreshTransform) name() string          { return "cargo-fallback-refresh" }
func (cargoFallbackRefreshTransform) matches(p string) bool { return strings.HasSuffix(p, "extra.rs") }
func (cargoFallbackRefreshTransform) asLanguage() string    { return "" }
func (f cargoFallbackRefreshTransform) apply(p string, src []byte) ([]byte, error) {
	if f.calls.Add(1) == 2 {
		return append(append([]byte(nil), src...), []byte("\n// concurrent write\n")...), nil
	}
	return src, nil
}

func TestTrackedCargoNewModuleThroughFallback(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			provider, consumer := t.TempDir(), t.TempDir()
			writeWorkspaceFile(t, provider, "Cargo.toml", "[package]\nname = 'other-crate'\nversion = '0.1.0'\n")
			writeWorkspaceFile(t, provider, "src/lib.rs", cargoLifecycleProvider)
			writeWorkspaceFile(t, provider, "src/util.rs", "pub fn helper() {}\n")
			writeWorkspaceFile(t, consumer, "Cargo.toml", "[package]\nname = 'app'\nversion = '0.1.0'\n[dependencies]\nother-crate = '1'\n")
			writeWorkspaceFile(t, consumer, "src/lib.rs", cargoLifecycleConsumer+"\nuse other_crate::extra::later;\npub fn after() { later(); }\n")
			entries := []config.RepoEntry{{Path: consumer, Name: "consumer", Workspace: "shared"}, {Path: provider, Name: "provider", Workspace: "shared"}}
			cfgPath := filepath.Join(t.TempDir(), "config.yaml")
			gc := &config.GlobalConfig{Repos: entries}
			gc.SetConfigPath(cfgPath)
			require.NoError(t, gc.Save())
			cm, err := config.NewConfigManager(cfgPath)
			require.NoError(t, err)
			reg := newTestRegistry()
			reg.Register(languages.NewRustExtractor())
			g := cargoWarmStore(t, backend)
			mi := NewMultiIndexer(g, reg, search.NewNull(), cm, zap.NewNop())
			t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
			_, err = mi.IndexAll()
			require.NoError(t, err)

			idx := mi.GetIndexer("provider")
			require.NotNil(t, idx)
			var calls atomic.Int32
			idx.transforms.transforms = append(idx.transforms.transforms, cargoFallbackRefreshTransform{calls: &calls})
			writeWorkspaceFile(t, provider, "src/extra.rs", "pub fn later() {}\n")
			_, err = mi.IncrementalReindexRepo("provider", []string{filepath.Join(provider, "src/extra.rs")})
			require.NoError(t, err)
			require.GreaterOrEqual(t, calls.Load(), int32(3), "fallback path must run")
			require.NotNil(t, g.GetNode("provider/src/extra.rs"))

			cold := cargoWarmStore(t, backend)
			coldIndexer := NewMultiIndexer(cold, reg, search.NewNull(), cm, zap.NewNop())
			t.Cleanup(func() { require.NoError(t, coldIndexer.Close(context.Background())) })
			_, err = coldIndexer.IndexAll()
			require.NoError(t, err)
			require.Equal(t, cargoLifecycleBindings(cold, "fallback"), cargoLifecycleBindings(g, "fallback"))

		})
	}
}
