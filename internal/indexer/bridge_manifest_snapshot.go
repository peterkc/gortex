package indexer

import (
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/zzet/gortex/internal/resolver"
)

var readBridgeManifestDir = os.ReadDir

// A file-set revision changes only when accepted files are added, removed, or
// replaced as a full inventory. Ordinary symbol/edge edits keep local tops valid.
type bridgeManifestInventory struct {
	indexer   *Indexer
	revision  uint64
	workspace string
}

type bridgeManifestStamp struct {
	size, modified int64
	kind           os.FileMode
	exists, valid  bool
}

func statBridgeManifest(file string) bridgeManifestStamp {
	info, err := os.Stat(file)
	if os.IsNotExist(err) {
		return bridgeManifestStamp{valid: true}
	}
	if err != nil {
		return bridgeManifestStamp{}
	}
	return bridgeManifestStamp{size: info.Size(), modified: info.ModTime().UnixNano(), kind: info.Mode().Type(), exists: true, valid: true}
}

type bridgeManifestSnapshot struct {
	revision  uint64
	roots     map[string]string
	inventory map[string]bridgeManifestInventory
	trees     map[string]manifestTree
	stampedAt time.Time
	reusable  bool
	// Cargo discovers nearest manifest boundaries on demand after publication.
	mu            sync.Mutex
	files         map[string]bridgeManifestStamp
	unknown       bool
	python        *resolver.PythonPackageLookup
	pythonBuilt   bool
	pythonEntries map[string]*pythonManifestEntry
	cargo         resolver.CargoCrateLookup
}

func (mi *MultiIndexer) newBridgeManifestSnapshot() *bridgeManifestSnapshot {
	s := &bridgeManifestSnapshot{
		roots: map[string]string{}, inventory: map[string]bridgeManifestInventory{},
		trees: map[string]manifestTree{}, files: map[string]bridgeManifestStamp{},
		stampedAt: time.Now(), reusable: true,
	}
	if mi.configMgr != nil {
		s.revision = mi.configMgr.Revision()
	}
	for repo, meta := range mi.AllMetadata() {
		if meta == nil || meta.RootPath == "" {
			continue
		}
		s.roots[repo] = meta.RootPath
		idx := mi.GetIndexer(repo)
		if idx == nil {
			// A hand-built registry cannot certify graph file inventory changes.
			s.reusable = false
		} else {
			s.inventory[repo] = bridgeManifestInventory{idx, idx.fileSetRevision.Load(), idx.WorkspaceID()}
		}
		tree := mi.manifestTreeFor(repo, meta.RootPath)
		if tree.sourced() {
			s.reusable = false
		}
		s.trees[repo] = &bridgeManifestTree{manifestTree: tree, snapshot: s}
	}
	return s
}

func (s *bridgeManifestSnapshot) matches(next *bridgeManifestSnapshot) bool {
	if s == nil || s.revision != next.revision || !maps.Equal(s.roots, next.roots) || !maps.Equal(s.inventory, next.inventory) || !next.reusable {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.reusable || s.unknown {
		return false
	}
	for file, stamp := range s.files {
		// A recently written file can change again without changing size/mtime on
		// a coarse filesystem clock. Never certify such an observation as stable.
		if !stamp.valid || stamp.exists && !time.Unix(0, stamp.modified).Before(s.stampedAt.Add(-manifestRacyWindow)) || statBridgeManifest(file) != stamp {
			return false
		}
	}
	return true
}

func (s *bridgeManifestSnapshot) watch(file string) bridgeManifestStamp {
	s.mu.Lock()
	defer s.mu.Unlock()
	stamp, seen := s.files[file]
	if !seen {
		stamp = statBridgeManifest(file)
		s.files[file] = stamp
		if !stamp.valid {
			s.unknown = true
		}
	}
	return stamp
}

func rejectBridgeManifest(tree manifestTree) {
	if watched, ok := tree.(*bridgeManifestTree); ok {
		watched.snapshot.mu.Lock()
		watched.snapshot.unknown = true
		watched.snapshot.mu.Unlock()
	}
}

// Watch the same files/glob directory prefixes the native builders use. This
// records stat evidence only, never file bytes or directory content hashes.
type bridgeManifestTree struct {
	manifestTree
	snapshot *bridgeManifestSnapshot
}

func (t *bridgeManifestTree) watch(rel string) bridgeManifestStamp {
	if t.sourced() {
		return bridgeManifestStamp{}
	}
	return t.snapshot.watch(filepath.Join(t.root(), filepath.FromSlash(rel)))
}
func (t *bridgeManifestTree) readFile(rel string) ([]byte, bool) {
	stamp := t.watch(rel)
	src, ok := t.manifestTree.readFile(rel)
	if !ok && stamp.exists {
		rejectBridgeManifest(t)
	}
	return src, ok
}
func (t *bridgeManifestTree) isFile(rel string) bool {
	t.watch(rel)
	return t.manifestTree.isFile(rel)
}
func (t *bridgeManifestTree) watchGlob(pattern string) {
	if t.sourced() {
		return
	}
	watchDir := func(rel string) {
		stamp := t.watch(rel)
		if stamp.exists && stamp.kind.IsDir() {
			if _, err := readBridgeManifestDir(filepath.Join(t.root(), filepath.FromSlash(rel))); err != nil {
				rejectBridgeManifest(t)
			}
		}
	}
	watchDir(".")
	parts := strings.Split(filepath.ToSlash(pattern), "/")
	for depth := 1; depth <= len(parts); depth++ {
		prefix := strings.Join(parts[:depth], "/")
		if !strings.ContainsAny(prefix, `*?[\`) {
			watchDir(prefix)
		}
		for _, dir := range t.manifestTree.matchDirs(prefix) {
			watchDir(dir)
		}
	}
}
func (t *bridgeManifestTree) matchFiles(pattern string) []string {
	t.watchGlob(pattern)
	return t.manifestTree.matchFiles(pattern)
}
func (t *bridgeManifestTree) matchDirs(pattern string) []string {
	t.watchGlob(pattern)
	return t.manifestTree.matchDirs(pattern)
}

func (mi *MultiIndexer) pythonManifestSnapshot() *bridgeManifestSnapshot {
	next := mi.newBridgeManifestSnapshot()
	mi.bridgeManifestMu.Lock()
	defer mi.bridgeManifestMu.Unlock()
	if mi.pythonPackageCache.matches(next) {
		return mi.pythonPackageCache
	}
	next.pythonEntries = readPythonPackageEntries(next.trees)
	mi.pythonPackageCache = next
	return next
}

func (mi *MultiIndexer) pythonPackageLookup() *resolver.PythonPackageLookup {
	snapshot := mi.pythonManifestSnapshot()
	mi.bridgeManifestMu.Lock()
	defer mi.bridgeManifestMu.Unlock()
	if !snapshot.pythonBuilt {
		snapshot.python = mi.buildPythonPackageLookup(snapshot.trees, snapshot.pythonEntries)
		snapshot.pythonBuilt = true
	}
	return snapshot.python
}

func (mi *MultiIndexer) cargoManifestSnapshot() *bridgeManifestSnapshot {
	next := mi.newBridgeManifestSnapshot()
	mi.bridgeManifestMu.Lock()
	defer mi.bridgeManifestMu.Unlock()
	if mi.cargoPackageCache.matches(next) {
		return mi.cargoPackageCache
	}
	next.cargo = mi.buildCargoCrateLookup(next.trees)
	mi.cargoPackageCache = next
	return next
}

func (mi *MultiIndexer) cargoCrateLookup() resolver.CargoCrateLookup {
	// Direct recovery callers need to know whether a bridge is possible before
	// scanning imports. Keep their cheap root admission and defer manifest reads.
	roots := 0
	for repo, meta := range mi.AllMetadata() {
		if meta != nil && meta.RootPath != "" && mi.manifestTreeFor(repo, meta.RootPath).isFile("Cargo.toml") {
			roots++
		}
	}
	if roots < 2 {
		return nil
	}
	return mi.lazyCargoCrateLookup()
}

// Like npm, resolver construction does no Cargo work before the first use.
func (mi *MultiIndexer) lazyCargoCrateLookup() resolver.CargoCrateLookup {
	var once sync.Once
	var lookup resolver.CargoCrateLookup
	return func(file, name string) []resolver.CargoCrate {
		once.Do(func() { lookup = mi.cargoManifestSnapshot().cargo })
		if lookup == nil {
			return nil
		}
		return lookup(file, name)
	}
}
