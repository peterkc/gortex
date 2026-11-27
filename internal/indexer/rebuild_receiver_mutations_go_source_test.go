package indexer

import (
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/resolver"
	"go.uber.org/zap"
)

const rebuildReceiverGoPath = "p/m.go"

func TestRebuildReceiverMutationsGoSource(t *testing.T) {
	const source = `package p
 type T struct { n int }
 func (s *T) Bump() { s.n = 1 }
 func (s *T) Tick() { s.Bump() }
 func (s *T) Masked() {

	s.n = 2; s.Bump()
	s.Bump()
 }
`
	// Keep both calls on the same lines in both cases; move only the write.
	shifted := strings.Replace(source, "\n\n\ts.n = 2; s.Bump()", "\n\ts.n = 2\n\ts.Bump()", 1)
	t.Setenv("GORTEX_TEST_REBUILD_BUILD_CHILD", "1")
	link := rebuildTestLinkOptions(t)
	for _, tc := range []struct {
		name, source string
		writeLine    int
	}{
		{"same_site", source, 7},
		{"different_site", shifted, 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			root, src := rebuildInputFixture(t)
			writeRebuildTSAliasFixtureFile(t, root, "go.mod", "module example.test/witness\n")
			writeRebuildTSAliasFixtureFile(t, root, rebuildReceiverGoPath, tc.source)
			cfg := config.Default().Index
			reg := parser.NewRegistry()
			reg.Register(languages.NewGoExtractor())
			idx := New(graph.New(), reg, cfg, zap.NewNop())
			t.Cleanup(idx.Close)
			idx.installRebuildParsePolicy(freezeRebuildParsePolicy(root, cfg))
			idx.SetRootPath(root)

			req := rebuildBuildRequest{Root: root, Witness: store_pebble.Witness{Recipe: store_pebble.HeadDigest{1}}}
			err := withRebuildCheckoutBuildInputs(ctx, t.TempDir(), src, req, nil, false,
				func(key string, mode fs.FileMode, member rebuildGitMember, raw *int64, emit func(rebuildSidecarRow) error) (rebuildCheckoutLeaf, error) {
					return idx.freezeRebuildCheckoutLeaf(ctx, src.FrozenRoot(), key, mode, member, nil, 1024, raw, emit)
				}, func(epoch rebuildCheckoutInputs, raw *int64, emit func(rebuildSidecarRow) error) (string, error) {
					return captureRebuildCheckoutResolverInputs(ctx, src, epoch, true, raw, emit)
				}, func(lease string, bound rebuildBuildRequest, inputs []rebuildInput) error {
					require.Len(t, inputs, 1)
					require.Equal(t, rebuildReceiverGoPath, inputs[0].Path)
					require.Equal(t, "go", inputs[0].Language)
					store := rebuildParseStore{dir: t.TempDir(), limit: 1 << 20}
					recipe := rebuildDigest{1}
					_, reused, err := idx.loadRebuildFile(ctx, src, store, inputs[0], recipe, rebuildTransformBounds{})
					require.NoError(t, err)
					require.False(t, reused)
					census := &rebuildExtractorCensus{
						admitted:          1,
						byPath:            map[string]string{rebuildReceiverGoPath: "go"},
						originByGraphPath: map[string]string{rebuildReceiverGoPath: rebuildReceiverGoPath},
					}
					job := rebuildFileStages{inputs: inputs, recipes: map[string]rebuildDigest{"go": recipe}}
					receipts := idx.rebuildDataflowReceipts(ctx, job, store, census)
					build, err := store_pebble.OpenBuild(filepath.Join(lease, "working"), store_pebble.FullProfile)
					require.NoError(t, err)
					defer func() { require.NoError(t, build.Close()) }()
					for facts, err := range receipts {
						require.NoError(t, err)
						requireRebuildGoReceiverLinks(t, facts.Edges, "unresolved::*.n", "unresolved::*.Bump", tc.writeLine)
						require.NoError(t, build.AddParsedFile("", facts))
					}

					// Decode the same receipt afresh, before resolving on native Graph.
					native := graph.New()
					for facts, err := range receipts {
						require.NoError(t, err)
						native.AddBatch(facts.Nodes, facts.Edges)
					}
					stats, err := resolver.New(native).ResolveAllContext(ctx)
					require.NoError(t, err)
					require.False(t, stats.Incomplete)
					require.NoError(t, runRebuildFrozenMain(ctx, lease, rebuildDigest(bound.Witness.FrozenInputs), build, census, link, bound.SkipPlaceholderSources))
					checkRebuildGoReceiverOutputs(t, build.RebuildView(), native, tc.writeLine)
					_, err = build.RebuildCapabilities(ctx, receipts, processExecMechanism, rebuildProcessImport,
						rebuildReceiverRules(), 1<<20)
					require.NoError(t, err)
					want := slices.Collect(native.EdgesByKind(graph.EdgeAccessesField))
					slices.SortFunc(want, compareRebuildTestFileEdgeKeys)
					var got []*graph.Edge
					require.NoError(t, build.RebuildView().VisitEdges(ctx, func(e *graph.Edge) error {
						if e.Kind == graph.EdgeAccessesField {
							got = append(got, e)
						}
						return nil
					}))
					require.Equal(t, want, got)
					return nil
				})
			require.NoError(t, err)
		})
	}
}

func requireRebuildGoReceiverLinks(t *testing.T, edges []*graph.Edge, writeTo, callTo string, writeLine int) {
	t.Helper()
	const p = rebuildReceiverGoPath
	var got []graph.EdgeIdentity
	for _, e := range edges {
		switch e.Kind {
		case graph.EdgeCalls:
			require.Equal(t, true, e.Meta["recv_self"], "%s:%d", e.From, e.Line)
			require.NotContains(t, e.Meta, "recv_field")
		case graph.EdgeWrites:
		default:
			continue
		}
		require.Equal(t, "T", e.Meta["receiver_type"], "%s:%d", e.From, e.Line)
		got = append(got, graph.EdgeIdentityFor(e))
	}
	require.ElementsMatch(t, []graph.EdgeIdentity{
		{From: p + "::T.Bump", To: writeTo, Kind: graph.EdgeWrites, FilePath: p, Line: 3},
		{From: p + "::T.Masked", To: writeTo, Kind: graph.EdgeWrites, FilePath: p, Line: writeLine},
		{From: p + "::T.Tick", To: callTo, Kind: graph.EdgeCalls, FilePath: p, Line: 4},
		{From: p + "::T.Masked", To: callTo, Kind: graph.EdgeCalls, FilePath: p, Line: 7},
		{From: p + "::T.Masked", To: callTo, Kind: graph.EdgeCalls, FilePath: p, Line: 8},
	}, got)
}

func checkRebuildGoReceiverOutputs(t *testing.T, view store_pebble.RebuildView, native *graph.Graph, writeLine int) {
	t.Helper()
	const p = rebuildReceiverGoPath
	const bump, tick, masked, field = p + "::T.Bump", p + "::T.Tick", p + "::T.Masked", p + "::T.n"
	const firstCall = 7
	ctx := t.Context()
	var before []*graph.Edge
	require.NoError(t, view.VisitEdges(ctx, func(e *graph.Edge) error {
		before = append(before, e)
		require.NotEqual(t, graph.EdgeAccessesField, e.Kind, "capabilities must not be present before the turn")
		return nil
	}))
	requireRebuildGoReceiverLinks(t, before, field, bump, writeLine)
	requireRebuildGoReceiverLinks(t, native.AllEdges(), field, bump, writeLine)

	write := func(from string, line int) *graph.Edge {
		return &graph.Edge{From: from, To: field, Kind: graph.EdgeAccessesField, FilePath: p, Line: line,
			Origin: graph.OriginASTResolved, Meta: map[string]any{"access": "write"}}
	}
	inferred := func(from string, line int) *graph.Edge {
		return &graph.Edge{From: from, To: field, Kind: graph.EdgeAccessesField, FilePath: p, Line: line,
			Origin: graph.OriginASTInferred, Meta: map[string]any{"access": "write", "indirect": true, "via": "Bump"}}
	}
	wantDirect := []*graph.Edge{write(bump, 3), write(masked, writeLine)}

	var candidates []*graph.Edge
	scratch := t.TempDir()
	_, err := view.VisitReceiverMutations(ctx, scratch, 1<<20, rebuildReceiverRules(), func(e *graph.Edge) error {
		candidates = append(candidates, e)
		return nil
	})
	require.NoError(t, err)
	require.ElementsMatch(t, []*graph.Edge{inferred(tick, 4), inferred(masked, firstCall)}, candidates)
	assertRebuildReceiverScratchEmpty(t, scratch)
	var after []*graph.Edge
	require.NoError(t, view.VisitEdges(ctx, func(e *graph.Edge) error {
		after = append(after, e)
		return nil
	}))
	require.Equal(t, before, after, "receiver visit must not write candidates")

	wantClaims := []indirectMutSpec{{from: tick, to: field, file: p, via: "Bump", line: 4},
		{from: masked, to: field, file: p, via: "Bump", line: firstCall}}
	require.ElementsMatch(t, wantClaims, indirectMutationEdges(native))
	// Only compare the SQLite receiver kernel, not a second link or graph.
	sqlite, err := store_sqlite.Open(filepath.Join(t.TempDir(), "native.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sqlite.Close()) })
	require.NoError(t, sqlite.AddBatchChecked(native.AllNodes(), native.AllEdges()))
	require.ElementsMatch(t, wantClaims, indirectMutationEdges(sqlite))

	// Election already consumed Masked's later call. Only the first claim
	// can meet a direct row here, and only an equal full identity masks it.
	wantFinal := append(slices.Clone(wantDirect), inferred(tick, 4))
	if writeLine != firstCall {
		wantFinal = append(wantFinal, inferred(masked, firstCall))
	}
	_, _, _, err = synthesizeCapabilityEdges(native)
	require.NoError(t, err)
	nativeRows := slices.Collect(native.EdgesByKind(graph.EdgeAccessesField))
	require.ElementsMatch(t, wantFinal, nativeRows)
}
