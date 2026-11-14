package indexer

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/search"
)

func externalLifetimeTree(call string) map[string]string {
	return map[string]string{
		"go.mod":  "module example.com/fixture\n\ngo 1.22\n",
		"util.go": "package fixture\n\nimport \"strings\"\n\nfunc NormalizeID(id string) string { return strings." + call + "(strings.TrimSpace(id)) }\n",
	}
}

func externalLifetimeIndexer(t *testing.T, store graph.Store) *Indexer {
	t.Helper()
	idx := newTestIndexer(store)
	t.Cleanup(idx.Close)
	idx.SetRepoPrefix(builderRepoPrefix)
	idx.SetWorkspaceID(builderRepoPrefix)
	idx.SetProjectID(builderRepoPrefix)
	return idx
}

// Given the exact NormalizeID expression, when only ToLower becomes ToUpper,
// then each native backend must equal an independent full index of that tree.
func TestExternalLifetimeIncremental(t *testing.T) {
	t.Run("sqlite", func(t *testing.T) {
		dir := builderTempDir(t, "fixture")
		builderWriteTree(t, dir, externalLifetimeTree("ToLower"))
		store, clean := builderOpenStore(t, "incremental"), builderOpenStore(t, "clean")
		idx := externalLifetimeIndexer(t, store)
		_, err := idx.Index(dir)
		require.NoError(t, err)
		externalLifetimeRequireBase(t, store)
		path := filepath.Join(dir, "util.go")
		bumpMtime(t, path, externalLifetimeTree("ToUpper")["util.go"])
		var steps []string
		idx.incrementalCatchupHook = func(kind string, files []string) {
			steps = append(steps, kind+":"+strings.Join(files, ","))
		}
		result, err := idx.IncrementalReindexPaths(dir, []string{path})
		require.NoError(t, err)
		require.Equal(t, 1, result.StaleFileCount, "must actually reparse util.go")
		require.Empty(t, result.FailedFiles)
		require.True(t, slices.ContainsFunc(steps, func(s string) bool { return strings.HasPrefix(s, "ref_facts:") }), "deferred catch-up must complete: %v", steps)
		_, err = externalLifetimeIndexer(t, clean).Index(dir)
		require.NoError(t, err)
		externalLifetimeCompare(t, store, clean)

		// TestExternalLifetimeIncrementalLifecycle covers deletion against a fresh index.
	})

	t.Run("synchronous-references", func(t *testing.T) {
		dir := builderTempDir(t, "synchronous")
		builderWriteTree(t, dir, externalLifetimeTree("ToLower"))
		store := builderOpenStore(t, "synchronous")
		idx := externalLifetimeIndexer(t, store)
		_, err := idx.Index(dir)
		require.NoError(t, err)
		externalLifetimeRequireBase(t, store)
		path := filepath.Join(dir, "util.go")
		bumpMtime(t, path, externalLifetimeTree("ToUpper")["util.go"])
		var steps []string
		idx.incrementalCatchupHook = func(kind string, _ []string) { steps = append(steps, kind) }
		// No marker batch: commitStructuralIncrementalBatch resolves inline.
		result, err := idx.incrementalReindexPathsMode(dir, []string{path}, incrementalPathMode{detectDeletions: true})
		require.NoError(t, err)
		require.Equal(t, 1, result.StaleFileCount)
		require.Contains(t, steps, "ref_facts")
		clean := builderOpenStore(t, "synchronous-clean")
		_, err = externalLifetimeIndexer(t, clean).Index(dir)
		require.NoError(t, err)
		externalLifetimeCompare(t, store, clean)
	})
}

type externalLifetimeProbeFailureExtractor struct {
	parser.Extractor
	calls     atomic.Int32
	onExtract func(int32)
}

func (e *externalLifetimeProbeFailureExtractor) Extract(path string, src []byte) (*parser.ExtractionResult, error) {
	call := e.calls.Add(1)
	if call == 1 {
		return nil, fmt.Errorf("forced delta probe failure")
	}
	if e.onExtract != nil {
		e.onExtract(call)
	}
	return e.Extractor.Extract(path, src)
}

// Given a failed delta probe, when the per-file fallback replaces ToLower with
// ToUpper without nested calls, then the whole graph equals a fresh SQLite
// index after either synchronous or deferred resolution.
func TestExternalLifetimeIncrementalFallback(t *testing.T) {
	for _, deferred := range []bool{false, true} {
		t.Run(fmt.Sprintf("deferred=%t", deferred), func(t *testing.T) {
			dir := builderTempDir(t, "fallback")
			body := "package fixture\n\nimport \"strings\"\n\nfunc NormalizeID(id string) string { return strings.ToLower(id) }\n"
			builderWriteTree(t, dir, map[string]string{
				"go.mod":   externalLifetimeTree("ToLower")["go.mod"],
				"util.go":  body,
				"other.go": "package fixture\nimport \"strings\"\nfunc Other(id string) string { return strings.TrimSpace(id) }\n",
			})
			store := builderOpenStore(t, "fallback")
			idx := externalLifetimeIndexer(t, store)
			goExtractor, ok := idx.registry.GetByLanguage("go")
			require.True(t, ok)
			failing := &externalLifetimeProbeFailureExtractor{Extractor: goExtractor}
			failing.calls.Store(1)
			idx.registry.Register(failing)
			_, err := idx.Index(dir)
			require.NoError(t, err)
			externalLifetimeRequireBase(t, store)
			failing.calls.Store(0)
			path := filepath.Join(dir, "util.go")
			bumpMtime(t, path, strings.Replace(body, "ToLower", "ToUpper", 1))
			var result *IndexResult
			if deferred {
				result, err = idx.IncrementalReindexPaths(dir, []string{path})
			} else {
				result, err = idx.incrementalReindexPathsMode(dir, []string{path}, incrementalPathMode{detectDeletions: true})
			}
			require.NoError(t, err)
			require.Equal(t, 1, result.StaleFileCount)
			require.Empty(t, result.FailedFiles)
			require.Equal(t, int32(2), failing.calls.Load(), "failed probe must be followed by fallback extraction")
			clean := builderOpenStore(t, "fallback-clean")
			cleanIdx := externalLifetimeIndexer(t, clean)
			// Extractor identity participates in the contract config stamp.
			// Use the same wrapper, with its one forced failure already spent.
			cleanExtractor := &externalLifetimeProbeFailureExtractor{Extractor: goExtractor}
			cleanExtractor.calls.Store(1)
			cleanIdx.registry.Register(cleanExtractor)
			_, err = cleanIdx.Index(dir)
			require.NoError(t, err)
			externalLifetimeCompare(t, store, clean)
		})
	}
}

// Given a forced fallback, when its first indexFile applies the replacement but
// rejects the read version, then the successful retry equals a fresh SQLite
// index after either synchronous or deferred resolution.
func TestExternalLifetimeIncrementalFallbackVersionChange(t *testing.T) {
	for _, deferred := range []bool{false, true} {
		t.Run(fmt.Sprintf("deferred=%t", deferred), func(t *testing.T) {
			dir := builderTempDir(t, "fallback-version-change")
			body := "package fixture\n\nimport \"strings\"\n\nfunc NormalizeID(id string) string { return strings.ToLower(id) }\n"
			builderWriteTree(t, dir, map[string]string{
				"go.mod":   externalLifetimeTree("ToLower")["go.mod"],
				"util.go":  body,
				"other.go": "package fixture\nimport \"strings\"\nfunc Other(id string) string { return strings.TrimSpace(id) }\n",
			})
			store := builderOpenStore(t, "fallback-version-change")
			idx := externalLifetimeIndexer(t, store)
			goExtractor, ok := idx.registry.GetByLanguage("go")
			require.True(t, ok)
			failing := &externalLifetimeProbeFailureExtractor{Extractor: goExtractor}
			failing.calls.Store(1)
			idx.registry.Register(failing)
			_, err := idx.Index(dir)
			require.NoError(t, err)
			externalLifetimeRequireBase(t, store)
			path := filepath.Join(dir, "util.go")
			edited := strings.Replace(body, "ToLower", "ToUpper", 1)
			bumpMtime(t, path, edited)
			failing.calls.Store(0)
			var observations struct {
				sync.Mutex
				mutationErr error
				retryErr    error
				retried     bool
			}
			failing.onExtract = func(call int32) {
				observations.Lock()
				defer observations.Unlock()
				if call == 2 {
					// The fallback owns the earlier read. Change its receipt without
					// changing source, so indexFile applies and then rejects it.
					info, statErr := os.Stat(path)
					observations.mutationErr = statErr
					if statErr == nil {
						mtime := info.ModTime().Add(time.Second)
						observations.mutationErr = os.Chtimes(path, mtime, mtime)
					}
				}
				if call == 3 {
					observations.retryErr = idx.fileIndexFailureError(path)
					observations.retried = true
				}
			}
			var result *IndexResult
			if deferred {
				result, err = idx.incrementalWatcherPaths(dir, []string{path}, incrementalPathMode{detectDeletions: true})
			} else {
				result, err = idx.incrementalReindexPathsMode(dir, []string{path}, incrementalPathMode{detectDeletions: true})
			}
			require.NoError(t, err)
			observations.Lock()
			mutationErr, retryErr, retried := observations.mutationErr, observations.retryErr, observations.retried
			observations.Unlock()
			require.NoError(t, mutationErr)
			require.True(t, retried, "the first fallback must fail its read receipt before retry")
			require.ErrorIs(t, retryErr, errFileVersionChanged)
			require.Equal(t, int32(3), failing.calls.Load())
			require.Empty(t, result.FailedFiles)
			clean := builderOpenStore(t, "fallback-version-change-clean")
			cleanIdx := externalLifetimeIndexer(t, clean)
			cleanExtractor := &externalLifetimeProbeFailureExtractor{Extractor: goExtractor}
			cleanExtractor.calls.Store(1)
			cleanIdx.registry.Register(cleanExtractor)
			_, err = cleanIdx.Index(dir)
			require.NoError(t, err)
			externalLifetimeCompare(t, store, clean)
		})
	}
}

func externalLifetimeMultiIndexer(t *testing.T, store graph.Store) *MultiIndexer {
	t.Helper()
	mi := NewMultiIndexer(store, newTestRegistry(), search.NewNull(), newTestConfigManager(t), zap.NewNop())
	t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
	return mi
}

// Given a SQLite repository with ToLower, when a save or parallel warm reconcile
// replaces it with ToUpper and the workspace tail completes, then the entire
// graph equals a fresh index. An unchanged warm reconcile removes nothing.
func TestExternalLifetimeMultiIndexer(t *testing.T) {
	for _, mode := range []string{"save", "warm-edit", "warm-unchanged", "warm-aborted"} {
		t.Run(mode, func(t *testing.T) {
			dir := builderTempDir(t, "multi")
			tree := externalLifetimeTree("ToLower")
			tree["one.go"] = "package fixture\nfunc One() {}\n"
			tree["two.go"] = "package fixture\nfunc Two() {}\n"
			builderWriteTree(t, dir, tree)
			entry := config.RepoEntry{Path: dir, Name: "repo"}
			store := builderOpenStore(t, "multi")
			mi := externalLifetimeMultiIndexer(t, store)
			_, err := mi.TrackRepo(entry)
			require.NoError(t, err)
			if mode != "save" {
				mi.BackfillWorkspaceSlugs()
			}
			externalLifetimeRequireBase(t, store)
			priorMtimes := mi.FileMtimes("repo")
			beforeNodes, beforeEdges := builderRenderNodes(store.AllNodes()), builderRenderEdges(store.AllEdges())
			path := filepath.Join(dir, "util.go")
			if mode != "warm-unchanged" {
				bumpMtime(t, path, externalLifetimeTree("ToUpper")["util.go"])
			}
			if mode == "save" {
				result, err := mi.IncrementalReindexRepo("repo", []string{path})
				require.NoError(t, err)
				require.Equal(t, 1, result.StaleFileCount)
				require.Empty(t, result.FailedFiles)
			} else {
				require.NoError(t, mi.Close(context.Background()))
				warm := externalLifetimeMultiIndexer(t, store)
				warm.BeginParallelBatch()
				defer warm.ResetBatch()
				result, err := warm.ReconcileRepoCtx(context.Background(), entry, priorMtimes)
				require.NoError(t, err)
				require.False(t, result.FullRetrack, "fixture must stay on the incremental warm route")
				require.Empty(t, result.FailedFiles)
				if mode != "warm-unchanged" {
					require.Equal(t, 1, result.StaleFileCount)
					require.NotNil(t, store.GetNode("repo::stdlib::strings::ToLower"), "cleanup must wait for workspace resolution")
				} else {
					require.Zero(t, result.StaleFileCount)
				}
				if mode == "warm-edit" {
					// A repeated registered reconcile must not lose pending cleanup.
					again, err := warm.ReconcileRepoCtx(context.Background(), entry, priorMtimes)
					require.NoError(t, err)
					require.Nil(t, again, "registered repositories take the existing no-op route")
				}
				require.NoError(t, warm.RunPreEnrichResolveFiles(context.Background(), result.DerivedInvalidation.Files, nil))
				if mode == "warm-aborted" {
					// An incomplete attempt cannot consume the cleanup frontier.
					run := warm.BeginDeferredPasses(context.Background(), nil)
					run.attemptAborted = true
					require.False(t, run.FinishTailResult().CrossRepoComplete)
					require.NotNil(t, store.GetNode("repo::stdlib::strings::ToLower"))
				}
				require.True(t, warm.RunDeferredPassesAllResult(context.Background()).CrossRepoComplete)
				if mode != "warm-unchanged" {
					require.Nil(t, store.GetNode("repo::stdlib::strings::ToLower"), "workspace tail must retire before EndBatch")
				}
				warm.EndBatch()
			}
			clean := builderOpenStore(t, "multi-clean")
			cleanMI := externalLifetimeMultiIndexer(t, clean)
			_, err = cleanMI.TrackRepo(entry)
			require.NoError(t, err)
			if mode != "save" {
				// Warmup's workspace resolve backfills synthetic-node stamps.
				// Compare with the same completed workspace boundary, not Track alone.
				cleanMI.BackfillWorkspaceSlugs()
			}
			if mode == "warm-unchanged" {
				require.Equal(t, beforeNodes, builderRenderNodes(store.AllNodes()))
				require.Equal(t, beforeEdges, builderRenderEdges(store.AllEdges()))
				externalLifetimeRows(t, "nodes", builderRenderNodes(store.AllNodes()), builderRenderNodes(clean.AllNodes()))
				externalLifetimeRows(t, "edges", builderRenderEdges(store.AllEdges()), builderRenderEdges(clean.AllEdges()))
			} else {
				externalLifetimeCompare(t, store, clean)
			}
		})
	}
}

func externalLifetimeRequireBase(t *testing.T, reader graph.Reader) {
	t.Helper()
	id := builderRepoPrefix + "::stdlib::strings::ToLower"
	require.NotNil(t, reader.GetNode(id), "fixture must create ToLower")
	require.True(t, slices.ContainsFunc(reader.GetOutEdges(id), func(e *graph.Edge) bool { return e.Kind == graph.EdgeMemberOf }), "fixture must create its membership")
}

func externalLifetimeReferences(reader graph.Reader) string {
	ids := []string{builderRepoPrefix + "::stdlib::strings::ToLower", builderRepoPrefix + "::stdlib::strings::ToUpper", builderRepoPrefix + "::stdlib::strings::TrimSpace", builderRepoPrefix + "::module::go:strings"}
	in, out := reader.GetInEdgesByNodeIDs(ids), reader.GetOutEdgesByNodeIDs(ids)
	var text strings.Builder
	for _, id := range ids {
		fmt.Fprintf(&text, "PROJECTION %s node=%s\n incoming=%v\n outgoing=%v\n", id, builderRenderNode(reader.GetNode(id)), builderRenderEdges(in[id]), builderRenderEdges(out[id]))
	}
	return text.String()
}

func externalLifetimeCompare(t *testing.T, got, clean graph.Reader) {
	t.Helper()
	id := builderRepoPrefix + "::stdlib::strings::ToLower"
	require.Nil(t, clean.GetNode(id), "independent clean build must never create old symbol")
	require.Empty(t, clean.GetOutEdges(id), "independent clean build must have no old membership")
	for _, name := range []string{"ToUpper", "TrimSpace"} {
		require.NotNil(t, clean.GetNode(builderRepoPrefix+"::stdlib::strings::"+name))
		if got.GetNode(builderRepoPrefix+"::stdlib::strings::"+name) == nil {
			t.Errorf("current stdlib symbol %s missing", name)
		}
	}
	if got.GetNode(id) != nil {
		t.Errorf("stale ToLower node: %s", builderRenderNode(got.GetNode(id)))
	}
	for _, edge := range got.GetOutEdges(id) {
		if edge.Kind == graph.EdgeMemberOf {
			t.Errorf("stale ToLower/member_of pair: %s", builderRenderEdge(edge))
		}
	}
	externalLifetimeRows(t, "nodes", builderRenderNodes(got.AllNodes()), builderRenderNodes(clean.AllNodes()))
	externalLifetimeRows(t, "edges", builderRenderEdges(got.AllEdges()), builderRenderEdges(clean.AllEdges()))
}

// Sorted multiset subtraction retains duplicates and prints every differing row.
func externalLifetimeRows(t *testing.T, kind string, got, want []string) {
	t.Helper()
	var extra, missing []string
	i, j := 0, 0
	for i < len(got) && j < len(want) {
		switch strings.Compare(got[i], want[j]) {
		case -1:
			extra = append(extra, got[i])
			i++
		case 1:
			missing = append(missing, want[j])
			j++
		default:
			i++
			j++
		}
	}
	extra = append(extra, got[i:]...)
	missing = append(missing, want[j:]...)
	if len(extra)+len(missing) > 0 {
		t.Errorf("complete %s differences (incremental=%d clean=%d extra=%d missing=%d):\n extra:\n%s\n missing:\n%s", kind, len(got), len(want), len(extra), len(missing), strings.Join(extra, "\n"), strings.Join(missing, "\n"))
	}
}

// Given one last caller, when its call is removed/re-added or the file is
// deleted through either common resolution mode, then every final graph equals
// a fresh index. A blank edit has no cleanup effect.
func TestExternalLifetimeIncrementalLifecycle(t *testing.T) {
	for _, deferred := range []bool{false, true} {
		t.Run(fmt.Sprintf("deferred=%t", deferred), func(t *testing.T) {
			dir := builderTempDir(t, "lifecycle")
			tree := externalLifetimeTree("ToLower")
			tree["types.go"] = "package fixture\nvar Kept string\n"
			builderWriteTree(t, dir, tree)
			store := builderOpenStore(t, "lifecycle")
			idx := externalLifetimeIndexer(t, store)
			_, err := idx.Index(dir)
			require.NoError(t, err)
			path := filepath.Join(dir, "util.go")
			update := func() *IndexResult {
				t.Helper()
				var result *IndexResult
				var err error
				if deferred {
					result, err = idx.IncrementalReindexPaths(dir, []string{path})
				} else {
					result, err = idx.incrementalReindexPathsMode(dir, []string{path}, incrementalPathMode{detectDeletions: true})
				}
				require.NoError(t, err)
				require.Empty(t, result.FailedFiles)
				return result
			}
			compare := func() {
				t.Helper()
				clean := builderOpenStore(t, "lifecycle-clean")
				_, err := externalLifetimeIndexer(t, clean).Index(dir)
				require.NoError(t, err)
				externalLifetimeRows(t, "nodes", builderRenderNodes(store.AllNodes()), builderRenderNodes(clean.AllNodes()))
				externalLifetimeRows(t, "edges", builderRenderEdges(store.AllEdges()), builderRenderEdges(clean.AllEdges()))
			}
			bumpMtime(t, path, "package fixture\n\nfunc NormalizeID(id string) string { return id }\n")
			require.Equal(t, 1, update().StaleFileCount)
			require.Nil(t, store.GetNode("repo::stdlib::strings::ToLower"))
			require.Nil(t, store.GetNode("repo::module::go:strings"))
			compare()
			bumpMtime(t, path, externalLifetimeTree("ToLower")["util.go"])
			require.Equal(t, 1, update().StaleFileCount)
			externalLifetimeRequireBase(t, store)
			compare()
			require.Zero(t, update().StaleFileCount)
			compare()
			require.NoError(t, os.Remove(path))
			require.Equal(t, 1, update().DeletedFileCount)
			require.Nil(t, store.GetNode("repo::module::go:strings"))
			compare()
		})
	}
}

// Given two files using the same symbol, when one drops its use, then the
// other file's complete incoming and outgoing dataflow stays equal to clean.
func TestExternalLifetimeIncrementalSharedReference(t *testing.T) {
	dir := builderTempDir(t, "shared")
	tree := externalLifetimeTree("ToLower")
	tree["other.go"] = "package fixture\nimport \"strings\"\nfunc Other(s string) string { return strings.ToLower(s) }\n"
	builderWriteTree(t, dir, tree)
	store := builderOpenStore(t, "shared")
	idx := externalLifetimeIndexer(t, store)
	_, err := idx.Index(dir)
	require.NoError(t, err)
	path := filepath.Join(dir, "util.go")
	bumpMtime(t, path, externalLifetimeTree("ToUpper")["util.go"])
	_, err = idx.IncrementalReindexPaths(dir, []string{path})
	require.NoError(t, err)
	externalLifetimeRequireBase(t, store)
	clean := builderOpenStore(t, "shared-clean")
	_, err = externalLifetimeIndexer(t, clean).Index(dir)
	require.NoError(t, err)
	externalLifetimeRows(t, "nodes", builderRenderNodes(store.AllNodes()), builderRenderNodes(clean.AllNodes()))
	externalLifetimeRows(t, "edges", builderRenderEdges(store.AllEdges()), builderRenderEdges(clean.AllEdges()))
}

func externalLifetimeSymbol(repo, pkg, name string) *graph.Node {
	return &graph.Node{ID: repo + "::stdlib::" + pkg + "::" + name, Kind: graph.KindFunction,
		Name: name, Language: "go", RepoPrefix: repo,
		Meta: map[string]any{"external": true, "module_path": pkg, "module_role": "stdlib"}}
}

func externalLifetimeModule(repo, pkg string) *graph.Node {
	return &graph.Node{ID: repo + "::module::go:" + pkg, Kind: graph.KindModule,
		Name: pkg, Language: "go", RepoPrefix: repo,
		Meta: map[string]any{"ecosystem": "go", "import_path": pkg, "role": "stdlib"}}
}

func externalLifetimeMembership(symbol, module *graph.Node) *graph.Edge {
	return &graph.Edge{From: symbol.ID, To: module.ID, Kind: graph.EdgeMemberOf, Origin: graph.OriginASTResolved}
}

type externalLifetimeCountingStore struct {
	*store_sqlite.Store
	nodeReads [][]string
	inReads   [][]string
	outReads  [][]string
}

func (s *externalLifetimeCountingStore) GetNodesByIDs(ids []string) map[string]*graph.Node {
	s.nodeReads = append(s.nodeReads, slices.Clone(ids))
	return s.Store.GetNodesByIDs(ids)
}

func (s *externalLifetimeCountingStore) GetInEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	s.inReads = append(s.inReads, slices.Clone(ids))
	return s.Store.GetInEdgesByNodeIDs(ids)
}

func (s *externalLifetimeCountingStore) GetOutEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	s.outReads = append(s.outReads, slices.Clone(ids))
	return s.Store.GetOutEdgesByNodeIDs(ids)
}

func (s *externalLifetimeCountingStore) AllNodes() []*graph.Node { panic("unbounded node read") }
func (s *externalLifetimeCountingStore) AllEdges() []*graph.Edge { panic("unbounded edge read") }

// Given a structural body edit that retains every stdlib call, when either
// resolution mode settles, then cleanup reads no stdlib adjacency. When the
// edited file drops a shared call, only that symbol needs a liveness read.
func TestExternalLifetimeIncrementalKeptCandidateReads(t *testing.T) {
	for _, deferred := range []bool{false, true} {
		t.Run(fmt.Sprintf("deferred=%t", deferred), func(t *testing.T) {
			dir := builderTempDir(t, "kept-candidates")
			tree := externalLifetimeTree("ToLower")
			tree["other.go"] = "package fixture\nimport \"strings\"\nfunc Other(s string) string { return strings.ToLower(s) }\n"
			builderWriteTree(t, dir, tree)
			store := &externalLifetimeCountingStore{Store: builderOpenStore(t, "kept-candidates")}
			idx := externalLifetimeIndexer(t, store)
			_, err := idx.Index(dir)
			require.NoError(t, err)
			path := filepath.Join(dir, "util.go")
			compare := func() {
				t.Helper()
				clean := builderOpenStore(t, "kept-candidates-clean")
				_, err := externalLifetimeIndexer(t, clean).Index(dir)
				require.NoError(t, err)
				externalLifetimeRows(t, "nodes", builderRenderNodes(store.Store.AllNodes()), builderRenderNodes(clean.AllNodes()))
				externalLifetimeRows(t, "edges", builderRenderEdges(store.Store.AllEdges()), builderRenderEdges(clean.AllEdges()))
			}
			update := func(body string) {
				t.Helper()
				bumpMtime(t, path, body)
				resolved := false
				idx.incrementalCatchupHook = func(kind string, _ []string) {
					if kind == "ref_facts" {
						resolved = true
						store.inReads, store.outReads = nil, nil
					}
				}
				var result *IndexResult
				var err error
				if deferred {
					result, err = idx.IncrementalReindexPaths(dir, []string{path})
				} else {
					result, err = idx.incrementalReindexPathsMode(dir, []string{path}, incrementalPathMode{detectDeletions: true})
				}
				require.NoError(t, err)
				require.Empty(t, result.FailedFiles)
				require.Equal(t, 1, result.StaleFileCount)
				require.True(t, resolved, "must take the structural resolution path")
			}
			stdlibReads := func(reads [][]string) [][]string {
				var result [][]string
				for _, read := range reads {
					var ids []string
					for _, id := range read {
						if strings.HasPrefix(id, "repo::stdlib::") || strings.HasPrefix(id, "repo::module::go:") {
							ids = append(ids, id)
						}
					}
					if len(ids) > 0 {
						result = append(result, ids)
					}
				}
				return result
			}
			// Given unchanged stdlib calls, when their body changes, then no
			// candidate adjacency is needed after the resolution tail.
			update("package fixture\nimport \"strings\"\nfunc NormalizeID(id string) string { cleaned := strings.TrimSpace(id); return strings.ToLower(cleaned) }\n")
			require.Empty(t, stdlibReads(store.inReads))
			require.Empty(t, stdlibReads(store.outReads))
			compare()

			// Given another caller of ToLower, when this file drops that call,
			// then only ToLower is projected and its live graph remains intact.
			update(externalLifetimeTree("ToUpper")["util.go"])
			want := [][]string{{"repo::stdlib::strings::ToLower"}}
			require.Equal(t, want, stdlibReads(store.inReads))
			require.Equal(t, want, stdlibReads(store.outReads))
			externalLifetimeRequireBase(t, store)
			compare()
		})
	}
}

// Given duplicate old destinations, pathless recorded sources and unrelated
// externals, when capture and cleanup run, then only the owned stdlib frontier
// is projected. Real incoming/outgoing references and other repos are preserved.
func TestExternalLifetimeIncrementalBoundedCleanup(t *testing.T) {
	store := &externalLifetimeCountingStore{Store: builderOpenStore(t, "bounded")}
	idx := externalLifetimeIndexer(t, store)
	dead, live := externalLifetimeSymbol("repo", "strings", "ToLower"), externalLifetimeSymbol("repo", "strings", "ToUpper")
	module := externalLifetimeModule("repo", "strings")
	last, lastModule := externalLifetimeSymbol("repo", "fmt", "Errorf"), externalLifetimeModule("repo", "fmt")
	other, otherModule := externalLifetimeSymbol("other", "strings", "ToLower"), externalLifetimeModule("other", "strings")
	imported := externalLifetimeModule("repo", "os")
	outgoing, outgoingModule := externalLifetimeSymbol("repo", "bytes", "Clone"), externalLifetimeModule("repo", "bytes")
	fileback := externalLifetimeSymbol("repo", "strings", "FileBacked")
	fileback.FilePath = "repo/source.go"
	dep := &graph.Node{ID: "dep::example.com/pkg::Call", Kind: graph.KindFunction}
	external := &graph.Node{ID: "external::example.com/pkg::Call", Kind: graph.KindFunction}
	builtin := &graph.Node{ID: "repo::builtin::go::type::string", Kind: graph.KindBuiltin}
	file := &graph.Node{ID: "repo/use.go", Kind: graph.KindFile, FilePath: "repo/use.go", RepoPrefix: "repo"}
	nodes := []*graph.Node{dead, live, module, last, lastModule, other, otherModule, imported, outgoing, outgoingModule, fileback, dep, external, builtin, file}
	for i := 0; i < 100; i++ {
		nodes = append(nodes, externalLifetimeSymbol("repo", "net", fmt.Sprintf("Unrelated%d", i)))
	}
	edges := []*graph.Edge{externalLifetimeMembership(dead, module), externalLifetimeMembership(live, module),
		externalLifetimeMembership(last, lastModule), externalLifetimeMembership(other, otherModule), externalLifetimeMembership(outgoing, outgoingModule),
		{From: file.ID, To: dead.ID, Kind: graph.EdgeCalls, FilePath: file.FilePath},
		{From: last.ID, To: dead.ID, Kind: graph.EdgeArgOf, FilePath: file.FilePath},
		{From: "repo/other.go::Use", To: live.ID, Kind: graph.EdgeCalls, FilePath: "repo/other.go"},
		{From: outgoing.ID, To: "repo/other.go::Use", Kind: graph.EdgeValueFlow, FilePath: "repo/other.go"},
		{From: "repo/other.go", To: imported.ID, Kind: graph.EdgeImports, FilePath: "repo/other.go"},
		{From: "other/use.go::Use", To: other.ID, Kind: graph.EdgeCalls, FilePath: "other/use.go"}}
	store.AddBatch(nodes, edges)
	otherIn, otherOut := builderRenderEdges(store.GetInEdges(other.ID)), builderRenderEdges(store.GetOutEdges(other.ID))
	old := map[string][]*graph.Edge{file.ID: {edges[5], edges[5], {From: file.ID, To: dep.ID}, {From: file.ID, To: external.ID}, {From: file.ID, To: other.ID}, {From: file.ID, To: fileback.ID}, {From: file.ID, To: builtin.ID}}}
	ids := idx.captureStdlibCandidates([]string{file.FilePath}, old)
	require.Equal(t, []string{lastModule.ID, module.ID, last.ID, dead.ID}, ids)
	// In particular, last was found only through its pathless recorded source.
	require.Equal(t, 2, graph.RemoveEdgesExact(store, edges[5:7]))
	ids = append(ids, ids...)
	ids = append(ids, live.ID, imported.ID, outgoing.ID, outgoingModule.ID, fileback.ID, dep.ID, external.ID, builtin.ID, "repo::stdlib::strings::Unknown", other.ID)
	token := store.BeginMutationReceipt()
	before := store_sqlite.WriteMark()
	n, e := idx.retireStdlibCandidates(ids, nil)
	require.Equal(t, 3, n)
	require.Equal(t, 2, e)
	require.Equal(t, store_sqlite.RowWriteCounts{Nodes: 3, Edges: 2}, store_sqlite.WriteMark().Since(before).Rows)
	receipt := store.EndMutationReceipt(token)
	require.False(t, receipt.Complete, "existing pathless eviction reports its unenumerated effect conservatively")
	for _, id := range []string{dead.ID, last.ID, lastModule.ID} {
		require.Nil(t, store.GetNode(id))
	}
	for _, node := range []*graph.Node{live, module, imported, outgoing, outgoingModule, other, otherModule, dep, external, builtin, fileback} {
		require.NotNil(t, store.GetNode(node.ID), node.ID)
	}
	require.Equal(t, otherIn, builderRenderEdges(store.GetInEdges(other.ID)))
	require.Equal(t, otherOut, builderRenderEdges(store.GetOutEdges(other.ID)))
	token = store.BeginMutationReceipt()
	before = store_sqlite.WriteMark()
	n, e = idx.retireStdlibCandidates(ids, nil)
	require.Zero(t, n)
	require.Zero(t, e)
	require.Zero(t, store_sqlite.WriteMark().Since(before).Rows.Total())
	require.True(t, store.EndMutationReceipt(token).Complete, "no-change cleanup must not void receipts")
	for _, reads := range [][][]string{store.nodeReads, store.inReads, store.outReads} {
		for _, read := range reads {
			require.Equal(t, len(read), len(appendUniqueSorted(nil, read...)), "duplicate projection: %v", read)
			for _, id := range read {
				require.NotContains(t, id, "Unrelated")
			}
		}
	}
	for _, reads := range [][][]string{store.inReads, store.outReads} {
		for _, read := range reads {
			for _, id := range read {
				require.True(t, ownedStdlibNode(store.GetNode(id), "repo") || id == dead.ID || id == last.ID || id == lastModule.ID, "unrelated liveness projection: %s", id)
			}
		}
	}
}

// Given deferred candidates from separate chunks, when catch-up sees either
// order (including no files to resolve), then it retires the same membership
// island only after all structural work has finished. Evidence scoping is off.
func TestExternalLifetimeIncrementalDeferredEmptyFrontier(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(fmt.Sprintf("reverse=%t", reverse), func(t *testing.T) {
			store := builderOpenStore(t, "empty")
			idx := externalLifetimeIndexer(t, store)
			a, b := externalLifetimeSymbol("repo", "strings", "ToLower"), externalLifetimeSymbol("repo", "strings", "TrimSpace")
			module := externalLifetimeModule("repo", "strings")
			store.AddBatch([]*graph.Node{a, b, module}, []*graph.Edge{externalLifetimeMembership(a, module), externalLifetimeMembership(b, module)})
			batch := &reparsePendingEnrichmentBatch{deferResolverCatchup: true}
			chunks := [][]string{{a.ID, module.ID}, {b.ID, module.ID}}
			if reverse {
				slices.Reverse(chunks)
			}
			for _, ids := range chunks {
				idx.settleStdlibCandidates(ids, nil, batch)
			}
			require.NotNil(t, store.GetNode(a.ID), "not retired before catch-up")
			require.Len(t, batch.stdlibCandidates, 3)
			idx.retireDeferredStdlibCandidates(batch)
			require.Empty(t, batch.stdlibCandidates)
			require.Empty(t, store.AllNodes())
			require.Empty(t, store.AllEdges())
		})
	}
}

// Given a real SQLite target edit and an FTS document for the old symbol,
// when the last reference disappears, then exact and token search omit it,
// every FTS record has a graph owner, and statistics equal the actual rows.
func TestExternalLifetimeIncrementalQueries(t *testing.T) {
	dir := builderTempDir(t, "queries")
	builderWriteTree(t, dir, externalLifetimeTree("ToLower"))
	store := builderOpenStore(t, "queries")
	idx := externalLifetimeIndexer(t, store)
	_, err := idx.Index(dir)
	require.NoError(t, err)
	old := "repo::stdlib::strings::ToLower"
	require.NoError(t, store.UpsertSymbolFTS(old, "retired lifetimeprobe"))
	hits, err := store.SearchSymbols("retired lifetimeprobe", 20)
	require.NoError(t, err)
	require.Len(t, hits, 1)
	require.Equal(t, old, hits[0].NodeID)
	path := filepath.Join(dir, "util.go")
	bumpMtime(t, path, externalLifetimeTree("ToUpper")["util.go"])
	_, err = idx.IncrementalReindexPaths(dir, []string{path})
	require.NoError(t, err)
	for _, query := range []string{"ToLower", "retired lifetimeprobe"} {
		hits, err = store.SearchSymbols(query, 20)
		require.NoError(t, err)
		require.Empty(t, hits)
	}
	for _, name := range []string{"ToUpper", "TrimSpace"} {
		hits, err = store.SearchSymbols(name, 20)
		require.NoError(t, err)
		require.Contains(t, hits, graph.SymbolHit{NodeID: "repo::stdlib::strings::" + name, Score: 100})
	}
	db, err := sql.Open("sqlite", store.Path())
	require.NoError(t, err)
	defer db.Close()
	var orphans int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM symbol_fts f
LEFT JOIN symbol_fts_rowid m ON m.fts_rowid=f.rowid
LEFT JOIN nodes n ON n.id=m.node_id AND n.view_gen=m.view_gen
WHERE n.id IS NULL`).Scan(&orphans))
	require.Zero(t, orphans)
	stats := store.Stats()
	require.Equal(t, len(store.AllNodes()), stats.TotalNodes)
	require.Equal(t, len(store.AllEdges()), stats.TotalEdges)
}

// Given a store that lacks explicit-ID eviction, when cleanup receives valid
// candidates, then it preserves the graph rather than falling back to a scan.
func TestExternalLifetimeIncrementalUnsupportedStore(t *testing.T) {
	store := builderOpenStore(t, "unsupported")
	symbol, module := externalLifetimeSymbol("repo", "strings", "ToLower"), externalLifetimeModule("repo", "strings")
	store.AddBatch([]*graph.Node{symbol, module}, []*graph.Edge{externalLifetimeMembership(symbol, module)})
	idx := externalLifetimeIndexer(t, struct{ graph.Store }{store})
	before := store_sqlite.WriteMark()
	n, e := idx.retireStdlibCandidates([]string{symbol.ID, module.ID}, nil)
	require.Zero(t, n)
	require.Zero(t, e)
	require.Zero(t, store_sqlite.WriteMark().Since(before).Rows.Total())
	require.NotNil(t, store.GetNode(symbol.ID))
	require.NotNil(t, store.GetNode(module.ID))
}

// Given a reference moving across structural chunks, when chunks run in either
// order with direct or deferred resolution, then cleanup sees final liveness
// and the whole graph equals an independent index of the edited files.
func TestExternalLifetimeIncrementalChunkOrder(t *testing.T) {
	for _, deferred := range []bool{false, true} {
		for _, reverse := range []bool{false, true} {
			t.Run(fmt.Sprintf("deferred=%t/reverse=%t", deferred, reverse), func(t *testing.T) {
				dir := builderTempDir(t, "chunks")
				tree := map[string]string{"go.mod": "module example.com/fixture\n\ngo 1.22\n"}
				var paths []string
				for i := 0; i <= incrementalBatchFiles; i++ {
					name := fmt.Sprintf("f%02d.go", i)
					tree[name] = fmt.Sprintf("package fixture\nfunc F%d(s string) string { return s }\n", i)
					paths = append(paths, filepath.Join(dir, name))
				}
				tree["f00.go"] = "package fixture\nimport \"strings\"\nfunc F0(s string) string { return strings.ToLower(s) }\n"
				builderWriteTree(t, dir, tree)
				store := builderOpenStore(t, "chunks")
				idx := externalLifetimeIndexer(t, store)
				_, err := idx.Index(dir)
				require.NoError(t, err)
				for i, path := range paths {
					body := fmt.Sprintf("package fixture\nfunc G%d(s string) string { return s + \"x\" }\n", i)
					if i == incrementalBatchFiles {
						body = fmt.Sprintf("package fixture\nimport \"strings\"\nfunc G%d(s string) string { return strings.ToLower(s) }\n", i)
					}
					bumpMtime(t, path, body)
				}
				if reverse {
					slices.Reverse(paths)
				}
				var batch *reparsePendingEnrichmentBatch
				if deferred {
					batch = &reparsePendingEnrichmentBatch{deferResolverCatchup: true}
				}
				plan, reparsed, failed, raced := idx.reindexIncrementalFilesBatched(paths, nil, batch, false)
				require.Empty(t, failed)
				require.Empty(t, raced)
				require.Len(t, reparsed, incrementalBatchFiles+1)
				if deferred {
					idx.runIncrementalResolutionCatchup(plan.Files, batch, func(files []string) { idx.resolver.ResolveFilesAndIncoming(files) })
					idx.retireDeferredStdlibCandidates(batch)
				}
				externalLifetimeRequireBase(t, store)
				clean := builderOpenStore(t, "chunks-clean")
				_, err = externalLifetimeIndexer(t, clean).Index(dir)
				require.NoError(t, err)
				externalLifetimeRows(t, "nodes", builderRenderNodes(store.AllNodes()), builderRenderNodes(clean.AllNodes()))
				externalLifetimeRows(t, "edges", builderRenderEdges(store.AllEdges()), builderRenderEdges(clean.AllEdges()))
			})
		}
	}
}

// Given an attributed import-only stdlib module, when its last recorded
// import disappears, then candidate cleanup removes the module and no file row.
func TestExternalLifetimeIncrementalImportOnly(t *testing.T) {
	store := builderOpenStore(t, "import-only")
	idx := externalLifetimeIndexer(t, store)
	module := externalLifetimeModule("repo", "time")
	file := &graph.Node{ID: "repo/use.go", FilePath: "repo/use.go", Kind: graph.KindFile, RepoPrefix: "repo"}
	edge := &graph.Edge{From: file.ID, To: module.ID, Kind: graph.EdgeImports, FilePath: file.FilePath}
	store.AddBatch([]*graph.Node{file, module}, []*graph.Edge{edge})
	ids := idx.captureStdlibCandidates([]string{file.FilePath}, store.GetOutEdgesByNodeIDs([]string{file.ID}))
	require.Equal(t, []string{module.ID}, ids)
	require.Equal(t, 1, graph.RemoveEdgesExact(store, []*graph.Edge{edge}))
	n, e := idx.retireStdlibCandidates(ids, nil)
	require.Equal(t, 1, n)
	require.Zero(t, e)
	require.Nil(t, store.GetNode(module.ID))
	require.Equal(t, builderRenderNodes([]*graph.Node{file}), builderRenderNodes(store.AllNodes()))
	require.Empty(t, store.AllEdges())
}

// Given a MultiIndexer forced deletion, when its complete shared resolver tail
// retires the last stdlib references, then returned counts include every removed
// node and edge, and the entire graph equals a fresh SQLite index.
func TestExternalLifetimeMultiIndexerForcedDeletion(t *testing.T) {
	dir := builderTempDir(t, "multi-forced-deletion")
	tree := externalLifetimeTree("ToLower")
	tree["types.go"] = "package fixture\nvar Kept string\n"
	builderWriteTree(t, dir, tree)
	entry := config.RepoEntry{Path: dir, Name: "repo"}
	store := builderOpenStore(t, "multi-forced-deletion")
	mi := externalLifetimeMultiIndexer(t, store)
	_, err := mi.TrackRepo(entry)
	require.NoError(t, err)
	beforeNodes, beforeEdges := store.NodeCount(), store.EdgeCount()
	require.NoError(t, os.Remove(filepath.Join(dir, "util.go")))
	n, e, err := mi.incrementalEvictRepoRaw("repo", "util.go")
	require.NoError(t, err)
	require.Equal(t, beforeNodes-store.NodeCount(), n)
	require.Equal(t, beforeEdges-store.EdgeCount(), e)
	require.Nil(t, store.GetNode("repo::stdlib::strings::ToLower"))
	require.Nil(t, store.GetNode("repo::module::go:strings"))
	clean := builderOpenStore(t, "multi-forced-deletion-clean")
	_, err = externalLifetimeMultiIndexer(t, clean).TrackRepo(entry)
	require.NoError(t, err)
	externalLifetimeRows(t, "nodes", builderRenderNodes(store.AllNodes()), builderRenderNodes(clean.AllNodes()))
	externalLifetimeRows(t, "edges", builderRenderEdges(store.AllEdges()), builderRenderEdges(clean.AllEdges()))
}

// Given an explicit deletion, when the complete forced-eviction tail finishes,
// then returned removal counts include retired stdlib rows exactly once.
func TestExternalLifetimeIncrementalForcedDeletion(t *testing.T) {
	dir := builderTempDir(t, "forced-deletion")
	tree := externalLifetimeTree("ToLower")
	tree["types.go"] = "package fixture\nvar Kept string\n"
	builderWriteTree(t, dir, tree)
	store := builderOpenStore(t, "forced-deletion")
	idx := externalLifetimeIndexer(t, store)
	_, err := idx.Index(dir)
	require.NoError(t, err)
	beforeNodes, beforeEdges := store.NodeCount(), store.EdgeCount()
	require.NoError(t, os.Remove(filepath.Join(dir, "util.go")))
	n, e, err := idx.incrementalEvictWatcherPath("util.go")
	require.NoError(t, err)
	require.Equal(t, beforeNodes-store.NodeCount(), n)
	require.Equal(t, beforeEdges-store.EdgeCount(), e)
	require.Nil(t, store.GetNode("repo::stdlib::strings::ToLower"))
	require.Nil(t, store.GetNode("repo::module::go:strings"))
	clean := builderOpenStore(t, "forced-deletion-clean")
	_, err = externalLifetimeIndexer(t, clean).Index(dir)
	require.NoError(t, err)
	externalLifetimeRows(t, "nodes", builderRenderNodes(store.AllNodes()), builderRenderNodes(clean.AllNodes()))
	externalLifetimeRows(t, "edges", builderRenderEdges(store.AllEdges()), builderRenderEdges(clean.AllEdges()))
}
