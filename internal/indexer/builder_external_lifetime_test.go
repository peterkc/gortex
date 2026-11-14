package indexer

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer/source"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func externalLifetimeCheckout(t *testing.T, store *store_sqlite.Store) string {
	t.Helper()
	builderIsolateGit(t)
	dir := builderTempDir(t, "checkout")
	builderGit(t, dir, "init", "--initial-branch=main")
	builderWriteTree(t, dir, externalLifetimeTree("ToLower"))
	builderGit(t, dir, "add", "-A")
	builderGit(t, dir, "commit", "-q", "-m", "base")
	builderIndex(t, store, dir)
	externalLifetimeRequireBase(t, store)
	builderWriteFile(t, dir, "util.go", externalLifetimeTree("ToUpper")["util.go"])
	return dir
}

// Given a committed base and the exact working-tree edit, when BuildDirtyLayer
// selects edit delta, then its published composition equals a fresh full index.
func TestExternalLifetimeDirtyDelta(t *testing.T) {
	store := builderOpenStore(t, "dirty-base")
	dir := externalLifetimeCheckout(t, store)
	recordLastEditDelta(nil)
	generation, report, err := builderNewBuilder(store).BuildDirtyLayer(context.Background(), DirtyLayerRequest{
		Identity: builderDirtyIdentity(), Base: store, CheckoutRoot: dir,
		RepoPrefix: builderRepoPrefix, WorkspaceID: builderRepoPrefix, ProjectID: builderRepoPrefix,
	})
	require.NoError(t, err)
	delta := LastEditDeltaReport()
	require.NotNil(t, delta, "must reach edit delta rather than sparse fallback")
	require.Contains(t, delta.Paths, "util.go")
	tombstones, err := store.AtGeneration(generation).NodeTombstones()
	require.NoError(t, err)
	fmt.Printf("EXTERNAL_LIFETIME dirty edit-delta reached paths=%v published tombstones=%v orphan=%d\n", delta.Paths, tombstones, report.OrphanStubTombstones)
	clean := builderOpenStore(t, "dirty-clean")
	builderIndex(t, clean, dir)
	externalLifetimeCompare(t, builderComposed(t, store, generation), clean)

	// Inspect Payload itself through the same native per-file engine. No store
	// rows are shared with the independent clean oracle or changed in the base.
	sidecar := builderContextDerivedHandle(t, store)
	dw := graph.NewDeltaWriter(store.AtGeneration(0), sidecar)
	idx := externalLifetimeIndexer(t, dw)
	result, err := idx.incrementalWatcherPaths(dir, []string{filepath.Join(dir, "util.go")}, incrementalPathMode{
		detectDeletions: true, forceExplicitFiles: true, exactPointSemantic: true,
	})
	require.NoError(t, err)
	require.Empty(t, result.FailedFiles)
	require.Empty(t, dw.Unsupported())
	payload := dw.Payload(map[string]struct{}{builderGraphPath(builderRepoPrefix, "util.go"): {}})
	fmt.Printf("EXTERNAL_LIFETIME DeltaWriter.Payload replace=%v delete=%v sources=%v tombstones=%v identities=%v\n", payload.ReplacePaths, payload.DeletePaths, payload.EdgeSources, payload.Tombstones, payload.IdentityClaims)
	id := builderRepoPrefix + "::stdlib::strings::ToLower"
	if !slices.Contains(payload.Tombstones, id) {
		t.Errorf("DeltaWriter.Payload missing dead ToLower tombstone: %v\n%s", payload.Tombstones, externalLifetimeReferences(dw.View()))
	}
	if !slices.Contains(tombstones, id) {
		t.Errorf("published edit delta missing dead ToLower tombstone: %v", tombstones)
	}
}

// Given the same edited tree, when a manifest invalidation hint forces the
// documented fallback, then sparse writeMasks must retire the same dead stub.
// go.mod's bytes stay unchanged; the hint selects the fallback without changing
// the tree used by the clean oracle.
func TestExternalLifetimeSparseFallback(t *testing.T) {
	store := builderOpenStore(t, "sparse-base")
	dir := externalLifetimeCheckout(t, store)
	target, err := source.NewFilesystemSource(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = target.Close() })
	builder := builderNewBuilder(store)
	core, logs := observer.New(zap.InfoLevel)
	builder.Logger = zap.New(core)
	recordLastEditDelta(nil)
	identity := builderDirtyIdentity()
	identity.GenerationKind = DirtyLayerGenerationKind
	generation, report, err := builder.buildWorkingTreeLayer(context.Background(), BuildRequest{
		Identity: identity, Base: store, Target: target, RootPath: dir,
		RepoPrefix: builderRepoPrefix, WorkspaceID: builderRepoPrefix, ProjectID: builderRepoPrefix,
		Changes: []LayerPathChange{{Path: "util.go", Kind: LayerPathModified}, {Path: "go.mod", Kind: LayerPathModified}},
	})
	require.NoError(t, err)
	require.Nil(t, LastEditDeltaReport(), "manifest fallback must bypass edit delta")
	fallback := logs.FilterMessage("indexer: working-tree edit built by the sparse closure builder").All()
	require.Len(t, fallback, 1)
	require.Equal(t, "dependency_manifest: go.mod", fallback[0].ContextMap()["reason"])
	handle := store.AtGeneration(generation)
	masks, err := handle.FileMasks()
	require.NoError(t, err)
	require.Contains(t, masks, store_sqlite.FileMask{RepoPrefix: builderRepoPrefix, FilePath: builderGraphPath(builderRepoPrefix, "util.go"), Mode: store_sqlite.OwnershipReplace})
	tombstones, err := handle.NodeTombstones()
	require.NoError(t, err)
	require.Equal(t, 1, report.OrphanStubTombstones, "only the dead ToLower stub is retired")
	fmt.Printf("EXTERNAL_LIFETIME sparse fallback reason=%v masks=%+v tombstones=%v orphan=%d\n", fallback[0].ContextMap()["reason"], masks, tombstones, report.OrphanStubTombstones)
	clean := builderOpenStore(t, "sparse-clean")
	builderIndex(t, clean, dir)
	externalLifetimeCompare(t, builderComposed(t, store, generation), clean)
	id := builderRepoPrefix + "::stdlib::strings::ToLower"
	if !slices.Contains(tombstones, id) {
		t.Errorf("sparse writeMasks missing dead ToLower tombstone: %v", tombstones)
	}
}

// Given attributed stdlib membership and covered references, when masks retire
// the last caller, then only its symbol and unused module are tombstoned.
// Real, uncertain, payload and other-repository references must remain visible.
func TestWriteMasksStdlibMembership(t *testing.T) {
	for _, name := range []string{"last reference", "other-file incoming", "other-file outgoing", "payload reference", "payload node", "live member", "direct import", "wrong module", "unknown origin", "file-backed", "unknown identity", "other repository", "recorded source", "no covered path"} {
		t.Run(name, func(t *testing.T) {
			store := builderOpenStore(t, "stdlib-masks")
			handle := builderContextDerivedHandle(t, store)
			base := store.AtGeneration(0)
			covered, other := builderGraphPath(builderRepoPrefix, "covered.go"), builderGraphPath(builderRepoPrefix, "other.go")
			owner := &graph.Node{ID: covered + "::Owner", FilePath: covered, Kind: graph.KindFunction}
			peer := &graph.Node{ID: other + "::Peer", FilePath: other, Kind: graph.KindFunction}
			symbol, module := externalLifetimeSymbol(builderRepoPrefix, "strings", "ToLower"), externalLifetimeModule(builderRepoPrefix, "strings")
			membership := externalLifetimeMembership(symbol, module)
			call := &graph.Edge{From: owner.ID, To: symbol.ID, Kind: graph.EdgeCalls, FilePath: covered}
			baseNodes := []*graph.Node{owner, peer, symbol, module}
			baseEdges := []*graph.Edge{call, membership}
			payloadNodes := []*graph.Node{owner}
			var payloadEdges []*graph.Edge
			want := []string{module.ID, symbol.ID}
			switch name {
			case "other-file incoming":
				baseEdges = append(baseEdges, &graph.Edge{From: peer.ID, To: symbol.ID, Kind: graph.EdgeCalls, FilePath: other})
				want = nil
			case "other-file outgoing":
				baseEdges = append(baseEdges, &graph.Edge{From: symbol.ID, To: peer.ID, Kind: graph.EdgeReturns, FilePath: other})
				want = nil
			case "payload reference":
				payloadEdges = []*graph.Edge{{From: symbol.ID, To: owner.ID, Kind: graph.EdgeReturns, FilePath: covered}}
				want = nil
			case "payload node":
				payloadNodes = append(payloadNodes, symbol)
				want = []string{symbol.ID} // the carried row replaces itself, not its live module
			case "live member":
				live := externalLifetimeSymbol(builderRepoPrefix, "strings", "TrimSpace")
				baseNodes = append(baseNodes, live)
				baseEdges = append(baseEdges, externalLifetimeMembership(live, module), &graph.Edge{From: peer.ID, To: live.ID, Kind: graph.EdgeCalls, FilePath: other})
				want = []string{symbol.ID}
			case "direct import":
				baseEdges = append(baseEdges, &graph.Edge{From: peer.ID, To: module.ID, Kind: graph.EdgeImports, FilePath: other})
				want = []string{symbol.ID}
			case "wrong module":
				membership.To = "repo::module::go:bytes"
				want = []string{module.ID}
			case "unknown origin":
				membership.Origin = ""
				want = nil
			case "file-backed":
				symbol.FilePath = other
				want = nil
			case "unknown identity":
				symbol.Meta = nil
				want = nil
			case "other repository":
				symbol.RepoPrefix = "other"
				want = nil
			case "recorded source":
				call.From, call.To, call.Kind = symbol.ID, owner.ID, graph.EdgeReturns
			case "no covered path":
				payloadNodes = nil
				want = nil
			}
			// Existing non-stdlib synthetic membership is deliberately unchanged.
			for _, id := range []string{"repo::dep::package::Symbol", "repo::external::package::Symbol"} {
				baseNodes = append(baseNodes, &graph.Node{ID: id, Kind: graph.KindFunction})
				baseEdges = append(baseEdges, &graph.Edge{From: owner.ID, To: id, Kind: graph.EdgeCalls, FilePath: covered}, &graph.Edge{From: id, To: "unknown-module", Kind: graph.EdgeMemberOf})
			}
			base.AddBatch(baseNodes, baseEdges)
			handle.AddBatch(payloadNodes, payloadEdges)
			beforeNodes, beforeEdges := builderRenderNodes(base.AllNodes()), builderRenderEdges(base.AllEdges())
			var report BuildReport
			err := builderNewBuilder(store).writeMasks(BuildRequest{RepoPrefix: builderRepoPrefix, Base: base}, buildPlan{}, handle, &report)
			require.NoError(t, err)
			tombstones, err := handle.NodeTombstones()
			require.NoError(t, err)
			require.True(t, slices.Equal(want, tombstones), "tombstones = %v, want %v", tombstones, want)
			require.Equal(t, beforeNodes, builderRenderNodes(base.AllNodes()), "mask analysis must not mutate the base")
			require.Equal(t, beforeEdges, builderRenderEdges(base.AllEdges()))
			markers, err := handle.EdgeSourceMasks()
			require.NoError(t, err)
			if len(want) == 0 {
				require.Empty(t, markers, "a live stub must not acquire a broad adjacency claim")
			}
		})
	}
}

func externalLifetimeCheckoutQueries(t *testing.T, store *store_sqlite.Store, chain []int64, reader graph.Reader, clean *store_sqlite.Store) {
	t.Helper()
	engine := query.NewEngine(store.AtGeneration(0))
	engine.SetSearch(search.NewSymbolSearcherBackend(store.AtGeneration(0)))
	var layers []query.ViewLayerSource
	for _, generation := range chain {
		handle := store.AtGeneration(generation)
		layer, err := graphview.NewGenerationLayer(handle)
		require.NoError(t, err)
		layers = append(layers, query.ViewLayerSource{Search: search.NewSymbolSearcherBackend(handle), Layer: layer})
	}
	engine = engine.WithViewLayers(reader, layers)
	cleanEngine := query.NewEngine(clean)
	cleanEngine.SetSearch(search.NewSymbolSearcherBackend(clean))
	for _, name := range []string{"ToLower", "ToUpper", "TrimSpace"} {
		externalLifetimeRows(t, "exact-name search", builderRenderNodes(reader.FindNodesByName(name)), builderRenderNodes(clean.FindNodesByName(name)))
		externalLifetimeRows(t, "served FTS search", builderRenderNodes(engine.SearchSymbols(name, 20)), builderRenderNodes(cleanEngine.SearchSymbols(name, 20)))
	}
	stats := reader.Stats()
	require.Equal(t, len(reader.AllNodes()), stats.TotalNodes)
	require.Equal(t, len(reader.AllEdges()), stats.TotalEdges)
	db, err := sql.Open("sqlite", store.Path())
	require.NoError(t, err)
	defer db.Close()
	var orphanRows int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM symbol_fts f
LEFT JOIN symbol_fts_rowid m ON m.fts_rowid=f.rowid
LEFT JOIN nodes n ON n.id=m.node_id AND n.view_gen=m.view_gen
WHERE n.id IS NULL`).Scan(&orphanRows))
	require.Zero(t, orphanRows)
}

func externalLifetimeBuildCheckout(t *testing.T, store *store_sqlite.Store, req BuildRequest, route string) int64 {
	t.Helper()
	target, err := source.NewFilesystemSource(req.RootPath)
	require.NoError(t, err)
	defer target.Close()
	req.Target = target
	req.Changes = []LayerPathChange{{Path: "util.go", Kind: LayerPathModified}}
	if route == "sparse" {
		req.Changes = append(req.Changes, LayerPathChange{Path: "go.mod", Kind: LayerPathModified})
	}
	recordLastEditDelta(nil)
	generation, _, err := builderNewBuilder(store).buildWorkingTreeLayer(t.Context(), req)
	require.NoError(t, err)
	if route == "dirty" {
		require.NotNil(t, LastEditDeltaReport(), "must reach edit delta")
	} else {
		require.Nil(t, LastEditDeltaReport(), "must reach sparse fallback")
	}
	return generation
}

// Given either repaired checkout composition, when the store reopens and the
// next generation re-adds ToLower, then the new view equals clean while a pinned
// prior generation still omits ToLower and keeps its own tombstones unchanged.
func TestExternalLifetimeSparseFallbackAndDirtyDeltaReopen(t *testing.T) {
	for _, route := range []string{"dirty", "sparse"} {
		t.Run(route, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "reopen.sqlite")
			store := builderOpenStoreAt(t, path)
			t.Cleanup(func() { _ = store.Close() })
			dir := externalLifetimeCheckout(t, store)
			request := BuildRequest{
				Identity: builderDirtyIdentity(), Base: store, RootPath: dir,
				RepoPrefix: builderRepoPrefix, WorkspaceID: builderRepoPrefix, ProjectID: builderRepoPrefix,
			}
			request.Identity.GenerationKind = DirtyLayerGenerationKind
			request.Identity.LowerViewFingerprint = "ToUpper"
			first := externalLifetimeBuildCheckout(t, store, request, route)
			clean := builderOpenStore(t, "edited-clean")
			builderIndex(t, clean, dir)
			externalLifetimeCompare(t, builderComposed(t, store, first), clean)
			require.NoError(t, store.Close())
			store = builderOpenStoreAt(t, path)
			pinned := builderComposed(t, store, first)
			lease := graphview.NewLeaseManager().Acquire(0, first)
			t.Cleanup(lease.Release)
			externalLifetimeCompare(t, pinned, clean)
			externalLifetimeCheckoutQueries(t, store, []int64{first}, pinned, clean)
			priorNodes, priorEdges := builderRenderNodes(pinned.AllNodes()), builderRenderEdges(pinned.AllEdges())
			priorTombstones, err := store.AtGeneration(first).NodeTombstones()
			require.NoError(t, err)
			require.Contains(t, priorTombstones, "repo::stdlib::strings::ToLower")

			builderWriteFile(t, dir, "util.go", externalLifetimeTree("ToLower")["util.go"])
			request.Base = commitLayerBase{Reader: pinned}
			request.Identity.BaseGenerationID = first
			request.Identity.LowerViewFingerprint = "ToLower"
			next := externalLifetimeBuildCheckout(t, store, request, route)
			require.NotEqual(t, first, next)
			readded := builderOpenStore(t, "readded-clean")
			builderIndex(t, readded, dir)
			current := dirtyChainComposed(t, store, []int64{first, next})
			externalLifetimeRows(t, "readded nodes", builderRenderNodes(current.AllNodes()), builderRenderNodes(readded.AllNodes()))
			externalLifetimeRows(t, "readded edges", builderRenderEdges(current.AllEdges()), builderRenderEdges(readded.AllEdges()))
			externalLifetimeCheckoutQueries(t, store, []int64{first, next}, current, readded)
			nextTombstones, err := store.AtGeneration(next).NodeTombstones()
			require.NoError(t, err)
			require.Contains(t, nextTombstones, "repo::stdlib::strings::ToUpper")
			require.NotContains(t, nextTombstones, "repo::stdlib::strings::TrimSpace")
			require.NotContains(t, nextTombstones, "repo::module::go:strings")
			require.Equal(t, priorNodes, builderRenderNodes(pinned.AllNodes()))
			require.Equal(t, priorEdges, builderRenderEdges(pinned.AllEdges()))
			afterTombstones, err := store.AtGeneration(first).NodeTombstones()
			require.NoError(t, err)
			require.Equal(t, priorTombstones, afterTombstones)
			require.Nil(t, pinned.GetNode("repo::stdlib::strings::ToLower"))
			require.NotNil(t, current.GetNode("repo::stdlib::strings::ToLower"))
		})
	}
}
