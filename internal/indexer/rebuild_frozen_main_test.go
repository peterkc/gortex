package indexer

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/excludes"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/parser/tsalias"
	"go.uber.org/zap"
)

func TestRebuildFrozenMain(t *testing.T) {
	for _, denied := range []bool{false, true} {
		name := "frozen fallback uses the literal caller scope"
		if denied {
			name = "denied first target does not fall through"
		}
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			root, src := rebuildInputFixture(t)
			const callerDir = "cafe\u0301"
			const callerRaw = callerDir + "/main.ts"
			const callerKey = "caf\u00e9/main.ts"
			const configRaw = callerDir + "/tsconfig.json"
			const configKey = "caf\u00e9/tsconfig.json"
			const target = "shared/x.ts"
			const spec = "@lib/x"
			const pending = "unresolved::import::" + spec
			const seed = "dep::example.org/seed"
			const collision = "dep::example.org/collision"
			const indirect = "dep::example.org/indirect"
			writeRebuildTSAliasFixtureFile(t, root, "go.mod", `module example.org/root
require (
	example.org/seed v1.0.0
	example.org/collision v1.0.0
	example.org/indirect v1.0.0 // indirect
)
`)
			writeRebuildTSAliasFixtureFile(t, root, callerRaw, `import "@lib/x"`)
			writeRebuildTSAliasFixtureFile(t, root, configRaw, `{"compilerOptions":{"paths":{"@lib/*":["../first/*","../shared/*"]}}}`)
			writeRebuildTSAliasFixtureFile(t, root, target, "export {}")
			var flat *excludes.Matcher
			if denied {
				writeRebuildTSAliasFixtureFile(t, root, "first/x.ts", "export {}")
				flat = excludes.New([]string{"first/"})
			} else {
				native := tsalias.Load(root)
				require.Equal(t, "shared/x", tsalias.Resolve(native.FindForFile(callerRaw), spec))
			}

			cfg := config.Default().Index
			reg := parser.NewRegistry()
			languages.RegisterAll(reg)
			idx := New(graph.New(), reg, cfg, zap.NewNop())
			t.Cleanup(idx.Close)
			req := rebuildBuildRequest{Root: root, Witness: store_pebble.Witness{Recipe: store_pebble.HeadDigest{1}}}
			t.Setenv("GORTEX_TEST_REBUILD_BUILD_CHILD", "1")
			link := rebuildTestLinkOptions(t)
			err := withRebuildCheckoutBuildInputs(ctx, t.TempDir(), src, req, flat, false,
				func(key string, mode fs.FileMode, member rebuildGitMember, raw *int64, emit func(rebuildSidecarRow) error) (rebuildCheckoutLeaf, error) {
					return idx.freezeRebuildCheckoutLeaf(ctx, src.FrozenRoot(), key, mode, member, nil, 1024, raw, emit)
				}, func(epoch rebuildCheckoutInputs, raw *int64, emit func(rebuildSidecarRow) error) (string, error) {
					return captureRebuildCheckoutResolverInputs(ctx, src, epoch, true, raw, emit)
				}, func(lease string, bound rebuildBuildRequest, inputs []rebuildInput) error {
					require.Len(t, inputs, 3)
					langs := make(map[string]string, len(inputs))
					for _, input := range inputs {
						langs[input.Path] = input.Language
					}
					require.Equal(t, map[string]string{callerRaw: "typescript", configRaw: "json", target: "typescript"}, langs)
					if !denied {
						require.NoError(t, os.Remove(filepath.Join(root, target)))
						writeRebuildTSAliasFixtureFile(t, root, "first/x.ts", "export {}")
						native := tsalias.Load(root)
						require.Equal(t, "first/x", tsalias.Resolve(native.FindForFile(callerRaw), spec))
					}
					require.NoError(t, os.Remove(filepath.Join(root, "go.mod")))
					build, err := store_pebble.OpenBuild(filepath.Join(lease, "working"), store_pebble.FullProfile)
					require.NoError(t, err)
					defer func() { require.NoError(t, build.Close()) }()
					build.AddNode(&graph.Node{ID: callerKey, Kind: graph.KindFile, FilePath: callerKey, Language: "typescript"})
					build.AddNode(&graph.Node{ID: target, Kind: graph.KindFile, FilePath: target, Language: "typescript"})
					build.AddNode(&graph.Node{ID: configKey, Kind: graph.KindFile, FilePath: configKey, Language: "json"})
					edge := graph.Edge{From: callerKey, To: pending, Kind: graph.EdgeImports, FilePath: callerKey, Line: 3}
					build.AddEdge(&edge)
					dep := graph.Edge{From: callerKey, To: "unresolved::import::example.org/seed/child", Kind: graph.EdgeImports,
						FilePath: callerKey, Line: 4}
					if !denied {
						build.AddEdge(&dep)
					}
					existing := &graph.Node{ID: collision, Name: "retained", Kind: graph.KindFunction, FilePath: target, Language: "typescript"}
					build.AddNode(existing)
					census := &rebuildExtractorCensus{admitted: 3,
						byPath:            langs,
						originByGraphPath: map[string]string{callerKey: callerRaw, configKey: configRaw, target: target},
					}
					err = runRebuildFrozenMain(ctx, lease, rebuildDigest(bound.Witness.FrozenInputs), build, census, link, bound.SkipPlaceholderSources)
					if denied {
						require.ErrorIs(t, err, errRebuildResolverInputDenied)
						require.Equal(t, buildRefusal(err).Refusal, buildRefusal(errRebuildResolverInputDenied).Refusal)
					} else {
						require.NoError(t, err)
						edge.To = target
					}
					view := build.RebuildView()
					seeded, readErr := view.Node(ctx, seed)
					require.NoError(t, readErr)
					require.Equal(t, &graph.Node{ID: seed, Kind: graph.KindContract, Name: seed, FilePath: "go.mod", Language: "contract",
						Meta: map[string]any{"type": "dependency", "role": "consumer"}}, seeded)
					kept, readErr := view.Node(ctx, collision)
					require.NoError(t, readErr)
					require.Equal(t, existing, kept)
					omitted, readErr := view.Node(ctx, indirect)
					require.NoError(t, readErr)
					require.Nil(t, omitted)
					var edges []graph.Edge
					require.NoError(t, build.RebuildView().VisitEdges(ctx, func(e *graph.Edge) error {
						edges = append(edges, *e)
						return nil
					}))
					want := []graph.Edge{edge}
					if !denied {
						dep.To = seed
						want = append([]graph.Edge{dep}, want...)
					}
					require.Equal(t, want, edges)
					return nil
				})
			require.NoError(t, err)
		})
	}
}

// Replay the same checkout facts after the live root manifest disagrees.
// A checked negative or versioned replacement permits the precise local
// rule. An applicable local replacement refuses before any page move saves.
func TestRebuildFrozenMainGoExtern(t *testing.T) {
	const caller, pkg = "main.go", "example.org/lib/pkg"
	const candidate = pkg + "/bind.go"
	const from, to = caller + "::Caller", candidate + "::Bind"
	const terminal = "sdk/sub::Missing"
	const versioned = "module sdk\nreplace example.org/lib => example.org/other v1.0.0\n"
	const local = "module sdk\nreplace example.org/lib => ./replacement\n"
	for _, tc := range []struct {
		name, frozen, later string
		refuse              bool
	}{
		{"versioned", versioned, local, false},
		{"local", local, versioned, true},
		{"checked absence", "", local, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			root, src := rebuildInputFixture(t)
			writeRebuildTSAliasFixtureFile(t, root, caller, "package main\nfunc Caller() {}\n")
			writeRebuildTSAliasFixtureFile(t, root, candidate, "package pkg\nfunc Bind() {}\n")
			writeRebuildTSAliasFixtureFile(t, root, "example.org/lib/go.mod", "module example.org/lib\n")
			if tc.frozen != "" {
				writeRebuildTSAliasFixtureFile(t, root, "go.mod", tc.frozen)
			}
			reg := parser.NewRegistry()
			languages.RegisterAll(reg)
			idx := New(graph.New(), reg, config.Default().Index, zap.NewNop())
			t.Cleanup(idx.Close)
			req := rebuildBuildRequest{Root: root, Witness: store_pebble.Witness{Recipe: store_pebble.HeadDigest{1}}}
			t.Setenv("GORTEX_TEST_REBUILD_BUILD_CHILD", "1")
			link := rebuildTestLinkOptions(t)
			err := withRebuildCheckoutBuildInputs(ctx, t.TempDir(), src, req, nil, false,
				func(key string, mode fs.FileMode, member rebuildGitMember, raw *int64, emit func(rebuildSidecarRow) error) (rebuildCheckoutLeaf, error) {
					return idx.freezeRebuildCheckoutLeaf(ctx, src.FrozenRoot(), key, mode, member, nil, 1024, raw, emit)
				}, func(epoch rebuildCheckoutInputs, raw *int64, emit func(rebuildSidecarRow) error) (string, error) {
					return captureRebuildCheckoutResolverInputs(ctx, src, epoch, true, raw, emit)
				}, func(lease string, bound rebuildBuildRequest, inputs []rebuildInput) error {
					require.Len(t, inputs, 2)
					writeRebuildTSAliasFixtureFile(t, root, "go.mod", tc.later)
					build, err := store_pebble.OpenBuild(filepath.Join(lease, "working"), store_pebble.FullProfile)
					require.NoError(t, err)
					defer func() { require.NoError(t, build.Close()) }()
					first := graph.Edge{From: from, To: "unresolved::extern::a-malformed", Kind: graph.EdgeCalls, FilePath: caller, Line: 1}
					precise := graph.Edge{From: from, To: "unresolved::extern::" + pkg + "::Bind", Kind: graph.EdgeCalls, FilePath: caller, Line: 2,
						Origin: graph.OriginASTInferred, Confidence: 0.5}
					stub := graph.Edge{From: from, To: "unresolved::extern::" + terminal, Kind: graph.EdgeCalls, FilePath: caller, Line: 3}
					build.AddBatch([]*graph.Node{
						{ID: caller, FilePath: caller, Language: "go", Kind: graph.KindFile},
						{ID: from, Name: "Caller", FilePath: caller, Language: "go", Kind: graph.KindFunction},
						{ID: candidate, FilePath: candidate, Language: "go", Kind: graph.KindFile},
						{ID: to, Name: "Bind", FilePath: candidate, Language: "go", Kind: graph.KindFunction},
					}, []*graph.Edge{&first, &precise, &stub})
					census := &rebuildExtractorCensus{admitted: 2,
						byPath:            map[string]string{caller: "go", candidate: "go"},
						originByGraphPath: map[string]string{caller: caller, candidate: candidate},
					}
					err = runRebuildFrozenMain(ctx, lease, rebuildDigest(bound.Witness.FrozenInputs), build, census, link, bound.SkipPlaceholderSources)
					if tc.refuse {
						require.ErrorIs(t, err, errRebuildGoEvidence)
						require.ErrorContains(t, err, "local Go replacement")
						require.Equal(t, buildRefusal(errRebuildGoEvidence).Refusal, buildRefusal(err).Refusal)
					} else {
						require.NoError(t, err)
						first.To = "external::a-malformed"
						precise.To, precise.Origin, precise.Confidence = to, "", 0
						precise.Meta = map[string]any{"go_import": pkg + "::Bind"}
						stub.To = "stdlib::" + terminal // root claim sdk does not register a tracked module
					}
					want := map[int]graph.Edge{1: first, 2: precise, 3: stub}
					if !tc.refuse {
						want[0] = graph.Edge{From: stub.To, To: "module::go:sdk/sub", Kind: graph.EdgeMemberOf, Origin: graph.OriginASTResolved}
						symbol, err := build.RebuildView().Node(ctx, stub.To)
						require.NoError(t, err)
						require.Equal(t, &graph.Node{ID: stub.To, Name: "Missing", Kind: graph.KindFunction, Language: "go",
							Meta: map[string]any{"external": true, "module_path": "sdk/sub", "module_role": "stdlib"}}, symbol)
					}
					saved := make(map[int]graph.Edge)
					n := 0
					require.NoError(t, build.RebuildView().VisitEdges(ctx, func(edge *graph.Edge) error {
						saved[edge.Line] = *edge
						n++
						return nil
					}))
					require.Equal(t, len(want), n)
					require.Equal(t, want, saved)
					return nil
				})
			require.NoError(t, err)
		})
	}
}

func TestRebuildFrozenMainGoReceiverReceiptOrder(t *testing.T) {
	const methodFile, pluginFile, typeFile = "pkg/m.go", "pkg/a.go", "pkg/types.go"
	const pythonFile = "deferred.py"
	method := &graph.Node{ID: methodFile + "::T.M", Name: "M", Kind: graph.KindMethod, FilePath: methodFile, Language: "go"}
	receiver := &graph.Node{ID: typeFile + "::T", Name: "T", Kind: graph.KindType, FilePath: typeFile, Language: "go"}
	first := &graph.Edge{From: method.ID, To: methodFile + "::T", Kind: graph.EdgeMemberOf, FilePath: methodFile, Line: 2,
		Meta: map[string]any{"producer": "parser"}}
	second := *first
	second.To = pluginFile + "::T"
	second.Meta = map[string]any{"producer": "plugin"}
	require.True(t, first.To > second.To)
	// An original pending edge enables the native main tail even when the
	// extractor census leaves that deferred caller out of main linking.
	deferred := &graph.Edge{From: pythonFile + "::run", To: "unresolved::Target", Kind: graph.EdgeCalls, FilePath: pythonFile, Line: 3}
	deferredCaller := &graph.Node{ID: deferred.From, Name: "run", Kind: graph.KindFunction, FilePath: pythonFile, Language: "python"}
	facts := []store_pebble.ParseFacts{
		{EffectiveLanguage: "go", File: graph.FileMetaRow{FilePath: methodFile, NodeCount: 1}, Nodes: []*graph.Node{method}, Edges: []*graph.Edge{first}},
		{EffectiveLanguage: "go", File: graph.FileMetaRow{FilePath: pluginFile}, Edges: []*graph.Edge{&second}},
		{EffectiveLanguage: "go", File: graph.FileMetaRow{FilePath: typeFile, NodeCount: 1}, Nodes: []*graph.Node{receiver}},
		{EffectiveLanguage: "python", File: graph.FileMetaRow{FilePath: pythonFile, NodeCount: 1}, Nodes: []*graph.Node{deferredCaller}, Edges: []*graph.Edge{deferred}},
	}
	lease := t.TempDir()
	build, err := store_pebble.OpenBuild(filepath.Join(lease, "working"), store_pebble.FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, build.Close()) })
	census := &rebuildExtractorCensus{
		admitted:          uint64(len(facts)),
		byPath:            map[string]string{methodFile: "go", pluginFile: "go", typeFile: "go", pythonFile: "python"},
		originByGraphPath: map[string]string{methodFile: methodFile, pluginFile: pluginFile, typeFile: typeFile, pythonFile: pythonFile},
		receipts: func(yield func(store_pebble.ParseFacts, error) bool) {
			for _, f := range facts {
				if !yield(f, nil) {
					return
				}
			}
		},
	}
	for _, f := range facts {
		require.NoError(t, build.AddParsedFile("", f))
	}
	require.NoError(t, runRebuildFrozenMain(t.Context(), lease, rebuildDigest{}, build, census, rebuildTestLinkOptions(t), false))
	winner := *first
	winner.To = receiver.ID
	var saved []*graph.Edge
	require.NoError(t, build.RebuildView().VisitEdges(t.Context(), func(e *graph.Edge) error {
		saved = append(saved, e)
		return nil
	}))
	require.ElementsMatch(t, []*graph.Edge{&winner, deferred}, saved)
}
