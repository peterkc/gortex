package gortex

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graph/storetest"
)

// TestStorageContract runs the K12 open-path cases of the storage contract
// suite (design-c.md 1.8) for the public Engine on every backend in the
// suite's backend list. The Engine opens the store its constructor selects.
func TestStorageContract(t *testing.T) {
	for _, backend := range storetest.Backends() {
		t.Run("K12/EngineIndexesIntoAPrivateStoreAndRemovesIt/"+backend, func(t *testing.T) {
			testEngineIndexesIntoAPrivateStoreAndRemovesIt(t, backend)
		})
		t.Run("K12/EngineStorePathSurvivesClose/"+backend, func(t *testing.T) {
			testEngineStorePathSurvivesClose(t, backend)
		})
	}
}

func withK12Backend(name string) Option {
	return func(s *settings) { s.backendName = name }
}

// k12PrivateTemp points the process temp directory at a fresh path and
// returns it, so a case can see every directory the Engine creates there.
func k12PrivateTemp(t *testing.T) string {
	t.Helper()
	tmp := filepath.Join(t.TempDir(), "tmp")
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", tmp, err)
	}
	t.Setenv("TMPDIR", tmp)
	t.Setenv("TMP", tmp)
	t.Setenv("TEMP", tmp)
	if got := os.TempDir(); got != tmp {
		t.Fatalf("os.TempDir() = %q, want the private directory %q", got, tmp)
	}
	return tmp
}

func tempEntries(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

// testEngineIndexesIntoAPrivateStoreAndRemovesIt: a default Engine keeps its
// store in its own temp directory, answers fixture F's R through the public
// queries after Index, removes the directory on Close, and a second Close is a
// no-op.
func testEngineIndexesIntoAPrivateStoreAndRemovesIt(t *testing.T, backend string) {
	if backend != "sqlite" {
		t.Fatalf("Engine has no backend selection and opens SQLite, not listed backend %q", backend)
	}
	f := storetest.NewFixtureF(t)
	tmp := k12PrivateTemp(t)

	engine, err := New(WithWorkers(1), withK12Backend(backend))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	requireK12SQLiteEngine(t, engine)
	if got := tempEntries(t, tmp); len(got) != 1 {
		_ = engine.Close()
		t.Fatalf("New made temp entries %v, want one store directory", got)
	}
	if _, err := engine.Index(f.R.Root); err != nil {
		_ = engine.Close()
		t.Fatalf("Index R: %v", err)
	}
	assertEngineServesFixtureR(t, engine, f, storetest.E1)

	if err := engine.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := tempEntries(t, tmp); len(got) != 0 {
		t.Fatalf("Close left %v in the temp directory", got)
	}
	if err := engine.Close(); err != nil {
		t.Fatalf("a second Close: %v", err)
	}
}

// testEngineStorePathSurvivesClose: an Engine on an explicit store path
// creates no temp directory, keeps its store on Close, and a new Engine on the
// same path answers fixture F's R without indexing again.
func testEngineStorePathSurvivesClose(t *testing.T, backend string) {
	if backend != "sqlite" {
		t.Fatalf("Engine has no backend selection and opens SQLite, not listed backend %q", backend)
	}
	f := storetest.NewFixtureF(t)
	tmp := k12PrivateTemp(t)
	path := filepath.Join(t.TempDir(), "engine", "store")

	engine, err := New(WithWorkers(1), WithStorePath(path), withK12Backend(backend))
	if err != nil {
		t.Fatalf("New on %s: %v", path, err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	requireK12SQLiteEngine(t, engine)
	if _, err := engine.Index(f.R.Root); err != nil {
		_ = engine.Close()
		t.Fatalf("Index R: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		_ = engine.Close()
		t.Fatalf("the explicit store path %s is missing: %v", path, err)
	}
	if got := tempEntries(t, tmp); len(got) != 0 {
		_ = engine.Close()
		t.Fatalf("an Engine on an explicit store path created temp entries %v", got)
	}
	if err := engine.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the explicit store path did not survive Close: %v", err)
	}
	if got := tempEntries(t, tmp); len(got) != 0 {
		t.Fatalf("Close left %v in the temp directory", got)
	}

	reopened, err := New(WithStorePath(path), withK12Backend(backend))
	if err != nil {
		t.Fatalf("reopen %s: %v", path, err)
	}
	defer func() {
		if err := reopened.Close(); err != nil {
			t.Errorf("Close the reopened Engine: %v", err)
		}
	}()
	requireK12SQLiteEngine(t, reopened)
	assertEngineServesFixtureR(t, reopened, f, storetest.E1)
}

// assertEngineServesFixtureR compares what the Engine answers for fixture F's
// R at epoch with the graph F declares, as the real Go extractor produces it
// from F's sources in a single-repository index (ids and files without the
// repository prefix): every declared node through GetSymbol and FindSymbols,
// and each declared function's calls through GetCallChain, at exactly the
// lines F's sources call from. A call into another declared node is compared
// with its callee and the call into S by line only, because the extractor
// names that callee itself; the failed lookup has no callee to chain to.
func assertEngineServesFixtureR(t *testing.T, engine *Engine, f *storetest.FixtureF, epoch storetest.Epoch) {
	t.Helper()
	local := func(s string) string { return strings.TrimPrefix(s, f.R.Prefix+"/") }
	payload := f.Payload(f.R.Prefix, epoch)
	for _, want := range payload.Nodes {
		got := engine.GetSymbol(local(want.ID))
		if got == nil {
			t.Errorf("GetSymbol(%s) found nothing", local(want.ID))
			continue
		}
		if got.Kind != want.Kind || got.Name != want.Name || got.FilePath != local(want.FilePath) ||
			got.StartLine != want.StartLine || got.EndLine != want.EndLine || got.Language != want.Language {
			t.Errorf("GetSymbol(%s) = kind=%s name=%s file=%s lines=%d-%d language=%s, want %s %s %s %d-%d %s",
				local(want.ID), got.Kind, got.Name, got.FilePath, got.StartLine, got.EndLine, got.Language,
				want.Kind, want.Name, local(want.FilePath), want.StartLine, want.EndLine, want.Language)
		}
		var found []string
		for _, node := range engine.FindSymbols(want.Name, want.Kind) {
			found = append(found, node.ID)
		}
		if !slices.Equal(found, []string{local(want.ID)}) {
			t.Errorf("FindSymbols(%s, %s) = %v, want [%s]", want.Name, want.Kind, found, local(want.ID))
		}
	}
	declared := make(map[string]bool, len(payload.Nodes))
	for _, node := range payload.Nodes {
		declared[local(node.ID)] = true
	}
	for _, function := range payload.Nodes {
		if function.Kind != graph.KindFunction {
			continue
		}
		caller := local(function.ID)
		var want []*graph.Edge
		for _, edge := range payload.Edges {
			if edge.From == function.ID && edge.Kind == graph.EdgeCalls {
				want = append(want, &graph.Edge{From: caller, To: local(edge.To), Kind: edge.Kind, Line: edge.Line})
			}
		}
		got := engine.GetCallChain(caller, 1, 0).Edges
		if wantKeys, gotKeys := callKeys(want, caller, declared), callKeys(got, caller, declared); !slices.Equal(gotKeys, wantKeys) {
			t.Errorf("GetCallChain(%s) calls %v, want %v", caller, gotKeys, wantKeys)
		}
	}
}

// callKeys names, sorted, each call out of caller by its line and, when the
// callee is a node F declares, by the callee too. The failed lookup is left
// out: a call chain follows only calls whose callee is a node.
func callKeys(edges []*graph.Edge, caller string, declared map[string]bool) []string {
	var keys []string
	for _, edge := range edges {
		if edge.From != caller || edge.Kind != graph.EdgeCalls || strings.HasPrefix(edge.To, graph.UnresolvedMarker) {
			continue
		}
		key := fmt.Sprintf("line %d", edge.Line)
		if declared[edge.To] {
			key += " -> " + edge.To
		}
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

func requireK12SQLiteEngine(t *testing.T, engine *Engine) {
	t.Helper()
	if store, ok := graph.Store(engine.store).(*store_sqlite.Store); !ok || store == nil {
		t.Fatalf("sqlite Engine opened %T, want a SQLite store", engine.store)
	}
}
