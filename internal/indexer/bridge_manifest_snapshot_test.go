package indexer

import (
	"io/fs"
	"iter"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"go.uber.org/zap"
)

type bridgeSnapshotCountStore struct {
	graph.Store
	files, nodes int
}

func (s *bridgeSnapshotCountStore) GetNode(id string) *graph.Node {
	s.nodes++
	return s.Store.GetNode(id)
}
func (s *bridgeSnapshotCountStore) FileNodeIdentitiesSeq(repos []string) iter.Seq[graph.FileNodeIdentity] {
	s.files++
	return graph.FileNodeIdentitiesSeq(s.Store, repos)
}

func backdateBridgeFixture(t *testing.T, root string) {
	t.Helper()
	old := time.Now().Add(-time.Hour)
	require.NoError(t, filepath.WalkDir(root, func(file string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Chtimes(file, old, old)
	}))
}

func TestPythonManifestSnapshotReusesParsedLookup(t *testing.T) {
	provider, consumer := t.TempDir(), t.TempDir()
	writeWorkspaceFile(t, provider, "pyproject.toml", "[project]\nname = 'pillow'\n")
	writeWorkspaceFile(t, provider, "src/PIL/__init__.py", "")
	writeWorkspaceFile(t, consumer, "pyproject.toml", "[project]\ndependencies = ['pillow']\n")
	writeWorkspaceFile(t, consumer, "app.py", "import PIL\n")
	g := &bridgeSnapshotCountStore{Store: graph.New()}
	for repo, files := range map[string][]string{"provider": {"src/PIL/__init__.py"}, "consumer": {"app.py"}} {
		for _, file := range files {
			g.AddNode(&graph.Node{ID: repo + "/" + file, FilePath: repo + "/" + file, Kind: graph.KindFile, RepoPrefix: repo})
		}
	}
	mi := &MultiIndexer{graph: g, repos: map[string]*RepoMetadata{}, indexers: map[string]*Indexer{}}
	for repo, root := range map[string]string{"provider": provider, "consumer": consumer} {
		idx := &Indexer{graph: g, repoPrefix: repo, logger: zap.NewNop()}
		idx.SetRootPath(root)
		initial := map[string]int64{}
		if repo == "consumer" {
			initial["app.py"] = 1
		}
		idx.SetFileMtimes(initial)
		mi.indexers[repo] = idx
		mi.repos[repo] = &RepoMetadata{RootPath: root}
		backdateBridgeFixture(t, root)
	}
	priorRead := readDiskFile
	reads := 0
	readDiskFile = func(file string) ([]byte, bool) { reads++; return priorRead(file) }
	t.Cleanup(func() { readDiskFile = priorRead })
	first := mi.pythonPackageLookup()
	require.NotNil(t, first)
	require.Len(t, first.Providers("consumer", "PIL"), 1)
	reads, g.files, g.nodes = 0, 0, 0
	second := mi.pythonPackageLookup()
	require.NotNil(t, second)
	require.Len(t, second.Providers("consumer", "PIL"), 1)
	require.Zero(t, reads, "unchanged pass must reuse parsed manifests")
	require.Zero(t, g.nodes, "unchanged provider layout must not be probed")
	require.Zero(t, g.files, "unchanged consumer inventory must not be scanned again")
	// A structural edit that preserves the file set keeps the snapshot.
	mi.indexers["consumer"].recordFileMtimeValue("app.py", 1)
	second = mi.pythonPackageLookup()
	require.Len(t, second.Providers("consumer", "PIL"), 1)
	// A new local namespace blocks the foreign package on the next pass.
	g.AddNode(&graph.Node{ID: "consumer/src/PIL/local.py", FilePath: "consumer/src/PIL/local.py", Kind: graph.KindFile, RepoPrefix: "consumer"})
	mi.indexers["consumer"].recordFileMtimeValue("src/PIL/local.py", 1)
	require.Empty(t, mi.pythonPackageLookup().Providers("consumer", "PIL"))
}

func TestCargoManifestSnapshotReusesParsedLookup(t *testing.T) {
	provider, consumer := t.TempDir(), t.TempDir()
	writeWorkspaceFile(t, provider, "Cargo.toml", "[package]\nname = 'pillow'\n")
	writeWorkspaceFile(t, consumer, "Cargo.toml", "[package]\nname = 'app'\n[dependencies]\npillow = '1'\n")
	mi := &MultiIndexer{graph: graph.New(), repos: map[string]*RepoMetadata{}, indexers: map[string]*Indexer{}}
	for repo, root := range map[string]string{"provider": provider, "consumer": consumer} {
		idx := &Indexer{}
		idx.SetRootPath(root)
		mi.indexers[repo] = idx
		mi.repos[repo] = &RepoMetadata{RootPath: root}
		backdateBridgeFixture(t, root)
	}
	priorRead := readDiskFile
	reads := 0
	readDiskFile = func(file string) ([]byte, bool) { reads++; return priorRead(file) }
	t.Cleanup(func() { readDiskFile = priorRead })
	first := mi.lazyCargoCrateLookup()
	require.NotNil(t, first)
	require.Zero(t, reads)
	require.Len(t, first("consumer/src/lib.rs", "pillow"), 1)
	reads = 0
	second := mi.cargoCrateLookup()
	require.NotNil(t, second)
	require.Len(t, second("consumer/src/lib.rs", "pillow"), 1)
	require.Zero(t, reads, "unchanged Cargo lookup must reuse parsed manifests")
	// A same-size dependency rename must invalidate the snapshot.
	writeWorkspaceFile(t, consumer, "Cargo.toml", "[package]\nname = 'app'\n[dependencies]\nmissing = '1'\n")
	require.Empty(t, mi.cargoCrateLookup()("consumer/src/lib.rs", "pillow"))
	// Restoring the manifest restores the dependency without a restart.
	writeWorkspaceFile(t, consumer, "Cargo.toml", "[package]\nname = 'app'\n[dependencies]\npillow = '1'\n")
	require.Len(t, mi.cargoCrateLookup()("consumer/src/lib.rs", "pillow"), 1)
}

func TestBridgeManifestSnapshotRacyBoundary(t *testing.T) {
	stampTime := time.Unix(1700000000, 0)
	for _, tc := range []struct {
		name     string
		offset   time.Duration
		reusable bool
	}{
		{"old", -time.Second, true}, {"boundary", 0, false}, {"recent", time.Second, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "pyproject.toml")
			require.NoError(t, os.WriteFile(file, []byte("aaaa"), 0600))
			modified := stampTime.Add(-manifestRacyWindow).Add(tc.offset)
			require.NoError(t, os.Chtimes(file, modified, modified))
			s := &bridgeManifestSnapshot{stampedAt: stampTime, reusable: true, files: map[string]bridgeManifestStamp{}}
			s.watch(file)
			// Same size/mtime can hide a second write on a coarse clock.
			require.NoError(t, os.WriteFile(file, []byte("bbbb"), 0600))
			require.NoError(t, os.Chtimes(file, modified, modified))
			require.Equal(t, tc.reusable, s.matches(s))
			s.unknown = true
			require.False(t, s.matches(s), "a failed read must not be reused")
		})
	}
}

func TestCargoManifestSnapshotWorkspaceAndBoundaryChanges(t *testing.T) {
	provider, consumer := t.TempDir(), t.TempDir()
	writeWorkspaceFile(t, provider, "Cargo.toml", "[workspace]\nmembers = ['crates/*']\n")
	require.NoError(t, os.MkdirAll(filepath.Join(provider, "crates/empty"), 0700))
	writeWorkspaceFile(t, consumer, "Cargo.toml", "[package]\nname = 'app'\n[dependencies]\npillow = '1'\n")
	mi := &MultiIndexer{repos: map[string]*RepoMetadata{}, indexers: map[string]*Indexer{}}
	for repo, root := range map[string]string{"provider": provider, "consumer": consumer} {
		idx := &Indexer{}
		idx.SetRootPath(root)
		mi.indexers[repo] = idx
		mi.repos[repo] = &RepoMetadata{RootPath: root}
		backdateBridgeFixture(t, root)
	}
	require.Empty(t, mi.cargoCrateLookup()("consumer/src/lib.rs", "pillow"))
	writeWorkspaceFile(t, provider, "crates/empty/Cargo.toml", "[package]\nname = 'pillow'\n")
	require.Len(t, mi.cargoCrateLookup()("consumer/src/lib.rs", "pillow"), 1)
	backdateBridgeFixture(t, provider)
	backdateBridgeFixture(t, consumer)
	require.Len(t, mi.cargoCrateLookup()("consumer/tools/child/src/lib.rs", "pillow"), 1)
	// A newly introduced nonmember boundary blocks inheritance from the root.
	writeWorkspaceFile(t, consumer, "tools/child/Cargo.toml", "[package]\nname = 'child'\n")
	require.Empty(t, mi.cargoCrateLookup()("consumer/tools/child/src/lib.rs", "pillow"))
	require.NoError(t, os.Remove(filepath.Join(consumer, "tools/child/Cargo.toml")))
	require.Len(t, mi.cargoCrateLookup()("consumer/tools/child/src/lib.rs", "pillow"), 1)
}

func TestBridgeManifestSnapshotInventoryAndRoots(t *testing.T) {
	idx := &Indexer{graph: graph.New(), repoPrefix: "repo", logger: zap.NewNop()}
	idx.SetRootPath(t.TempDir())
	mi := &MultiIndexer{repos: map[string]*RepoMetadata{"repo": {RootPath: idx.RootPath()}}, indexers: map[string]*Indexer{"repo": idx}}
	idx.SetFileMtimes(map[string]int64{"a.py": 1})
	s := mi.newBridgeManifestSnapshot()
	idx.recordFileMtimeValue("a.py", 2)
	require.True(t, s.matches(mi.newBridgeManifestSnapshot()), "ordinary edits preserve file inventory")
	idx.recordFileMtimeValue("b.py", 2)
	require.False(t, s.matches(mi.newBridgeManifestSnapshot()), "additions change inventory")
	s = mi.newBridgeManifestSnapshot()
	idx.invalidateColdFileMtimes([]string{"b.py"})
	require.False(t, s.matches(mi.newBridgeManifestSnapshot()), "removals change inventory")
	s = mi.newBridgeManifestSnapshot()
	idx.SetFileMtimes(map[string]int64{"a.py": 2})
	require.False(t, s.matches(mi.newBridgeManifestSnapshot()), "full rebuild invalidates provider identities")
	s = mi.newBridgeManifestSnapshot()
	mi.indexers["repo"] = &Indexer{}
	require.False(t, s.matches(mi.newBridgeManifestSnapshot()), "retrack replaces the owner")
	s = mi.newBridgeManifestSnapshot()
	mi.repos["repo"] = &RepoMetadata{RootPath: t.TempDir()}
	require.False(t, s.matches(mi.newBridgeManifestSnapshot()), "a new root is not the old snapshot")
}

func TestBridgeManifestSnapshotSourceAuthority(t *testing.T) {
	provider, consumer, committed := t.TempDir(), t.TempDir(), t.TempDir()
	writeWorkspaceFile(t, provider, "Cargo.toml", "[package]\nname = 'pillow'\n")
	writeWorkspaceFile(t, provider, "pyproject.toml", "[project]\nname = 'pillow'\n")
	writeWorkspaceFile(t, provider, "PIL/__init__.py", "")
	writeWorkspaceFile(t, consumer, "Cargo.toml", "[package]\nname = 'app'\n[dependencies]\npillow = '1'\n")
	writeWorkspaceFile(t, consumer, "pyproject.toml", "[project]\ndependencies = ['pillow']\n")
	writeWorkspaceFile(t, committed, "Cargo.toml", "[package]\nname = 'app'\n")
	writeWorkspaceFile(t, committed, "pyproject.toml", "[project]\nname = 'app'\n")
	g := graph.New()
	g.AddNode(&graph.Node{ID: "provider/PIL/__init__.py", FilePath: "provider/PIL/__init__.py", RepoPrefix: "provider", Kind: graph.KindFile})
	idx := &Indexer{}
	idx.SetRootPath(consumer)
	mi := &MultiIndexer{graph: g, repos: map[string]*RepoMetadata{"provider": {RootPath: provider}, "consumer": {RootPath: consumer}}, indexers: map[string]*Indexer{"consumer": idx}}
	require.Len(t, mi.cargoCrateLookup()("consumer/src/lib.rs", "pillow"), 1)
	require.NotNil(t, mi.pythonPackageLookup())
	idx.SetContentSource(snapshotOf(t, committed))
	require.Empty(t, mi.cargoCrateLookup()("consumer/src/lib.rs", "pillow"))
	require.Nil(t, mi.pythonPackageLookup(), "dirty dependency must not escape the installed source")
	idx.SetContentSource(nil)
	require.Len(t, mi.cargoCrateLookup()("consumer/src/lib.rs", "pillow"), 1)
	require.NotNil(t, mi.pythonPackageLookup())
}

func TestPythonManifestSnapshotFailedGlobIsUnknown(t *testing.T) {
	provider, consumer := t.TempDir(), t.TempDir()
	writeWorkspaceFile(t, provider, "pyproject.toml", "[project]\nname='library'\n")
	writeWorkspaceFile(t, consumer, "requirements-private.txt", "library\n")
	g := &pythonLookupScanStore{Store: graph.New()}
	g.AddEdge(&graph.Edge{From: "consumer/app.py", FilePath: "consumer/app.py", Kind: graph.EdgeImports, To: "external::pkg.extra"})
	mi := &MultiIndexer{graph: g, repos: map[string]*RepoMetadata{"provider": {RootPath: provider}, "consumer": {RootPath: consumer}}}
	priorRead := readDiskFile
	readDiskFile = func(file string) ([]byte, bool) {
		if filepath.Base(file) == "requirements-private.txt" {
			return nil, false
		}
		return priorRead(file)
	}
	t.Cleanup(func() { readDiskFile = priorRead })
	// stat can succeed even though enumeration fails; mirror Glob's empty result.
	priorDir := readBridgeManifestDir
	readBridgeManifestDir = func(dir string) ([]os.DirEntry, error) {
		if dir == consumer {
			return nil, fs.ErrPermission
		}
		return priorDir(dir)
	}
	t.Cleanup(func() { readBridgeManifestDir = priorDir })
	snapshot := mi.newBridgeManifestSnapshot()
	snapshot.trees["consumer"] = &bridgeManifestTree{manifestTree: failingGlobManifestTree{manifestTree: newDiskManifestTree(consumer)}, snapshot: snapshot}
	snapshot.pythonEntries = readPythonPackageEntries(snapshot.trees)
	require.True(t, snapshot.hasUnknownManifest(), "failed glob cannot certify an absent declaration")
	require.True(t, snapshot.declaresPythonBridge())
}

type failingGlobManifestTree struct{ manifestTree }

func (t failingGlobManifestTree) matchFiles(string) []string { return nil }
func (t failingGlobManifestTree) matchDirs(string) []string  { return nil }
