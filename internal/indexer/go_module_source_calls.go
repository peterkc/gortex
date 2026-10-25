package indexer

import (
	"crypto/sha256"
	"github.com/zzet/gortex/internal/parser/crashpool"
	"maps"
	"os"
	"path/filepath"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/resolver"
	"go.uber.org/zap"
)

// Cache source facts, not resolution decisions. Provider/config changes still
// resolve the cloned calls again; unchanged caller bytes need no second parse.
type goModuleSourceCacheEntry struct {
	digest [32]byte
	edges  []*graph.Edge
}

func cloneGoModuleSourceEdges(edges []*graph.Edge) []*graph.Edge {
	out := make([]*graph.Edge, 0, len(edges))
	for _, e := range edges {
		c := *e
		c.Meta = maps.Clone(e.Meta)
		out = append(out, &c)
	}
	return out
}

func (mi *MultiIndexer) finishGoModuleCalls(cr *resolver.CrossRepoResolver) {
	for _, file := range cr.TakeGoModuleSourceFiles() {
		prefix := graph.RepoPrefixOfID(file)
		idx := mi.GetIndexer(prefix)
		if idx == nil {
			continue
		}
		rel := strings.TrimPrefix(file, prefix+"/")
		abs := filepath.Join(idx.rootPath, filepath.FromSlash(rel))
		parseLease, err := idx.acquireSharedParsePath(abs)
		if err != nil {
			mi.logger.Warn("admit Go module caller", zap.String("file", file), zap.Error(err))
			continue
		}
		src, err := os.ReadFile(abs)
		if err != nil {
			mi.logger.Warn("read Go module caller", zap.String("file", file), zap.Error(err))
			parseLease.Release()
			continue
		}
		if idx.config.MaxFileSize > 0 && int64(len(src)) > idx.config.MaxFileSize {
			parseLease.Release()
			continue
		}
		ext, ok := idx.registry.GetByLanguage("go")
		if !ok {
			parseLease.Release()
			continue
		}
		src = idx.transforms.run(rel, src)
		digest := sha256.Sum256(src)
		idx.goModuleSourceMu.Lock()
		cached, hit := idx.goModuleSourceCache[rel]
		idx.goModuleSourceMu.Unlock()
		if hit && cached.digest == digest {
			cr.RestoreGoModuleSourceCalls(cloneGoModuleSourceEdges(cached.edges))
			parseLease.Release()
			continue
		}
		var pool *crashpool.Pool
		var quarantine *crashpool.Quarantine
		if idx.crashIsolationEnabled() {
			pool, quarantine = idx.sharedParsePool()
		}
		result, skipped, err := idx.extractFileWithRawLease(parseLease, pool, quarantine, abs, rel, "go", ext, src)
		if quarantine != nil && quarantine.Len() > 0 {
			_ = quarantine.Save()
		}
		if err != nil {
			mi.logger.Warn("extract Go module caller", zap.String("file", file), zap.Error(err))
		}
		if result == nil {
			parseLease.Release()
			continue
		}
		if err == nil && !skipped {
			idx.applyRepoPrefix(result.Nodes, result.Edges)
			idx.goModuleSourceMu.Lock()
			if idx.goModuleSourceCache == nil {
				idx.goModuleSourceCache = make(map[string]goModuleSourceCacheEntry)
			}
			idx.goModuleSourceCache[rel] = goModuleSourceCacheEntry{digest: digest, edges: cloneGoModuleSourceEdges(result.Edges)}
			idx.goModuleSourceMu.Unlock()
			cr.RestoreGoModuleSourceCalls(result.Edges)
		}
		result.ReleaseTree()
		parseLease.Release()
	}
	mi.synthesizeDemotedGoModuleCalls(cr.TakeDemotedGoModuleCallFiles())
}
