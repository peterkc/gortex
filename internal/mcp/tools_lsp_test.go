package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/lspuri"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/semantic"
	"github.com/zzet/gortex/internal/semantic/lsp"
)

// The LSP half of the enrichment-output rule.
//
// enrichNodeOnDemand and confirmSymbolRefsOnDemand are WRITES raised from READ
// paths: get_symbol faults in an LSP-grade semantic_type, and
// find_usages / get_callers fault in compiler-confirmed incoming references.
// Both used to stamp `s.graph` — generation zero, the shared corpus — from a
// language server rooted in whatever tracked repository contained the file,
// no matter which view the request read. They now name their output through
// the authority like every other enrichment, and a request that reads a
// checkout of its own is refused before a server is spawned or a byte hovered.
//
// These tests need no live language server. A registered passive candidate
// with a missing endpoint exercises admission before startup fails; a manager
// without providers exercises the inert selection path. A standing receipt
// for the same owner makes admission observable: the helper's own admission
// supersedes it, including when that admission is abandoned.

// lspEnrichmentStack is a viewStack with a semantic manager attached (no
// providers, so nothing spawns) and a real graph node to enrich.
type lspEnrichmentStack struct {
	*viewStack
	node      *graph.Node
	root      string
	authority *indexer.OutputGenerationAuthority
}

func newLSPEnrichmentStack(t *testing.T) *lspEnrichmentStack {
	t.Helper()
	stack := newViewStack(t)
	// A zero Manager registers no providers and no router. Tests that need
	// effectful admission add an available candidate explicitly.
	stack.srv.semanticMgr = &semantic.Manager{}

	var node *graph.Node
	for _, n := range stack.store.AllNodes() {
		if n.Kind == graph.KindFunction && n.RepoPrefix == "repo" && n.Name == "Keeper" {
			node = n
			break
		}
	}
	if node == nil {
		t.Fatal("the fixture indexed no repo.Keeper function to enrich")
	}
	abs, err := stack.srv.absolutePath(node.FilePath)
	if err != nil {
		t.Fatalf("absolutePath(%q): %v", node.FilePath, err)
	}
	root, err := stack.srv.workspaceRootFor(abs)
	if err != nil {
		t.Fatalf("workspaceRootFor(%q): %v", abs, err)
	}
	if root != stack.repoRoot {
		t.Fatalf("the fixture node resolves to workspace %q, want %q", root, stack.repoRoot)
	}
	return &lspEnrichmentStack{
		viewStack: stack, node: node, root: root,
		authority: stack.srv.outputGenerationAuthority(),
	}
}

// standingReceipt admits an enrichment for the owner the on-demand helper will
// name, so a later admission for the same owner supersedes it and no admission
// leaves it fulfilable. It is the only way to observe an admission that is
// abandoned immediately afterwards.
func (s *lspEnrichmentStack) standingReceipt(t *testing.T, producer string) *EnrichmentOutput {
	t.Helper()
	out, err := BeginBaseEnrichment(context.Background(), s.authority, s.srv.graph,
		producer, s.node.RepoPrefix, s.root)
	if err != nil {
		t.Fatalf("standing %s receipt: %v", producer, err)
	}
	return out
}

// The command is available, so this candidate is admitted, but its passive
// endpoint does not exist. Startup fails without spawning a subprocess. Even
// this failed attempt must name its output and remain ineligible for replay.
func registerFailingLSPCandidate(t *testing.T, srv *Server) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	provider := lsp.NewProviderFromSpec(&lsp.ServerSpec{
		Name: "failing-enrichment-fixture", Command: executable, Languages: []string{"go"},
		Connect: &lsp.ConnectSpec{Network: "unix", Address: filepath.Join(t.TempDir(), "absent.sock")},
	}, zap.NewNop())
	srv.semanticMgr = semantic.NewManager(semantic.Config{}, zap.NewNop())
	srv.semanticMgr.RegisterProvider(provider)
	t.Cleanup(func() {
		if err := provider.Close(); err != nil {
			t.Error(err)
		}
	})
}

func TestOnDemandLSPWithoutProviderLeavesOutputCurrent(t *testing.T) {
	for _, producer := range []string{EnrichProducerSemanticType, EnrichProducerSymbolRefs} {
		t.Run(producer, func(t *testing.T) {
			s := newLSPEnrichmentStack(t)
			standing := s.standingReceipt(t, producer)
			attempt := &freshSymbolAttempt{}
			ctx := context.WithValue(t.Context(), freshSymbolAttemptKey{}, attempt)
			if producer == EnrichProducerSemanticType {
				s.srv.enrichNodeOnDemand(ctx, s.node)
			} else {
				s.srv.confirmSymbolRefsOnDemand(ctx, s.node)
			}
			if err := standing.Complete(); err != nil {
				t.Fatalf("no provider superseded a standing output: %v", err)
			}
			if attempt.replayUnsafe {
				t.Fatal("no-provider selection made a read ineligible for replay")
			}
		})
	}
}

func TestLSPProviderSelectionDoesNotStartAndCapturesItsCandidate(t *testing.T) {
	stack, node, _, connections := newReferenceRetryStack(t, "zero", nil)
	standing, err := BeginBaseEnrichment(t.Context(), stack.srv.outputGenerationAuthority(), stack.srv.graph,
		EnrichProducerSemanticType, node.RepoPrefix, stack.repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	start, _, err := stack.srv.lspProviderStartForPath(node.FilePath)
	if err != nil || start == nil {
		t.Fatalf("select candidate: %v", err)
	}
	if got := connections.Load(); got != 0 {
		t.Fatalf("selection opened %d connections before output admission", got)
	}
	if err := standing.Complete(); err != nil {
		t.Fatalf("candidate selection superseded a standing output: %v", err)
	}
	// A later manager change cannot make the captured start select another
	// provider or silently classify a selected candidate as an inert absence.
	stack.srv.semanticMgr = &semantic.Manager{}
	if provider, release, err := start(); err != nil || provider == nil {
		t.Fatalf("start captured candidate: %v", err)
	} else {
		release()
	}
	if got := connections.Load(); got != 1 {
		t.Fatalf("captured candidate opened %d connections, want one", got)
	}
}

// TestOnDemandSemanticTypeNamesItsOutputGeneration is the production-entrypoint
// trace for the get_symbol fault-in: enrichNodeOnDemand is the function
// tools_core.go calls, and it must name the corpus output through the
// authority before it touches a language server.
//
// Revert-red: with the pre-item body — provider.EnrichNode(s.graph, root, node)
// with no ctx and no admission — nothing supersedes the standing receipt and
// Complete returns nil.
func TestOnDemandSemanticTypeNamesItsOutputGeneration(t *testing.T) {
	s := newLSPEnrichmentStack(t)
	registerFailingLSPCandidate(t, s.srv)
	standing := s.standingReceipt(t, EnrichProducerSemanticType)
	attempt := &freshSymbolAttempt{}

	s.srv.enrichNodeOnDemand(context.WithValue(t.Context(), freshSymbolAttemptKey{}, attempt), s.node)

	err := standing.Complete()
	if !errors.Is(err, indexer.ErrOutputMutationReceiptSuperseded) {
		t.Fatalf("the on-demand semantic_type write named no output generation (standing receipt settled with %v)", err)
	}
	if !attempt.replayUnsafe {
		t.Fatal("an admitted provider startup failure remained replayable")
	}
}

// TestOnDemandSymbolRefsNameTheirOutputGeneration is the same trace for the
// find_usages / get_callers fault-in.
func TestOnDemandSymbolRefsNameTheirOutputGeneration(t *testing.T) {
	s := newLSPEnrichmentStack(t)
	registerFailingLSPCandidate(t, s.srv)
	standing := s.standingReceipt(t, EnrichProducerSymbolRefs)
	attempt := &freshSymbolAttempt{}

	s.srv.confirmSymbolRefsOnDemand(context.WithValue(t.Context(), freshSymbolAttemptKey{}, attempt), s.node)

	err := standing.Complete()
	if !errors.Is(err, indexer.ErrOutputMutationReceiptSuperseded) {
		t.Fatalf("the on-demand symbol-refs write named no output generation (standing receipt settled with %v)", err)
	}
	if !attempt.replayUnsafe {
		t.Fatal("an admitted provider startup failure remained replayable")
	}
}

// TestOnDemandLSPEnrichmentUnderARoutedViewTouchesNothing is the refusal.
//
// A request that reads a checkout of its own has no writable output — its
// generations are published and sealed — so the fault-in must not admit an
// output, must not spawn a language server rooted in the tracked primary's
// tree, and must not stamp the shared corpus behind the request's back.
//
// Revert-red: with beginEnrichmentOutput handing every request the base output
// (the pre-item behaviour of every producer), the standing receipt is
// superseded and this fails.
func TestOnDemandLSPEnrichmentUnderARoutedViewTouchesNothing(t *testing.T) {
	for _, tc := range []struct {
		name     string
		producer string
		call     func(*Server, context.Context, *graph.Node)
	}{
		{"semantic_type", EnrichProducerSemanticType,
			func(srv *Server, ctx context.Context, n *graph.Node) { srv.enrichNodeOnDemand(ctx, n) }},
		{"symbol_refs", EnrichProducerSymbolRefs,
			func(srv *Server, ctx context.Context, n *graph.Node) { srv.confirmSymbolRefsOnDemand(ctx, n) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newLSPEnrichmentStack(t)
			registerFailingLSPCandidate(t, s.srv)
			ctx := routedEnrichmentCtx(t, s.viewStack)
			standing := s.standingReceipt(t, tc.producer)

			tc.call(s.srv, ctx, s.node)

			if err := standing.Complete(); err != nil {
				t.Fatalf("a routed request admitted an on-demand LSP enrichment: %v", err)
			}
		})
	}
}

// TestRoutedSymbolRefsRefusalDoesNotBurnTheConfirmationLedger guards the
// follow-on cost of the refusal: refsConfirmed is a per-process "already done"
// ledger, so a refusal that still marked the symbol would make every LATER
// unrouted request skip the fault-in for it too — the refusal would silently
// degrade the corpus answers as well as the routed one.
//
// Revert-red: move s.refsConfirmed.Store above the `if out == nil { return }`
// guard in confirmSymbolRefsOnDemand and this fails.
//
// Scope note: the fixture registers no language server, so the corpus arm
// cannot be used as the positive control here — a run with no provider has
// never marked the ledger, before the refusal was added or after (the earlier
// body returned on the same lspProviderForPath error). What this pins is that
// the OUTPUT refusal returns on the same terms, ahead of the ledger write.
func TestRoutedSymbolRefsRefusalDoesNotBurnTheConfirmationLedger(t *testing.T) {
	s := newLSPEnrichmentStack(t)
	ctx := routedEnrichmentCtx(t, s.viewStack)

	s.srv.confirmSymbolRefsOnDemand(ctx, s.node)

	if _, marked := s.srv.refsConfirmed.Load(s.node.ID); marked {
		t.Fatal("a refused routed confirmation marked the symbol confirmed for every later request")
	}
	marked := 0
	s.srv.refsConfirmed.Range(func(any, any) bool { marked++; return true })
	if marked != 0 {
		t.Fatalf("a refused routed confirmation wrote %d ledger entries", marked)
	}
}

// TestRoutedLSPEnrichmentRefusalIsAnnotated closes the "silently thinner
// answer" half. The refusal is right, but a get_symbol / find_usages answer
// served without the fault-in genuinely carries less than the same call
// against the corpus does, and an answer that is thinner without saying so is
// the kind of quiet degradation the view contract exists to prevent.
//
// Revert-red: delete the noteDegraded call in lspEnrichmentTarget and the
// request records nothing.
func TestRoutedLSPEnrichmentRefusalIsAnnotated(t *testing.T) {
	for _, tc := range []struct {
		name string
		want graphview.CapabilityID
		call func(*Server, context.Context, *graph.Node)
	}{
		{"semantic_type", graphview.CapLSPHover,
			func(srv *Server, ctx context.Context, n *graph.Node) { srv.enrichNodeOnDemand(ctx, n) }},
		{"symbol_refs", graphview.CapLSPReferences,
			func(srv *Server, ctx context.Context, n *graph.Node) { srv.confirmSymbolRefsOnDemand(ctx, n) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newLSPEnrichmentStack(t)
			ctx, view := routedEnrichmentView(t, s.viewStack)

			tc.call(s.srv, ctx, s.node)

			degraded, _ := view.annotations()
			found := false
			for _, status := range degraded {
				if status.Capability == tc.want {
					found = true
					if status.State != graphview.StateUnavailable {
						t.Fatalf("%s annotated as %q, want %q", tc.want, status.State, graphview.StateUnavailable)
					}
				}
			}
			if !found {
				t.Fatalf("a refused on-demand enrichment left the answer unannotated: %v", degraded)
			}
		})
	}
}

// TestLSPEnrichmentCapabilityNamesWhatWasNotServed pins the producer →
// capability mapping the annotation above reports, so the two on-demand
// enrichments are never collapsed onto one capability.
func TestLSPEnrichmentCapabilityNamesWhatWasNotServed(t *testing.T) {
	if got := lspEnrichmentCapability(EnrichProducerSymbolRefs); got != graphview.CapLSPReferences {
		t.Fatalf("symbol refs annotate %q, want %q", got, graphview.CapLSPReferences)
	}
	if got := lspEnrichmentCapability(EnrichProducerSemanticType); got != graphview.CapLSPHover {
		t.Fatalf("semantic type annotates %q, want %q", got, graphview.CapLSPHover)
	}
}

// pinLSPPeer uses the same framed protocol boundary as the reference-retry
// fixture, but registers the router rather than a legacy manager provider.
type pinLSPPeer struct {
	stack   *viewStack
	node    *graph.Node
	router  *lsp.Router
	spec    *lsp.ServerSpec
	path    string
	entered chan struct{}
	resume  func()
	changes []string // read only after a protocol barrier
}

func newPinLSPPeer(t *testing.T, hold, outcome string) *pinLSPPeer {
	t.Helper()
	stack := newViewStack(t)
	path := filepath.Join(stack.repoRoot, "pin.rs")
	require.NoError(t, os.WriteFile(path, []byte("fn target() {}\nfn caller() { target(); }\n"), 0o600))
	node := viewRepoNode("repo/pin.rs::target", "target", graph.KindFunction, "repo/pin.rs", 1)
	node.Language = "rust"
	caller := viewRepoNode("repo/pin.rs::caller", "caller", graph.KindFunction, "repo/pin.rs", 2)
	caller.Language = "rust"
	stack.store.AddBatch([]*graph.Node{node, caller}, []*graph.Edge{{From: caller.ID, To: node.ID, Kind: graph.EdgeCalls, FilePath: caller.FilePath, Line: 2, Origin: graph.OriginASTResolved}})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	resume := make(chan struct{})
	f := &pinLSPPeer{stack: stack, node: node, path: path, entered: make(chan struct{}), resume: sync.OnceFunc(func() { close(resume) })}
	// SpecForPath returns the canonical pointer used by the inert selector.
	// These focused tests are serial and restore it before the next case.
	f.spec = lsp.SpecForPath(path)
	saved := *f.spec
	*f.spec = lsp.ServerSpec{Name: saved.Name, Languages: []string{"rust"}, Extensions: []string{".rs"}, Connect: &lsp.ConnectSpec{Network: "tcp", Address: ln.Addr().String()}}
	t.Cleanup(func() { *f.spec = saved })
	f.router = lsp.NewRouter(stack.repoRoot, zap.NewNop())
	f.router.RegisterSpec(f.spec)
	stack.srv.semanticMgr = semantic.NewManager(semantic.Config{}, zap.NewNop())
	stack.srv.semanticMgr.SetLSPRouter(f.router)
	done, accepted := make(chan struct{}), make(chan struct{})
	var active net.Conn
	go func() {
		defer close(done)
		conn, err := ln.Accept()
		active = conn
		close(accepted)
		if err != nil {
			return
		}
		defer conn.Close()
		reader := bufio.NewReader(conn)
		var held bool
		for {
			size := 0
			for {
				line, err := reader.ReadString('\n')
				if err != nil {
					return
				}
				if line == "\r\n" {
					break
				}
				if strings.HasPrefix(line, "Content-Length:") {
					size, _ = strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "Content-Length:")))
				}
			}
			body := make([]byte, size)
			if _, err := io.ReadFull(reader, body); err != nil {
				return
			}
			var req struct {
				ID     json.RawMessage
				Method string
				Params json.RawMessage
			}
			if json.Unmarshal(body, &req) != nil {
				return
			}
			if req.Method == hold && !held {
				held = true
				close(f.entered)
				<-resume
			}
			write := func(payload any) error {
				b, err := json.Marshal(payload)
				if err != nil {
					return err
				}
				_, err = fmt.Fprintf(conn, "Content-Length: %d\r\n\r\n%s", len(b), b)
				return err
			}
			if req.Method == "textDocument/didChange" {
				var change lsp.DidChangeTextDocumentParams
				if json.Unmarshal(req.Params, &change) == nil {
					for _, c := range change.ContentChanges {
						f.changes = append(f.changes, c.Text)
					}
				}
			}
			if req.Method == hold && (hold == "textDocument/didOpen" || hold == "textDocument/didChange") {
				if write(map[string]any{"jsonrpc": "2.0", "method": "textDocument/publishDiagnostics", "params": map[string]any{"uri": lspuri.PathToURI(path), "diagnostics": []lsp.Diagnostic{{Message: "fixture diagnostic", Severity: 2}}}}) != nil {
					return
				}
			}
			if len(req.ID) == 0 {
				continue
			}
			var result any
			switch req.Method {
			case "initialize":
				result = map[string]any{"capabilities": map[string]any{"hoverProvider": true, "callHierarchyProvider": true, "codeActionProvider": true}}
			case "textDocument/hover":
				result = map[string]any{"contents": map[string]any{"kind": "plaintext", "value": "i32"}}
			case "textDocument/prepareCallHierarchy":
				result = []lsp.CallHierarchyItem{{Name: "target", Kind: 12, URI: lspuri.PathToURI(path), Range: lsp.Range{Start: lsp.Position{Line: 0}}, SelectionRange: lsp.Range{Start: lsp.Position{Line: 0, Character: 3}}}}
			case "callHierarchy/incomingCalls":
				result = []lsp.CallHierarchyIncomingCall{{From: lsp.CallHierarchyItem{Name: "caller", Kind: 12, URI: lspuri.PathToURI(path), Range: lsp.Range{Start: lsp.Position{Line: 1}}, SelectionRange: lsp.Range{Start: lsp.Position{Line: 1, Character: 3}}}, FromRanges: []lsp.Range{{Start: lsp.Position{Line: 1, Character: 14}}}}}
			case "textDocument/codeAction":
				result = []lsp.CodeAction{{Title: "fixture action", Kind: "quickfix", Edit: &lsp.WorkspaceEdit{Changes: map[string][]lsp.TextEdit{}}}}
				if outcome == "empty" || hold == "textDocument/codeAction" && outcome == "fix-empty" {
					result = []lsp.CodeAction{}
				}
				if outcome == "apply-error" {
					result = []lsp.CodeAction{{Title: "bad command", Command: &lsp.Command{Command: "fixture.failed"}}}
				}
			}
			if outcome == "empty" && req.Method == "textDocument/prepareCallHierarchy" {
				result = []lsp.CallHierarchyItem{}
			}
			reply := map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result}
			if (outcome == "error" && req.Method == hold) || (outcome == "apply-error" && req.Method == "workspace/executeCommand") {
				delete(reply, "result")
				reply["error"] = map[string]any{"code": -32603, "message": "injected pin fixture error"}
			}
			if write(reply) != nil {
				return
			}
		}
	}()
	t.Cleanup(func() {
		f.resume()
		_ = f.router.Close()
		_ = ln.Close()
		<-accepted
		if active != nil {
			_ = active.Close()
		}
		<-done
	})
	return f
}

func TestLSPToolsHoldRouterPinDuringCall(t *testing.T) {
	t.Run("inert_selection", func(t *testing.T) {
		f := newPinLSPPeer(t, "", "success")
		start, _, err := f.stack.srv.lspProviderStartForPath(f.path)
		require.NoError(t, err)
		assert.Empty(t, f.router.Stats(), "selection must not start or pin a provider")
		provider, release, err := start()
		require.NoError(t, err)
		require.NotNil(t, provider)
		require.Len(t, f.router.Stats(), 1)
		assert.Equal(t, 1, f.router.Stats()[0].InUse)
		release()
		assert.Zero(t, f.router.Stats()[0].InUse)
	})
	for _, holder := range []string{"enrichNodeOnDemand", "confirmSymbolRefsAt", "handleGetDiagnostics", "handleGetCodeActions", "handleApplyCodeAction", "handleFixAllInFile", "simulateDiagnosticsAtStep"} {
		t.Run(holder, func(t *testing.T) {
			outcomes := []string{"success", "missing-file"}
			switch holder {
			case "enrichNodeOnDemand":
				outcomes = append(outcomes, "error")
			case "confirmSymbolRefsAt":
				outcomes = append(outcomes, "error", "empty", "incoming-error")
			case "handleGetCodeActions", "handleFixAllInFile":
				outcomes = append(outcomes, "error")
			case "handleApplyCodeAction":
				outcomes = append(outcomes, "error", "empty", "apply-error")
			case "simulateDiagnosticsAtStep":
				outcomes = append(outcomes, "push-error")
			}
			for _, outcome := range outcomes {
				t.Run(outcome, func(t *testing.T) {
					hold := "textDocument/codeAction"
					switch holder {
					case "enrichNodeOnDemand":
						hold = "textDocument/hover"
					case "confirmSymbolRefsAt":
						hold = "textDocument/prepareCallHierarchy"
						if outcome == "incoming-error" {
							hold = "callHierarchy/incomingCalls"
						}
					case "handleGetDiagnostics":
						hold = "textDocument/didOpen"
					case "simulateDiagnosticsAtStep":
						hold = "textDocument/didChange"
					}
					peerOutcome := outcome
					if outcome == "incoming-error" {
						peerOutcome = "error"
					}
					if holder == "handleFixAllInFile" && outcome == "success" {
						peerOutcome = "fix-empty"
					}
					if outcome == "missing-file" || outcome == "push-error" {
						hold = ""
					}
					f := newPinLSPPeer(t, hold, peerOutcome)
					if outcome == "missing-file" {
						require.NoError(t, os.Remove(f.path))
					}
					if outcome == "push-error" {
						p, err := f.router.ForSpecWorkspace(f.spec, f.stack.repoRoot)
						require.NoError(t, err)
						require.NoError(t, p.EnsureFileOpen(f.stack.repoRoot, "pin.rs"))
						require.NoError(t, p.Close()) // Cached-open file reaches the didChange failure, not the open failure.
					}
					args := map[string]any{"path": f.path, "index": 0, "wait": true, "timeout_ms": 500, "max_iterations": 1, "diagnostic_timeout_ms": 1}
					request := mcp.CallToolRequest{Params: mcp.CallToolParams{Arguments: args}}
					done := make(chan struct{})
					var result *mcp.CallToolResult
					var callErr error
					var diagnostics []map[string]any
					go func() {
						defer close(done)
						s := f.stack.srv
						switch holder {
						case "enrichNodeOnDemand":
							s.enrichNodeOnDemand(t.Context(), f.node)
						case "confirmSymbolRefsAt":
							s.confirmSymbolRefsAt(t.Context(), f.node, time.Now())
						case "handleGetDiagnostics":
							result, callErr = s.handleGetDiagnostics(t.Context(), request)
						case "handleGetCodeActions":
							result, callErr = s.handleGetCodeActions(t.Context(), request)
						case "handleApplyCodeAction":
							result, callErr = s.handleApplyCodeAction(t.Context(), request)
						case "handleFixAllInFile":
							result, callErr = s.handleFixAllInFile(t.Context(), request)
						case "simulateDiagnosticsAtStep":
							sim := &simulation{snapshots: [][]daemon.OverlayFile{{{Path: f.path, Content: "fn simulated() {}\n"}}}}
							diagnostics = s.simulateDiagnosticsAtStep(t.Context(), sim, 0, []string{f.path}, 500*time.Millisecond)
						}
					}()
					if hold != "" {
						select {
						case <-f.entered:
						case <-done:
							t.Fatal("holder returned before reaching protocol barrier")
						case <-t.Context().Done():
							t.Fatal("holder never entered")
						}
						stats := f.router.Stats()
						require.Len(t, stats, 1)
						assert.Equal(t, 1, stats[0].InUse, "pin must cover the entire holder")
						assert.Zero(t, f.router.CloseCheckoutWorkspace("rust", f.stack.repoRoot))
						f.resume()
					}
					select {
					case <-done:
					case <-t.Context().Done():
						t.Fatal("holder did not join")
					}
					require.NoError(t, callErr)
					stats := f.router.Stats()
					if assert.Len(t, stats, 1) {
						assert.Zero(t, stats[0].InUse, "every return must release")
					}
					if result != nil {
						wantError := outcome == "missing-file" || outcome == "error" || outcome == "apply-error" || holder == "handleApplyCodeAction" && outcome == "empty"
						assert.Equal(t, wantError, result.IsError)
						if !wantError {
							require.NotEmpty(t, result.Content)
							wire, ok := result.Content[0].(mcp.TextContent)
							require.True(t, ok)
							var payload map[string]any
							require.NoError(t, json.Unmarshal([]byte(wire.Text), &payload))
							assert.Equal(t, "lsp-"+f.spec.Command, payload["provider"])
							switch holder {
							case "handleGetDiagnostics":
								assert.Equal(t, float64(1), payload["total"])
							case "handleGetCodeActions":
								assert.Equal(t, float64(1), payload["total"])
							case "handleApplyCodeAction":
								assert.Equal(t, "fixture action", payload["applied"])
							case "handleFixAllInFile":
								assert.Equal(t, float64(0), payload["applied_actions"])
							}
						}
					}
					if outcome == "success" {
						switch holder {
						case "enrichNodeOnDemand":
							assert.NotEmpty(t, f.node.Meta["semantic_type"])
						case "confirmSymbolRefsAt":
							entry, ok := f.stack.srv.refsConfirmed.Load(f.node.ID)
							require.True(t, ok)
							assert.True(t, entry.(*referenceConfirmation).confirmed)
						case "simulateDiagnosticsAtStep":
							require.Len(t, diagnostics, 1)
							assert.Equal(t, "lsp-"+f.spec.Command, diagnostics[0]["server"])
							if len(stats) == 1 {
								p, err := f.router.ForSpecWorkspace(f.spec, f.stack.repoRoot)
								require.NoError(t, err)
								require.NoError(t, p.Client().Call("test/barrier", nil, nil))
								assert.Equal(t, []string{"fn simulated() {}\n", "fn target() {}\nfn caller() { target(); }\n"}, f.changes)
							}
						}
					}
					if outcome == "missing-file" || outcome == "push-error" {
						assert.Empty(t, diagnostics)
					}
					assert.Equal(t, 1, f.router.CloseCheckoutWorkspace("rust", f.stack.repoRoot))
				})
			}
		})
	}
}
