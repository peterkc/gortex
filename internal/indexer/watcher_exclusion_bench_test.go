package indexer

import (
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/excludes"
)

var watcherExclusionResult bool

// BenchmarkWatcherExclusion measures warm-cache event checks. Fixture creation
// and each path's first per-folder ignore-file read are outside the timed loop.
func BenchmarkWatcherExclusion(b *testing.B) {
	for _, negation := range []bool{false, true} {
		name := "without_negation"
		if negation {
			name = "with_gitkeep"
		}
		b.Run(name, func(b *testing.B) {
			root := b.TempDir()
			for _, dir := range []string{"src", "node_modules/pkg", ".claude"} {
				if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
					b.Fatal(err)
				}
			}
			for _, rel := range []string{"src/main.go", "node_modules/pkg/index.js", ".claude/mcp.json"} {
				if err := os.WriteFile(filepath.Join(root, rel), []byte("fixture\n"), 0o644); err != nil {
					b.Fatal(err)
				}
			}
			patterns := append([]string{}, excludes.Builtin...)
			if negation {
				patterns = append(patterns, "!.gitkeep")
			}
			w, err := NewWatcher(&Indexer{rootPath: root}, config.WatchConfig{Exclude: patterns}, zap.NewNop())
			if err != nil {
				b.Fatal(err)
			}
			filter := &watcherExcludeFilter{w: w}
			for _, tc := range []struct{ name, rel string }{
				{"admitted_file", "src/main.go"},
				{"admitted_folder", "src"},
				{"ignored_tree", "node_modules/pkg/index.js"},
				{"mcp", ".claude/mcp.json"},
			} {
				path := filepath.Join(root, tc.rel)
				b.Run(tc.name, func(b *testing.B) {
					b.Run("backend", func(b *testing.B) {
						watcherExclusionResult = filter.ShouldInclude(path)
						b.ReportAllocs()
						b.ResetTimer()
						for i := 0; i < b.N; i++ {
							watcherExclusionResult = filter.ShouldInclude(path)
						}
					})
					b.Run("full", func(b *testing.B) {
						watcherExclusionResult = w.isExcluded(path)
						b.ReportAllocs()
						b.ResetTimer()
						for i := 0; i < b.N; i++ {
							watcherExclusionResult = w.isExcluded(path)
						}
					})
				})
			}
		})
	}
}
