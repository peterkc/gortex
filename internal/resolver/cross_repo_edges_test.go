package resolver

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

type liveCrossRepoBatchSpy struct{ *crossRepoBatchSpy }

func (s *liveCrossRepoBatchSpy) MarkEdgesCrossRepo(edges []*graph.Edge) int {
	for _, edge := range edges {
		edge.CrossRepo = true
	}
	return len(edges)
}

type crossRepoBatchSpy struct {
	graph.Store
	addEdgeCalls  int
	addBatchCalls int
}

func (s *crossRepoBatchSpy) AddEdge(edge *graph.Edge) {
	s.addEdgeCalls++
	s.Store.AddEdge(edge)
}

func (s *crossRepoBatchSpy) AddBatch(nodes []*graph.Node, edges []*graph.Edge) {
	s.addBatchCalls++
	s.Store.AddBatch(nodes, edges)
}

func detectCrossRepoEdges(t *testing.T, g graph.Store) int {
	t.Helper()
	count, err := DetectCrossRepoEdges(g)
	assert.NoError(t, err)
	return count
}

func detectCrossRepoEdgesForRepos(t *testing.T, g graph.Store, repos []string) int {
	t.Helper()
	count, err := DetectCrossRepoEdgesForRepos(g, repos)
	assert.NoError(t, err)
	return count
}

// countOutEdgesByKind returns how many out-edges of the given kind the
// node fromID has.
func countOutEdgesByKind(g graph.Store, fromID string, kind graph.EdgeKind) int {
	n := 0
	for _, e := range g.GetOutEdges(fromID) {
		if e.Kind == kind {
			n++
		}
	}
	return n
}

// firstOutEdgeByKind returns the first out-edge of fromID with the given
// kind, or nil.
func firstOutEdgeByKind(g graph.Store, fromID string, kind graph.EdgeKind) *graph.Edge {
	for _, e := range g.GetOutEdges(fromID) {
		if e.Kind == kind {
			return e
		}
	}
	return nil
}

func TestDetectCrossRepoEdges_CallsAcrossRepos(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "repoA/a.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "repoA/a.go", RepoPrefix: "repoA"})
	g.AddNode(&graph.Node{ID: "repoB/b.go::Callee", Kind: graph.KindFunction, Name: "Callee", FilePath: "repoB/b.go", RepoPrefix: "repoB"})

	base := &graph.Edge{
		From: "repoA/a.go::Caller", To: "repoB/b.go::Callee",
		Kind: graph.EdgeCalls, FilePath: "repoA/a.go", Line: 7,
		Confidence: 0.8, Origin: graph.OriginASTInferred,
	}
	g.AddEdge(base)

	emitted := detectCrossRepoEdges(t, g)
	assert.Equal(t, 1, emitted)

	// Parallel edge materialised.
	cr := firstOutEdgeByKind(g, "repoA/a.go::Caller", graph.EdgeCrossRepoCalls)
	if assert.NotNil(t, cr, "expected a cross_repo_calls edge") {
		assert.Equal(t, "repoB/b.go::Callee", cr.To)
		assert.Equal(t, "repoA/a.go", cr.FilePath)
		assert.Equal(t, 7, cr.Line)
		// Origin / confidence inherited from the base edge.
		assert.Equal(t, graph.OriginASTInferred, cr.Origin)
		assert.Equal(t, 0.8, cr.Confidence)
		assert.True(t, cr.CrossRepo)
		assert.Equal(t, "calls", cr.Meta["base_kind"])
		assert.Equal(t, "repoA", cr.Meta["source_repo"])
		assert.Equal(t, "repoB", cr.Meta["target_repo"])
	}

	// Base edge keeps its kind and now carries the bool flag.
	assert.Equal(t, graph.EdgeCalls, base.Kind)
	assert.True(t, base.CrossRepo)
}

func TestDetectCrossRepoEdges_ImplementsAndExtends(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "repoA/a.go::Impl", Kind: graph.KindType, Name: "Impl", FilePath: "repoA/a.go", RepoPrefix: "repoA"})
	g.AddNode(&graph.Node{ID: "repoB/b.go::Iface", Kind: graph.KindInterface, Name: "Iface", FilePath: "repoB/b.go", RepoPrefix: "repoB"})
	g.AddNode(&graph.Node{ID: "repoA/a.go::Child", Kind: graph.KindType, Name: "Child", FilePath: "repoA/a.go", RepoPrefix: "repoA"})
	g.AddNode(&graph.Node{ID: "repoB/b.go::Parent", Kind: graph.KindType, Name: "Parent", FilePath: "repoB/b.go", RepoPrefix: "repoB"})

	g.AddEdge(&graph.Edge{From: "repoA/a.go::Impl", To: "repoB/b.go::Iface", Kind: graph.EdgeImplements, FilePath: "repoA/a.go", Line: 3, Origin: graph.OriginASTResolved})
	g.AddEdge(&graph.Edge{From: "repoA/a.go::Child", To: "repoB/b.go::Parent", Kind: graph.EdgeExtends, FilePath: "repoA/a.go", Line: 4, Origin: graph.OriginASTResolved})

	emitted := detectCrossRepoEdges(t, g)
	assert.Equal(t, 2, emitted)

	assert.Equal(t, 1, countOutEdgesByKind(g, "repoA/a.go::Impl", graph.EdgeCrossRepoImplements))
	assert.Equal(t, 1, countOutEdgesByKind(g, "repoA/a.go::Child", graph.EdgeCrossRepoExtends))
}

func TestDetectCrossRepoEdges_SameRepoUntouched(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "repoA/a.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "repoA/a.go", RepoPrefix: "repoA"})
	g.AddNode(&graph.Node{ID: "repoA/b.go::Callee", Kind: graph.KindFunction, Name: "Callee", FilePath: "repoA/b.go", RepoPrefix: "repoA"})

	base := &graph.Edge{From: "repoA/a.go::Caller", To: "repoA/b.go::Callee", Kind: graph.EdgeCalls, FilePath: "repoA/a.go", Line: 1}
	g.AddEdge(base)

	emitted := detectCrossRepoEdges(t, g)
	assert.Equal(t, 0, emitted)
	assert.Equal(t, 0, countOutEdgesByKind(g, "repoA/a.go::Caller", graph.EdgeCrossRepoCalls))
	assert.False(t, base.CrossRepo)
}

func TestDetectCrossRepoEdges_SkipsStubsAndUnstampedNodes(t *testing.T) {
	g := graph.New()
	// Caller in repoA; one edge to an unresolved stub, one to a node
	// whose RepoPrefix was never stamped.
	g.AddNode(&graph.Node{ID: "repoA/a.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "repoA/a.go", RepoPrefix: "repoA"})
	g.AddNode(&graph.Node{ID: "loose.go::Loose", Kind: graph.KindFunction, Name: "Loose", FilePath: "loose.go"}) // no RepoPrefix

	g.AddEdge(&graph.Edge{From: "repoA/a.go::Caller", To: "unresolved::Missing", Kind: graph.EdgeCalls, FilePath: "repoA/a.go", Line: 1})
	g.AddEdge(&graph.Edge{From: "repoA/a.go::Caller", To: "external::net/http", Kind: graph.EdgeCalls, FilePath: "repoA/a.go", Line: 2})
	g.AddEdge(&graph.Edge{From: "repoA/a.go::Caller", To: "loose.go::Loose", Kind: graph.EdgeCalls, FilePath: "repoA/a.go", Line: 3})

	emitted := detectCrossRepoEdges(t, g)
	assert.Equal(t, 0, emitted)
	assert.Equal(t, 0, countOutEdgesByKind(g, "repoA/a.go::Caller", graph.EdgeCrossRepoCalls))
}

func TestDetectCrossRepoEdges_Idempotent(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "repoA/a.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "repoA/a.go", RepoPrefix: "repoA"})
	g.AddNode(&graph.Node{ID: "repoB/b.go::Callee", Kind: graph.KindFunction, Name: "Callee", FilePath: "repoB/b.go", RepoPrefix: "repoB"})
	g.AddEdge(&graph.Edge{From: "repoA/a.go::Caller", To: "repoB/b.go::Callee", Kind: graph.EdgeCalls, FilePath: "repoA/a.go", Line: 7})

	first := detectCrossRepoEdges(t, g)
	second := detectCrossRepoEdges(t, g)
	assert.Equal(t, 1, first)
	// Re-running re-counts the same relationship but graph.AddEdge
	// dedupes by edgeKey — the parallel edge is not duplicated.
	assert.Equal(t, 1, second)
	assert.Equal(t, 1, countOutEdgesByKind(g, "repoA/a.go::Caller", graph.EdgeCrossRepoCalls))
}

// The pass must not feed on its own output: a cross_repo_* edge is not
// itself a base kind, so a second pass finds nothing new from it.
func TestDetectCrossRepoEdges_DoesNotRecurseOnOwnOutput(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "repoA/a.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "repoA/a.go", RepoPrefix: "repoA"})
	g.AddNode(&graph.Node{ID: "repoB/b.go::Callee", Kind: graph.KindFunction, Name: "Callee", FilePath: "repoB/b.go", RepoPrefix: "repoB"})
	g.AddEdge(&graph.Edge{From: "repoA/a.go::Caller", To: "repoB/b.go::Callee", Kind: graph.EdgeCalls, FilePath: "repoA/a.go", Line: 7})

	detectCrossRepoEdges(t, g)
	// Exactly one base + one parallel edge — never a cross_repo_calls
	// parallel of a cross_repo_calls edge.
	assert.Equal(t, 1, countOutEdgesByKind(g, "repoA/a.go::Caller", graph.EdgeCalls))
	assert.Equal(t, 1, countOutEdgesByKind(g, "repoA/a.go::Caller", graph.EdgeCrossRepoCalls))
}

func TestDetectCrossRepoEdgesForReposUsesIncidentScopeAndBatchWrite(t *testing.T) {
	base := graph.New()
	for _, node := range []*graph.Node{
		{ID: "repoA/a.go::A", Kind: graph.KindFunction, FilePath: "repoA/a.go", RepoPrefix: "repoA"},
		{ID: "repoB/b.go::B", Kind: graph.KindFunction, FilePath: "repoB/b.go", RepoPrefix: "repoB"},
		{ID: "repoC/c.go::C", Kind: graph.KindFunction, FilePath: "repoC/c.go", RepoPrefix: "repoC"},
		{ID: "repoD/d.go::D", Kind: graph.KindFunction, FilePath: "repoD/d.go", RepoPrefix: "repoD"},
	} {
		base.AddNode(node)
	}
	inbound := &graph.Edge{From: "repoA/a.go::A", To: "repoB/b.go::B", Kind: graph.EdgeCalls, FilePath: "repoA/a.go", Line: 3}
	outside := &graph.Edge{From: "repoC/c.go::C", To: "repoD/d.go::D", Kind: graph.EdgeCalls, FilePath: "repoC/c.go", Line: 4}
	base.AddBatch(nil, []*graph.Edge{inbound, outside})
	spy := &liveCrossRepoBatchSpy{crossRepoBatchSpy: &crossRepoBatchSpy{Store: base}}

	emitted := detectCrossRepoEdgesForRepos(t, spy, []string{"repoB"})
	assert.Equal(t, 1, emitted, "an inbound relationship must be included in the changed-target frontier")
	assert.True(t, inbound.CrossRepo)
	assert.False(t, outside.CrossRepo)
	assert.Equal(t, 0, spy.addEdgeCalls)
	assert.Equal(t, 1, spy.addBatchCalls)
	assert.Equal(t, 1, countOutEdgesByKind(base, inbound.From, graph.EdgeCrossRepoCalls))
	assert.Equal(t, 0, countOutEdgesByKind(base, outside.From, graph.EdgeCrossRepoCalls))

	// The exact batched existence lookup makes a warm replay write-free.
	detectCrossRepoEdgesForRepos(t, spy, []string{"repoB"})
	assert.Equal(t, 1, spy.addBatchCalls)
}

func TestDetectCrossRepoEdgesDetachedRowsRequireMarker(t *testing.T) {
	for _, detector := range []struct {
		name string
		run  func(graph.Store, *graph.Edge) (int, error)
	}{
		{"full", func(g graph.Store, e *graph.Edge) (int, error) { return DetectCrossRepoEdges(g) }},
		{"skip", func(g graph.Store, e *graph.Edge) (int, error) {
			count, skipped, err := DetectCrossRepoEdgesWithSkip(g)
			assert.False(t, skipped, "unsupported must not mean single repo")
			return count, err
		}},
		{"repos", func(g graph.Store, e *graph.Edge) (int, error) {
			return DetectCrossRepoEdgesForRepos(g, []string{"repoB"})
		}},
		{"reindexes", func(g graph.Store, e *graph.Edge) (int, error) {
			return DetectCrossRepoEdgesForReindexes(g, []graph.EdgeReindex{{Edge: e}})
		}},
		{"files", func(g graph.Store, e *graph.Edge) (int, error) {
			return DetectCrossRepoEdgesForFiles(g, []string{"repoB/b.go"})
		}},
		{"mutation", func(g graph.Store, e *graph.Edge) (int, error) {
			return DetectCrossRepoEdgesForMutationFiles(g, nil, []string{"repoB/b.go"})
		}},
	} {
		t.Run(detector.name, func(t *testing.T) {
			// Given detached base rows without a flag marker, when any detector
			// runs, then it must refuse before adding a contradictory parallel row.
			base, err := store_sqlite.Open(filepath.Join(t.TempDir(), "cross-repo.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := base.Close(); err != nil {
					t.Error(err)
				}
			})
			base.AddBatch([]*graph.Node{
				{ID: "repoA/a.go::A", Kind: graph.KindFunction, FilePath: "repoA/a.go", RepoPrefix: "repoA"},
				{ID: "repoB/b.go::B", Kind: graph.KindFunction, FilePath: "repoB/b.go", RepoPrefix: "repoB"},
			}, []*graph.Edge{
				{From: "repoA/a.go::A", To: "repoB/b.go::B", Kind: graph.EdgeCalls, FilePath: "repoA/a.go", Line: 4, Meta: map[string]any{"opaque": "retain"}},
			})
			spy := &crossRepoBatchSpy{Store: base}
			edge := firstOutEdgeByKind(base, "repoA/a.go::A", graph.EdgeCalls)
			count, err := detector.run(spy, edge)
			assert.Zero(t, count)
			assert.ErrorIs(t, err, errors.ErrUnsupported)
			assert.Zero(t, spy.addBatchCalls, "unsupported detached flag must not publish parallel rows")
			assert.False(t, edge.CrossRepo, "refusal must not mutate candidate pointers")
			assert.False(t, firstOutEdgeByKind(base, edge.From, graph.EdgeCalls).CrossRepo)
			assert.Zero(t, countOutEdgesByKind(base, edge.From, graph.EdgeCrossRepoCalls))
		})
	}
}

func TestCrossRepoMaterializationRequiresKnownPersistence(t *testing.T) {
	for _, marked := range []bool{false, true} {
		t.Run(map[bool]string{false: "unknown_live_wrapper", true: "already_marked"}[marked], func(t *testing.T) {
			// Given a Graph wrapper without a marker, when an unmarked base
			// needs promotion, then reject; already-marked rows still materialize.
			base := graph.New()
			base.AddBatch([]*graph.Node{
				{ID: "a::A", RepoPrefix: "a"}, {ID: "b::B", RepoPrefix: "b"},
			}, nil)
			edge := &graph.Edge{From: "a::A", To: "b::B", Kind: graph.EdgeCalls, CrossRepo: marked}
			base.AddEdge(edge)
			spy := &crossRepoBatchSpy{Store: base}
			count, err := DetectCrossRepoEdges(spy)
			if marked {
				assert.NoError(t, err)
				assert.Equal(t, 1, count)
				assert.Equal(t, 1, spy.addBatchCalls)
			} else {
				assert.ErrorIs(t, err, errors.ErrUnsupported)
				assert.Zero(t, count)
				assert.Zero(t, spy.addBatchCalls)
			}
			assert.Equal(t, marked, edge.CrossRepo)
		})
	}
}

func TestCrossRepoMaterializationPreflightsTheWholeBatch(t *testing.T) {
	// Given mixed marked/unmarked and duplicate rows, when any required flag
	// lacks persistence, then no pointers or parallel rows change in the batch.
	spy := &crossRepoBatchSpy{Store: graph.New()}
	marked := &graph.Edge{From: "a::A", To: "b::B", Kind: graph.EdgeCalls, CrossRepo: true}
	unmarked := &graph.Edge{From: "a::C", To: "b::D", Kind: graph.EdgeCalls}
	rows := []graph.CrossRepoCandidateRow{
		{Edge: nil}, {Edge: &graph.Edge{Kind: graph.EdgeCrossRepoCalls}},
		{Edge: marked, FromRepo: "a", ToRepo: "b"},
		{Edge: unmarked, FromRepo: "a", ToRepo: "b"},
		{Edge: unmarked, FromRepo: "a", ToRepo: "b"},
	}
	count, err := materializeCrossRepoCandidates(spy, rows)
	assert.ErrorIs(t, err, errors.ErrUnsupported)
	assert.Zero(t, count)
	assert.False(t, unmarked.CrossRepo)
	assert.True(t, marked.CrossRepo)
	assert.Zero(t, spy.addBatchCalls)
	count, err = materializeCrossRepoCandidates(spy, rows[:2])
	assert.NoError(t, err)
	assert.Zero(t, count)
	count, skipped, err := DetectCrossRepoEdgesWithSkip(spy)
	assert.NoError(t, err)
	assert.Zero(t, count)
	assert.True(t, skipped)
}

func TestCrossRepoSQLiteFlagPersistenceSurvivesOwnershipChanges(t *testing.T) {
	// Given SQLite's marker, when an endpoint moves repositories and is removed
	// and re-added, then each fresh base/parallel layer stays consistent on repeats.
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "cross-repo.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	from := &graph.Node{ID: "a/a.go::A", Kind: graph.KindFunction, FilePath: "a/a.go", RepoPrefix: "a"}
	to := &graph.Node{ID: "b/b.go::B", Kind: graph.KindFunction, FilePath: "b/b.go", RepoPrefix: "b"}
	store.AddBatch([]*graph.Node{from, to}, nil)
	for _, repo := range []string{"b", "c", "b"} {
		// File replacement evicts both layers before the endpoint is re-added.
		store.EvictFile(to.FilePath)
		to.RepoPrefix = repo
		store.AddNode(to)
		store.AddEdge(&graph.Edge{From: from.ID, To: to.ID, Kind: graph.EdgeCalls, FilePath: from.FilePath, Line: 3, Meta: map[string]any{"opaque": "retain"}})
		assert.Equal(t, 1, detectCrossRepoEdges(t, store))
		assert.Equal(t, 1, detectCrossRepoEdgesForRepos(t, store, []string{repo, repo, ""}))
		base := firstOutEdgeByKind(store, from.ID, graph.EdgeCalls)
		assert.True(t, base.CrossRepo)
		assert.Equal(t, "retain", base.Meta["opaque"], "marker must not rewrite opaque metadata")
		parallel := firstOutEdgeByKind(store, from.ID, graph.EdgeCrossRepoCalls)
		assert.Equal(t, repo, parallel.Meta["target_repo"])
		assert.Equal(t, 1, countOutEdgesByKind(store, from.ID, graph.EdgeCrossRepoCalls))
	}
}

func TestCrossRepoMaterializationRefusalReachesStatsOwners(t *testing.T) {
	for _, operation := range []string{"repo", "file", "mutation", "go_module_batch"} {
		t.Run(operation, func(t *testing.T) {
			// Given required flag promotion without a marker, when a stats-only
			// owner runs, then its pass records one named incomplete refusal.
			base := graph.New()
			base.AddBatch([]*graph.Node{
				{ID: "a/a.go::A", Kind: graph.KindFunction, FilePath: "a/a.go", RepoPrefix: "a"},
				{ID: "b/b.go::B", Kind: graph.KindFunction, FilePath: "b/b.go", RepoPrefix: "b"},
			}, nil)
			edge := &graph.Edge{From: "a/a.go::A", To: "b/b.go::B", Kind: graph.EdgeCalls, FilePath: "a/a.go", Line: 3}
			base.AddEdge(edge)
			spy := &crossRepoBatchSpy{Store: base}
			cr := NewCrossRepo(spy)
			core, logs := observer.New(zap.InfoLevel)
			cr.SetLogger(zap.New(core))
			var stats *CrossRepoStats
			switch operation {
			case "repo":
				stats = cr.ResolveForRepo("b")
			case "file":
				stats = cr.ResolveForFile("b", "b.go")
			case "mutation":
				stats = cr.ResolveMutationFrontiers(nil, nil, []string{"b/b.go"})
			case "go_module_batch":
				stats = &CrossRepoStats{ByRepo: make(map[string]int)}
				assert.True(t, cr.applyGoModuleReindexes([]graph.EdgeReindex{{Edge: edge}}, stats), "base application succeeded; derived refusal must not queue it as dropped")
			}
			assert.ErrorIs(t, stats.MaterializationError, errors.ErrUnsupported)
			assert.True(t, stats.Incomplete)
			assert.Equal(t, 1, logs.FilterMessage("cross-repo materialization refused").Len())
			assert.False(t, edge.CrossRepo)
			assert.Zero(t, countOutEdgesByKind(base, edge.From, graph.EdgeCrossRepoCalls))
		})
	}
}

func TestCrossRepoResolveAllAllowsPersistedFlagsWithoutMarker(t *testing.T) {
	// Given normal resolution that persists CrossRepo with its base reindex,
	// when the store lacks a separate marker, then the full pass still succeeds.
	base, _ := crossRepoStreamFixture(2)
	store := &crossRepoBatchSpy{Store: base}
	stats, err := NewCrossRepo(store).ResolveAllContext(t.Context())
	assert.NoError(t, err)
	assert.False(t, stats.Incomplete)
	assert.NoError(t, stats.MaterializationError)
	assert.Equal(t, 2, stats.Resolved)
	assert.Equal(t, 2, countOutEdgesByKind(base, "repoA/pkg/a.go::Caller", graph.EdgeCrossRepoCalls))
}
