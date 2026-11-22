package resolver

import (
	"errors"
	"maps"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func rebuildExternTestNode(id, name, file, repo, workspace, lang string, kind graph.NodeKind) *graph.Node {
	return &graph.Node{ID: id, Name: name, QualName: id, FilePath: file, RepoPrefix: repo, WorkspaceID: workspace, Language: lang, Kind: kind}
}

func rebuildExternTestEdge(from, file, spec string, line int) *graph.Edge {
	e := &graph.Edge{From: from, To: "unresolved::extern::" + spec, Kind: graph.EdgeCalls,
		FilePath: file, Line: line, Confidence: 0.625, ConfidenceLabel: "medium", Origin: graph.OriginASTInferred,
		Meta: map[string]any{"payload": "kept"}}
	setEdgeTerminal(e, terminalReasonNoDefinition)
	return e
}

// Given competing indexed definitions, missing sources and literal terminal
// spellings, compare each full saved edge to the configured native reducer.
func TestRebuildExternNativeRows(t *testing.T) {
	const c, vue, py, goSrc = "repo/main.cpp", "repo/main.vue", "repo/main.pyi", "repo/main.go"
	n := rebuildExternTestNode
	nodes := []*graph.Node{
		n(c, "caller", c, "repo", "app", " C++ ", graph.KindFunction),
		n(vue, "caller", vue, "repo", "app", "vue", graph.KindFunction),
		n(py, "caller", py, "repo", "app", "python", graph.KindFunction),
		n(goSrc, "caller", goSrc, "repo", "app", "go", graph.KindFunction),
		n("aa/wrong", "Emit", "aa/wrong/pkg/a.cpp", "aa", "other", "c", graph.KindFunction), // imprecise foreign path
		n("repo/0", "Emit", "repo/pkg/a.rs", "repo", "app", "rust", graph.KindFunction),
		n("repo/1", "Emit", "repo/pkg/a.c", "repo", "app", "c", graph.KindVariable),
		n("repo/2", "Emit", "repo/pkg/a.cpp", "repo", "app", "cpp", graph.KindFunction),
		n("repo/3", "Emit", "repo/pkg/b.c", "repo", "app", "c", graph.KindInterface),
		n("repo/4", "Emit", "repo/pkg/c.c", "repo", "app", "c", graph.KindMethod),
		n("aa/z", "Cross", "aa/sdk/pkg/a.c", "aa", "app", "c", graph.KindMethod),
		n("repo/cross", "Cross", "repo/pkg/a.cpp", "repo", "app", "cpp", graph.KindFunction),
		n("repo/a", "Render", "repo/pkg/a.ts", "repo", "app", "typescript", graph.KindType),
		n("repo/z", "Render", "repo/pkg/b.js", "repo", "app", "javascript", graph.KindFunction),
		n("repo/fn", "Keep", "repo/pkg/a.dat", "repo", "app", "", graph.KindFunction),
		n("aa/fn", "Keep", "aa/sdk/pkg/a.go", "aa", "app", "go", graph.KindFunction),
		n("aa/fallback", "KeepRemote", "aa/sdk/pkg/a.go", "aa", "app", "go", graph.KindFunction),
		n("aa/p1", "Call", "aa/pkg/api.py", "aa", "other", "python", graph.KindFunction),
		n("ab/p2", "Call", "ab/pkg/api.py", "ab", "other", "python", graph.KindFunction),
		n("repo/p1", "Call", "repo/pkg/api.pyi", "repo", "app", "python", graph.KindFunction),
		n("repo/p2", "Alias", "repo/pkg/thing.py", "repo", "app", "python", graph.KindFunction),
		n("repo/p3", "Alias", "repo/pkg/thing/__init__.py", "repo", "app", "python", graph.KindType),
		n("repo/p4", "thing", "repo/pkg/__init__.pyi", "repo", "app", "python", graph.KindType),
		n("repo/p5", "thing", "repo/pkg/__init__.py", "repo", "app", "python", graph.KindMethod),
		n("aa/p4", "Foreign", "aa/only.py", "aa", "other", "python", graph.KindFunction),
		n("no-repo::stdlib::prior::Target", "present", "", "", "", "", graph.KindFunction),
		n("dep::example.test/sdk", "dep::example.test/sdk", "repo/go.mod", "repo", "", "contract", graph.KindContract),
	}
	late := n("repo/late", "Late", "repo/pkg/late.c", "repo", "app", "c", graph.KindFunction)
	var input, want []*graph.Edge
	for i, tc := range []struct {
		from, file, spec, to string
		inCross, outCross    bool
	}{
		{c, c, "odd/pkg::Emit", "repo/3", true, true},
		{c, c, "example.test/aa/sdk/pkg::Cross", "aa/z", false, true},
		{c, c, "pkg::Late", late.ID, false, false},
		{vue, vue, "pkg::Render", "repo/a", false, false},
		{goSrc, "repo/source.txt", "example.test/aa/sdk/pkg::Keep", "repo/fn", true, false},
		{goSrc, "repo/source.txt", "example.test/aa/sdk/pkg::KeepRemote", "aa/fallback", false, true},
		{py, py, "pkg.api::Call", "repo/p1", true, true},
		{py, py, "pkg.thing::Alias", "repo/p4", false, false},
		{py, py, "only::Foreign", "aa/p4", false, true},
		{c, c, "example.test/pkg::Missing", "dep::example.test/pkg::Missing", true, true},
		{c, c, "a::b::Missing", "repo::stdlib::a::b::Missing", false, false},
		{c, c, "pkg::", "repo::stdlib::pkg::", false, false},
		{"repo/unresolved::source", c, "lib::Missing", "repo::stdlib::lib::Missing", false, false},
		{"no-repo::stdlib::prior::Target", c, "lib::Missing", "stdlib::lib::Missing", false, false},
		{"dep::example.test/sdk", c, "lib::Missing", "repo::stdlib::lib::Missing", false, false},
		{goSrc, goSrc, "malformed", "external::malformed", true, true},
		{goSrc, goSrc, "::", "dep::::", true, true},
	} {
		e := rebuildExternTestEdge(tc.from, tc.file, tc.spec, i+1)
		e.CrossRepo = tc.inCross
		if tc.spec == "odd/pkg::Emit" {
			e.Kind = graph.EdgeExtends // extern outranks type dispatch; not a kind promotion
		}
		input = append(input, e)
		saved := rebuildImportTestMove(e, tc.to, false)
		saved.CrossRepo = tc.outCross
		want = append(want, saved)
	}
	want = rebuildVueSortedRows(want)
	native := rebuildImportNativeRows(t, append(nodes, late), input, func(r *Resolver) {
		r.SetDeferGoModuleMisses(true)
		r.SetGoModules(map[string]string{"repo": "example.test/app", "aa": "example.test/sdk"})
		r.SetCrossWorkspaceDepLookup(func(string) []CrossWorkspaceDepRule { return nil })
	})
	require.Equal(t, want, native)

	// SQLite drops these struct-only fields; Pebble must keep them too.
	for _, e := range append(input, want...) {
		e.Context, e.Alias, e.Via = "argument", "local", "source"
	}
	ctx, b, opts := frontierFixture(t, 1, 0)
	b.AddBatch(nodes, input)
	require.NoError(t, runRebuildLinkFrontier(ctx, b, opts, nil, func(f *rebuildLinkFrontier, page []*graph.Edge) error {
		b.AddNode(late) // not in the immutable candidate partitions
		moves, err := reduceRebuildMainPage(ctx, f, page)
		if err != nil {
			return err
		}
		require.Len(t, moves, len(input))
		return b.ApplyLinkReindexes(ctx, moves)
	}))
	require.Equal(t, want, rebuildFnPointerSaved(t, b))
	checkFrontierScratch(t, opts.ScratchParent)
}

// Compare full rows with the native bulk reducer, configured as an
// owner-backed checkout with no tracked module identities.
func TestRebuildGoExternEmptyTrackedNativeRows(t *testing.T) {
	const caller, tsCaller, repo = "repo/main.go", "repo/view.ts", "repo"
	const pkg = "example.test/sdk/pkg"
	n := rebuildExternTestNode
	nodes := []*graph.Node{
		n(caller, "caller", caller, repo, "app", "go", graph.KindFunction),
		n(tsCaller, "callerTS", tsCaller, repo, "app", "typescript", graph.KindFunction),
		n("repo/0-different", "Bind", "repo/other/"+pkg+"/wrong.go", repo, "app", "go", graph.KindFunction),
		n("repo/1-loose", "Bind", "repo/loose/pkg/loose.go", repo, "app", "go", graph.KindFunction),
		n("repo/2-precise", "Bind", "repo/"+pkg+"/first.go", repo, "app", "go", graph.KindMethod),
		n("repo/3-precise", "Bind", "repo/"+pkg+"/last.go", repo, "app", "go", graph.KindType),
		n("repo/0-wrong-loose", "Loose", "repo/wrong/pkg/a.go", repo, "app", "go", graph.KindFunction),
		n("repo/4-unknown", "Loose", "repo/other/pkg/b.go", repo, "app", "go", graph.KindInterface),
		n("repo/z-blank", "Lang", "repo/"+pkg+"/blank.go", repo, "app", "", graph.KindFunction),
		n("repo/a-go", "Lang", "repo/"+pkg+"/typed.go", repo, "app", "go", graph.KindFunction),
		n("repo/ts", "Bind", "repo/"+pkg+"/only.ts", repo, "app", "typescript", graph.KindFunction),
	}
	slices.Reverse(nodes)
	owner := func(q GoImportCandidate) GoPackageOwnershipResult {
		require.Equal(t, pkg, q.ImportPath)
		require.Equal(t, caller, q.ImporterFilePath)
		require.Equal(t, repo, q.ImporterRepoPrefix)
		require.NotNil(t, q.CandidateNode)
		require.Equal(t, q.CandidateNode.ID, q.CandidateID)
		require.Equal(t, q.CandidateNode.FilePath, q.CandidateFilePath)
		require.Equal(t, q.CandidateNode.RepoPrefix, q.CandidateRepoPrefix)
		switch q.CandidateID {
		case "repo/0-different", "repo/0-wrong-loose":
			return GoPackageOwnershipDifferent
		case "repo/2-precise", "repo/3-precise":
			return GoPackageOwnershipExact
		default:
			return GoPackageOwnershipUnknown
		}
	}
	var input, want, nativeInput []*graph.Edge
	for i, tc := range []struct {
		from, file, spec, to       string
		precise, keepCross, restub bool
	}{
		{caller, caller, pkg + "::Bind", "repo/2-precise", true, false, false},
		{caller, caller, pkg + "::Loose", "repo/4-unknown", false, false, false},
		{caller, caller, pkg + "::Lang", "repo/z-blank", true, false, true},
		{caller, "repo/site.txt", pkg + "::Bind", "repo/1-loose", false, false, false},
		{tsCaller, caller, pkg + "::Bind", "repo/ts", false, true, false},
		{caller, caller, pkg + "::Missing", "dep::" + pkg + "::Missing", false, true, false},
		{caller, caller, "sdk/sub::Missing", "repo::stdlib::sdk/sub::Missing", false, true, false},
	} {
		e := rebuildExternTestEdge(tc.from, tc.file, tc.spec, i+1)
		e.Alias, e.Context, e.Via, e.CrossRepo = "local", "argument", "source", true
		if tc.restub {
			stub := e.To
			e.To, e.Origin, e.Tier, e.Confidence = tc.to, graph.OriginASTResolved, "verified", 0.8
			graph.StashRestubProvenance(e)
			e.To = stub
		}
		input = append(input, e)
		saved := rebuildImportTestMove(e, tc.to, false)
		saved.CrossRepo = tc.keepCross
		if tc.precise {
			saved.Confidence, saved.ConfidenceLabel, saved.Origin = 0, "", ""
			saved.Meta["go_import"] = tc.spec
			if tc.restub {
				require.True(t, graph.RestoreRestubProvenance(saved))
				require.Equal(t, graph.OriginASTResolved, saved.Origin)
				require.Equal(t, "verified", saved.Tier)
				require.Equal(t, 0.8, saved.Confidence)
			}
		}
		want = append(want, saved)
		copy := *e
		copy.Meta = maps.Clone(e.Meta)
		nativeInput = append(nativeInput, &copy)
	}
	want = rebuildVueSortedRows(want)
	g := graph.New()
	g.AddBatch(nodes, nativeInput)
	r := New(g)
	r.bulkMode = true
	r.SetDeferGoModuleMisses(true)
	r.SetGoModuleReplacementLookup(func() map[string][]GoModuleReplacement {
		return map[string][]GoModuleReplacement{repo: {{Module: "example.test/sdk"}}}
	})
	r.refreshGoModules()
	r.SetGoPackageOwnership(owner)
	r.buildDirIndexes()
	require.NoError(t, r.warmLookupCache(nativeInput))
	defer r.clearLookupCache()
	var moves []graph.EdgeReindex
	var stats ResolveStats
	for _, old := range nativeInput {
		copy := *old
		copy.Meta = maps.Clone(old.Meta)
		oldTo, changed := r.resolveEdge(&copy, &stats)
		require.True(t, changed)
		clearEdgeTerminal(&copy)
		moves = append(moves, graph.EdgeReindex{Edge: &copy, OldTo: oldTo, OldKind: old.Kind})
	}
	g.ReindexEdges(moves)
	require.EqualValues(t, 5, stats.Resolved)
	require.EqualValues(t, 2, stats.External)
	require.EqualValues(t, 0, stats.Unresolved)
	require.Equal(t, want, rebuildVueSortedRows(g.AllEdges()))
	externalIDs := []string{"dep::" + pkg + "::Missing", "repo::stdlib::sdk/sub::Missing"}
	for _, id := range externalIDs {
		require.Nil(t, g.GetNode(id)) // the main resolver does not materialize terminals
	}
	r.attributeGoExternalCalls()
	wantAfterAttribution := rebuildVueSortedRows(g.AllEdges())
	require.Len(t, wantAfterAttribution, len(want)+len(externalIDs))

	ctx, b, opts := frontierFixture(t, 1, 0)
	b.AddBatch(nodes, input)
	checks := 0
	require.NoError(t, runRebuildMain(ctx, b, opts, false, RebuildMainInputs{
		CheckGoExtern: func(path string) error {
			require.Contains(t, []string{pkg, "sdk/sub"}, path)
			checks++
			return nil
		},
		GoOwnership: func(q GoImportCandidate) (GoPackageOwnershipResult, error) {
			return owner(q), nil
		},
	}))
	require.Equal(t, 6, checks) // Edge file suffix, not source language.
	require.Equal(t, wantAfterAttribution, rebuildFnPointerSaved(t, b))
	for _, id := range externalIDs {
		saved, err := b.RebuildView().Node(ctx, id)
		require.NoError(t, err)
		require.NotNil(t, saved)
		require.Equal(t, g.GetNode(id), saved)
	}
	checkFrontierScratch(t, opts.ScratchParent)
}

func TestRebuildGoExternUnprovedOwnershipRefusesPage(t *testing.T) {
	const file = "main.go"
	const pkg = "example.test/pkg"
	n := rebuildExternTestNode
	ctx, b, opts := frontierFixture(t, 1, 0)
	first := rebuildExternTestEdge(file, file, "a-malformed", 1)
	blocked := rebuildExternTestEdge(file, file, pkg+"::Bind", 2)
	before := rebuildVueSortedRows([]*graph.Edge{first, blocked})
	b.AddBatch([]*graph.Node{
		n(file, "caller", file, "", "", "go", graph.KindFunction),
		n("bind", "Bind", pkg+"/bind.go", "", "", "go", graph.KindFunction),
	}, before)
	unproved := errors.New("unproved candidate package")
	calls := 0
	err := runRebuildMain(ctx, b, opts, false, RebuildMainInputs{
		CheckGoExtern: func(string) error { return nil },
		GoOwnership: func(q GoImportCandidate) (GoPackageOwnershipResult, error) {
			calls++
			require.Equal(t, "bind", q.CandidateID)
			return GoPackageOwnershipUnknown, unproved
		},
	})
	require.ErrorIs(t, err, unproved)
	require.Equal(t, 1, calls)
	require.Equal(t, before, rebuildFnPointerSaved(t, b))
	checkFrontierScratch(t, opts.ScratchParent)
}

// Given an earlier hit and a later decision that requires absent cold input,
// refuse the whole page without mutating any saved row or leaving scratch.
func TestRebuildExternRefusesIncompletePage(t *testing.T) {
	const caller, goSrc = "repo/main.c", "repo/main.go"
	n := rebuildExternTestNode
	for _, tc := range []struct {
		name, from, file, spec, missing, nativeTo string
		node                                      *graph.Node
		configure                                 func(*Resolver)
	}{
		{
			name: "GoModuleDeferral", from: goSrc, file: goSrc, spec: "example.test/pkg::Remote",
			missing: "Go module identities/replacements, package ownership and cold deferral", nativeTo: "unresolved::extern::example.test/pkg::Remote",
			configure: func(r *Resolver) {
				r.SetGoModules(map[string]string{"other": "example.test/pkg"})
				r.SetDeferGoModuleMisses(true)
			},
		},
		{
			name: "GoModulePrecedesDirectory", from: goSrc, file: goSrc, spec: "example.test/app/pkg::Remote",
			missing: "Go module identities/replacements, package ownership and cold deferral", nativeTo: "repo/remote",
			node: n("repo/remote", "Remote", "repo/pkg/remote.go", "repo", "app", "go", graph.KindFunction),
			configure: func(r *Resolver) {
				r.SetGoModules(map[string]string{"repo": "example.test/app"})
				r.SetDeferGoModuleMisses(true)
			},
		},
		{
			name: "GoPackageOwnership", from: goSrc, file: "repo/source.txt", spec: "pkg::Remote",
			missing: "Go package ownership", nativeTo: "repo::stdlib::pkg::Remote",
			node: n("repo/remote", "Remote", "repo/pkg/remote.go", "repo", "app", "go", graph.KindFunction),
			configure: func(r *Resolver) {
				r.SetGoPackageOwnership(func(GoImportCandidate) GoPackageOwnershipResult { return GoPackageOwnershipDifferent })
			},
		},
		{
			name: "CrossWorkspaceEligibility", from: caller, file: caller, spec: "other/pkg::Remote",
			missing: "cross-workspace dependency eligibility", nativeTo: "repo::stdlib::other/pkg::Remote",
			node: n("other/remote", "Remote", "other/pkg/remote.c", "other", "other-app", "c", graph.KindFunction),
			configure: func(r *Resolver) {
				r.SetCrossWorkspaceDepLookup(func(string) []CrossWorkspaceDepRule { return nil })
			},
		},
		{
			name: "MissingContractSource", from: "dep::example.test/sdk", file: "repo/file.txt", spec: "fmt::Missing",
			missing: "transient dependency-contract source identity", nativeTo: "repo::stdlib::fmt::Missing",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, b, opts := frontierFixture(t, 1, 0)
			nodes := []*graph.Node{
				n("a/start", "start", "a/main.c", "repo", "app", "c", graph.KindFunction),
				n("repo/keep", "Keep", "repo/pkg/keep.c", "repo", "app", "c", graph.KindFunction),
				n(caller, "caller", caller, "repo", "app", "c", graph.KindFunction),
				n(goSrc, "caller", goSrc, "repo", "app", "go", graph.KindFunction),
			}
			if tc.node != nil {
				nodes = append(nodes, tc.node)
			}
			first := rebuildExternTestEdge("a/start", "a/main.c", "pkg::Keep", 1)
			blocked := rebuildExternTestEdge(tc.from, tc.file, tc.spec, 2)
			before := rebuildVueSortedRows([]*graph.Edge{first, blocked})
			b.AddBatch(nodes, before)
			if tc.name == "MissingContractSource" {
				// Native seeds dependency-contract nodes before the main resolver.
				nodes = append(nodes, n(tc.from, tc.from, "repo/go.mod", "repo", "", "contract", graph.KindContract))
			}
			wantBlocked := blocked
			if tc.nativeTo != blocked.To {
				wantBlocked = rebuildImportTestMove(blocked, tc.nativeTo, false)
				if tc.name == "GoModulePrecedesDirectory" {
					wantBlocked.Confidence, wantBlocked.ConfidenceLabel, wantBlocked.Origin = 0, "", ""
				}
			}
			wantFirst := rebuildImportTestMove(first, "repo/keep", false)
			require.Equal(t, rebuildVueSortedRows([]*graph.Edge{wantFirst, wantBlocked}), rebuildImportNativeRows(t, nodes, before, tc.configure))
			pages := 0
			err := runRebuildLinkFrontier(ctx, b, opts, nil, func(f *rebuildLinkFrontier, page []*graph.Edge) error {
				pages++
				require.Equal(t, before, page)
				firstMove, err := reduceRebuildMainPage(ctx, f, page[:1])
				require.NoError(t, err)
				require.Len(t, firstMove, 1)
				require.Equal(t, wantFirst, firstMove[0].Edge)
				moves, err := reduceRebuildMainPage(ctx, f, page)
				require.Nil(t, moves)
				require.Equal(t, before, page)
				return err
			})
			require.Equal(t, 1, pages)
			require.ErrorIs(t, err, errRebuildMainRule)
			require.ErrorContains(t, err, tc.missing)
			require.Equal(t, before, rebuildFnPointerSaved(t, b))
			checkFrontierScratch(t, opts.ScratchParent)
		})
	}
}
