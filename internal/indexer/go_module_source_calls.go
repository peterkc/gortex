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
var readGoModuleCallerFile = os.ReadFile

type goModuleSourceCacheEntry struct {
	digest         [32]byte
	edges          []*graph.Edge
	size, modified int64
}

func cloneGoModuleSourceEdges(edges []*graph.Edge) []*graph.Edge {
	out := make([]*graph.Edge, 0, len(edges))
	for _, e := range edges {
		if resolver.GoModuleCallSpec(e) == "" {
			continue
		}
		c := *e
		c.Meta = maps.Clone(e.Meta)
		out = append(out, &c)
	}
	return out
}

func (mi *MultiIndexer) finishGoModuleCalls(cr *resolver.CrossRepoResolver) {
	for _, file := range cr.TakeGoModuleSourceFiles() {
		mi.restoreGoModuleSourceFile(cr, file)
	}
	mi.synthesizeDemotedGoModuleCalls(cr.TakeDemotedGoModuleCallFiles())
}

func (mi *MultiIndexer) restoreGoModuleSourceFile(cr *resolver.CrossRepoResolver, file string) {
	prefix := graph.RepoPrefixOfID(file)
	idx := mi.GetIndexer(prefix)
	if idx == nil {
		return
	}
	rel := strings.TrimPrefix(file, prefix+"/")
	abs := filepath.Join(idx.rootPath, filepath.FromSlash(rel))
	info, err := os.Stat(abs)
	if err != nil {
		mi.logger.Warn("stat Go module caller", zap.String("file", file), zap.Error(err))
		return
	}
	idx.goModuleSourceMu.Lock()
	cached, hit := idx.goModuleSourceCache[rel]
	idx.goModuleSourceMu.Unlock()
	if hit && cached.size == info.Size() && cached.modified == info.ModTime().UnixNano() {
		cr.RestoreGoModuleSourceCalls(cloneGoModuleSourceEdges(cached.edges))
		return
	}
	parseLease, err := idx.acquireSharedParsePath(abs)
	if err != nil {
		mi.logger.Warn("admit Go module caller", zap.String("file", file), zap.Error(err))
		return
	}
	defer parseLease.Release()
	src, err := readGoModuleCallerFile(abs)
	if err != nil {
		mi.logger.Warn("read Go module caller", zap.String("file", file), zap.Error(err))
		return
	}
	if idx.config.MaxFileSize > 0 && int64(len(src)) > idx.config.MaxFileSize {
		return
	}
	ext, ok := idx.registry.GetByLanguage("go")
	if !ok {
		return
	}
	src = idx.transforms.run(rel, src)
	digest := sha256.Sum256(src)
	if hit && cached.digest == digest {
		cached.size, cached.modified = info.Size(), info.ModTime().UnixNano()
		idx.goModuleSourceMu.Lock()
		idx.goModuleSourceCache[rel] = cached
		idx.goModuleSourceMu.Unlock()
		cr.RestoreGoModuleSourceCalls(cloneGoModuleSourceEdges(cached.edges))
		return
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
		return
	}
	if err == nil && !skipped {
		idx.applyRepoPrefix(result.Nodes, result.Edges)
		idx.goModuleSourceMu.Lock()
		if idx.goModuleSourceCache == nil {
			idx.goModuleSourceCache = make(map[string]goModuleSourceCacheEntry)
		}
		idx.goModuleSourceCache[rel] = goModuleSourceCacheEntry{digest: digest, edges: cloneGoModuleSourceEdges(result.Edges), size: info.Size(), modified: info.ModTime().UnixNano()}
		idx.goModuleSourceMu.Unlock()
		cr.RestoreGoModuleSourceCalls(result.Edges)
	}
	result.ReleaseTree()
}
