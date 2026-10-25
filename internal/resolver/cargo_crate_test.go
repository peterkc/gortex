package resolver

import (
	"context"
	"iter"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestCargoImportUntrackedSkipsNodeReads(t *testing.T) {
	g := &resolverBatchCountingStore{Store: graph.New()}
	cr := NewCrossRepo(g)
	cr.SetCargoCrateLookup(func(_, _ string) []CargoCrate { return nil })
	e := &graph.Edge{From: "app/src/lib.rs", To: "external::serde/Item", Kind: graph.EdgeImports, FilePath: "app/src/lib.rs"}
	assert.Nil(t, cr.cargoImportTarget(e, "serde/Item"))
	assert.Zero(t, g.getNodeCalls, "unknown crates must be rejected before caller node reads")
}

type cargoPagingStore struct {
	graph.Store
	fullScans, lightScans, pages, maxSites int
	cursorOpen                             bool
}

func (s *cargoPagingStore) EdgesByKind(kind graph.EdgeKind) iter.Seq[*graph.Edge] {
	s.fullScans++
	return s.Store.EdgesByKind(kind)
}

func (s *cargoPagingStore) EdgesLightSeq(kinds ...graph.EdgeKind) iter.Seq[*graph.Edge] {
	s.lightScans++
	return func(yield func(*graph.Edge) bool) {
		s.cursorOpen = true
		defer func() { s.cursorOpen = false }()
		for e := range graph.EdgesLightSeq(s.Store, kinds...) {
			if !yield(e) {
				return
			}
		}
	}
}

func (s *cargoPagingStore) GetEdgeCandidates(endpoints []graph.EdgeEndpoint, sites []graph.EdgeSite) graph.EdgeCandidateSet {
	if s.cursorOpen {
		panic("hydration while light cursor is open")
	}
	s.pages++
	s.maxSites = max(s.maxSites, len(sites))
	return s.Store.GetEdgeCandidates(endpoints, sites)
}

func TestCargoImportLightPaging(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			var store graph.Store = graph.New()
			if backend == "sqlite" {
				db, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, db.Close()) })
				store = db
			}
			g := &cargoPagingStore{Store: store}
			const count = resolvePendingPageRows + 1
			edges := make([]*graph.Edge, 0, count+1)
			for line := 1; line <= count; line++ {
				edges = append(edges, &graph.Edge{From: "app/src/lib.rs", To: "external::other_crate/Item", Kind: graph.EdgeImports, FilePath: "app/src/lib.rs", Line: line, Meta: map[string]any{"marker": "preserved"}})
			}
			edges = append(edges, &graph.Edge{From: "app/src/lib.rs", To: "external::serde/Item", Kind: graph.EdgeImports, FilePath: "app/src/lib.rs", Line: count + 1})
			g.AddBatch([]*graph.Node{
				{ID: "app/src/lib.rs", Kind: graph.KindFile, FilePath: "app/src/lib.rs", RepoPrefix: "app", Language: "rust"},
				{ID: "lib/src/lib.rs", Kind: graph.KindFile, FilePath: "lib/src/lib.rs", RepoPrefix: "lib", Language: "rust"},
			}, edges)
			cr := NewCrossRepo(g)
			cr.SetCargoCrateLookup(func(_, name string) []CargoCrate {
				if name != "other_crate" {
					return nil
				}
				return []CargoCrate{{Root: "lib/src/lib.rs", Name: "other_crate"}}
			})
			stats := &CrossRepoStats{ByRepo: map[string]int{}}
			_, err := cr.resolveCargoImports(context.Background(), graph.EdgesLightSeq(g, graph.EdgeImports), stats)
			require.NoError(t, err)
			assert.Zero(t, g.fullScans)
			assert.Equal(t, 1, g.lightScans)
			assert.Equal(t, 2, g.pages)
			assert.Equal(t, resolvePendingPageRows, g.maxSites)
			assert.Equal(t, count, stats.Resolved)
			imports := g.GetOutEdges("app/src/lib.rs")
			require.Len(t, imports, count+1)
			for _, edge := range imports {
				if edge.Line == count+1 {
					assert.Equal(t, "external::serde/Item", edge.To)
					assert.False(t, edge.CrossRepo)
					continue
				}
				assert.Equal(t, "lib/src/lib.rs", edge.To)
				assert.True(t, edge.CrossRepo)
				assert.Equal(t, "preserved", edge.Meta["marker"])
			}
		})
	}
}
