//go:build unix

package indexer

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/indexer/source"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/parser/tsalias"
	"github.com/zzet/gortex/internal/resolver"
	"go.uber.org/zap"
)

func TestRebuildTSAliasPostParseSource(t *testing.T) {
	ctx := t.Context()
	root, src := rebuildInputFixture(t)
	const caller, target, spec = "app/main.ts", "shared/x.ts", "@lib/x"
	const cPath = "src/main.c"
	writeRebuildTSAliasFixtureFile(t, root, cPath, `void c_leaf(void) {}
void c_entry(void) {
    c_leaf();
}
`)
	writeRebuildTSAliasFixtureFile(t, root, "tsconfig.json", `{"compilerOptions":{"paths":{"@lib/*":["missing/*","shared/*"]}}}`)
	writeRebuildTSAliasFixtureFile(t, root, caller, "// Import sites\nimport \"@lib/x\"\nimport \"../shared/x\"\n")
	writeRebuildTSAliasFixtureFile(t, root, target, "export function chosen() {}\n")

	cfg := config.Default().Index
	reg := parser.NewRegistry()
	reg.Register(languages.NewTypeScriptExtractor())
	reg.Register(languages.NewCExtractor())
	idx := New(graph.New(), reg, cfg, zap.NewNop())
	t.Cleanup(idx.Close)
	t.Cleanup(func() { forgetTSAliasCache(root) })
	idx.installRebuildParsePolicy(freezeRebuildParsePolicy(root, cfg))
	idx.SetRootPath(root)
	t.Setenv("GORTEX_TEST_REBUILD_BUILD_CHILD", "1")
	link := rebuildTestLinkOptions(t)
	req := rebuildBuildRequest{Root: root, Witness: store_pebble.Witness{Recipe: store_pebble.HeadDigest{1}}}
	err := withRebuildCheckoutBuildInputs(ctx, t.TempDir(), src, req, nil, false,
		func(key string, mode fs.FileMode, member rebuildGitMember, raw *int64, emit func(rebuildSidecarRow) error) (rebuildCheckoutLeaf, error) {
			return idx.freezeRebuildCheckoutLeaf(ctx, src.FrozenRoot(), key, mode, member, nil, 1024, raw, emit)
		}, func(epoch rebuildCheckoutInputs, raw *int64, emit func(rebuildSidecarRow) error) (string, error) {
			return captureRebuildCheckoutResolverInputs(ctx, src, epoch, true, raw, emit)
		}, func(lease string, bound rebuildBuildRequest, inputs []rebuildInput) error {
			store := rebuildParseStore{dir: t.TempDir(), limit: 1 << 20}
			recipe := rebuildDigest{1}
			census := &rebuildExtractorCensus{
				admitted: uint64(len(inputs)), byPath: make(map[string]string), originByGraphPath: make(map[string]string),
			}
			for _, input := range inputs {
				facts, reused, err := idx.loadRebuildFile(ctx, src, store, input, recipe, rebuildTransformBounds{})
				require.NoError(t, err)
				require.False(t, reused)
				census.byPath[input.Path] = facts.EffectiveLanguage
				census.originByGraphPath[input.Path] = input.Path
			}
			require.Equal(t, map[string]string{cPath: "c", caller: "typescript", target: "typescript"}, census.byPath)
			job := rebuildFileStages{inputs: inputs, recipes: map[string]rebuildDigest{"c": recipe, "typescript": recipe}}
			receipts := idx.rebuildDataflowReceipts(ctx, job, store, census)
			build, err := store_pebble.OpenBuild(filepath.Join(lease, "working"), store_pebble.FullProfile)
			require.NoError(t, err)
			defer func() { require.NoError(t, build.Close()) }()
			for facts, err := range receipts {
				require.NoError(t, err)
				require.NoError(t, build.AddParsedFile("", facts))
			}
			// Seal the same three parse receipts before any linking; all heads
			// below are test-local and have no production publication route.
			var files []store_pebble.ManifestFile
			for _, input := range inputs {
				files = append(files, store_pebble.ManifestFile{
					Path: input.Path, Language: input.Language, Kind: byte(input.Kind), Size: input.Size,
					Content: store_pebble.HeadDigest(input.Content), Extractor: store_pebble.HeadDigest(idx.rebuildExtractorRecipe(recipe, input)),
				})
			}
			witness := bound.Witness
			witness.Stage, witness.Profile = store_pebble.StageManifest, store_pebble.FullProfile
			manifest, err := store_pebble.PrepareManifest(ctx, t.TempDir(), files, witness, rebuildTestIncarnation)
			require.NoError(t, err)
			require.Equal(t, uint64(3), manifest.Witness.Families[0].Count)
			headDir := filepath.Join(t.TempDir(), "head")
			head, err := store_pebble.OpenHead(headDir, rebuildTestIncarnation, store_pebble.FullProfile)
			require.NoError(t, err)
			defer func() {
				if head != nil {
					require.NoError(t, head.Close())
				}
			}()
			_, err = head.PublishStage(ctx, manifest)
			require.NoError(t, err)
			fileGraph, err := build.PrepareFileGraph(ctx, t.TempDir(), manifest.Witness, rebuildTestIncarnation)
			require.NoError(t, err)
			retained, err := build.RetainFileGraph(ctx, fileGraph)
			require.NoError(t, err)
			_, err = head.PublishStage(ctx, fileGraph)
			require.NoError(t, err)

			view := build.RebuildView()
			imports := func() []*graph.Edge {
				var rows []*graph.Edge
				require.NoError(t, view.VisitEdges(ctx, func(e *graph.Edge) error {
					if e.Kind == graph.EdgeImports {
						rows = append(rows, e)
					}
					return nil
				}))
				return rows
			}
			raw := graph.Edge{From: caller, To: "unresolved::import::" + spec, Kind: graph.EdgeImports, FilePath: caller, Line: 2}
			relative := raw
			relative.To, relative.Line = "unresolved::import::../shared/x", 3
			require.ElementsMatch(t, []*graph.Edge{&raw, &relative}, imports())
			cRaw := graph.Edge{From: cPath + "::c_entry", To: "unresolved::c_leaf", Kind: graph.EdgeCalls, FilePath: cPath, Line: 3}
			cOld := graph.EdgeIdentityFor(&cRaw)
			cBound := cRaw
			cBound.To, cBound.Origin, cBound.Confidence = cPath+"::c_leaf", graph.OriginASTResolved, 0.9
			cNew := graph.EdgeIdentityFor(&cBound)
			before, err := view.Edge(ctx, cOld)
			require.NoError(t, err)
			require.Equal(t, &cRaw, before)
			absent, err := view.Edge(ctx, cNew)
			require.NoError(t, err)
			require.Nil(t, absent)
			var deferredDecls []*graph.Node
			for _, id := range []string{cOld.From, cNew.To} {
				n, err := view.Node(ctx, id)
				require.NoError(t, err)
				require.NotNil(t, n, id)
				require.Equal(t, graph.KindFunction, n.Kind)
				require.Equal(t, "c", n.Language)
				deferredDecls = append(deferredDecls, n)
			}

			// Decode the exact receipts afresh for each native arm. Keep the
			// ordinary resolver order and all declarations, including C.
			replayNative := func() *graph.Graph {
				t.Helper()
				native := graph.New()
				oracle := New(native, reg, cfg, zap.NewNop())
				defer oracle.Close()
				oracle.SetSemanticManager(nil)
				oracle.SetResolverLSPHelper(nil)
				oracle.SetRootPath(root)
				for facts, err := range receipts {
					require.NoError(t, err)
					native.AddBatch(facts.Nodes, facts.Edges)
				}
				require.ElementsMatch(t, []*graph.Edge{&raw, &relative}, slices.Collect(native.EdgesByKind(graph.EdgeImports)))
				require.Equal(t, []*graph.Edge{before}, slices.Collect(native.EdgesByKind(graph.EdgeCalls)))
				stats, err := oracle.resolver.ResolveAllContext(ctx)
				require.NoError(t, err)
				require.False(t, stats.Incomplete)
				return native
			}
			require.Equal(t, "shared/x", idx.resolvePathAliasImport(caller, spec))
			native := replayNative()
			require.Equal(t, []*graph.Edge{&cBound}, slices.Collect(native.EdgesByKind(graph.EdgeCalls)))
			settled, settledRelative := raw, relative
			settled.To, settledRelative.To = target, target
			nativeImports := slices.Collect(native.EdgesByKind(graph.EdgeImports))
			require.ElementsMatch(t, []*graph.Edge{&settled, &settledRelative}, nativeImports)

			// The earlier target appears after parsing. Keep the old target
			// on disk so the change isolates the order of existence probes.
			writeRebuildTSAliasFixtureFile(t, root, "missing/x.ts", "export const earlier = 2\n")
			require.Equal(t, "missing/x", idx.resolvePathAliasImport(caller, spec))

			// A live replay has no receipt for the new target and loses just
			// the aliased bind. The same frozen sidecar still has the old probe.
			live := raw
			live.To = "external::" + spec
			require.ElementsMatch(t, []*graph.Edge{&live, &settledRelative}, slices.Collect(replayNative().EdgesByKind(graph.EdgeImports)))

			require.NoError(t, runRebuildFrozenMain(ctx, lease, rebuildDigest(bound.Witness.FrozenInputs), build, census, nil, link, bound.SkipPlaceholderSources))
			privateImports := imports()
			require.Equal(t, []*graph.Edge{&settled, &settledRelative}, privateImports, "private complete edge-key order")
			require.ElementsMatch(t, nativeImports, privateImports, "complete retained Imports family in this fixture")
			pending, err := view.Edge(ctx, cOld)
			require.NoError(t, err)
			require.Equal(t, before, pending, "the whole deferred C before-value must survive")
			absent, err = view.Edge(ctx, cNew)
			require.NoError(t, err)
			require.Nil(t, absent)
			for _, before := range deferredDecls {
				n, err := view.Node(ctx, before.ID)
				require.NoError(t, err)
				require.Equal(t, before, n, "deferred declaration %q", before.ID)
			}
			// This test-only chain includes the unproved raw C pending row;
			// it is NOT a D72 relation selection proof or a production route.
			relations, err := build.PrepareRelations(ctx, t.TempDir(), retained, nil)
			require.NoError(t, err)
			require.Nil(t, relations.Witness.Relations, "relation selection remains unproved")
			_, err = head.PublishStage(ctx, relations)
			require.NoError(t, err)
			derived, err := store_pebble.PrepareDeferredDerived(t.TempDir(), relations)
			require.NoError(t, err)
			_, err = head.PublishStage(ctx, derived)
			require.NoError(t, err)

			// Select from the full final native Graph, not the raw parse
			// receipts or the changed live replay. No language-wide filter:
			// both independent C declarations remain searchable.
			nativeNodes := native.AllNodes()
			slices.SortFunc(nativeNodes, func(a, b *graph.Node) int { return strings.Compare(a.ID, b.ID) })
			var selected []*graph.Node
			var items []graph.SymbolFTSItem
			want := make(map[string]string)
			for _, n := range nativeNodes {
				if !idx.shouldIndexForSearch(n) {
					continue
				}
				text := ftsTokensFor(n, bound.ProjectName)
				require.NotContains(t, want, n.ID)
				want[n.ID] = text
				selected = append(selected, n)
				items = append(items, graph.SymbolFTSItem{NodeID: n.ID, Tokens: text})
			}
			for _, id := range []string{cOld.From, cNew.To, target + "::chosen"} {
				require.NotEmpty(t, want[id], id)
			}
			got := make(map[string]string)
			require.NoError(t, view.VisitNodes(ctx, func(n *graph.Node) error {
				if idx.shouldIndexForSearch(n) {
					require.NotContains(t, got, n.ID)
					got[n.ID] = ftsTokensFor(n, bound.ProjectName)
				}
				return nil
			}))
			require.Equal(t, want, got, "entire selected document membership and rendered token rows")

			// SQLite is only the native rank engine. Its nodes and FTS rows
			// come from the selected final Graph, and it receives no edges.
			// Canonical ID insertion order matches the Pebble document rowids.
			rank := newFTSStore(t)
			require.NoError(t, rank.AddBatchCheckedContext(ctx, selected, nil))
			require.NoError(t, rank.BatchUpsertSymbolFTS(items))
			const limit = 100
			require.Less(t, len(want), limit)
			rows, next, err := rank.SymbolFTSGenerationRows(ctx, 0, "", 0, limit)
			require.NoError(t, err)
			require.Zero(t, next)
			require.Len(t, rows, len(want))
			for i, row := range rows {
				require.Equal(t, items[i].NodeID, row.NodeID)
				require.Equal(t, items[i].Tokens, row.Tokens)
				require.Equal(t, selected[i].RepoPrefix, row.RepoPrefix)
			}
			t.Logf("same-receipt native selected symbol documents (%d): %v", len(rows), rows)

			stage, err := idx.prepareRebuildSearch(ctx, build, t.TempDir(), derived.Witness, rebuildTestIncarnation, bound.ProjectName)
			require.NoError(t, err)
			// Independently supplied native-selected inputs must seal the same
			// complete symbol family (documents, stats, postings and exact names).
			// This is not another native parse or relation oracle.
			expected, err := store_pebble.OpenBuild(filepath.Join(t.TempDir(), "native-search"), store_pebble.FullProfile)
			require.NoError(t, err)
			defer func() { require.NoError(t, expected.Close()) }()
			expected.AddBatch(selected, nil)
			expectedStage, err := expected.PrepareSearch(ctx, t.TempDir(), derived.Witness, rebuildTestIncarnation, func(n *graph.Node) (string, bool) {
				text, ok := want[n.ID]
				return text, ok
			})
			require.NoError(t, err)
			require.NotEmpty(t, expectedStage.Files)
			require.Equal(t, expectedStage.Witness.Families, stage.Witness.Families, "selected search gold rows")
			_, err = head.PublishStage(ctx, stage)
			require.NoError(t, err)
			require.NoError(t, head.Close())
			head = nil
			head, err = store_pebble.OpenHead(headDir, rebuildTestIncarnation, store_pebble.FullProfile)
			require.NoError(t, err)
			snapshot := head.OpenSnapshot()
			defer func() { require.NoError(t, snapshot.Close()) }()
			saved, err := snapshot.Publication()
			require.NoError(t, err)
			require.Equal(t, stage.Witness, saved)
			require.Nil(t, saved.Relations)
			var unproved *store_pebble.ErrLinksNotAvailable
			require.ErrorAs(t, saved.RequireRelationCoverage(nil), &unproved)
			require.Equal(t, []*graph.Edge{before}, slices.Collect(snapshot.EdgesByKind(graph.EdgeCalls)), "test head includes the raw C before-value")
			require.ElementsMatch(t, nativeImports, slices.Collect(snapshot.EdgesByKind(graph.EdgeImports)))
			require.NoError(t, snapshot.ReadError())
			for _, tc := range []struct {
				query string
				ids   []string
				exact bool
			}{
				{"c_entry", []string{cOld.From}, true},
				{"c_leaf", []string{cNew.To}, true},
				{"chosen", []string{target + "::chosen"}, true},
				{"entry leaf", []string{cOld.From, cNew.To}, false},
				{"entry chosen", []string{cOld.From, target + "::chosen"}, false},
				{"shared main", []string{cOld.From, cNew.To, target + "::chosen"}, false},
			} {
				wantHits, err := rank.SearchSymbolsRepoScopedContext(ctx, tc.query, nil, limit)
				require.NoError(t, err)
				gotHits, err := snapshot.SearchSymbolsRepoScopedContext(ctx, tc.query, nil, limit)
				require.NoError(t, err)
				require.Less(t, len(wantHits), limit)
				for _, id := range tc.ids {
					require.True(t, slices.ContainsFunc(wantHits, func(h graph.SymbolHit) bool { return h.NodeID == id }), "%s: missing %s", tc.query, id)
				}
				for _, hit := range wantHits {
					if tc.exact {
						require.Equal(t, float64(100), hit.Score, tc.query)
					} else {
						require.Positive(t, hit.Score, tc.query)
						require.Less(t, hit.Score, float64(100), tc.query)
					}
				}
				if tc.exact {
					require.Len(t, wantHits, len(tc.ids), tc.query)
				}
				require.Equal(t, wantHits, gotHits, "raw ordered rank IDs and scores for %q", tc.query)
				t.Logf("raw selected rank %q (%d): %v", tc.query, len(gotHits), gotHits)
			}
			return nil
		})
	require.NoError(t, err)
}

// Exercise the owner capture and the same frozen providers used by main, with
// a real post-parse import query and a fresh lease after the checkout is gone.
func TestRebuildResolverInputsRestart(t *testing.T) {
	const caller, target = "app/main.ts", "shared/x.ts"
	const goCaller, goTarget = "main.go", "pkg/lib.go"
	const alias, npmSpec = "@lib/x", "alias/sub"
	var first store_pebble.Witness
	for _, basename := range []string{"repo", "repo[1]"} {
		t.Run(basename, func(t *testing.T) {
			ctx := t.Context()
			root := filepath.Join(t.TempDir(), basename)
			require.NoError(t, os.Mkdir(root, 0o700))
			files := map[string]string{
				"go.mod":            "module example.org/fixture\n",
				goCaller:            "package main\nimport _ \"example.org/fixture/pkg\"\n",
				goTarget:            "package pkg\nfunc Bind() {}\n",
				caller:              `import "@lib/x"` + "\n" + `import "alias/sub"` + "\n",
				target:              "export {}\n",
				"tsconfig.json":     `{"compilerOptions":{"paths":{"@lib/*":["wrong/*"]}}}`,
				"app/tsconfig.json": `{"extends":"../never.json","compilerOptions":{"paths":{"@lib/*":["../missing/*","../shared/*"]}}}`,
				"npm.manifest":      `{"dependencies":{"alias":"npm:actual@1.0.0"}}`,
			}
			for key, body := range files {
				writeRebuildTSAliasFixtureFile(t, root, key, body)
			}
			require.NoError(t, os.Symlink("npm.manifest", filepath.Join(root, "package.json")))
			src, err := source.NewFilesystemSource(root)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, src.Close()) })
			native := tsalias.Load(root)
			require.Equal(t, "shared/x", tsalias.Resolve(native.FindForFile(caller), alias))
			require.Equal(t, "actual/sub", newNpmAliasIndex(map[string]string{"": root}).Resolve(caller, npmSpec))
			candidate := resolver.GoImportCandidate{ImporterFilePath: goCaller, CandidateFilePath: goTarget, ImportPath: "example.org/fixture/pkg"}
			owner, err := goPackageOwnershipFromFiles(ctx, src, "", map[string]string{goCaller: "", goTarget: "pkg"})
			require.NoError(t, err)
			require.Equal(t, resolver.GoPackageOwnershipExact, owner(candidate))

			cfg := config.Default().Index
			reg := parser.NewRegistry()
			reg.Register(languages.NewTypeScriptExtractor())
			reg.Register(languages.NewGoExtractor())
			idx := New(graph.New(), reg, cfg, zap.NewNop())
			t.Cleanup(idx.Close)
			idx.installRebuildParsePolicy(freezeRebuildParsePolicy(root, cfg))
			idx.SetRootPath(root)
			leases, headDir := t.TempDir(), t.TempDir()
			var witness store_pebble.Witness
			var specs []string
			goFiles := make(map[string]bool)
			req := rebuildBuildRequest{Root: root, Witness: store_pebble.Witness{Recipe: store_pebble.HeadDigest{1}}}
			err = withRebuildCheckoutBuildInputs(ctx, leases, src, req, nil, false,
				func(key string, mode fs.FileMode, member rebuildGitMember, raw *int64, emit func(rebuildSidecarRow) error) (rebuildCheckoutLeaf, error) {
					return idx.freezeRebuildCheckoutLeaf(ctx, src.FrozenRoot(), key, mode, member, nil, 1024, raw, emit)
				}, func(epoch rebuildCheckoutInputs, raw *int64, emit func(rebuildSidecarRow) error) (string, error) {
					return captureRebuildCheckoutResolverInputs(ctx, src, epoch, true, raw, emit)
				}, func(lease string, bound rebuildBuildRequest, inputs []rebuildInput) error {
					require.NoError(t, bound.checkProjectRecipe())
					require.NotZero(t, bound.Witness.FrozenInputs)
					store := rebuildParseStore{dir: t.TempDir(), limit: 1 << 20}
					recipe := rebuildDigest{1}
					var manifest []store_pebble.ManifestFile
					for _, in := range inputs {
						facts, reused, err := idx.loadRebuildFile(ctx, src, store, in, recipe, rebuildTransformBounds{})
						require.NoError(t, err)
						require.False(t, reused)
						goFiles[in.Path] = facts.EffectiveLanguage == "go"
						if in.Path == caller {
							for _, edge := range facts.Edges {
								if edge.Kind == graph.EdgeImports {
									spec, ok := strings.CutPrefix(edge.To, "unresolved::import::")
									require.True(t, ok)
									specs = append(specs, spec)
								}
							}
						}
						manifest = append(manifest, store_pebble.ManifestFile{Path: in.Path, Language: in.Language, Kind: byte(in.Kind), Size: in.Size, Content: store_pebble.HeadDigest(in.Content), Extractor: store_pebble.HeadDigest(idx.rebuildExtractorRecipe(recipe, in))})
					}
					require.ElementsMatch(t, []string{alias, npmSpec}, specs)
					w := bound.Witness
					w.Stage, w.Profile = store_pebble.StageManifest, store_pebble.FullProfile
					stage, err := store_pebble.PrepareManifest(ctx, t.TempDir(), manifest, w, rebuildTestIncarnation)
					require.NoError(t, err)
					require.NoError(t, retainRebuildInputSidecar(ctx, headDir, lease, rebuildDigest(w.FrozenInputs)))
					head, err := store_pebble.OpenHead(headDir, rebuildTestIncarnation, w.Profile)
					require.NoError(t, err)
					defer func() { require.NoError(t, head.Close()) }()
					_, err = head.PublishStage(ctx, stage)
					require.NoError(t, err)
					witness = stage.Witness
					return nil
				})
			require.NoError(t, err)
			if basename == "repo" {
				first = witness
			} else {
				require.Equal(t, first, witness)
			}
			entries, err := os.ReadDir(leases)
			require.NoError(t, err)
			require.Empty(t, entries)
			writeRebuildTSAliasFixtureFile(t, root, "go.mod", "module example.org/changed\n")
			writeRebuildTSAliasFixtureFile(t, root, "npm.manifest", `{"dependencies":{"alias":"npm:changed@1"}}`)
			writeRebuildTSAliasFixtureFile(t, root, "missing/x.ts", "export {}")
			require.NoError(t, os.Remove(filepath.Join(root, target)))
			require.NoError(t, os.Rename(root, filepath.Join(t.TempDir(), "moved")))
			head, err := store_pebble.OpenHead(headDir, rebuildTestIncarnation, witness.Profile)
			require.NoError(t, err)
			defer func() { require.NoError(t, head.Close()) }()
			saved, _, err := head.ReconcilePublication(nil)
			require.NoError(t, err)
			require.Equal(t, witness, saved)
			digest := rebuildDigest(saved.FrozenInputs)
			resume := t.TempDir()
			require.NoError(t, restoreRebuildInputSidecar(ctx, headDir, resume, digest))
			goFacts, aliases, err := openRebuildResolverConfigFacts(ctx, resume, digest)
			require.NoError(t, err)
			ns, npm, followed, err := openRebuildNpmManifestFacts(ctx, resume, digest)
			require.NoError(t, err)
			require.NotZero(t, followed)
			glob, err := newRebuildWorkspaceGlob(ctx, ns)
			require.NoError(t, err)
			lookup := newRebuildNpmLookup(ctx, rebuildNpmInputs{namespace: ns, manifests: npm, followedRoot: followed,
				isFile: func(_ context.Context, key string) (bool, error) { return ns.probeFile(key) }, matchDirs: glob.matchDirs})
			got, err := rebuildGoOwnership(candidate, func(key string) (bool, error) { return goFiles[key], nil }, func(key string) (string, error) { return key, nil }, goFacts.lookup)
			require.NoError(t, err)
			require.Equal(t, resolver.GoPackageOwnershipExact, got)
			for _, spec := range specs {
				switch spec {
				case alias:
					stem, err := aliases.resolve(ns.probeFile, caller, spec)
					require.NoError(t, err)
					require.Equal(t, tsalias.FrozenAliasStem("shared/x"), stem)
				case npmSpec:
					rewrite, err := lookup.rewrite(caller, spec)
					require.NoError(t, err)
					require.Equal(t, "actual/sub", rewrite)
				}
			}
		})
	}
}
