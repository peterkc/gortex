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
	ctx, b, opts := frontierFixture(t, 1, 0)
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
	ctx, b, opts := frontierFixture(t, 1, 0)
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
	ctx, b, opts := frontierFixture(t, 1, 0)
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
			ctx, b, opts := frontierFixture(t, 1, 0)
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
			ctx, b, opts := frontierFixture(t, 1, 0)
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
		name, missing   string
		proof, external bool
	}{
		{name: "Unproved", missing: "checked external npm dependency result"},
		{name: "External", proof: true, external: true, missing: "native file-directory buckets"},
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
			ctx, b, opts := frontierFixture(t, 1, 0)
			b.AddBatch(nodes, []*graph.Edge{edge})
			noRewrite := func(_, _ string) (string, error) { return "", nil }
			in := RebuildMainInputs{PathAlias: noRewrite, RewriteNpm: noRewrite}
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
			}
			checkFrontierScratch(t, opts.ScratchParent)
		})
	}
}

func TestRebuildImportDirectoryFrontierBorrow(t *testing.T) {
	ctx, b, opts := frontierFixture(t, 1, resolvePendingPageRows+1)
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
