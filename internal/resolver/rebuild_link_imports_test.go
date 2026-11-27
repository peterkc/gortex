package resolver

import (
	"maps"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func rebuildImportTestNode(id, repo, workspace, qual, lang string, kind graph.NodeKind) *graph.Node {
	return &graph.Node{ID: id, RepoPrefix: repo, WorkspaceID: workspace, QualName: qual,
		Kind: kind, Language: lang, FilePath: id, Name: filepath.Base(id)}
}

func rebuildImportTestEdge(from, file, spec string, line int) *graph.Edge {
	e := &graph.Edge{From: from, To: "unresolved::import::" + spec, Kind: graph.EdgeImports,
		FilePath: file, Line: line, Confidence: 0.625, ConfidenceLabel: "medium", Origin: graph.OriginASTInferred,
		Meta: map[string]any{"payload": "kept"}}
	setEdgeTerminal(e, terminalReasonNoDefinition)
	return e
}

func rebuildImportTestMove(old *graph.Edge, to string, crossRepo bool) *graph.Edge {
	e := *old
	e.To, e.CrossRepo = to, crossRepo || e.CrossRepo
	e.Meta = maps.Clone(e.Meta)
	clearEdgeTerminal(&e)
	return &e
}

func rebuildImportNativeRows(t *testing.T, nodes []*graph.Node, edges []*graph.Edge, configure func(*Resolver)) []*graph.Edge {
	t.Helper()
	sql, err := store_sqlite.Open(filepath.Join(t.TempDir(), "native.sqlite"))
	require.NoError(t, err)
	defer func() { require.NoError(t, sql.Close()) }()
	require.NoError(t, sql.AddBatchChecked(nodes, edges))
	r := New(sql)
	r.bulkMode = true
	if configure != nil {
		configure(r)
	}
	r.buildDirIndexes()
	r.buildDepModuleIndex()
	var page []*graph.Edge
	for _, e := range rebuildVueSortedRows(sql.AllEdges()) {
		if graph.IsUnresolvedTarget(e.To) {
			page = append(page, e)
		}
	}
	require.NoError(t, r.warmLookupCache(page))
	defer r.clearLookupCache()
	var moves []graph.EdgeReindex
	for _, old := range page {
		e := *old
		e.Meta = maps.Clone(old.Meta)
		if oldTo, changed := r.resolveEdge(&e, &ResolveStats{}); changed {
			if !graph.IsUnresolvedTarget(e.To) {
				clearEdgeTerminal(&e)
			}
			moves = append(moves, graph.EdgeReindex{Edge: &e, OldTo: oldTo, OldKind: old.Kind})
		}
	}
	if len(moves) != 0 {
		require.True(t, sql.ReindexEdgesApplied(moves))
	}
	return rebuildVueSortedRows(sql.AllEdges())
}

func TestRebuildImportNativeRows(t *testing.T) {
	const js = "repo/ui/main.vue"
	const other = "repo/main.rs"
	nodes := []*graph.Node{
		rebuildImportTestNode(js, "repo", "app", "", "vue", graph.KindFile),
		rebuildImportTestNode(other, "repo", "app", "", "rust", graph.KindFile),
	}
	for _, id := range []string{"mod.ts", "mod.js", "emit.mts", "barrel/index.ts", "Component.vue", "wrong.ts"} {
		nodes = append(nodes, rebuildImportTestNode("repo/ui/"+id, "repo", "app", "", "typescript", graph.KindFile))
	}
	nodes = append(nodes,
		rebuildImportTestNode("repo/ui/emit.mts::Run", "repo", "app", "", "typescript", graph.KindFunction),
		rebuildImportTestNode("repo/scoped", "repo", "app", "./not-a-file::Named", "typescript", graph.KindFunction),
		rebuildImportTestNode("repo/a", "repo", "other", "scope/package", "rust", graph.KindPackage),
		rebuildImportTestNode("repo/b", "repo", "app", "scope/package", "rust", graph.KindPackage),
		rebuildImportTestNode("repo/z", "repo", "app", "scope/package", "rust", graph.KindPackage),
		rebuildImportTestNode("foreign/a", "foreign", "app", "scope/package", "rust", graph.KindPackage),
		rebuildImportTestNode("foreign/b", "foreign", "app", "foreign-only", "rust", graph.KindPackage),
		rebuildImportTestNode(externalCallPrefix+"npm:foreign-only", "repo", "app", "foreign-only", "typescript", graph.KindPackage),
		rebuildImportTestNode("repo/local", "repo", "other", "local-only", "rust", graph.KindPackage),
		rebuildImportTestNode("root/pkg", "", "", "no-source", "rust", graph.KindPackage),
		rebuildImportTestNode("repo/foreign", "repo", "app", "no-source", "rust", graph.KindPackage),
	)
	var input, want []*graph.Edge
	for i, tc := range []struct {
		from, file, spec, to string
		crossRepo            bool
	}{
		{js, js, "./mod.js", "repo/ui/mod.js", false},
		{js, "", "./emit.mjs::Run", "repo/ui/emit.mts::Run", false},
		{js, js, "./barrel::Missing", "repo/ui/barrel/index.ts", false},
		{js, js, "./Component.vue", "repo/ui/Component.vue", false},
		{js, js, "./not-a-file::Named", "repo/scoped", false},
		{other, other, "scope/package", "repo/b", false},
		{other, other, "foreign-only", "foreign/b", true},
		{other, other, "local-only", "repo/local", false},
		{"missing", "repo/missing.rs", "no-source", "root/pkg", false},
	} {
		e := rebuildImportTestEdge(tc.from, tc.file, tc.spec, i+1)
		if tc.spec == "local-only" {
			e.CrossRepo = true // the native early branches do not clear this flag
		}
		input = append(input, e)
		want = append(want, rebuildImportTestMove(e, tc.to, tc.crossRepo))
	}
	for _, e := range []*graph.Edge{
		rebuildImportTestEdge(other, "repo/main.lua", "scope/package", 20),
		rebuildImportTestEdge(other, "repo/main.tscn", "res://Component.tscn", 21),
	} {
		input, want = append(input, e), append(want, e)
	}
	want = rebuildVueSortedRows(want)
	gotNative := rebuildImportNativeRows(t, nodes, input, func(r *Resolver) {
		r.SetPathAliasResolver(func(_, _ string) string { return "repo/ui/wrong" })
		r.SetNpmAliasResolver(func(file, spec string) string {
			if file == js && spec == "./mod.js" {
				return "./wrong"
			}
			return ""
		})
	})
	require.Equal(t, want, gotNative)
	ctx, b, opts := frontierFixture(t, 0)
	// SQLite does not save these struct-only fields (scanEdgeWithMeta).
	// The new writer does; assert their exact retained payload separately.
	for _, rows := range [][]*graph.Edge{input, want} {
		for _, e := range rows {
			if e.Line == 1 {
				e.Context, e.Alias, e.Via = "import binding", "LocalName", "fixture"
			}
		}
	}
	b.AddBatch(nodes, input)
	require.NoError(t, runRebuildMain(ctx, b, opts, false, RebuildMainInputs{}))
	require.Equal(t, want, rebuildFnPointerSaved(t, b))
}

func TestRebuildImportWithCheckedJSTSInputs(t *testing.T) {
	const caller = "repo/app/main.ts"
	const alias = "repo/app/local.ts"
	const foreign = "provider/package"
	nodes := []*graph.Node{
		rebuildImportTestNode(caller, "repo", "repo", "", "typescript", graph.KindFile),
		rebuildImportTestNode(alias, "repo", "repo", "", "typescript", graph.KindFile),
		rebuildImportTestNode(foreign, "provider", "provider", "rewritten::Item", "javascript", graph.KindPackage),
	}
	edges := []*graph.Edge{
		rebuildImportTestEdge(caller, caller, "local", 1),
		rebuildImportTestEdge(caller, caller, "package::Item", 2),
	}
	pathAlias := func(file, module string) string {
		require.Equal(t, caller, file)
		if module == "local" {
			return "repo/app/local"
		}
		return ""
	}
	rewrite := func(file, spec string) string {
		require.Equal(t, caller, file)
		require.Equal(t, "package::Item", spec) // not reached for the first hit
		return "rewritten::Item"
	}
	want := rebuildVueSortedRows([]*graph.Edge{
		rebuildImportTestMove(edges[0], alias, false),
		rebuildImportTestMove(edges[1], foreign, true),
	})
	require.Equal(t, want, rebuildImportNativeRows(t, nodes, edges, func(r *Resolver) {
		r.SetPathAliasResolver(pathAlias)
		r.SetNpmAliasResolver(rewrite)
	}))
	ctx, b, opts := frontierFixture(t, 0)
	b.AddBatch(nodes, edges)
	require.NoError(t, runRebuildMain(ctx, b, opts, false, RebuildMainInputs{
		PathAlias: func(file, module string) (string, error) { return pathAlias(file, module), nil },
		RewriteNpm: func(file, spec string) (string, error) {
			return rewrite(file, spec), nil
		},
	}))
	require.Equal(t, want, rebuildFnPointerSaved(t, b))
}

func TestRebuildImportWithCheckedGoOwnership(t *testing.T) {
	const caller = "repo/app/main.go"
	const spec = "example.test/pkg"
	wrong := rebuildImportTestNode("repo/a-wrong", "repo", "repo", spec, "go", graph.KindPackage)
	right := rebuildImportTestNode("repo/z-right", "repo", "repo", spec, "go", graph.KindPackage)
	nodes := []*graph.Node{
		rebuildImportTestNode(caller, "repo", "repo", "", "go", graph.KindFile), wrong, right,
	}
	edge := rebuildImportTestEdge(caller, caller, spec, 1)
	owner := func(q GoImportCandidate) GoPackageOwnershipResult {
		require.Equal(t, spec, q.ImportPath)
		require.Equal(t, caller, q.ImporterFilePath)
		require.Equal(t, "repo", q.ImporterRepoPrefix)
		require.Equal(t, q.CandidateID, q.CandidateNode.ID)
		require.Equal(t, q.CandidateFilePath, q.CandidateNode.FilePath)
		if q.CandidateID == wrong.ID {
			return GoPackageOwnershipDifferent
		}
		return GoPackageOwnershipExact
	}
	want := []*graph.Edge{rebuildImportTestMove(edge, right.ID, false)}
	require.Equal(t, want, rebuildImportNativeRows(t, nodes, []*graph.Edge{edge}, func(r *Resolver) {
		r.SetGoPackageOwnership(owner)
	}))
	ctx, b, opts := frontierFixture(t, 0)
	b.AddBatch(nodes, []*graph.Edge{edge})
	in := RebuildMainInputs{GoOwnership: func(q GoImportCandidate) (GoPackageOwnershipResult, error) {
		if q.CandidateID == right.ID {
			return GoPackageOwnershipUnknown, errRebuildMainRule
		}
		return owner(q), nil
	}}
	require.ErrorIs(t, runRebuildMain(ctx, b, opts, false, in), errRebuildMainRule)
	require.Equal(t, []*graph.Edge{edge}, rebuildFnPointerSaved(t, b))
	in.GoOwnership = func(q GoImportCandidate) (GoPackageOwnershipResult, error) { return owner(q), nil }
	require.NoError(t, runRebuildMain(ctx, b, opts, false, in))
	require.Equal(t, want, rebuildFnPointerSaved(t, b))
}

func TestRebuildImportRefusesUnprovedInputs(t *testing.T) {
	const js = "repo/app/main.ts"
	const goFile = "repo/app/main.go"
	const rs = "repo/app/main.rs"
	const php = "repo/app/main.php"
	qualified := func(id, spec, lang string) *graph.Node {
		return rebuildImportTestNode(id, "repo", "repo", spec, lang, graph.KindPackage)
	}
	file := func(id, lang string) *graph.Node {
		return rebuildImportTestNode(id, "repo", "repo", "", lang, graph.KindFile)
	}
	for _, tc := range []struct {
		name, caller, lang, spec, missing, nativeTo string
		nodes                                       []*graph.Node
		configure                                   func(*Resolver)
	}{
		{
			name: "TSAliasBeforeQualified", caller: js, lang: "typescript", spec: "pkg",
			missing: "TS path-alias/baseUrl", nativeTo: "repo/app/alias.ts",
			nodes:     []*graph.Node{qualified("repo/pkg", "pkg", "typescript"), file("repo/app/alias.ts", "typescript")},
			configure: func(r *Resolver) { r.SetPathAliasResolver(func(_, _ string) string { return "repo/app/alias" }) },
		},
		{
			name: "NpmVerbatimPayload", caller: js, lang: "typescript", spec: "::Named",
			missing: "npm alias/workspace/exports", nativeTo: "repo/real-pkg",
			nodes: []*graph.Node{qualified("repo/real-pkg", "real-pkg", "typescript")},
			configure: func(r *Resolver) {
				r.SetNpmAliasResolver(func(_, raw string) string {
					if raw == "::Named" {
						return "real-pkg"
					}
					return ""
				})
			},
		},
		{
			name: "PHPWarmFQN", caller: php, lang: "php", spec: `App\Thing`,
			missing: "native PHP FQN warm candidate group", nativeTo: "repo/php-fallback",
			nodes: []*graph.Node{file(rs, "rust"), qualified("repo/php-fallback", `App\Thing`, "php"),
				{ID: "repo/class", RepoPrefix: "repo", Kind: graph.KindType, Language: "php",
					Name: "Thing", FilePath: "repo/Thing.php", Meta: map[string]any{MetaScopeNamespace: "App"}}},
		},
		{
			name: "GoOwnerRejectsQualified", caller: goFile, lang: "go", spec: "example.test/pkg",
			missing: "Go package ownership", nativeTo: "external::example.test/pkg",
			nodes: []*graph.Node{qualified("repo/wrong", "example.test/pkg", "go")},
			configure: func(r *Resolver) {
				r.SetGoPackageOwnership(func(q GoImportCandidate) GoPackageOwnershipResult {
					if q.CandidateID == "repo/wrong" {
						return GoPackageOwnershipDifferent
					}
					return GoPackageOwnershipUnknown
				})
			},
		},
		{
			name: "WorkspaceBreaksDirectoryTie", caller: rs, lang: "rust", spec: "pkg",
			missing: "native file-directory buckets and workspace membership", nativeTo: "repo/app/pkg/b.rs",
			nodes: []*graph.Node{file("repo/aaa/pkg/a.rs", "rust"), file("repo/app/pkg/b.rs", "rust")},
			configure: func(r *Resolver) {
				r.SetWorkspaceMembership(func(file string) string {
					if strings.HasPrefix(file, "repo/app/") {
						return "repo/app"
					}
					return "repo/aaa"
				})
			},
		},
		{
			name: "CompleteNativeMiss", caller: js, lang: "typescript", spec: "./missing",
			missing: "native file-directory buckets", nativeTo: "external::./missing",
		},
		{
			name: "QualifiedForeignAmbiguity", caller: rs, lang: "rust", spec: "pkg",
			missing: "native file-directory buckets", nativeTo: "unresolved::import::pkg",
			nodes: []*graph.Node{
				rebuildImportTestNode("foreign/a", "foreign", "repo", "pkg", "rust", graph.KindPackage),
				rebuildImportTestNode("foreign/b", "foreign", "repo", "pkg", "rust", graph.KindPackage),
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, b, opts := frontierFixture(t, 0)
			nodes := append(tc.nodes, file(tc.caller, tc.lang), file("a/start.ts", "typescript"), file("a/keep.ts", "typescript"))
			first := rebuildImportTestEdge("a/start.ts", "a/start.ts", "./keep", 1)
			b.AddBatch(nodes, []*graph.Edge{first})
			require.NoError(t, runRebuildMain(ctx, b, opts, false, RebuildMainInputs{}))
			saved := rebuildImportTestMove(first, "a/keep.ts", false)
			require.Equal(t, []*graph.Edge{saved}, rebuildFnPointerSaved(t, b))

			tentative := rebuildImportTestEdge("a/start.ts", "a/start.ts", "./keep", 2)
			blocked := rebuildImportTestEdge(tc.caller, tc.caller, tc.spec, 3)
			if tc.lang == "php" {
				blocked.Meta["fqn"] = tc.spec
			}
			before := []*graph.Edge{saved, tentative, blocked}
			wantBlocked := blocked
			if tc.nativeTo != blocked.To {
				wantBlocked = rebuildImportTestMove(blocked, tc.nativeTo, false)
			}
			wantNative := []*graph.Edge{saved, rebuildImportTestMove(tentative, "a/keep.ts", false), wantBlocked}
			if tc.lang == "php" {
				// Imports do not warm names themselves. An unrelated Rust
				// name in this page installs a repo-level negative for
				// Thing, so PHP bypasses the otherwise matching declaration.
				withoutWarm := rebuildImportNativeRows(t, nodes, before, nil)
				require.Equal(t, rebuildVueSortedRows([]*graph.Edge{
					saved, wantNative[1], rebuildImportTestMove(blocked, "repo/class", false),
				}), withoutWarm)
				warm := &graph.Edge{From: rs, To: "unresolved::Thing", Kind: graph.EdgeTests, FilePath: rs, Line: 4}
				before, wantNative = append(before, warm), append(wantNative, warm)
				b.AddEdge(warm)
			}
			b.AddBatch(nil, []*graph.Edge{tentative, blocked})
			before = rebuildVueSortedRows(before)
			native := rebuildImportNativeRows(t, nodes, before, tc.configure)
			require.Equal(t, rebuildVueSortedRows(wantNative), native)
			err := runRebuildMain(ctx, b, opts, false, RebuildMainInputs{})
			require.ErrorIs(t, err, errRebuildMainRule)
			require.ErrorContains(t, err, tc.missing)
			require.Equal(t, before, rebuildFnPointerSaved(t, b))
		})
	}
}

// Given a qualified-name miss, the directory rank, bucket order and checked
// workspace result must match the cold resolver on the same saved rows.
func TestRebuildImportFileDirectories(t *testing.T) {
	const caller, spec = "repo/app/main.go", "example.test/pkg"
	for _, tc := range []struct {
		name, spec, want string
		files            []*graph.Node
		owner            func(GoImportCandidate) GoPackageOwnershipResult
		workspace        WorkspaceMembership
		crossRepo        bool
		missingWorkspace bool
	}{
		{
			name: "FolderOverFallback", spec: spec, want: "repo/pkg/folder.go",
			files: []*graph.Node{
				rebuildImportTestNode("repo/aaa/pkg/first.go", "repo", "", "", "go", graph.KindFile),
				rebuildImportTestNode("repo/pkg/different.go", "repo", "", "", "go", graph.KindFile),
				rebuildImportTestNode("repo/pkg/folder.go", "repo", "", "", "go", graph.KindFile),
			},
			owner: func(q GoImportCandidate) GoPackageOwnershipResult {
				if q.CandidateFilePath == "repo/pkg/different.go" {
					return GoPackageOwnershipDifferent
				}
				return GoPackageOwnershipUnknown
			},
		},
		{
			name: "ExactOverFolder", spec: spec, want: "repo/z/pkg/exact.go",
			files: []*graph.Node{
				rebuildImportTestNode("repo/pkg/folder.go", "repo", "", "", "go", graph.KindFile),
				rebuildImportTestNode("repo/z/pkg/exact.go", "repo", "", "", "go", graph.KindFile),
			},
			owner: func(q GoImportCandidate) GoPackageOwnershipResult {
				if q.CandidateFilePath == "repo/z/pkg/exact.go" {
					return GoPackageOwnershipExact
				}
				return GoPackageOwnershipUnknown
			},
		},
		{
			name: "WorkspaceTie", spec: spec, want: "repo/app/pkg/other.go", missingWorkspace: true,
			files: []*graph.Node{
				rebuildImportTestNode("repo/app/pkg/other.go", "repo", "saved-app", "", "go", graph.KindFile),
				rebuildImportTestNode("repo/aaa/pkg/first.go", "repo", "saved-app", "", "go", graph.KindFile),
			},
			workspace: func(file string) string {
				if strings.HasPrefix(file, "repo/app/") {
					return "pkg-manager-app"
				}
				return "pkg-manager-other"
			},
		},
		{
			name: "SamePathIDTie", spec: spec, want: "first-id",
			files: []*graph.Node{
				{ID: "second-id", FilePath: "repo/pkg/shared.go", RepoPrefix: "repo", Language: "go", Kind: graph.KindFile},
				{ID: "first-id", FilePath: "repo/pkg/shared.go", RepoPrefix: "repo", Language: "go", Kind: graph.KindFile},
			},
		},
		{
			name: "ExactBucketBeforeLastAndIDTie", spec: "shared/pkg", want: "z-id-first", crossRepo: true,
			files: []*graph.Node{
				{ID: "a-id-later-bucket", FilePath: "aaa/shared/pkg/first.go", RepoPrefix: "aaa", Language: "go", Kind: graph.KindFile},
				{ID: "b-id-later-path", FilePath: "shared/pkg/z.go", RepoPrefix: "shared", Language: "go", Kind: graph.KindFile},
				{ID: "z-id-second", FilePath: "shared/pkg/a.go", RepoPrefix: "shared", Language: "go", Kind: graph.KindFile},
				{ID: "z-id-first", FilePath: "shared/pkg/a.go", RepoPrefix: "shared", Language: "go", Kind: graph.KindFile},
				{ID: "not-precise", FilePath: "aaa/pkg/a.go", RepoPrefix: "aaa", Language: "go", Kind: graph.KindFile},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nodes := append(tc.files, rebuildImportTestNode(caller, "repo", "saved-app", "", "go", graph.KindFile))
			edge := rebuildImportTestEdge(caller, caller, tc.spec, 11)
			want := []*graph.Edge{rebuildImportTestMove(edge, tc.want, tc.crossRepo)}
			owner := func(q GoImportCandidate) GoPackageOwnershipResult {
				require.Equal(t, tc.spec, q.ImportPath)
				require.Equal(t, caller, q.ImporterFilePath)
				require.Nil(t, q.CandidateNode, "directory ownership receives the compact file identity")
				if tc.owner != nil {
					return tc.owner(q)
				}
				return GoPackageOwnershipUnknown
			}
			workspace := tc.workspace
			if workspace == nil {
				workspace = func(string) string { return "" }
			}
			require.Equal(t, want, rebuildImportNativeRows(t, nodes, []*graph.Edge{edge}, func(r *Resolver) {
				r.SetGoPackageOwnership(owner)
				r.SetWorkspaceMembership(workspace)
			}))
			ctx, b, opts := frontierFixture(t, 0)
			opts.SpillBytes = 1
			b.AddBatch(nodes, []*graph.Edge{edge})
			in := RebuildMainInputs{GoOwnership: func(q GoImportCandidate) (GoPackageOwnershipResult, error) {
				return owner(q), nil
			}}
			if tc.missingWorkspace {
				err := runRebuildMain(ctx, b, opts, false, in)
				require.ErrorIs(t, err, errRebuildMainRule)
				require.ErrorContains(t, err, "workspace membership")
				require.Equal(t, []*graph.Edge{edge}, rebuildFnPointerSaved(t, b))
				checkFrontierScratch(t, opts.ScratchParent)
				in.WorkspaceMembership = func(file string) (string, error) { return workspace(file), nil }
			}
			require.NoError(t, runRebuildMain(ctx, b, opts, false, in))
			require.Equal(t, want, rebuildFnPointerSaved(t, b))
			checkFrontierScratch(t, opts.ScratchParent)
		})
	}
}

// A plausible index.ts is not local evidence until the native external
// dependency query has a checked negative. Neither refusal changes the old key.
func TestRebuildImportFileDirectoryBareJSTS(t *testing.T) {
	const caller, spec = "repo/app/main.ts", "pkg"
	const entry = "repo/lib/pkg/index.ts"
	nodes := []*graph.Node{
		rebuildImportTestNode(caller, "repo", "", "", "typescript", graph.KindFile),
		rebuildImportTestNode("repo/lib/pkg/arbitrary.ts", "repo", "", "", "typescript", graph.KindFile),
		rebuildImportTestNode(entry, "repo", "", "", "typescript", graph.KindFile),
	}
	edge := rebuildImportTestEdge(caller, caller, spec, 3)
	for _, tc := range []struct {
		name, missing          string
		proof, external, ready bool
	}{
		{name: "Unproved", ready: true, missing: "checked external npm dependency result"},
		{name: "External", proof: true, external: true, missing: "native file-directory buckets"},
		{name: "ExternalWithCompleteContracts", proof: true, external: true, ready: true},
		{name: "IndexOnly", proof: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			declaresExternal := func(file, module string) bool {
				require.Equal(t, caller, file)
				require.Equal(t, spec, module)
				return tc.external
			}
			nativeTo := entry
			if tc.external {
				nativeTo = "external::" + spec
			}
			wantNative := []*graph.Edge{rebuildImportTestMove(edge, nativeTo, false)}
			require.Equal(t, wantNative, rebuildImportNativeRows(t, nodes, []*graph.Edge{edge}, func(r *Resolver) {
				r.SetNpmDependencyLookup(declaresExternal)
				r.SetWorkspaceMembership(func(string) string { return "" })
			}))
			ctx, b, opts := frontierFixture(t, 0)
			b.AddBatch(nodes, []*graph.Edge{edge})
			noRewrite := func(_, _ string) (string, error) { return "", nil }
			in := RebuildMainInputs{PathAlias: noRewrite, RewriteNpm: noRewrite, DependencyContractsReady: tc.ready}
			if tc.proof {
				in.ExternalNpmDependency = func(file, module string) (bool, error) { return declaresExternal(file, module), nil }
			}
			err := runRebuildMain(ctx, b, opts, false, in)
			if tc.missing != "" {
				require.ErrorIs(t, err, errRebuildMainRule)
				require.ErrorContains(t, err, tc.missing)
				require.Equal(t, []*graph.Edge{edge}, rebuildFnPointerSaved(t, b))
			} else {
				require.NoError(t, err)
				require.Equal(t, wantNative, rebuildFnPointerSaved(t, b))
				if tc.external {
					n, err := b.RebuildView().Node(ctx, nativeTo)
					require.NoError(t, err)
					require.Nil(t, n, "terminal import must not create a node")
				}
			}
			checkFrontierScratch(t, opts.ScratchParent)
		})
	}
}

func TestRebuildImportDirectoryFrontierBorrow(t *testing.T) {
	ctx, b, opts := frontierFixture(t, resolvePendingPageRows+1)
	const file = "repo/pkg/file.go"
	b.AddNode(rebuildImportTestNode(file, "repo", "", "", "go", graph.KindFile))
	var first *store_pebble.RebuildImportDirectories
	pages := 0
	require.NoError(t, runRebuildLinkFrontier(ctx, b, opts, nil, func(f *rebuildLinkFrontier, _ []*graph.Edge) error {
		dirs, err := f.importDirectories(ctx)
		if err != nil {
			return err
		}
		if first == nil {
			first = dirs
		} else {
			require.Same(t, first, dirs)
		}
		var found string
		if err := dirs.VisitExactDirectory(ctx, "repo/pkg", func(row graph.FileNodeIdentity) (bool, error) {
			found = row.ID
			return false, nil
		}); err != nil {
			return err
		}
		require.Equal(t, file, found)
		pages++
		return nil
	}))
	require.Equal(t, 2, pages)
	checkFrontierScratch(t, opts.ScratchParent)
}

// Native dependency contracts match the longest whole path prefix within
// the caller's repo, but only after the directory tier misses.
func TestRebuildImportDependencyContracts(t *testing.T) {
	const caller, sdk = "repo/app/main.go", "example.test/sdk"
	const dirTarget = "repo/lib/local/pkg.go"
	const dep = "dep::" + sdk
	const nested = dep + "/nested"
	nodes := []*graph.Node{
		rebuildImportTestNode(caller, "repo", "app", "", "go", graph.KindFile),
		rebuildImportTestNode(dirTarget, "repo", "app", "", "go", graph.KindFile),
		rebuildImportTestNode(dep, "repo", "", dep, "contract", graph.KindContract),
		rebuildImportTestNode(nested, "repo", "", nested, "contract", graph.KindContract),
		rebuildImportTestNode(nested+"/foreign", "elsewhere", "", nested+"/foreign", "contract", graph.KindContract),
		rebuildImportTestNode(nested+"/wrong-kind", "repo", "", "", "go", graph.KindPackage),
		rebuildImportTestNode(nested+"::tracked", "repo", "", "", "contract", graph.KindContract),
		rebuildImportTestNode(dep+"/local", "repo", "", dep+"/local", "contract", graph.KindContract),
	}
	var edges, want []*graph.Edge
	for i, tc := range []struct{ spec, to string }{
		{sdk, dep},
		{sdk + "/nested/tool", nested},
		{sdk + "/nestedly/tool", dep},
		{sdk + "/nested/foreign/tool", nested},
		{sdk + "/nested/wrong-kind/tool", nested},
		{sdk + "/nested::tracked/tool", dep},
		{sdk + "/local", dirTarget},
		{sdk + "ish/tool", "external::" + sdk + "ish/tool"},
	} {
		edge := rebuildImportTestEdge(caller, caller, tc.spec, i+1)
		edge.CrossRepo = tc.spec == sdk // native does not clear an existing flag
		edges = append(edges, edge)
		want = append(want, rebuildImportTestMove(edge, tc.to, false))
	}
	want = rebuildVueSortedRows(want)
	owner := func(GoImportCandidate) GoPackageOwnershipResult { return GoPackageOwnershipUnknown }
	require.Equal(t, want, rebuildImportNativeRows(t, nodes, edges, func(r *Resolver) {
		r.SetGoPackageOwnership(owner)
		r.SetWorkspaceMembership(func(string) string { return "" })
	}))
	ctx, b, opts := frontierFixture(t, 0)
	b.AddBatch(nodes, edges)
	require.NoError(t, RunRebuildMain(ctx, b, opts, RebuildMainInputs{
		SelectCallers:            func(page []*graph.Edge) ([]*graph.Edge, error) { return page, nil },
		GoOwnership:              func(q GoImportCandidate) (GoPackageOwnershipResult, error) { return owner(q), nil },
		DependencyContractsReady: true,
	}))
	require.Equal(t, want, rebuildFnPointerSaved(t, b))
	checkFrontierScratch(t, opts.ScratchParent)
}

// The rewritten full spelling excludes synthesized external calls, but
// native's package-prefix retry accepts them. Ambiguity only stops a miss.
func TestRebuildImportRewrittenPackagePrefix(t *testing.T) {
	const caller = "repo/app/main.ts"
	const full = "@scope/hit/Part::Export"
	const prefix = externalCallPrefix + "fixture:prefix"
	const dep = "dep::example.test/sdk"
	nodes := []*graph.Node{
		rebuildImportTestNode(caller, "repo", "app", "", "typescript", graph.KindFile),
		rebuildImportTestNode(externalCallPrefix+"fixture:full", "repo", "app", full, "typescript", graph.KindPackage),
		rebuildImportTestNode(prefix, "repo", "app", "@scope/hit", "typescript", graph.KindPackage),
		rebuildImportTestNode("elsewhere/prefix", "elsewhere", "app", "@scope/hit", "typescript", graph.KindPackage),
		rebuildImportTestNode("elsewhere/only", "elsewhere", "app", "@scope/foreign", "typescript", graph.KindPackage),
		rebuildImportTestNode("repo/npm-package", "repo", "app", "example.test", "typescript", graph.KindPackage),
		rebuildImportTestNode(dep, "repo", "", dep, "contract", graph.KindContract),
	}
	for _, qualified := range []string{full, "@scope/full/Part::Export", "@scope/prefix"} {
		for _, repo := range []string{"foreign-a", "foreign-b"} {
			nodes = append(nodes, rebuildImportTestNode(repo+"/"+qualified, repo, "app", qualified, "typescript", graph.KindPackage))
		}
	}
	var edges, want []*graph.Edge
	rewrites := make(map[string]string)
	for i, tc := range []struct {
		raw, effective, to string
		crossRepo          bool
	}{
		{"hit/Part::Export", full, prefix, false},
		{"dep/Part::Export", "example.test/sdk/Part::Export", dep, false}, // dependency bridge before npm retry
		{"foreign/Part::Export", "@scope/foreign/Part::Export", "elsewhere/only", true},
		{"full/Part::Export", "@scope/full/Part::Export", "", false},     // full ambiguity, retry miss
		{"prefix/Part::Export", "@scope/prefix/Part::Export", "", false}, // retry ambiguity
		{"missing/Part::Export", "@scope/missing/Part::Export", "external::@scope/missing/Part::Export", false},
		{"@scope/hit/Other::Export", "", "external::@scope/hit/Other::Export", false},
		{"@scope/hit/Same::Export", "@scope/hit/Same::Export", "external::@scope/hit/Same::Export", false},
	} {
		rewrites[tc.raw] = tc.effective
		edge := rebuildImportTestEdge(caller, caller, tc.raw, i+1)
		edges = append(edges, edge)
		if tc.to == "" {
			want = append(want, edge)
		} else {
			want = append(want, rebuildImportTestMove(edge, tc.to, tc.crossRepo))
		}
	}
	want = rebuildVueSortedRows(want)
	rewrite := func(file, raw string) string {
		require.Equal(t, caller, file)
		require.Contains(t, rewrites, raw) // raw spelling includes the export suffix
		return rewrites[raw]
	}
	require.Equal(t, want, rebuildImportNativeRows(t, nodes, edges, func(r *Resolver) {
		r.SetNpmAliasResolver(rewrite)
		r.SetWorkspaceMembership(func(string) string { return "" })
	}))
	// Only Pebble retains the full saved binding payload.
	for _, rows := range [][]*graph.Edge{edges, want} {
		for _, e := range rows {
			e.Context, e.Alias, e.Via = "import binding", "LocalName", "fixture"
		}
	}
	ctx, b, opts := frontierFixture(t, 0)
	b.AddBatch(nodes, edges)
	in := RebuildMainInputs{
		PathAlias:                func(_, _ string) (string, error) { return "", nil },
		RewriteNpm:               func(file, spec string) (string, error) { return rewrite(file, spec), nil },
		ExternalNpmDependency:    func(_, _ string) (bool, error) { return false, nil },
		DependencyContractsReady: true,
	}
	require.NoError(t, runRebuildMain(ctx, b, opts, false, in))
	require.Equal(t, want, rebuildFnPointerSaved(t, b))
	checkFrontierScratch(t, opts.ScratchParent)
}

func TestRebuildImportTerminalRewriteSeals(t *testing.T) {
	ctx, b, opts := frontierFixture(t, 0)
	const file, repo = "repo/app/main.ts", "repo"
	const original, effective = "pkg/sub::Export", "@scope/pkg/sub::Export"
	caller := rebuildImportTestNode(file, repo, "app", "", "typescript", graph.KindFile)
	raw := rebuildImportTestEdge(file, file, original, 3)
	want := rebuildImportTestMove(raw, "external::"+effective, false)
	rewrite := func(path, spec string) string {
		require.Equal(t, file, path)
		require.Equal(t, original, spec)
		return effective
	}
	require.Equal(t, []*graph.Edge{want}, rebuildImportNativeRows(t, []*graph.Node{caller}, []*graph.Edge{raw}, func(r *Resolver) {
		r.SetNpmAliasResolver(rewrite)
		r.SetWorkspaceMembership(func(string) string { return "" })
	}))
	require.NoError(t, b.AddParsedFile(repo, store_pebble.ParseFacts{
		EffectiveLanguage: "typescript",
		File:              graph.FileMetaRow{FilePath: file, NodeCount: 1},
		Nodes:             []*graph.Node{caller},
		Edges:             []*graph.Edge{raw},
	}))
	manifest, err := store_pebble.PrepareManifest(ctx, t.TempDir(), []store_pebble.ManifestFile{{Path: file, Kind: 'f', Language: "typescript"}}, store_pebble.Witness{
		Profile: store_pebble.FullProfile, Stage: store_pebble.StageManifest, Input: store_pebble.HeadDigest{1}, Recipe: store_pebble.HeadDigest{2},
	}, "terminal-import")
	require.NoError(t, err)
	graphStage, err := b.PrepareFileGraph(ctx, t.TempDir(), manifest.Witness, manifest.Incarnation)
	require.NoError(t, err)
	base, err := b.RetainFileGraph(ctx, graphStage)
	require.NoError(t, err)
	in := RebuildMainInputs{
		PathAlias:                func(_, _ string) (string, error) { return "", nil },
		RewriteNpm:               func(path, spec string) (string, error) { return rewrite(path, spec), nil },
		ExternalNpmDependency:    func(_, _ string) (bool, error) { return false, nil },
		DependencyContractsReady: true,
	}
	require.NoError(t, runRebuildMain(ctx, b, opts, false, in))
	require.Equal(t, []*graph.Edge{want}, rebuildFnPointerSaved(t, b))
	stage, err := b.PrepareRelations(ctx, t.TempDir(), base, nil)
	require.NoError(t, err)
	require.Equal(t, store_pebble.StageRelations, stage.Witness.Stage)
	require.NotEmpty(t, stage.Files)
}

func TestRebuildImportPostDirectoryRefusals(t *testing.T) {
	t.Run("PrefixBudget", func(t *testing.T) {
		ctx, b, opts := frontierFixture(t, 0)
		// Each key fits alone. Repeated full-component probes exhaust the
		// shared budget without mutating the private graph.
		spec := strings.Repeat("/", 4096)
		id, err := lookupRebuildDepModule(ctx, b.RebuildView(), "", spec)
		require.Empty(t, id)
		require.ErrorIs(t, err, store_pebble.ErrRebuildLinkInputSize)
		checkFrontierScratch(t, opts.ScratchParent)
	})
	const first, caller = "repo/a.ts", "repo/b.ts"
	for _, tc := range []struct {
		name, spec, reason string
		ready              bool
	}{
		{name: "UnprovedContracts", spec: "example.test/sdk/Part::Export", reason: "native file-directory buckets"},
		{name: "Empty", ready: true, reason: "terminal result"},
		{name: "NUL", spec: "./missing\x00::Export", ready: true, reason: "valid native external target"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, b, opts := frontierFixture(t, 0)
			before := []*graph.Edge{
				rebuildImportTestEdge(first, first, "./found", 1),
				rebuildImportTestEdge(caller, caller, tc.spec, 2),
			}
			b.AddBatch([]*graph.Node{
				rebuildImportTestNode(first, "repo", "", "", "typescript", graph.KindFile),
				rebuildImportTestNode(caller, "repo", "", "", "typescript", graph.KindFile),
				rebuildImportTestNode("repo/found.ts", "repo", "", "", "typescript", graph.KindFile),
				rebuildImportTestNode("dep::example.test/sdk", "repo", "", "dep::example.test/sdk", "contract", graph.KindContract),
			}, before)
			noRewrite := func(_, _ string) (string, error) { return "", nil }
			err := runRebuildMain(ctx, b, opts, false, RebuildMainInputs{
				PathAlias: noRewrite, RewriteNpm: noRewrite,
				ExternalNpmDependency:    func(_, _ string) (bool, error) { return false, nil },
				DependencyContractsReady: tc.ready,
			})
			require.ErrorIs(t, err, errRebuildMainRule)
			require.ErrorContains(t, err, tc.reason)
			require.Equal(t, before, rebuildFnPointerSaved(t, b))
			checkFrontierScratch(t, opts.ScratchParent)
		})
	}
}
