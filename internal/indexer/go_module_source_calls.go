package indexer

import (
	"context"
	"crypto/sha256"
	"github.com/zzet/gortex/internal/parser/crashpool"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/resolver"
	"go.uber.org/zap"
)

// goModuleSourceCacheEntry keeps source facts, not resolution decisions.
// Equal stats are trusted only when the recorded read was outside the racy window.
type goModuleSourceCacheEntry struct {
	digest         [32]byte
	edges          []*graph.Edge
	size, modified int64
	recordedAt     time.Time
}

// FAT timestamps can cover two seconds (HFS+ one). Entries recorded within
// that granularity must verify content even when size and mtime match.
const goModuleSourceRacyWindow = 2 * time.Second

var readGoModuleCallerFile = os.ReadFile

// goModuleSourceNow stamps the start of a verifying read. Tests pin it so each
// case takes one branch of the racy-file rule regardless of runner speed.
var goModuleSourceNow = time.Now

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

// Known saves must verify again even when an editor restores an old mtime.
func (idx *Indexer) invalidateGoModuleSourceStamp(rel string) {
	idx.goModuleSourceMu.Lock()
	defer idx.goModuleSourceMu.Unlock()
	if cached, ok := idx.goModuleSourceCache[rel]; ok {
		cached.recordedAt = time.Time{}
		idx.goModuleSourceCache[rel] = cached
	}
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
	if idx.config.MaxFileSize > 0 && info.Size() > idx.config.MaxFileSize {
		return
	}
	idx.goModuleSourceMu.Lock()
	cached, hit := idx.goModuleSourceCache[rel]
	idx.goModuleSourceMu.Unlock()
	if hit && cached.size == info.Size() && cached.modified == info.ModTime().UnixNano() && info.ModTime().Before(cached.recordedAt.Add(-goModuleSourceRacyWindow)) {
		cr.RestoreGoModuleSourceCalls(cloneGoModuleSourceEdges(cached.edges))
		return
	}
	parseLease, err := acquireParseAdmission(context.Background(), info.Size(), 0, nil, idx.parseAdmission.Load())
	if err != nil {
		mi.logger.Warn("admit Go module caller", zap.String("file", file), zap.Error(err))
		return
	}
	defer parseLease.Release()
	recordedAt := goModuleSourceNow() // Record wall time before reading the bytes being verified.
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
		cached.size, cached.modified, cached.recordedAt = info.Size(), info.ModTime().UnixNano(), recordedAt
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
		idx.goModuleSourceCache[rel] = goModuleSourceCacheEntry{digest: digest, edges: cloneGoModuleSourceEdges(result.Edges), size: info.Size(), modified: info.ModTime().UnixNano(), recordedAt: recordedAt}
		idx.goModuleSourceMu.Unlock()
		cr.RestoreGoModuleSourceCalls(result.Edges)
	}
	result.ReleaseTree()
}
