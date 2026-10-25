package indexer

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/resolver"
	"go.uber.org/zap"
)

func (mi *MultiIndexer) finishGoModuleCalls(cr *resolver.CrossRepoResolver) {
	for _, file := range cr.TakeGoModuleSourceFiles() {
		prefix := graph.RepoPrefixOfID(file)
		idx := mi.GetIndexer(prefix)
		if idx == nil {
			continue
		}
		rel := strings.TrimPrefix(file, prefix+"/")
		abs := filepath.Join(idx.rootPath, filepath.FromSlash(rel))
		src, err := os.ReadFile(abs)
		if err != nil {
			mi.logger.Warn("read Go module caller", zap.String("file", file), zap.Error(err))
			continue
		}
		if idx.config.MaxFileSize > 0 && int64(len(src)) > idx.config.MaxFileSize {
			continue
		}
		ext, ok := idx.registry.GetByLanguage("go")
		if !ok {
			continue
		}
		src = idx.transforms.run(rel, src)
		result, skipped, err := idx.extractFile(nil, nil, abs, rel, "go", ext, src)
		if err != nil {
			mi.logger.Warn("extract Go module caller", zap.String("file", file), zap.Error(err))
		}
		if result == nil {
			continue
		}
		if err == nil && !skipped {
			idx.applyRepoPrefix(result.Nodes, result.Edges)
			cr.RestoreGoModuleSourceCalls(result.Edges)
		}
		result.ReleaseTree()
	}
	mi.synthesizeDemotedGoModuleCalls(cr.TakeDemotedGoModuleCallFiles())
}
