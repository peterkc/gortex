package resolver

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// The fixtures supply intended main jobs separately from the final saved rows.
// This seam includes losing claims and completed LSP overwrites. The native
// oracle runs the pinned spool, exact-site hydration, closure and guard kernel
// over SQLite, then compares every saved edge and the actual revert attempts.
type rebuildGuardFixture struct {
	nodes []*graph.Node
	edges []*graph.Edge
	moves []graph.EdgeReindex
}

func (f *rebuildGuardFixture) node(id, file, lang, repo string, kind graph.NodeKind, name string) *graph.Node {
	n := &graph.Node{ID: id, FilePath: file, Language: lang, RepoPrefix: repo, Kind: kind, Name: name}
	f.nodes = append(f.nodes, n)
	return n
}

func (f *rebuildGuardFixture) edge(from, to, file string, kind graph.EdgeKind, line int) *graph.Edge {
	e := &graph.Edge{From: from, To: to, FilePath: file, Kind: kind, Line: line}
	f.edges = append(f.edges, e)
	return e
}

func (f *rebuildGuardFixture) job(from, to, file, oldTo string, kind graph.EdgeKind, line int) *graph.Edge {
	e := f.edge(from, to, file, kind, line)
	e.Origin, e.Tier, e.Confidence, e.ConfidenceLabel = graph.OriginASTInferred, "heuristic", 0.6, "INFERRED"
	e.Meta = map[string]any{"resolution": "name", "payload": "source bytes"}
	f.moves = append(f.moves, graph.EdgeReindex{Edge: e, OldTo: oldTo})
	return e
}

func (f rebuildGuardFixture) native(t *testing.T, sourceOrder ...bool) (rows []*graph.Edge, reverted, pages int) {
	t.Helper()
	db, err := store_sqlite.Open(filepath.Join(t.TempDir(), "native.sqlite"))
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()
	require.NoError(t, db.AddBatchChecked(f.nodes, f.edges))
	var oracle graph.Store = db
	if len(sourceOrder) != 0 {
		oracle = &rebuildGuardSiteOracle{Store: db, reverse: sourceOrder[0]}
	}
	r := New(oracle)
	spool, err := newResolveGuardSpool()
	require.NoError(t, err)
	defer spool.close()
	jobs := make([]reindexJob, 0, len(f.moves))
	for _, m := range f.moves {
		e := m.Edge
		jobs = append(jobs, reindexJob{edge: e, oldTo: m.OldTo, newTo: e.To, kind: e.Kind, oldKind: m.OldKind,
			confidence: e.Confidence, origin: e.Origin, crossRepo: e.CrossRepo})
	}
	require.NoError(t, spool.appendJobs([][]reindexJob{jobs}))
	closure := r.buildImportClosure()
	before := db.UnresolvedEdgeInsertions()
	if len(closure) > 0 {
		for done := false; !done; {
			records, exhausted, err := spool.nextPage(resolvePendingPageRows)
			require.NoError(t, err)
			page := guardJobsFromRecords(oracle, records)
			r.warmGuardLookupCache(page)
			reverted += r.guardCrossPackageCallEdges(page, closure)
			r.clearLookupCache()
			pages++
			done = exhausted
		}
	}
	require.Equal(t, uint64(reverted), db.UnresolvedEdgeInsertions()-before, "includes stationary collision attempts")
	rows = db.AllEdges()
	slices.SortFunc(rows, compareRebuildImportEdges)
	return rows, reverted, pages
}

func (f rebuildGuardFixture) private(t *testing.T, spillAt int, refresh func([]lspSpoolRevert) error) ([]*graph.Edge, rebuildGuardStats) {
	t.Helper()
	ctx := t.Context()
	b := rebuildFnPointerBuild(t)
	b.AddBatch(f.nodes, f.edges)
	scratch := t.TempDir()
	g := newRebuildCrossPackageGuard(scratch, spillAt)
	t.Cleanup(func() { require.NoError(t, g.Close()) })
	require.NoError(t, g.Append(ctx, f.moves))
	stats, err := g.Apply(ctx, b, true, refresh)
	require.NoError(t, err)
	name := ""
	if g.spool.file != nil {
		name = g.spool.file.Name()
		require.Equal(t, scratch, filepath.Dir(name))
	}
	require.NoError(t, g.Close())
	if name != "" {
		_, err := os.Stat(name)
		require.ErrorIs(t, err, os.ErrNotExist)
	}
	return rebuildFnPointerSaved(t, b), stats
}

func guardSavedEdge(t *testing.T, rows []*graph.Edge, id graph.EdgeIdentity) *graph.Edge {
	t.Helper()
	for _, e := range rows {
		if graph.EdgeIdentityFor(e) == id {
			return e
		}
	}
	t.Fatalf("saved row missing: %+v", id)
	return nil
}

func TestRebuildCrossPackageGuardPayload(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("GORTEX_RESOLVE_FROM_RECONCILE", "1")
	var f rebuildGuardFixture
	const file = "a/app/caller.ts"
	f.node(file, file, "typescript", "a", graph.KindFile, "caller.ts")
	f.node("a/caller", file, "typescript", "a", graph.KindFunction, "caller")
	f.node("b/barrel", "b/entry/barrel.ts", "typescript", "b", graph.KindFile, "barrel")
	f.node("c/barrel", "c/lib/barrel.ts", "typescript", "c", graph.KindFile, "barrel")
	f.node("d/keep", "d/pkg/keep.ts", "typescript", "d", graph.KindFunction, "keep")
	f.node("e/drop", "e/pkg/drop.ts", "typescript", "e", graph.KindFunction, "drop")
	f.edge(file, "b/barrel", file, graph.EdgeImports, 1)
	f.edge("b/barrel", "c/barrel", "wrong/file", graph.EdgeReExports, 1)
	f.edge("c/barrel", "d/keep", "c/lib/barrel.ts", graph.EdgeReExports, 1)
	f.edge("c/barrel", "e/drop", "c/lib/barrel.ts", graph.EdgeImports, 2) // no transit over ordinary imports
	kept := f.job("a/caller", "d/keep", file, "unresolved::keep", graph.EdgeCalls, 10)
	kept.CrossRepo = true
	drop := f.job("a/caller", "e/drop", file, "unresolved::*.drop", graph.EdgeReferences, 11)
	f.moves[len(f.moves)-1].OldKind = graph.EdgeReads
	drop.CrossRepo, drop.Tier = true, "legacy-inferred"
	for _, kind := range []graph.EdgeKind{graph.EdgeArgOf, graph.EdgeValueFlow} {
		e := f.edge("e/drop", "a/caller", file, kind, 11)
		e.Meta = map[string]any{"source-site": "matching"}
	}
	wrongLine := f.edge("e/drop", "a/caller", file, graph.EdgeValueFlow, 12)
	wrongFile := f.edge("e/drop", "a/caller", "a/other.ts", graph.EdgeArgOf, 11)
	stationary := f.edge("a/caller", "unresolved::drop", file, graph.EdgeCalls, 13)
	stationary.Origin, stationary.Tier, stationary.Confidence = graph.OriginTextMatched, "legacy", 0.1
	stationary.Meta = map[string]any{"payload": "stationary wins"}
	f.job("a/caller", "e/drop", file, stationary.To, graph.EdgeCalls, stationary.Line).Tier = ""
	lsp := f.job("a/caller", "e/drop", file, "unresolved::drop", graph.EdgeCalls, 14)
	lspNew := *lsp
	lspNew.Tier, lspNew.Origin, lspNew.Confidence = "lsp", graph.OriginLSPResolved, 1
	f.edges[len(f.edges)-1] = &lspNew
	losing := f.job("a/caller", "e/drop", file, "unresolved::drop", graph.EdgeCalls, 15)
	winner := *losing
	winner.Meta = map[string]any{"payload": "different but still weak"}
	f.edges[len(f.edges)-1] = &winner
	strong := f.job("a/caller", "e/drop", file, "unresolved::drop", graph.EdgeCalls, 16)
	strong.Origin = graph.OriginASTResolved
	knownNoEvidence := f.job("missing", "e/drop", "a/unindexed.ts", "unresolved::drop", graph.EdgeCalls, 17)
	want, reverted, pages := f.native(t)
	var refresh []lspSpoolRevert
	got, stats := f.private(t, 1, func(page []lspSpoolRevert) error {
		refresh = append(refresh, page...)
		return nil
	})
	require.Equal(t, want, got)
	require.Equal(t, 2, reverted)
	require.Equal(t, reverted, stats.Reverted)
	require.Equal(t, pages, stats.Pages)
	require.Equal(t, 6, stats.Recorded)
	require.Equal(t, 4, stats.Matched)
	require.Equal(t, 2, stats.Sources)
	require.Positive(t, stats.SpillBytes)
	require.Len(t, refresh, 2)
	require.Equal(t, "e/drop", refresh[0].oldBoundTo)
	require.Equal(t, "unresolved::*.drop", refresh[0].edge.To)
	require.Equal(t, "", refresh[0].edge.Origin)
	for _, e := range []*graph.Edge{kept, wrongLine, wrongFile, &lspNew, &winner, strong, knownNoEvidence} {
		require.Equal(t, e, guardSavedEdge(t, got, graph.EdgeIdentityFor(e)))
	}
	for _, kind := range []graph.EdgeKind{graph.EdgeArgOf, graph.EdgeValueFlow} {
		e := guardSavedEdge(t, got, graph.EdgeIdentity{From: "a/unresolved::*.drop", To: "a/caller", Kind: kind, FilePath: file, Line: 11})
		require.Equal(t, map[string]any{"source-site": "matching"}, e.Meta)
	}
	e := guardSavedEdge(t, got, graph.EdgeIdentity{From: drop.From, To: "unresolved::*.drop", Kind: graph.EdgeReferences, FilePath: file, Line: 11})
	require.Equal(t, map[string]any{"payload": "source bytes", "guard_reverted": true}, e.Meta)
	require.True(t, e.CrossRepo)
	require.Empty(t, e.Origin)
	require.Empty(t, e.ConfidenceLabel)
	require.Zero(t, e.Confidence)
	require.Equal(t, "heuristic", e.Tier)
	e = guardSavedEdge(t, got, graph.EdgeIdentityFor(stationary))
	require.Equal(t, stationary.Meta, e.Meta)
	require.Equal(t, stationary.Confidence, e.Confidence)
	require.Empty(t, e.Origin)
	require.Empty(t, e.Tier, "destination tier is derived from the moving pointer")
}

func TestRebuildCrossPackageGuardClosureEmpty(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	var f rebuildGuardFixture
	f.node("caller", "app/caller.ts", "typescript", "", graph.KindFunction, "caller")
	f.node("target", "lib/target.ts", "typescript", "", graph.KindFunction, "target")
	e := f.job("caller", "target", "app/caller.ts", "unresolved::target", graph.EdgeCalls, 7)
	f.edge("caller", "unresolved::import::missing", "app/caller.ts", graph.EdgeImports, 1)
	want, reverted, pages := f.native(t)
	got, stats := f.private(t, 1, nil)
	require.Equal(t, want, got)
	require.Len(t, got, 2)
	require.Equal(t, e, guardSavedEdge(t, got, graph.EdgeIdentityFor(e)))
	require.Equal(t, 1, stats.Recorded, "a job existed, but the global closure was empty")
	require.Zero(t, pages)
	require.Zero(t, reverted)
	require.Zero(t, stats.Pages)
	require.Zero(t, stats.Matched)
}

func TestRebuildCrossPackageGuardCollisionOrder(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("GORTEX_RESOLVE_FROM_RECONCILE", "1")
	for _, padding := range []int{0, resolvePendingPageRows - 1} {
		t.Run(fmt.Sprint(padding), func(t *testing.T) {
			var f rebuildGuardFixture
			const file = "app/caller.ts"
			f.node(file, file, "typescript", "", graph.KindFile, "caller.ts")
			f.node("caller", file, "typescript", "", graph.KindFunction, "caller")
			f.node("target", "lib/target.ts", "typescript", "", graph.KindFunction, "target")
			if padding != 0 {
				f.node("a-pad", file, "typescript", "", graph.KindFunction, "a-pad")
				meta := map[string]any{"payload": strings.Repeat("x", 1024)}
				for i := range padding {
					pad := f.edge("a-pad", "a-pad", file, graph.EdgeCalls, i+1)
					pad.Meta = meta
					f.moves = append(f.moves, graph.EdgeReindex{Edge: pad, OldTo: "unresolved::pad"})
				}
			}
			e := f.job("caller", "target", file, "unresolved::A", graph.EdgeCalls, 2)
			f.moves = append(f.moves, graph.EdgeReindex{Edge: e, OldTo: "unresolved::B"})
			// Force both orders at the unresolved frontier without changing
			// the native spool cuts, hydration, reducer or SQLite writer.
			want, reverts, pages := f.native(t)
			n := len(f.moves)
			f.moves[n-2], f.moves[n-1] = f.moves[n-1], f.moves[n-2]
			opposite, oppositeReverts, oppositePages := f.native(t)
			f.moves[n-2], f.moves[n-1] = f.moves[n-1], f.moves[n-2]
			require.Equal(t, reverts, oppositeReverts)
			require.Equal(t, pages, oppositePages)
			require.NotEqual(t, want, opposite, "D27: the native frontier order changes the saved placeholder")
			chosen := "unresolved::B" // same-page matches borrow the same pointer; last verdict sets To
			if padding != 0 {
				chosen = "unresolved::A" // next page cannot hydrate the earlier target
			}
			id := graph.EdgeIdentityFor(e)
			id.To = chosen
			require.Equal(t, true, guardSavedEdge(t, want, id).Meta["guard_reverted"])
			for _, threshold := range []int{1, 1 << 20} {
				got, stats := f.private(t, threshold, nil)
				require.Equal(t, want, got, "complete old-key order locks the native oracle")
				require.Equal(t, reverts, stats.Reverted)
				require.Equal(t, pages, stats.Pages)
				if padding != 0 {
					require.Equal(t, 2, stats.Pages)
					require.Equal(t, 1, stats.Reverted)
					require.Greater(t, stats.SpillBytes, int64(1<<20))
				} else {
					require.Equal(t, 1, stats.Pages)
					require.Equal(t, 2, stats.Reverted)
				}
			}
		})
	}
}

func TestRebuildCrossPackageGuardReadFailure(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	var f rebuildGuardFixture
	f.node("caller.ts", "caller.ts", "typescript", "", graph.KindFile, "caller.ts")
	f.node("target", "lib/target.ts", "typescript", "", graph.KindFunction, "target")
	e := f.job("caller.ts", "target", "caller.ts", "unresolved::target", graph.EdgeCalls, 2)
	b := rebuildFnPointerBuild(t)
	b.AddBatch(f.nodes, f.edges)
	g := newRebuildCrossPackageGuard(t.TempDir(), 1)
	t.Cleanup(func() { require.NoError(t, g.Close()) })
	require.NoError(t, g.Append(t.Context(), f.moves))
	require.NotNil(t, g.spool.file)
	name := g.spool.file.Name()
	require.NoError(t, g.spool.writer.Flush())
	require.NoError(t, g.spool.file.Truncate(0))
	_, err := g.Apply(t.Context(), b, true, nil)
	require.ErrorIs(t, err, ErrRebuildCrossPackageGuard)
	require.NoError(t, g.Close())
	_, err = os.Stat(name)
	require.ErrorIs(t, err, os.ErrNotExist)
	require.Equal(t, []*graph.Edge{e}, rebuildFnPointerSaved(t, b), "refusal made no guard mutations")
	_, err = b.OpenSnapshot()
	require.ErrorIs(t, err, store_pebble.ErrUnpublishedBuild)
}

// Force the otherwise unordered native exact-site census only for source
// reverts. The original pointer sharing, target guard and SQL writer remain.
type rebuildGuardSiteOracle struct {
	*store_sqlite.Store
	reverse bool
}

func (s *rebuildGuardSiteOracle) GetEdgeCandidates(endpoints []graph.EdgeEndpoint, sites []graph.EdgeSite) graph.EdgeCandidateSet {
	result := s.Store.GetEdgeCandidates(endpoints, sites)
	for _, site := range sites {
		if !graph.PlaceholderSourceKind(site.Kind) {
			continue
		}
		edges := result.Site(site.From, site.Line, site.Kind)
		slices.SortFunc(edges, compareRebuildImportEdges)
		if s.reverse {
			slices.Reverse(edges)
		}
	}
	return result
}

func TestRebuildCrossPackageGuardSourceWindow(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("GORTEX_RESOLVE_FROM_RECONCILE", "1")
	var f rebuildGuardFixture
	const file = "app/caller.ts"
	f.node(file, file, "typescript", "", graph.KindFile, "caller.ts")
	f.node("target", "lib/target.ts", "typescript", "", graph.KindFunction, "target")
	e := f.job(file, "target", file, "unresolved::A", graph.EdgeCalls, 2)
	f.moves = append(f.moves, graph.EdgeReindex{Edge: e, OldTo: "unresolved::B"})
	for i := range 5000 {
		f.edge("target", fmt.Sprintf("pad%04d", i), file, graph.EdgeArgOf, 2)
	}
	match := f.edge("target", "zz-match", file, graph.EdgeArgOf, 2)
	match.Meta = map[string]any{"payload": "moves only if the native window also frees its destination"}
	f.edge("unresolved::A", "zz-match", file, graph.EdgeArgOf, 2)
	want, reverted, _ := f.native(t, false)
	other, otherReverted, _ := f.native(t, true)
	require.Equal(t, 2, reverted)
	require.Equal(t, reverted, otherReverted)
	require.Len(t, want, 5003)
	require.Len(t, other, 5002)
	id := graph.EdgeIdentityFor(match)
	id.From = "unresolved::A"
	require.Equal(t, match.Meta, guardSavedEdge(t, want, id).Meta)
	got, stats := f.private(t, 1, nil)
	require.Equal(t, want, got)
	require.Equal(t, 5002, stats.Sources)
	require.Equal(t, reverted, stats.Reverted)
}

func TestRebuildCrossPackageGuardCSharp(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("GORTEX_RESOLVE_FROM_RECONCILE", "1")
	var f rebuildGuardFixture
	const outer, nested = "repo/app/Caller.cs", "repo/nested/app/Caller.cs"
	f.node("repo/Outer.csproj", "repo/Outer.csproj", "xml", "repo", graph.KindFile, "Outer.csproj")
	f.node("repo/nested/Nested.csproj", "repo/nested/Nested.csproj", "xml", "repo", graph.KindFile, "Nested.csproj")
	usings := f.node("repo/shared/Usings.cs", "repo/shared/Usings.cs", "csharp", "repo", graph.KindFile, "Usings.cs")
	usings.Meta = map[string]any{"global_usings": []string{"Lib"}, "global_using_static": []string{"Tools.Helpers"}}
	f.node(outer, outer, "csharp", "repo", graph.KindFile, "Caller.cs").Meta = map[string]any{
		"usings": []string{"App.Only", "Hidden"}, "scoped_usings": []string{"|App.Only", "Sibling|Hidden"},
	}
	f.node(nested, nested, "csharp", "repo", graph.KindFile, "Caller.cs")
	f.node("repo/outer-caller", outer, "csharp", "repo", graph.KindMethod, "OuterCaller").Meta = map[string]any{"scope_ns": "App"}
	f.node("repo/nested-caller", nested, "csharp", "repo", graph.KindMethod, "NestedCaller")
	f.node("repo/Service", "repo/lib/Service.cs", "csharp", "repo", graph.KindType, "Service")
	f.edge(nested, "external::Fallback", nested, graph.EdgeImports, 1)

	csharpJob := func(from, file, name, ns, tag, receiver string, line int) *graph.Edge {
		target := f.node("repo/"+name, "repo/lib/Targets.cs", "csharp", "repo", graph.KindMethod, name)
		target.Meta = map[string]any{"scope_ns": ns, "receiver": "Helpers", "static": true}
		old := "unresolved::" + name
		if tag != "using_static" {
			old = "unresolved::*." + name
		}
		if tag == "extension_method" {
			target.Meta["extension"] = true
			target.Meta["this_param_type"] = "External"
		}
		e := f.job(from, target.ID, file, old, graph.EdgeCalls, line)
		e.Meta["resolution"] = tag
		e.Meta["receiver_type"] = receiver
		return e
	}
	global := csharpJob("repo/outer-caller", outer, "Ext", "Lib", "extension_method", "External", 10)
	static := csharpJob("repo/outer-caller", outer, "Static", "Tools", "using_static", "", 11)
	hidden := csharpJob("repo/outer-caller", outer, "Hidden", "Hidden", "extension_method", "External", 12)
	otherUnit := csharpJob("repo/nested-caller", nested, "NestedStatic", "Tools", "using_static", "", 13)
	lone := csharpJob("repo/outer-caller", outer, "Serve", "App.Only", "name", "Service", 14)
	decoy := csharpJob("repo/outer-caller", outer, "Decoy", "Decoy", "name", "Service", 15)
	fallback := csharpJob("repo/nested-caller", nested, "FallbackExt", "Fallback", "extension_method", "External", 16)
	want, reverted, pages := f.native(t)
	got, stats := f.private(t, 1, nil)
	require.Equal(t, want, got)
	require.Equal(t, 3, reverted)
	require.Equal(t, reverted, stats.Reverted)
	require.Equal(t, pages, stats.Pages)
	require.Equal(t, 7, stats.Matched)
	for _, retained := range []*graph.Edge{global, static, lone, fallback} {
		require.Equal(t, retained, guardSavedEdge(t, got, graph.EdgeIdentityFor(retained)))
	}
	for _, dropped := range []*graph.Edge{hidden, otherUnit, decoy} {
		var oldTo string
		for _, m := range f.moves {
			if m.Edge == dropped {
				oldTo = m.OldTo
			}
		}
		id := graph.EdgeIdentityFor(dropped)
		id.To = oldTo
		require.Equal(t, true, guardSavedEdge(t, got, id).Meta["guard_reverted"])
	}
}
