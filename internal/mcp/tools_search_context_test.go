package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
	"github.com/zzet/gortex/internal/search/rerank"
)

type searchSymbolsContextBackend struct {
	block             bool
	started           chan struct{}
	startOnce         sync.Once
	bundles           []search.SymbolBundle
	contextCalls      atomic.Int32
	legacyBundleCalls atomic.Int32
	legacySearchCalls atomic.Int32
}

func (*searchSymbolsContextBackend) Add(string, ...string) {}
func (*searchSymbolsContextBackend) Remove(string)         {}
func (b *searchSymbolsContextBackend) Search(string, int) []search.SearchResult {
	b.legacySearchCalls.Add(1)
	return nil
}
func (*searchSymbolsContextBackend) Count() int { return 1 }
func (*searchSymbolsContextBackend) Close()     {}

func (b *searchSymbolsContextBackend) SearchSymbolBundles(_ string, _ int) []search.SymbolBundle {
	b.legacyBundleCalls.Add(1)
	return b.bundles
}

func (b *searchSymbolsContextBackend) SearchSymbolBundlesContext(ctx context.Context, _ string, _ int) []search.SymbolBundle {
	b.contextCalls.Add(1)
	b.startOnce.Do(func() {
		if b.started != nil {
			close(b.started)
		}
	})
	if b.block {
		<-ctx.Done()
		return nil
	}
	return b.bundles
}

func newSearchSymbolsContextServer(backend search.Backend) *Server {
	g := graph.New()
	eng := query.NewEngine(g)
	eng.SetSearch(backend)
	eng.SetRerank(nil)
	return NewServer(eng, g, nil, nil, zap.NewNop(), nil)
}

func searchSymbolsContextRequest(queryText string) mcpgo.CallToolRequest {
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"query":  queryText,
		"limit":  5,
		"assist": "off",
	}
	return req
}

type searchSymbolsContextResponse struct {
	result *mcpgo.CallToolResult
	err    error
}

func callSearchSymbolsContext(srv *Server, ctx context.Context, req mcpgo.CallToolRequest) <-chan searchSymbolsContextResponse {
	done := make(chan searchSymbolsContextResponse, 1)
	go func() {
		result, err := srv.handleSearchSymbols(ctx, req)
		done <- searchSymbolsContextResponse{result: result, err: err}
	}()
	return done
}

func TestSearchSymbolsCancellationStopsAdmittedBackend(t *testing.T) {
	backend := &searchSymbolsContextBackend{
		block:   true,
		started: make(chan struct{}),
	}
	srv := newSearchSymbolsContextServer(backend)
	ctx, cancel := context.WithCancel(context.Background())
	done := callSearchSymbolsContext(srv, ctx, searchSymbolsContextRequest("blockingLookup"))

	select {
	case <-backend.started:
	case <-time.After(5 * time.Second):
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
		}
		t.Fatal("search_symbols never entered the context-aware backend")
	}
	cancel()

	var got searchSymbolsContextResponse
	select {
	case got = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("search_symbols worker did not exit after cancellation")
	}
	if !errors.Is(got.err, context.Canceled) {
		t.Fatalf("handleSearchSymbols error = %v, want context.Canceled (result=%#v)", got.err, got.result)
	}
	if got.result != nil {
		t.Fatalf("canceled search_symbols returned authoritative result: %#v", got.result)
	}
	if calls := backend.contextCalls.Load(); calls != 1 {
		t.Fatalf("context calls = %d, want one admitted primary call and no fallback", calls)
	}
	if calls := backend.legacyBundleCalls.Load() + backend.legacySearchCalls.Load(); calls != 0 {
		t.Fatalf("canceled search_symbols used legacy backend %d times", calls)
	}
}

func TestSearchSymbolsPreCanceledAndLiveControls(t *testing.T) {
	req := searchSymbolsContextRequest("blockingLookup")

	preCanceled := &searchSymbolsContextBackend{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := newSearchSymbolsContextServer(preCanceled).handleSearchSymbols(ctx, req)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled error = %v, want context.Canceled (result=%#v)", err, result)
	}
	if result != nil || preCanceled.contextCalls.Load() != 0 || preCanceled.legacyBundleCalls.Load() != 0 || preCanceled.legacySearchCalls.Load() != 0 {
		t.Fatalf("pre-canceled request did work: result=%#v context=%d bundle=%d search=%d",
			result, preCanceled.contextCalls.Load(), preCanceled.legacyBundleCalls.Load(), preCanceled.legacySearchCalls.Load())
	}

	for _, tc := range []struct {
		name    string
		bundles []search.SymbolBundle
	}{
		{name: "empty", bundles: []search.SymbolBundle{}},
		{name: "nonempty", bundles: []search.SymbolBundle{{Node: &graph.Node{ID: "node-1", Name: "blockingLookup", Kind: graph.KindFunction}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := &searchSymbolsContextBackend{bundles: tc.bundles}
			liveCtx, liveCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer liveCancel()
			got := <-callSearchSymbolsContext(newSearchSymbolsContextServer(backend), liveCtx, req)
			if got.err != nil {
				t.Fatal(got.err)
			}
			if got.result == nil || got.result.IsError || len(got.result.Content) == 0 {
				t.Fatalf("uncanceled %s control lost result contract: %#v", tc.name, got.result)
			}
			if calls := backend.contextCalls.Load(); calls == 0 {
				t.Fatal("live control did not exercise the context-aware backend")
			}
			if calls := backend.legacyBundleCalls.Load() + backend.legacySearchCalls.Load(); calls != 0 {
				t.Fatalf("live context-aware control used legacy backend %d times", calls)
			}
		})
	}
}

func TestSearchSymbolsScopedContextMatchesLegacyHelper(t *testing.T) {
	nodes := []*graph.Node{
		{ID: "a", Name: "Alpha", Kind: graph.KindFunction, WorkspaceID: "workspace"},
		{ID: "b", Name: "Beta", Kind: graph.KindMethod, WorkspaceID: "workspace"},
	}
	backend := &searchSymbolsContextBackend{bundles: []search.SymbolBundle{{Node: nodes[0]}, {Node: nodes[1]}}}
	eng := query.NewEngine(graph.New())
	eng.SetSearch(backend)
	eng.SetRerank(nil)
	scope := query.QueryOptions{WorkspaceID: "workspace", RerankContext: &rerank.Context{}}

	legacy, legacyPrimary := fetchAndMergeBM25Timed(eng, "alpha", []string{"beta"}, 5, scope, nil)
	legacyContextCalls := backend.contextCalls.Load()
	contextual, contextualPrimary := fetchAndMergeBM25TimedContext(context.Background(), eng, "alpha", []string{"beta"}, 5, scope, nil)
	ids := func(in []*graph.Node) []string {
		out := make([]string, 0, len(in))
		for _, node := range in {
			out = append(out, node.ID)
		}
		return out
	}
	if !reflect.DeepEqual(ids(contextual), ids(legacy)) || contextualPrimary != legacyPrimary {
		t.Fatalf("context helper ids/primary = %v/%d, legacy = %v/%d", ids(contextual), contextualPrimary, ids(legacy), legacyPrimary)
	}
	if legacyContextCalls == 0 || backend.contextCalls.Load() <= legacyContextCalls {
		t.Fatalf("parity test missed a helper path: after legacy=%d after context=%d", legacyContextCalls, backend.contextCalls.Load())
	}
	if calls := backend.legacyBundleCalls.Load() + backend.legacySearchCalls.Load(); calls != 0 {
		t.Fatalf("scoped helpers fell back to contextless backend %d times", calls)
	}
}

type searchSymbolsFaultStore struct {
	graph.SymbolSearcher
	err                      error
	bundleCalls, symbolCalls atomic.Int32
}

func (*searchSymbolsFaultStore) SymbolFTSCount() (int, error) { return 1, nil }
func (s *searchSymbolsFaultStore) SearchSymbols(_ string, _ int) ([]graph.SymbolHit, error) {
	s.symbolCalls.Add(1)
	return nil, s.err
}
func (s *searchSymbolsFaultStore) SearchSymbolsContext(context.Context, string, int) ([]graph.SymbolHit, error) {
	s.symbolCalls.Add(1)
	return nil, s.err
}
func (s *searchSymbolsFaultStore) SearchSymbolBundlesContext(context.Context, string, int) ([]graph.SymbolBundle, error) {
	s.bundleCalls.Add(1)
	return nil, s.err
}

func TestSearchSymbolsWarnsForFailedTextAndKeepsIndependentEvidence(t *testing.T) {
	for _, format := range []string{"json", "compact", "toon", "gcx"} {
		t.Run(format, func(t *testing.T) {
			g := graph.New()
			g.AddNode(&graph.Node{ID: "repo/main.go::needle", Name: "needle pipeline", Kind: graph.KindFunction, FilePath: "repo/main.go", RepoPrefix: "repo"})
			source := &searchSymbolsFaultStore{err: errors.New("private backend failure")}
			eng := query.NewEngine(g)
			eng.SetSearch(search.NewSwappable(search.NewSymbolSearcherBackend(source)))
			eng.SetRerank(nil)
			srv := NewServer(eng, g, nil, nil, zap.NewNop(), nil)
			req := searchSymbolsContextRequest("needle pipeline")
			args := req.Params.Arguments.(map[string]any)
			args["format"] = format
			if format == "compact" {
				args["compact"] = true
			}
			// Given failed text ranking and a valid exact-name tier, keep the hit and name the failed channel.
			result, err := srv.handleSearchSymbols(context.Background(), req)
			if err != nil || result == nil || result.IsError {
				t.Fatalf("partial search=%#v/%v", result, err)
			}
			if source.bundleCalls.Load() == 0 && source.symbolCalls.Load() == 0 {
				t.Fatal("fixture did not execute the faulting adapter")
			}
			body := resultText(result)
			if !strings.Contains(body, "needle") {
				t.Fatalf("independent exact evidence lost: %s", body)
			}
			if result.Meta == nil || result.Meta.AdditionalFields["search_complete"] != false || !strings.Contains(body, "text") {
				t.Fatalf("failed text channel is not visible: %s / %#v", body, result.Meta)
			}
			if format == "json" && !json.Valid([]byte(body)) {
				t.Fatalf("warning broke structured JSON: %s", body)
			}
			warning, _ := result.Meta.AdditionalFields["search_warning"].(map[string]any)
			if !reflect.DeepEqual(warning["failed_channels"], []string{"text"}) {
				t.Fatalf("failed channel names=%#v", warning)
			}
			if strings.Contains(body, "private backend failure") {
				t.Fatal("backend details leaked into the warning")
			}
			if source.symbolCalls.Load() != 0 {
				t.Fatalf("failed bundle query was replayed: %d", source.symbolCalls.Load())
			}
			// When the same shared adapter succeeds on a later request, no prior warning may leak.
			source.err = nil
			healthy, err := srv.handleSearchSymbols(context.Background(), req)
			if err != nil || healthy == nil || healthy.IsError {
				t.Fatalf("healthy search=%#v/%v", healthy, err)
			}
			if healthy.Meta != nil && healthy.Meta.AdditionalFields["search_complete"] == false {
				t.Fatalf("cross-request warning leak: %#v", healthy.Meta)
			}
		})
	}

	t.Run("fresh exact pages", func(t *testing.T) {
		stack, _ := freshSymbolFixture(t)
		writeViewRepo(t, stack.worktreeRoot, "main-ws", map[string]string{
			"page1.go": "package repo\nfunc NeedleOne() {}\n",
			"page2.go": "package repo\nfunc NeedleTwo() {}\n",
		})
		generation, handle, err := stack.store.BeginPayloadGeneration(context.Background(), store_sqlite.PayloadGenerationRequest{OwnerKind: "dedicated_graph", GraphID: stack.graphID, LayerID: "search-warning", CheckoutID: viewTestWorktree, GenerationKind: "dirty", BaseGenerationID: stack.commit, TreeOID: "tree-search-warning"})
		require.NoError(t, err)
		nodes := []*graph.Node{viewFileNode("repo/page1.go", 3), viewFileNode("repo/page2.go", 3), viewRepoNode("repo/page1.go::NeedleOne", "NeedleOne", graph.KindFunction, "repo/page1.go", 2), viewRepoNode("repo/page2.go::NeedleTwo", "NeedleTwo", graph.KindFunction, "repo/page2.go", 2)}
		for _, node := range nodes {
			node.WorkspaceID = "main-ws"
			node.ProjectID = "repo"
		}
		require.NoError(t, handle.AddBatchChecked(nodes, nil))
		require.NoError(t, handle.SetFileMasks([]store_sqlite.FileMask{{RepoPrefix: "repo", FilePath: "repo/page1.go", Mode: store_sqlite.OwnershipReplace}, {RepoPrefix: "repo", FilePath: "repo/page2.go", Mode: store_sqlite.OwnershipReplace}}))
		require.NoError(t, stack.store.PublishPayloadGeneration(context.Background(), generation, 5000))
		stack.dirty = generation
		routeViewCheckout(t, stack.store, stack.graphID, stack.commit, stack.dirty, store_sqlite.RouteActive)
		source := &searchSymbolsFaultStore{err: errors.New("private paged backend failure")}
		stack.srv.engine.SetSearch(search.NewSwappable(search.NewSymbolSearcherBackend(source)))
		stack.srv.engine.SetRerank(nil)
		args := map[string]any{"query": "Needle", "limit": 1, "max_bytes": 0, "assist": "off", "format": "json", "view": map[string]any{"kind": "auto"}, requireExactArgName: true, requireFreshArgName: true}
		ctx := WithSessionCWD(WithSessionID(context.Background(), viewTestSession), stack.worktreeRoot)
		handler := stack.srv.wrapToolHandler(func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
			if req.GetString("cursor", "") == "" && ctx.Value(freshSymbolAttemptKey{}) == nil {
				return nil, errors.New("fixture did not enter a fresh-exact attempt")
			}
			return stack.srv.handleSearchSymbols(ctx, req)
		})
		// Given failed text in a fresh-exact paged search, both publication and replay must retain its warning.
		first, err := handler(ctx, makeReq("search_symbols", args))
		require.NoError(t, err)
		require.False(t, first.IsError, resultText(first))
		require.Positive(t, source.bundleCalls.Load(), "the faulting adapter must actually run")
		require.Zero(t, source.symbolCalls.Load(), "failed text must not replay")
		firstBody := decodeJSONResult(t, first)
		cursor, ok := firstBody["next_cursor"].(string)
		require.True(t, ok, "first page: %#v", firstBody)
		require.NotEmpty(t, cursor)
		assert.Equal(t, false, firstBody["search_complete"])
		firstWarning, _ := firstBody["search_warning"].(map[string]any)
		assert.Equal(t, []any{"text"}, firstWarning["failed_channels"])
		assert.Equal(t, "Search channel failure: text. Results are partial; recall may be incomplete.", firstWarning["message"])
		require.NotNil(t, first.Meta)
		assert.Equal(t, false, first.Meta.AdditionalFields["search_complete"])
		assert.NotContains(t, resultText(first), "private paged backend failure")
		// A new healthy query must not deduplicate against the incomplete sequence.
		source.err = nil
		healthy, err := handler(ctx, makeReq("search_symbols", args))
		require.NoError(t, err)
		require.False(t, healthy.IsError, resultText(healthy))
		assert.NotContains(t, decodeJSONResult(t, healthy), "search_warning")
		if healthy.Meta != nil {
			assert.NotEqual(t, false, healthy.Meta.AdditionalFields["search_complete"])
		}
		calls := source.bundleCalls.Load() + source.symbolCalls.Load()
		continuedArgs := cloneFreshSymbolArguments(args)
		continuedArgs["cursor"] = cursor
		second, err := handler(ctx, makeReq("search_symbols", continuedArgs))
		require.NoError(t, err)
		require.False(t, second.IsError, resultText(second))
		secondBody := decodeJSONResult(t, second)
		assert.Equal(t, false, secondBody["search_complete"])
		secondWarning, _ := secondBody["search_warning"].(map[string]any)
		assert.Equal(t, []any{"text"}, secondWarning["failed_channels"])
		require.NotNil(t, second.Meta)
		assert.Equal(t, false, second.Meta.AdditionalFields["search_complete"])
		assert.Equal(t, calls, source.bundleCalls.Load()+source.symbolCalls.Load(), "continuation must not search again")
		assert.NotEqual(t, firstBody["results"], secondBody["results"], "the second page must advance")
		replay, err := handler(ctx, makeReq("search_symbols", continuedArgs))
		require.NoError(t, err)
		require.False(t, replay.IsError, resultText(replay))
		replayBody := decodeJSONResult(t, replay)
		assert.Equal(t, false, replayBody["search_complete"])
		assert.Equal(t, secondBody["search_warning"], replayBody["search_warning"])
		assert.Equal(t, calls, source.bundleCalls.Load()+source.symbolCalls.Load(), "replay must not search again")
		require.Zero(t, stack.leases.Held())
	})

}
