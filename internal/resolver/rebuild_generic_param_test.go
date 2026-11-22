package resolver

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestRebuildGenericParamsNativeReplay(t *testing.T) {
	const file, decl = "pkg/use.go", "decl/owner.go"
	const owner, other = decl + "::F", decl + "::G"
	const site, bare = owner + "#body#nested", "unresolved::T"
	const param, otherParam = owner + "#tparam:T", other + "#tparam:T"
	const plugin, verbatim = owner + "#tparam:P#local:plugin", owner + "#plugin:literal"
	// Neither owner nor source has a node. No member_of edges exist and the
	// generic declarations are recorded in a different file from the uses.
	nodes := []*graph.Node{
		{ID: param, Name: "T", Kind: graph.KindGenericParam, FilePath: decl, Language: "go"},
		{ID: otherParam, Name: "T", Kind: graph.KindGenericParam, FilePath: decl, Language: "go"},
		{ID: owner + "#a", Name: "A", Kind: graph.KindGenericParam, FilePath: decl, Language: "go"},
		{ID: owner + "#b", Name: "A", Kind: graph.KindGenericParam, FilePath: decl, Language: "go"},
		{ID: owner + "#c", Name: "A", Kind: graph.KindGenericParam, FilePath: decl, Language: "go"},
		{ID: owner + "#tparam:J", Name: "J", Kind: graph.KindGenericParam, FilePath: decl, Language: "typescript"},
		{ID: owner + "#type:K", Name: "K", Kind: graph.KindType, FilePath: decl, Language: "go"},
		{ID: "no-owner", Name: "N", Kind: graph.KindGenericParam, FilePath: decl, Language: "go"},
		{ID: "#opaque#tparam:H", Name: "H", Kind: graph.KindGenericParam, FilePath: decl, Language: "go"},
		{ID: plugin, Name: "P", Kind: graph.KindGenericParam, FilePath: decl, Language: "go"},
		{ID: verbatim, Name: "T ", Kind: graph.KindGenericParam, FilePath: decl, Language: "go"},
	}
	// Exact matching these names would succeed if the bare-target shape check
	// were replaced by a prefix check alone.
	for _, name := range []string{"", "pkg.T", "*.T", "T:label", "T#label"} {
		nodes = append(nodes, &graph.Node{
			ID: owner + "#plugin:" + name, Name: name, Kind: graph.KindGenericParam, FilePath: decl, Language: "go",
		})
	}

	var input, want []*graph.Edge
	for i, row := range []struct {
		from      string
		kind      graph.EdgeKind
		line      int
		old, want string
	}{
		{site, graph.EdgeReferences, 1, bare, param},
		{site, graph.EdgeTypedAs, 2, bare, param},
		{owner, graph.EdgeReturns, 3, bare, param},
		{site, graph.EdgeInstantiates, 4, bare, param},
		{other + "#body", graph.EdgeReferences, 5, bare, otherParam},
		{site, graph.EdgeReferences, 6, "unresolved::T ", verbatim},
		// A third declaration must not restore a winner.
		{site, graph.EdgeReferences, 7, "unresolved::A", "unresolved::A"},
		{site, graph.EdgeReferences, 8, "unresolved::J", "unresolved::J"},
		{site, graph.EdgeReferences, 9, "unresolved::K", "unresolved::K"},
		{"no-owner", graph.EdgeReferences, 10, "unresolved::N", "unresolved::N"},
		{"#opaque#site", graph.EdgeReferences, 11, "unresolved::H", "unresolved::H"},
		{site, graph.EdgeCalls, 12, bare, bare},
		{site, graph.EdgeReferences, 13, "repo::" + bare, "repo::" + bare},
		{site, graph.EdgeReferences, 14, "T", "T"},
		{site, graph.EdgeReferences, 15, "unresolved::", "unresolved::"},
		{site, graph.EdgeReferences, 16, "unresolved::pkg.T", "unresolved::pkg.T"},
		{site, graph.EdgeReferences, 17, "unresolved::*.T", "unresolved::*.T"},
		{site, graph.EdgeReferences, 18, "unresolved::T:label", "unresolved::T:label"},
		{site, graph.EdgeReferences, 19, "unresolved::T#label", "unresolved::T#label"},
		// An empty want means the old row is consumed without a visible replacement.
		{site, graph.EdgeReferences, 20, bare, ""},
		{site, graph.EdgeReferences, 20, param, param},
		// The plugin target is visible to references, but not to instantiates.
		{site, graph.EdgeInstantiates, 21, "unresolved::P", ""},
		{site, graph.EdgeReferences, 21, "unresolved::P", plugin},
	} {
		e := &graph.Edge{From: row.from, To: row.old, Kind: row.kind, FilePath: file, Line: row.line,
			Confidence: 0.25, ConfidenceLabel: "heuristic", Origin: "extractor", Tier: "syntax",
			Meta: map[string]any{"old_target": row.old}}
		if i%2 == 0 {
			e.Confidence, e.ConfidenceLabel = 0.75, "confirmed"
			e.Origin, e.Tier, e.CrossRepo = "fixture", "semantic", true
		}
		input = append(input, e)
		if row.want != "" {
			saved := *e
			saved.To = row.want
			want = append(want, &saved)
		}
	}
	rebuildFnPointerSortRows(input)
	rebuildFnPointerSortRows(want)

	sql, err := store_sqlite.Open(filepath.Join(t.TempDir(), "native.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sql.Close()) })
	require.NoError(t, sql.AddBatchChecked(nodes, input))
	saved := sql.AllEdges()
	rebuildFnPointerSortRows(saved)
	require.Equal(t, input, saved)
	r := New(sql)
	census, stats := r.buildPostBareAttributionCensus(defaultAttributionCensusLimits)
	require.NotNil(t, census)
	defer census.close()
	require.Empty(t, stats.fallbackReason)
	require.Equal(t, len(input), stats.rowsScanned)
	r.bindGenericParamRefsFromCensus(census)
	native := sql.AllEdges()
	rebuildFnPointerSortRows(native)
	require.Equal(t, want, native)

	build := openRebuildBareScopeFixture(t)
	build.AddBatch(nodes, input)
	require.Equal(t, input, rebuildFnPointerSaved(t, build))
	count, err := build.RebuildGenericParams(t.Context())
	require.NoError(t, err)
	require.Equal(t, 9, count) // seven visible moves and two consumed rows
	require.Equal(t, native, rebuildFnPointerSaved(t, build))

	count, err = build.RebuildGenericParams(t.Context())
	require.NoError(t, err)
	require.Zero(t, count)
	require.Equal(t, native, rebuildFnPointerSaved(t, build))
}

func TestRunRebuildMainGenericParamsAfterDataflow(t *testing.T) {
	const file, owner = "pkg/use.go", "pkg/use.go::F"
	const param = owner + "#tparam:T::Run"
	ref := &graph.Edge{From: owner, To: "unresolved::T", Kind: graph.EdgeReferences, FilePath: file, Line: 5}
	arg := &graph.Edge{From: "unresolved::arg", To: "unresolved::*.Run", Kind: graph.EdgeArgOf, FilePath: file, Line: 5}
	build := openRebuildBareScopeFixture(t)
	build.AddBatch([]*graph.Node{
		{ID: param, Name: "T", Kind: graph.KindGenericParam, FilePath: file, Language: "go"},
	}, []*graph.Edge{ref, arg})
	require.Equal(t, []*graph.Edge{ref, arg}, rebuildFnPointerSaved(t, build))

	// The raw frontier enables the tails even when no caller is selected.
	// Binding ref too early would supply a resolved Run site to dataflow.
	seen := 0
	options := store_pebble.RebuildLinkOptions{ScratchParent: t.TempDir(), Partitions: 1, SpillBytes: 1}
	require.NoError(t, RunRebuildMain(t.Context(), build, options, RebuildMainInputs{
		SelectCallers: func(page []*graph.Edge) ([]*graph.Edge, error) {
			seen += len(page)
			return nil, nil
		},
	}))
	require.Equal(t, 2, seen)
	bound := *ref
	bound.To = param
	require.Equal(t, []*graph.Edge{&bound, arg}, rebuildFnPointerSaved(t, build))
}
