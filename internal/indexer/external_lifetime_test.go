package indexer

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
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
		fmt.Printf("EXTERNAL_LIFETIME backend=sqlite reached IncrementalReindexPaths; catchup=%v\n", steps)
		externalLifetimeCompare(t, store, clean)

		// Deletion can have an empty resolve frontier. Inspect its final
		// bounded reference projection rather than assume catch-up ran.
		steps = nil
		require.NoError(t, os.Remove(path))
		result, err = idx.IncrementalReindexPaths(dir, []string{path})
		require.NoError(t, err)
		require.Equal(t, 1, result.DeletedFileCount)
		fmt.Printf("EXTERNAL_LIFETIME backend=sqlite after deletion catchup=%v\n%s", steps, externalLifetimeReferences(store))
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
		fmt.Printf("EXTERNAL_LIFETIME synchronous structural resolve steps=%v\n", steps)
		clean := builderOpenStore(t, "synchronous-clean")
		_, err = externalLifetimeIndexer(t, clean).Index(dir)
		require.NoError(t, err)
		externalLifetimeCompare(t, store, clean)
	})
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
	fmt.Printf("EXTERNAL_LIFETIME post-resolve bounded references\n%s", externalLifetimeReferences(got))
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
	n, e := idx.retireStdlibCandidates(ids)
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
	n, e = idx.retireStdlibCandidates(ids)
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
				idx.settleStdlibCandidates(ids, batch)
			}
			require.NotNil(t, store.GetNode(a.ID), "not retired before catch-up")
			require.Len(t, batch.stdlibCandidates, 3)
			require.Nil(t, idx.runIncrementalResolutionCatchup(nil, batch, func([]string) { t.Fatal("empty frontier must not resolve") }))
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
	n, e := idx.retireStdlibCandidates([]string{symbol.ID, module.ID})
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
	n, e := idx.retireStdlibCandidates(ids)
	require.Equal(t, 1, n)
	require.Zero(t, e)
	require.Nil(t, store.GetNode(module.ID))
	require.Equal(t, builderRenderNodes([]*graph.Node{file}), builderRenderNodes(store.AllNodes()))
	require.Empty(t, store.AllEdges())
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
