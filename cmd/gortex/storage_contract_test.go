package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/storetest"
	"github.com/zzet/gortex/internal/indexer"
)

// TestStorageContract runs the K12 open-path cases of the storage contract
// suite (design-c.md 1.8) for the daemon entry, the eval store and the init
// index on every backend in the suite's backend list. The daemon opens the
// backend named by its --backend flag; the eval and init paths open the
// store their own constructors select.
func TestStorageContract(t *testing.T) {
	for _, backend := range storetest.Backends() {
		t.Run("K12/DaemonColdBuildServesFixtureF/"+backend, func(t *testing.T) {
			testDaemonColdBuildServesFixtureF(t, backend)
		})
		t.Run("K12/DaemonIncrementalReindexReplacesTheEditedFile/"+backend, func(t *testing.T) {
			testDaemonIncrementalReindexReplacesTheEditedFile(t, backend)
		})
		t.Run("K12/DaemonWatchReindexesTheEditedFile/"+backend, func(t *testing.T) {
			testDaemonWatchReindexesTheEditedFile(t, backend)
		})
		t.Run("K12/DaemonServesTheWorktreeCheckoutView/"+backend, func(t *testing.T) {
			testDaemonServesTheWorktreeCheckoutView(t, backend)
		})
		t.Run("K12/DaemonRestartServesTheClosedStore/"+backend, func(t *testing.T) {
			testDaemonRestartServesTheClosedStore(t, backend)
		})
		t.Run("K12/EvalStoreIsPrivateAndRemovedOnCleanup/"+backend, func(t *testing.T) {
			testEvalStoreIsPrivateAndRemovedOnCleanup(t, backend)
		})
		t.Run("K12/InitIndexesIntoAPrivateStoreAndRemovesIt/"+backend, func(t *testing.T) {
			testInitIndexesIntoAPrivateStoreAndRemovesIt(t, backend)
		})
	}
}

// k12CaseDeadline bounds one case, so a wedged daemon fails the case instead
// of hanging until the package timeout.
const k12CaseDeadline = 2 * time.Minute

func k12Context(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), k12CaseDeadline)
	t.Cleanup(cancel)
	return ctx
}

// k12Daemon is one daemon built through the daemon entry over fixture F, with
// R and S tracked and warmed.
type k12Daemon struct {
	fixture *storetest.FixtureF
	state   *daemonState
	watcher *indexer.MultiWatcher
	mcp     *mcpDispatcher
	// close stops the watcher, the contract runtime and the stack, once.
	close func()
}

// k12DaemonEnv points the daemon at private per-user directories, a config
// with semantic enrichment off, and a private store opened as backend.
func k12DaemonEnv(t *testing.T, backend string) {
	t.Helper()
	base := t.TempDir()
	home := filepath.Join(base, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatalf("mkdir home: %v", err)
	}
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	configPath := filepath.Join(base, ".gortex.yaml")
	if err := os.WriteFile(configPath, []byte("semantic:\n  enabled: false\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	restoreCfg, restoreBackend, restorePath := cfgFile, daemonBackend, daemonBackendPath
	t.Cleanup(func() { cfgFile, daemonBackend, daemonBackendPath = restoreCfg, restoreBackend, restorePath })
	cfgFile = configPath
	daemonBackend = backend
	daemonBackendPath = filepath.Join(base, "store")
}

// openK12Daemon builds the daemon state and installs its contract runtime,
// as the daemon entry does before warmup. The returned func closes both once.
func openK12Daemon(t *testing.T) (*daemonState, func()) {
	t.Helper()
	state, err := buildDaemonState(zap.NewNop())
	if err != nil {
		t.Fatalf("buildDaemonState: %v", err)
	}
	runtime, err := installDaemonContractAnalysis(state, zap.NewNop())
	if err != nil {
		_ = state.shared.Close()
		t.Fatalf("install the contract runtime: %v", err)
	}
	var once sync.Once
	closeAll := func() {
		once.Do(func() {
			// Cleanup runs after the case's context is canceled; shutdown gets
			// its own bound, as the daemon's teardown does.
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := runtime.Close(ctx); err != nil {
				t.Errorf("close the contract runtime: %v", err)
			}
			if err := state.shared.Close(); err != nil {
				t.Errorf("close the daemon stack: %v", err)
			}
		})
	}
	t.Cleanup(closeAll)
	return state, closeAll
}

// startK12Daemon builds the daemon over fixture F on backend, tracks R and S,
// seeds the checkout catalog and runs warmup to its committed-base
// publications. The watcher runs only when watch is set, so a case that
// reindexes explicitly is not raced by it.
func startK12Daemon(ctx context.Context, t *testing.T, backend string, watch bool) *k12Daemon {
	t.Helper()
	f := storetest.NewFixtureF(t)
	k12DaemonEnv(t, backend)
	state, closeState := openK12Daemon(t)
	for _, repo := range []storetest.Repository{f.R, f.S} {
		registered, err := state.lifecycle.Register(ctx, config.RepoEntry{Path: repo.Root}, indexer.TrackSourceCLI)
		if err != nil {
			t.Fatalf("register %s: %v", repo.Prefix, err)
		}
		if registered.Prefix != repo.Prefix {
			t.Fatalf("register %s gave prefix %q, want %q", repo.Root, registered.Prefix, repo.Prefix)
		}
	}
	if err := state.lifecycle.Seed(ctx); err != nil {
		t.Fatalf("seed the checkout catalog: %v", err)
	}
	startWatcher := func(context.Context, *indexer.MultiWatcher) error { return nil }
	if watch {
		startWatcher = func(_ context.Context, w *indexer.MultiWatcher) error { return w.Start() }
	}
	watcher, _ := warmupDaemonStateContext(ctx, state, zap.NewNop(), func() {}, startWatcher)
	if watcher == nil {
		closeState()
		t.Fatal("warmup returned no watcher")
	}
	var once sync.Once
	closeAll := func() {
		once.Do(func() {
			if err := watcher.Stop(); err != nil {
				t.Errorf("stop the watcher: %v", err)
			}
			closeState()
		})
	}
	t.Cleanup(closeAll)
	if err := state.basePublisher.Wait(ctx); err != nil {
		t.Fatalf("wait for the committed-base publications: %v", err)
	}
	return &k12Daemon{
		fixture: f, state: state, watcher: watcher, close: closeAll,
		mcp: newMCPDispatcher(state.mcpServer, state.multiIndexer, zap.NewNop()),
	}
}

// testDaemonColdBuildServesFixtureF: after a cold build through the daemon,
// the store and the MCP server answer fixture F at E1, the version both
// primary working trees hold.
func testDaemonColdBuildServesFixtureF(t *testing.T, backend string) {
	ctx := k12Context(t)
	d := startK12Daemon(ctx, t, backend, false)
	f := d.fixture
	assertServesFixture(t, d.state.graph, f, f.R.Prefix, storetest.E1, false)
	assertServesFixture(t, d.state.graph, f, f.S.Prefix, storetest.E1, false)

	run := fixtureNode(t, f, f.R.Prefix, storetest.E1, "r/app/run.go::Run")
	answer, _ := d.callTool(ctx, t, f.R.Root, "search", map[string]any{"operation": "symbols", "query": run.Name})
	want := []any{map[string]any{
		"id": run.ID, "name": run.Name, "kind": string(run.Kind), "file_path": run.FilePath,
		"start_line": float64(run.StartLine), "repo_prefix": run.RepoPrefix,
	}}
	if got := projectResults(answer["results"], "id", "name", "kind", "file_path", "start_line", "repo_prefix"); !jsonEqual(got, want) {
		t.Errorf("MCP search %q from R = %v, want %v", run.Name, got, want)
	}

	call := fixtureEdge(t, f, f.R.Prefix, storetest.E1, "r/app/twice.go::Twice", run.ID)
	answer, _ = d.callTool(ctx, t, f.R.Root, "relations", map[string]any{
		"operation": "callers", "target": map[string]any{"symbol": run.ID},
	})
	wantEdges := []any{map[string]any{
		"from": call.From, "to": call.To, "kind": string(call.Kind), "file_path": call.FilePath, "line": float64(call.Line),
	}}
	if got := projectResults(answer["edges"], "from", "to", "kind", "file_path", "line"); !jsonEqual(got, wantEdges) {
		t.Errorf("MCP callers of %s = %v, want %v", run.ID, got, wantEdges)
	}
}

// testDaemonIncrementalReindexReplacesTheEditedFile: R's app/run.go goes back
// to its E0 bytes and the daemon's incremental reindex replaces that file's
// facts: R is served at E0, with Twice's call into Run kept.
func testDaemonIncrementalReindexReplacesTheEditedFile(t *testing.T, backend string) {
	ctx := k12Context(t)
	d := startK12Daemon(ctx, t, backend, false)
	f := d.fixture
	path := restoreFixtureFile(t, f, "app/run.go", storetest.E0)
	result, err := d.state.multiIndexer.IncrementalReindexRepo(f.R.Prefix, []string{path})
	if err != nil {
		t.Fatalf("incremental reindex of %s: %v", path, err)
	}
	if len(result.FailedFiles) != 0 {
		t.Fatalf("incremental reindex failed files: %v", result.FailedFiles)
	}
	assertServesFixture(t, d.state.graph, f, f.R.Prefix, storetest.E0, false)
}

// testDaemonWatchReindexesTheEditedFile: the same edit reaches the store
// through the daemon's watcher alone.
func testDaemonWatchReindexesTheEditedFile(t *testing.T, backend string) {
	ctx := k12Context(t)
	d := startK12Daemon(ctx, t, backend, true)
	f := d.fixture
	path := restoreFixtureFile(t, f, "app/run.go", storetest.E0)
	for {
		select {
		case event := <-d.watcher.Events():
			if event.FilePath != path {
				continue
			}
			assertServesFixture(t, d.state.graph, f, f.R.Prefix, storetest.E0, false)
			return
		case <-ctx.Done():
			t.Fatalf("the watcher reported no change of %s: %v", path, ctx.Err())
		}
	}
}

// testDaemonServesTheWorktreeCheckoutView: R's committed base is published
// for its linked worktree R2, an MCP request from R2 waits for R2's own view
// and answers from it, and the Materializer's view of R2 serves R at E0 while
// the primary still serves E1.
func testDaemonServesTheWorktreeCheckoutView(t *testing.T, backend string) {
	ctx := k12Context(t)
	d := startK12Daemon(ctx, t, backend, false)
	f := d.fixture
	assertBasePublished(t, d.state, f.R.Prefix)

	run := fixtureNode(t, f, f.R.Prefix, storetest.E0, "r/app/run.go::Run")
	call := fixtureEdge(t, f, f.R.Prefix, storetest.E0, "r/app/twice.go::Twice", run.ID)
	worktree := fixtureCheckout(t, f, storetest.CheckoutR2).Root
	answer, meta := d.callTool(ctx, t, worktree, "relations", map[string]any{
		"operation": "callers", "target": map[string]any{"symbol": run.ID},
		"require_fresh": true, "require_exact": true,
		"wait_deadline": time.Now().Add(45 * time.Second).Format(time.RFC3339),
	})
	freshness, _ := meta["freshness"].(map[string]any)
	checkoutID, _ := freshness["checkout_id"].(string)
	if checkoutID == "" || freshness["actual_view"] != "worktree:"+checkoutID || freshness["exact"] != true {
		t.Fatalf("MCP callers from R2 were answered by %v, want R2's exact worktree view", freshness)
	}
	wantEdges := []any{map[string]any{
		"from": call.From, "to": call.To, "kind": string(call.Kind), "file_path": call.FilePath, "line": float64(call.Line),
	}}
	if got := projectResults(answer["edges"], "from", "to", "kind", "file_path", "line"); !jsonEqual(got, wantEdges) {
		t.Errorf("MCP callers of %s from R2 = %v, want %v", run.ID, got, wantEdges)
	}
	var served []any
	for _, node := range projectResults(answer["nodes"], "id", "start_line", "end_line") {
		if node.(map[string]any)["id"] == run.ID {
			served = append(served, node)
		}
	}
	wantRun := []any{map[string]any{"id": run.ID, "start_line": float64(run.StartLine), "end_line": float64(run.EndLine)}}
	if !jsonEqual(served, wantRun) {
		t.Errorf("MCP callers from R2 serve %s as %v, want E0's %v", run.ID, served, wantRun)
	}

	materializer := d.state.mcpServer.Materializer()
	if materializer == nil || materializer.Leases != d.state.lifecycle.ViewLeases() {
		t.Fatal("the daemon serves views through no Materializer on the lifecycle's lease manager")
	}
	view, err := materializer.MaterializeCheckout(ctx, checkoutID)
	if err != nil {
		t.Fatalf("materialize R2's view: %v", err)
	}
	defer view.Close()
	assertServesFixture(t, view.Reader, f, f.R.Prefix, storetest.E0, false)
	assertServesFixture(t, d.state.graph, f, f.R.Prefix, storetest.E1, false)
}

// testDaemonRestartServesTheClosedStore: closing the daemon releases its
// store, and a daemon built again over the same store serves fixture F with
// no reindex and installs its Materializer on its lifecycle's lease manager.
func testDaemonRestartServesTheClosedStore(t *testing.T, backend string) {
	ctx := k12Context(t)
	d := startK12Daemon(ctx, t, backend, false)
	f := d.fixture
	d.close()

	state, _ := openK12Daemon(t)
	assertServesFixture(t, state.graph, f, f.R.Prefix, storetest.E1, false)
	assertServesFixture(t, state.graph, f, f.S.Prefix, storetest.E1, false)
	materializer := state.mcpServer.Materializer()
	if materializer == nil || materializer.Leases != state.lifecycle.ViewLeases() {
		t.Fatal("the restarted daemon serves views through no Materializer on the lifecycle's lease manager")
	}
}

// assertBasePublished requires warmup to have published a committed base for
// the repository: the runtime the stack installed reached a publication.
func assertBasePublished(t *testing.T, state *daemonState, prefix string) {
	t.Helper()
	for _, outcome := range state.basePublisher.Outcomes() {
		if outcome.RepoPrefix != prefix {
			continue
		}
		if outcome.Err != nil || outcome.Skipped != "" || outcome.GenerationID <= 0 {
			t.Fatalf("committed base of %s: %+v, want a published generation", prefix, outcome)
		}
		return
	}
	t.Fatalf("warmup scheduled no committed base for %s", prefix)
}

// callTool sends one tools/call through the daemon's MCP dispatcher for a
// session whose working directory is cwd. It returns the tool's JSON answer
// and the response metadata; a tool error fails the case.
func (d *k12Daemon) callTool(ctx context.Context, t *testing.T, cwd, tool string, args map[string]any) (map[string]any, map[string]any) {
	t.Helper()
	sess := &daemon.Session{ID: "k12-" + filepath.Base(cwd), CWD: cwd}
	initialize := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"k12","version":"1"}}}`
	if _, err := d.mcp.Dispatch(ctx, sess, []byte(initialize)); err != nil {
		t.Fatalf("initialize an MCP session in %s: %v", cwd, err)
	}
	frame, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args},
	})
	if err != nil {
		t.Fatalf("encode the %s call: %v", tool, err)
	}
	reply, err := d.mcp.Dispatch(ctx, sess, frame)
	if err != nil {
		t.Fatalf("MCP %s from %s: %v", tool, cwd, err)
	}
	var parsed struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool           `json:"isError"`
			Meta    map[string]any `json:"_meta"`
		} `json:"result"`
		Error any `json:"error"`
	}
	if err := json.Unmarshal(reply, &parsed); err != nil {
		t.Fatalf("decode the MCP %s reply %s: %v", tool, reply, err)
	}
	if parsed.Error != nil || parsed.Result.IsError || len(parsed.Result.Content) != 1 {
		t.Fatalf("MCP %s from %s failed: %s", tool, cwd, reply)
	}
	var answer map[string]any
	if err := json.Unmarshal([]byte(parsed.Result.Content[0].Text), &answer); err != nil {
		t.Fatalf("decode the MCP %s answer %q: %v", tool, parsed.Result.Content[0].Text, err)
	}
	return answer, parsed.Result.Meta
}

// projectResults keeps the named fields of each object in a JSON list, so a
// comparison covers exactly those fields, every entry and their order.
func projectResults(list any, fields ...string) []any {
	entries, _ := list.([]any)
	projected := make([]any, 0, len(entries))
	for _, entry := range entries {
		object, _ := entry.(map[string]any)
		kept := make(map[string]any, len(fields))
		for _, field := range fields {
			kept[field] = object[field]
		}
		projected = append(projected, kept)
	}
	return projected
}

func jsonEqual(a, b any) bool {
	left, errLeft := json.Marshal(a)
	right, errRight := json.Marshal(b)
	return errLeft == nil && errRight == nil && string(left) == string(right)
}

// restoreFixtureFile writes the bytes R's file rel has at epoch, read from
// R's own git history, into R's primary working tree and returns its path.
func restoreFixtureFile(t *testing.T, f *storetest.FixtureF, rel string, epoch storetest.Epoch) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "git", "-C", f.R.Root, "show", f.R.Commit[epoch]+":"+rel)
	body, err := cmd.Output()
	if err != nil {
		t.Fatalf("read %s at E%d from R's history: %v", rel, epoch, err)
	}
	path := filepath.Join(f.R.Root, filepath.FromSlash(rel))
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func fixtureCheckout(t *testing.T, f *storetest.FixtureF, id string) storetest.Checkout {
	t.Helper()
	for _, checkout := range f.Checkouts {
		if checkout.ID == id {
			return checkout
		}
	}
	t.Fatalf("fixture F has no checkout %s", id)
	return storetest.Checkout{}
}

func fixtureNode(t *testing.T, f *storetest.FixtureF, prefix string, epoch storetest.Epoch, id string) *graph.Node {
	t.Helper()
	for _, node := range f.Payload(prefix, epoch).Nodes {
		if node.ID == id {
			return node
		}
	}
	t.Fatalf("fixture F declares no node %s", id)
	return nil
}

func fixtureEdge(t *testing.T, f *storetest.FixtureF, prefix string, epoch storetest.Epoch, from, to string) *graph.Edge {
	t.Helper()
	for _, edge := range f.Payload(prefix, epoch).Edges {
		if edge.From == from && edge.To == to {
			return edge
		}
	}
	t.Fatalf("fixture F declares no edge %s -> %s", from, to)
	return nil
}

// assertServesFixture compares what r serves of fixture F's repository
// prefix at epoch with the graph F declares for it, as the real Go extractor
// produces it from F's sources:
//
//   - every declared node with its kind, name, file, lines, language and
//     repository;
//   - each declared call into another node of the repository, and the failed
//     lookup, exactly once;
//   - for each declared function, call edges at exactly the lines F's sources
//     call from, so a stale or duplicated call fails.
//
// R's go.mod replaces S with ../s, but the daemon's base-corpus call still
// names external-call::dep::example.test/s/api, not F's S node. These cases
// compare that call's line only; they do not force another resolution pass.
// The resolver's reference edge is not compared. Single-repository paths
// (eval, init) do not index S and serve IDs and files without the repository
// prefix and no repository.
func assertServesFixture(t *testing.T, r graph.Reader, f *storetest.FixtureF, prefix string, epoch storetest.Epoch, single bool) {
	t.Helper()
	local, repo := func(s string) string { return s }, prefix
	if single {
		local, repo = func(s string) string { return strings.TrimPrefix(s, prefix+"/") }, ""
	}
	payload := f.Payload(prefix, epoch)
	declared := make(map[string]bool, len(payload.Nodes))
	for _, want := range payload.Nodes {
		declared[want.ID] = true
		got := r.GetNode(local(want.ID))
		if got == nil {
			t.Errorf("E%d: node %s is not served", epoch, local(want.ID))
			continue
		}
		if got.Kind != want.Kind || got.Name != want.Name || got.FilePath != local(want.FilePath) ||
			got.StartLine != want.StartLine || got.EndLine != want.EndLine ||
			got.Language != want.Language || got.RepoPrefix != repo {
			t.Errorf("E%d: node %s served as kind=%s name=%s file=%s lines=%d-%d language=%s repo=%q, want %s %s %s %d-%d %s %q",
				epoch, local(want.ID), got.Kind, got.Name, got.FilePath, got.StartLine, got.EndLine, got.Language, got.RepoPrefix,
				want.Kind, want.Name, local(want.FilePath), want.StartLine, want.EndLine, want.Language, repo)
		}
	}
	callLines := make(map[string][]int)
	for _, want := range payload.Edges {
		if want.Kind != graph.EdgeCalls {
			continue
		}
		callLines[want.From] = append(callLines[want.From], want.Line)
		if !declared[want.To] && !strings.HasPrefix(want.To, graph.UnresolvedMarker) {
			continue
		}
		count := 0
		for _, got := range r.GetOutEdges(local(want.From)) {
			if got.To == local(want.To) && got.Kind == want.Kind && got.FilePath == local(want.FilePath) && got.Line == want.Line {
				count++
			}
		}
		if count != 1 {
			t.Errorf("E%d: call %s -> %s at line %d is served %d times, want once",
				epoch, local(want.From), local(want.To), want.Line, count)
		}
	}
	for _, node := range payload.Nodes {
		if node.Kind != graph.KindFunction {
			continue
		}
		var got []int
		for _, edge := range r.GetOutEdges(local(node.ID)) {
			if edge.Kind == graph.EdgeCalls {
				got = append(got, edge.Line)
			}
		}
		want := slices.Clone(callLines[node.ID])
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Errorf("E%d: %s calls from lines %v, want %v", epoch, local(node.ID), got, want)
		}
	}
}
