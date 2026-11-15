package indexer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

// contractStateGraph embeds *graph.Graph (auto-satisfying graph.Store) and
// adds the ContractStateStore capability, standing in for the on-disk backend
// so a test can observe the marker without opening SQLite.
type contractStateGraph struct {
	*graph.Graph

	state map[string]graph.ContractState
}

func newContractStateGraph() *contractStateGraph {
	return &contractStateGraph{Graph: graph.New(), state: map[string]graph.ContractState{}}
}

func (c *contractStateGraph) SetContractState(st graph.ContractState) error {
	c.state[st.RepoPrefix] = st
	return nil
}

func (c *contractStateGraph) GetContractState(repoPrefix string) (graph.ContractState, bool, error) {
	st, ok := c.state[repoPrefix]
	if !ok {
		return graph.ContractState{RepoPrefix: repoPrefix}, false, nil
	}
	return st, true, nil
}

// writeGoRouteRepo lays down a repo whose single route is registered with a Go
// 1.22 method pattern — the shape the contract extractor turns into an
// EdgeHandlesRoute.
func writeGoRouteRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	src := `package app

import "net/http"

type handlers struct{}

func (h handlers) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/users", h.listUsers)
}

func (h handlers) listUsers(w http.ResponseWriter, r *http.Request) {
	_ = w
	_ = r
}
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "app.go"), []byte(src), 0o644))
	return dir
}

func handlesRouteEdgeCount(g graph.Store) int {
	n := 0
	for _, e := range g.AllEdges() {
		if e.Kind == graph.EdgeHandlesRoute {
			n++
		}
	}
	return n
}

func newRouteIndexer(t *testing.T, g graph.Store) *Indexer {
	t.Helper()
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	return New(g, reg, config.Default().Index, zap.NewNop())
}

// A lost deferred tail leaves the contract tier EMPTY — not degraded — and
// writes no completion marker. This is the state issue #436 reported: the node
// layer is complete, `analyze kind=routes` returns a clean zero, and per-file
// mtime admission will never re-extract the tier on a later restart. The
// absent marker is what makes that zero reportable instead of silent.
func TestLostContractTailLeavesTierEmptyAndUnmarked(t *testing.T) {
	dir := writeGoRouteRepo(t)
	g := newContractStateGraph()
	idx := newRouteIndexer(t, g)
	idx.SetDeferResolve(true)

	_, err := idx.Index(dir)
	require.NoError(t, err)

	// The tail never runs: the parked registry is the only place the
	// extracted contracts live.
	require.NotNil(t, idx.pendingContractReg, "contracts should be parked for the deferred tail")
	require.Zero(t, handlesRouteEdgeCount(g), "an uncommitted tier has no route edges")

	_, found, err := g.GetContractState("")
	require.NoError(t, err)
	require.False(t, found, "no marker may be recorded for a tier that was never committed")
}

// Running the deferred tail commits the tier and records the marker, so a
// later query can tell a built tier from an unbuilt one.
func TestDeferredContractTailRecordsCompletionMarker(t *testing.T) {
	dir := writeGoRouteRepo(t)
	g := newContractStateGraph()
	idx := newRouteIndexer(t, g)
	idx.SetDeferResolve(true)

	_, err := idx.Index(dir)
	require.NoError(t, err)
	idx.ResolveAll()
	idx.runDeferredContracts()

	require.NotZero(t, handlesRouteEdgeCount(g), "the tail must commit the route edges")

	st, found, err := g.GetContractState("")
	require.NoError(t, err)
	require.True(t, found, "the completed tail must record a contract-tier marker")
	require.Positive(t, st.ContractCount, "the marker records what the pass committed")
	require.Positive(t, st.CompletedAt)
}

// The inline (non-deferred) full-index path commits contracts itself and must
// record the same marker — otherwise a single-repo daemon would report every
// healthy tier as unbuilt.
func TestInlineContractPassRecordsCompletionMarker(t *testing.T) {
	dir := writeGoRouteRepo(t)
	g := newContractStateGraph()
	idx := newRouteIndexer(t, g)

	_, err := idx.Index(dir)
	require.NoError(t, err)

	require.NotZero(t, handlesRouteEdgeCount(g))
	st, found, err := g.GetContractState("")
	require.NoError(t, err)
	require.True(t, found, "the inline contract pass must record a contract-tier marker")
	require.Positive(t, st.ContractCount)
}

// A repo that genuinely declares no contracts still gets a marker: the marker
// records that the pass RAN, not that it found something. Without this, "no
// contracts here" would be indistinguishable from "the tier was never built" —
// the very confusion the marker exists to remove.
func TestContractPassRecordsMarkerForRepoWithNoContracts(t *testing.T) {
	dir := t.TempDir()
	src := `package app

func Add(a, b int) int { return a + b }
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "math.go"), []byte(src), 0o644))

	g := newContractStateGraph()
	idx := newRouteIndexer(t, g)
	_, err := idx.Index(dir)
	require.NoError(t, err)

	_, found, err := g.GetContractState("")
	require.NoError(t, err)
	require.True(t, found, "a contract pass that found nothing still completed")
}

// The bulk-load path parses into an in-memory shadow and drains it to disk
// afterwards, so the inline contract pass commits while idx.graph is the
// throwaway shadow. The marker must still land on the store the contract nodes
// end up in — otherwise a shadow-backed index would drain a healthy tier to
// disk and leave it looking permanently unbuilt.
func TestContractMarkerLandsOnDiskStoreBehindShadow(t *testing.T) {
	dir := writeGoRouteRepo(t)

	s, err := store_sqlite.Open(filepath.Join(t.TempDir(), "store.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	idx := newRouteIndexer(t, graph.Store(s))
	idx.SetRootPath(dir)
	_, err = idx.IndexCtx(context.Background(), dir)
	require.NoError(t, err)

	require.NotZero(t, handlesRouteEdgeCount(s), "route edges must reach the disk store")
	st, found, err := s.GetContractState("")
	require.NoError(t, err)
	require.True(t, found, "the marker must reach the disk store, not die with the shadow")
	require.Positive(t, st.ContractCount)
}

// A backend without the capability (the in-memory graph, which rebuilds its
// tier on every start) must not fail the commit.
func TestContractMarkerIsNoOpWithoutCapableStore(t *testing.T) {
	dir := writeGoRouteRepo(t)
	g := graph.New()
	idx := newRouteIndexer(t, g)

	_, err := idx.Index(dir)
	require.NoError(t, err)
	require.NotZero(t, handlesRouteEdgeCount(g))
}

// Keep graph behavior and completion-state telemetry, but hide cleanup methods.
type springContractStateStore struct {
	graph.Store
	graph.ContractStateStore
}

func TestSpringMigrationFailureRefusesContractCompletion(t *testing.T) {
	for _, deferred := range []bool{false, true} {
		name := "inline"
		if deferred {
			name = "deferred"
		}
		t.Run(name, func(t *testing.T) {
			// Given legacy Spring rows without cleanup support, when the real
			// index/contract tail runs, then it reports failure and marks no completion.
			root := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(root, "application.yml"), []byte("app.value: hidden\n"), 0600))
			base := newContractStateGraph()
			base.AddBatch([]*graph.Node{
				{ID: "repo/application.yml", Kind: graph.KindFile, Name: "application.yml", FilePath: "repo/application.yml", Language: "yaml", RepoPrefix: "repo"},
				{ID: "cfg::spring::app.value", Kind: graph.KindConfigKey, FilePath: "repo/application.yml", Meta: map[string]any{"source": "spring"}},
			}, nil)
			store := &springContractStateStore{Store: base, ContractStateStore: base}
			core, logs := observer.New(zap.WarnLevel)
			idx := New(store, newTestRegistry(), config.Default().Index, zap.New(core))
			t.Cleanup(idx.Close)
			idx.SetRepoPrefix("repo")
			idx.SetDeferResolve(deferred)
			idx.SetDeferGlobalPasses(true)
			result, err := idx.IndexCtx(t.Context(), root)
			if deferred {
				require.NoError(t, err)
				pending := idx.pendingContractReg
				require.NotNil(t, pending)
				idx.runDeferredContracts()
				assert.Same(t, pending, idx.pendingContractReg, "failed deferred work must remain pending")
				assert.Len(t, logs.FilterMessage("indexer: deferred contract commit refused").All(), 1)
			} else {
				assert.ErrorIs(t, err, errors.ErrUnsupported)
				assert.Nil(t, result)
			}
			assert.ErrorIs(t, idx.contractInputError(), errors.ErrUnsupported)
			_, complete, readErr := store.GetContractState("repo")
			require.NoError(t, readErr)
			assert.False(t, complete, "refused migration must not publish a completion marker")
			assert.NotNil(t, base.GetNode("cfg::spring::app.value"))
			assert.Nil(t, base.GetNode("cfg::spring::repo::repo::app.value"))
		})
	}
}

func TestContractInputFailureRefusesRowsAndCompletion(t *testing.T) {
	for _, failed := range []bool{false, true} {
		name := "source_available"
		if failed {
			name = "source_read_fails"
		}
		t.Run(name, func(t *testing.T) {
			g := newContractStateGraph()
			g.AddBatch([]*graph.Node{
				{ID: "repo/router.go::register", Name: "register", Kind: graph.KindFunction, FilePath: "repo/router.go", RepoPrefix: "repo", Language: "go"},
				{ID: "repo/handler.go::serve", Name: "serve", Kind: graph.KindFunction, FilePath: "repo/handler.go", RepoPrefix: "repo", Language: "go", StartLine: 3, EndLine: 3},
			}, nil)
			core, logs := observer.New(zap.WarnLevel)
			idx := New(g, newTestRegistry(), config.Default().Index, zap.New(core))
			t.Cleanup(idx.Close)
			idx.SetRootPath(t.TempDir())
			idx.SetRepoPrefix("repo")
			idx.contractAnalysisOnly = true
			readFailure := errors.New("accepted handler source failed")
			var paths []string
			idx.contractAcceptedFileSource = func(path string) ([]byte, error) {
				paths = append(paths, path)
				if failed {
					return nil, readFailure
				}
				return []byte("package app\nimport \"net/http\"\nfunc serve(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }\n"), nil
			}
			reg := contracts.NewRegistry()
			reg.Add(contracts.Contract{ID: "http::GET::/users", Type: contracts.ContractHTTP, Role: contracts.RoleProvider, FilePath: "repo/router.go", SymbolID: "repo/router.go::register", Line: 1, RepoPrefix: "repo", Meta: map[string]any{"handler_ident": "serve"}})
			idx.pendingContractReg = reg
			// Given accepted handler source fails during non-Spring enrichment,
			// when the deferred caller commits, then refusal precedes rows and tier completion.
			idx.runDeferredContracts()
			require.Contains(t, paths, "repo/handler.go", "the commit must actually read the accepted handler source")
			_, complete, err := g.GetContractState("repo")
			require.NoError(t, err)
			if failed {
				assert.ErrorIs(t, idx.contractInputError(), readFailure)
				assert.Len(t, logs.FilterMessage("indexer: deferred contract commit refused").All(), 1)
				assert.Same(t, reg, idx.pendingContractReg)
				assert.Nil(t, idx.contractRegistry, "refused input must not become the committed registry")
				assert.Len(t, g.AllNodes(), 2, "no contract rows may be published after input failure")
				assert.Empty(t, g.AllEdges(), "no contract edges may be published after input failure")
				assert.False(t, complete, "refused input must not publish a tier completion marker")
			} else {
				require.NoError(t, idx.contractInputError())
				require.NotNil(t, g.GetNode("http::GET::/users"))
				require.Positive(t, handlesRouteEdgeCount(g))
				require.True(t, complete)
				require.Nil(t, idx.pendingContractReg)
				require.Empty(t, logs.FilterMessage("indexer: deferred contract commit refused").All())
			}
		})
	}
}
