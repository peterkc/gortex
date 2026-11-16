package resolver

import (
	"fmt"
	"iter"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

// Count full rows returned by the production reads, not a modeled query.
type frameworkFileReadCounter struct {
	graph.Store
	rows int
	reads int
	files []string
}

func (s *frameworkFileReadCounter) GetFileNodesByPaths(files []string) map[string][]*graph.Node {
	s.reads++
	s.files = slices.Clone(files)
	groups := s.Store.GetFileNodesByPaths(files)
	for _, nodes := range groups {
		s.rows += len(nodes)
	}
	return groups
}

type frameworkKindReadCounter struct {
	*frameworkFileReadCounter
	stream graph.NodesInFilesByKindStreamer
	streamReads int
	kinds []graph.NodeKind
}

func (s *frameworkKindReadCounter) NodesInFilesByKindSeq(files []string, kinds []graph.NodeKind) iter.Seq2[string, []*graph.Node] {
	return func(yield func(string, []*graph.Node) bool) {
		s.streamReads++
		s.files = slices.Clone(files)
		s.kinds = slices.Clone(kinds)
		for file, nodes := range s.stream.NodesInFilesByKindSeq(files, kinds) {
			s.rows += len(nodes)
			if !yield(file, nodes) {
				return
			}
		}
	}
}

func newFrameworkKindReadCounter(t *testing.T) *frameworkKindReadCounter {
	t.Helper()
	store := openFrameworkScopedSQLiteStore(t)
	return &frameworkKindReadCounter{
		frameworkFileReadCounter: &frameworkFileReadCounter{Store: store},
		stream: store.(graph.NodesInFilesByKindStreamer),
	}
}

func seedFrameworkMixedKindFiles(store graph.Store) []string {
	files := []string{"repo/a.go", "repo/b.go", "repo/c.go"}
	var nodes []*graph.Node
	for _, file := range files {
		for i := 0; i < 120; i++ {
			kind := graph.KindLocal
			switch {
			case i < 5:
				kind = graph.KindFunction
			case i < 10:
				kind = graph.KindMethod
			}
			nodes = append(nodes, frameworkTestNode("repo", file, fmt.Sprintf("%s::%03d", file, i), kind, "symbol", "go", map[string]any{"payload": "retain"}))
		}
	}
	store.AddBatch(nodes, nil)
	return files
}

func TestFrameworkFileFrontierKindReadCountsSelectedRows(t *testing.T) {
	// Given 360 full rows and duplicate paths/kinds, when the frontier requests
	// functions and methods, then only 30 rows decode, in first-kind/ID order.
	store := newFrameworkKindReadCounter(t)
	files := seedFrameworkMixedKindFiles(store.Store)
	files = append(files, files[0], "", "missing.go")
	kinds := []graph.NodeKind{graph.KindMethod, "", graph.KindFunction, graph.KindMethod}
	originalFiles, originalKinds := slices.Clone(files), slices.Clone(kinds)
	got := slices.Collect(frameworkFileFrontierNodes(store, []string{"repo", "repo"}, files, kinds...))
	require.Positive(t, store.rows, "the production read must run before equality is checked")
	require.Equal(t, 30, store.rows, "kind-filtered frontier must not decode all 360 full rows")
	require.Equal(t, 1, store.streamReads)
	require.Zero(t, store.reads)
	require.Equal(t, []graph.NodeKind{graph.KindMethod, graph.KindFunction}, store.kinds)
	require.Equal(t, []string{"repo/a.go", "repo/b.go", "repo/c.go", "missing.go"}, store.files)
	require.Equal(t, originalFiles, files)
	require.Equal(t, originalKinds, kinds)
	legacy := &frameworkFileReadCounter{Store: store.Store}
	want := slices.Collect(frameworkFileFrontierNodes(legacy, []string{"repo"}, files, kinds...))
	require.Equal(t, 360, legacy.rows)
	require.Len(t, got, 30)
	require.Equal(t, want, got)
	require.Equal(t, graph.KindMethod, got[0].Kind)
	require.Equal(t, graph.KindFunction, got[15].Kind)
	var yielded int
	for range frameworkFileFrontierNodes(store, nil, files, kinds...) {
		yielded++
		break
	}
	require.Equal(t, 1, yielded, "a consumer may stop after one node")
}

func TestFrameworkFileFrontierKindReadKeepsUnfilteredAndEmptyInputs(t *testing.T) {
	// Given empty inputs, when no usable kind or file is requested, then do
	// not read. An omitted kind list still performs the unfiltered census.
	for _, tc := range []struct {
		name string
		files []string
		kinds []graph.NodeKind
		wantRows int
	}{
		{"no_files", nil, []graph.NodeKind{graph.KindFunction}, 0},
		{"empty_files", []string{"", ""}, []graph.NodeKind{graph.KindFunction}, 0},
		{"empty_kinds", []string{"repo/a.go"}, []graph.NodeKind{"", ""}, 0},
		{"unfiltered", []string{"repo/a.go"}, nil, 120},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newFrameworkKindReadCounter(t)
			seedFrameworkMixedKindFiles(store.Store)
			got := slices.Collect(frameworkFileFrontierNodes(store, nil, tc.files, tc.kinds...))
			require.Equal(t, tc.wantRows, store.rows)
			require.Len(t, got, tc.wantRows)
			if len(tc.files) == 0 || len(tc.kinds) == 0 || tc.name == "empty_kinds" {
				require.Zero(t, store.streamReads)
			}
			if tc.wantRows > 0 {
				require.Equal(t, 1, store.reads)
			}
		})
	}
	require.Empty(t, slices.Collect(frameworkFileFrontierNodes(nil, nil, []string{"repo/a.go"}, graph.KindFunction)))
}

func TestFrameworkFileFrontierKindReadMatchesUnfiltered(t *testing.T) {
	// Given memory, SQLite, unknown wrappers and DeltaWriter, when kinds and
	// repo filters run, then complete results equal the unfiltered read path.
	for _, backend := range []string{"memory", "sqlite", "wrapper", "delta"} {
		t.Run(backend, func(t *testing.T) {
			var store graph.Store
			switch backend {
			case "memory":
				store = graph.New()
			default:
				store = openFrameworkScopedSQLiteStore(t)
			}
			files := seedFrameworkMixedKindFiles(store)
			store.AddNode(frameworkTestNode("foreign", files[0], "foreign::method", graph.KindMethod, "foreign", "go", nil))
			switch backend {
			case "wrapper":
				store = &frameworkFileReadCounter{Store: store}
			case "delta":
				store = graph.NewDeltaWriter(store, nil)
				store.AddNode(frameworkTestNode("repo", files[0], "repo::delta", graph.KindFunction, "delta", "go", nil))
			}
			kinds := []graph.NodeKind{graph.KindMethod, "", graph.KindFunction, graph.KindMethod}
			legacy := &frameworkFileReadCounter{Store: store}
			for _, prefixes := range [][]string{nil, {"repo", "repo"}, {"foreign"}, {"absent"}} {
				want := slices.Collect(frameworkFileFrontierNodes(legacy, prefixes, files, kinds...))
				got := slices.Collect(frameworkFileFrontierNodes(store, prefixes, files, kinds...))
				require.Positive(t, legacy.rows)
				require.Equal(t, want, got)
			}
		})
	}
}

func TestFrameworkFileFrontierKindReadReloadsAtEachPass(t *testing.T) {
	// Given a reusable sequence, when durable rows change between passes,
	// then every pass rereads rather than reusing a pass-local expansion.
	store := newFrameworkKindReadCounter(t)
	files := seedFrameworkMixedKindFiles(store.Store)
	seq := frameworkFileFrontierNodes(store, []string{"repo"}, files, graph.KindMethod)
	first := slices.Collect(seq)
	require.Len(t, first, 15)
	added := frameworkTestNode("repo", files[0], "repo::added", graph.KindMethod, "added", "go", nil)
	store.AddNode(added)
	second := slices.Collect(seq)
	require.Len(t, second, 16)
	require.Contains(t, second, store.GetNode(added.ID))
	store.EvictFile(files[0])
	require.Len(t, slices.Collect(seq), 10)
	store.AddNode(added)
	require.Len(t, slices.Collect(seq), 11)
	require.Equal(t, 4, store.streamReads)
	require.Zero(t, store.reads)
}

func TestFrameworkDerivedKindReadPreservesPassOutputAndCaps(t *testing.T) {
	// Given a changed file larger than the retained-row cap, when one pass
	// publishes a node/edge, then the next sees them without retaining the
	// earlier pass's unrelated dynamic lookup, on both backend read paths.
	for _, backend := range []string{"memory", "sqlite", "sqlite_legacy"} {
		t.Run(backend, func(t *testing.T) {
			var store graph.Store
			switch backend {
			case "memory":
				store = graph.New()
			default:
				store = openFrameworkScopedSQLiteStore(t)
			}
			if backend == "sqlite_legacy" {
				store = &frameworkFileReadCounter{Store: store}
			}
			var nodes []*graph.Node
			for i := 0; i < frameworkScopeRetainedRowCap+1; i++ {
				nodes = append(nodes, frameworkTestNode("repo", "changed.go", fmt.Sprintf("repo::%05d", i), graph.KindFunction, "", "go", nil))
			}
			private := frameworkTestNode("repo", "private.go", "repo::private", graph.KindMethod, "private", "go", nil)
			nodes = append(nodes, private)
			store.AddBatch(nodes, nil)
			seed := newFrameworkScopedSeed(store, map[string]bool{"repo": true}, []string{"changed.go"})
			passA := seed.newPassStore()
			require.Len(t, slices.Collect(passA.NodesByKind(graph.KindFunction)), frameworkScopeRetainedRowCap+1)
			require.Len(t, passA.FindNodesByName("private"), 1)
			published := frameworkTestNode("repo", "changed.go", "repo::published", graph.KindMethod, "published", "go", nil)
			edge := &graph.Edge{From: nodes[0].ID, To: published.ID, Kind: graph.EdgeCalls, FilePath: "changed.go", Line: 7}
			require.Equal(t, 1, runLegacyFrameworkSynth(passA, func(g graph.Store) int {
				g.AddBatch([]*graph.Node{published}, []*graph.Edge{edge})
				return 1
			}))
			passB := seed.newPassStore()
			methods := slices.Collect(passB.NodesByKind(graph.KindMethod))
			require.Len(t, methods, 1)
			require.Equal(t, published.ID, methods[0].ID)
			require.True(t, frameworkEdgeSeqContains(passB.EdgesByKind(graph.EdgeCalls), edge))
			for _, pass := range []*frameworkScopedStore{passA, passB} {
				stats := pass.stats()
				require.Positive(t, stats.RetainedRows)
				require.LessOrEqual(t, stats.RetainedRows, stats.RowCap)
				require.LessOrEqual(t, stats.RetainedBytes, stats.ByteCap)
			}
			require.Len(t, store.GetOutEdges(nodes[0].ID), 1)
		})
	}
}
