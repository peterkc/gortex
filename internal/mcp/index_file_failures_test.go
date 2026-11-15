package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/indexer/source"
	"github.com/zzet/gortex/internal/query"
)

func TestIndexFileFailures_SearchAndHealthRecover(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte("package app\n\nfunc Alpha() {}\n"), 0o644))
	g := graph.New()
	idx := indexer.New(g, testRegistry(), config.Default().Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)
	srv := NewServer(query.NewEngine(g), g, idx, nil, zap.NewNop(), nil)
	require.NoError(t, g.ReplaceFileIndexFailures("", []graph.FileIndexFailure{{Path: "blocked.go", Error: "open blocked.go: permission denied", PermissionDenied: true}}))
	require.Nil(t, g.GetNode("blocked.go"), "an initial failure must not need an existing file node")

	decode := func(result *mcplib.CallToolResult) map[string]any {
		t.Helper()
		require.False(t, result.IsError)
		var payload map[string]any
		require.NoError(t, json.Unmarshal([]byte(result.Content[0].(mcplib.TextContent).Text), &payload))
		return payload
	}
	for _, tc := range []struct {
		term  string
		count float64
	}{{"Alpha", 1}, {"MissingNeedle", 0}} {
		result, err := srv.handleSearchText(context.Background(), makeReq("search_text", map[string]any{"query": tc.term, "format": "json"}))
		require.NoError(t, err)
		payload := decode(result)
		require.Equal(t, tc.count, payload["count"])
		require.Equal(t, false, payload["index_complete"])
		warning := payload["index_warning"].(map[string]any)
		require.Equal(t, float64(1), warning["unreadable_file_count"])
		require.Contains(t, warning["message"], "zero matches do not prove absence")
	}
	pathScoped, err := srv.handleSearchText(context.Background(), makeReq("search_text", map[string]any{"query": "Alpha", "format": "json", "path": "main.go"}))
	require.NoError(t, err)
	pathPayload := decode(pathScoped)
	require.Equal(t, float64(1), pathPayload["count"])
	require.NotContains(t, pathPayload, "index_warning", "a failure outside the requested path must not qualify its results")
	for _, format := range []string{"json", "compact", "toon", "gcx"} {
		args := map[string]any{"query": "Alpha", "format": format, "assist": "off"}
		if format == "compact" {
			args["compact"] = true
		}
		result, err := srv.handleSearchSymbols(context.Background(), makeReq("search_symbols", args))
		require.NoError(t, err)
		require.False(t, result.IsError)
		require.Len(t, result.Content, 1, "freshness decoration requires one text block")
		var bodies []string
		for _, content := range result.Content {
			if text, ok := content.(mcplib.TextContent); ok {
				bodies = append(bodies, text.Text)
			}
		}
		require.Contains(t, strings.Join(bodies, "\n"), "index_incomplete", format)
		if format == "json" {
			payload := decode(result)
			require.Equal(t, false, payload["truncated"], "index failures must not rewrite pagination completeness")
		}
	}
	health, err := srv.buildIndexHealthPayloadCtx(context.Background())
	require.NoError(t, err)
	require.Equal(t, "degraded", health["status"])
	require.Equal(t, 1, health["failed_file_count"])
	require.Equal(t, 1, health["unreadable_file_count"])
	require.Less(t, health["health_score"].(float64), float64(100))
	require.Contains(t, compactIndexHealth(health, time.Now(), true), "status=degraded")

	require.NoError(t, g.ReplaceFileIndexFailures("", nil))
	result, err := srv.handleSearchText(context.Background(), makeReq("search_text", map[string]any{"query": "MissingNeedle", "format": "json"}))
	require.NoError(t, err)
	require.NotContains(t, decode(result), "index_warning")
	health, err = srv.buildIndexHealthPayloadCtx(context.Background())
	require.NoError(t, err)
	require.Equal(t, "ready", health["status"])
	require.Equal(t, true, health["index_complete"])
	require.Equal(t, 0, health["failed_file_count"])
}

func TestScopedIndexFileFailures(t *testing.T) {
	failures := []indexer.FileIndexFailure{
		{Path: "alpha/svc/blocked.go", RepoPrefix: "alpha", WorkspaceID: "shared", ProjectID: "backend"},
		{Path: "beta/svc/blocked.go", RepoPrefix: "beta", WorkspaceID: "shared", ProjectID: "frontend"},
		{Path: "gamma/svc/blocked.go", RepoPrefix: "gamma", WorkspaceID: "other", ProjectID: "backend"},
		{Path: "svc/unowned.go"},
	}
	for _, tc := range []struct {
		name  string
		scope ResolvedScope
		paths []string
		want  []string
	}{
		{"repo", ResolvedScope{RepoAllow: map[string]bool{"alpha": true}}, nil, []string{"alpha/svc/blocked.go"}},
		{"workspace", ResolvedScope{WorkspaceID: "shared"}, nil, []string{"alpha/svc/blocked.go", "beta/svc/blocked.go"}},
		{"project", ResolvedScope{ProjectID: "backend"}, nil, []string{"alpha/svc/blocked.go", "gamma/svc/blocked.go"}},
		{"combined", ResolvedScope{WorkspaceID: "shared", ProjectID: "backend"}, []string{"svc"}, []string{"alpha/svc/blocked.go"}},
		{"prefixed path", ResolvedScope{}, []string{"beta/svc"}, []string{"beta/svc/blocked.go"}},
		{"path boundary", ResolvedScope{}, []string{"sv"}, nil},
		{"other path", ResolvedScope{}, []string{"tests"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var paths []string
			for _, failure := range scopedIndexFileFailures(failures, tc.scope, tc.paths) {
				paths = append(paths, failure.Path)
			}
			require.Equal(t, tc.want, paths)
		})
	}
}

func TestIndexFileFailureSummaryBoundsSamples(t *testing.T) {
	var failures []indexer.FileIndexFailure
	for i := 0; i < indexFailureSampleLimit+3; i++ {
		failures = append(failures, indexer.FileIndexFailure{Path: fmt.Sprintf("alpha/file%d.go", i), RepoPrefix: "alpha", PermissionDenied: i%2 == 0})
	}
	summary := indexFileFailureSummary(failures)
	require.Equal(t, len(failures), summary["failed_file_count"])
	require.Len(t, summary["failed_files"], indexFailureSampleLimit)
	require.Equal(t, true, summary["failed_files_truncated"])
	require.Equal(t, map[string]int{"alpha": 23}, summary["failed_files_by_repo"])
	require.Equal(t, map[string]int{"alpha": 12}, summary["unreadable_files_by_repo"])
}

func TestIndexHealthFailureCountsAvoidDoubleCountingParseErrors(t *testing.T) {
	g := graph.New()
	idx := indexer.New(g, testRegistry(), config.Default().Index, zap.NewNop())
	idx.SetRepoPrefix("alpha")
	idx.SetRootPath(t.TempDir())
	failures := []indexer.FileIndexFailure{{Path: "alpha/bad.go", RepoPrefix: "alpha"}}
	for _, key := range []string{"alpha/bad.go", "bad.go", filepath.Join(idx.RootPath(), "bad.go")} {
		errors := []indexer.IndexError{{FilePath: key, Error: "parse failure"}}
		total, successful := indexHealthFailureCounts(g, 2, 1, indexHealthParseErrorsForCounts(errors, idx), failures)
		require.Equal(t, 2, total)
		require.Equal(t, 1, successful)
		require.Equal(t, key, errors[0].FilePath, "normalization must preserve reported parse-error paths")
	}
	other := []indexer.IndexError{{FilePath: filepath.Join(t.TempDir(), "bad.go"), Error: "other repository"}}
	_, successful := indexHealthFailureCounts(g, 3, 2, indexHealthParseErrorsForCounts(other, idx), failures)
	require.Equal(t, 1, successful, "matching basenames from another root must not collapse distinct failures")
	for _, tc := range []struct{ relative, graphPath string }{
		{"alpha/bad.go", "alpha/alpha/bad.go"},
		{"cafe\u0301.go", "alpha/caf\u00e9.go"},
	} {
		errors := []indexer.IndexError{{FilePath: filepath.Join(idx.RootPath(), tc.relative), Error: "parse failure"}}
		normalized := indexHealthParseErrorsForCounts(errors, idx)
		require.Equal(t, tc.graphPath, normalized[0].FilePath)
		_, successful := indexHealthFailureCounts(g, 2, 1, normalized, []indexer.FileIndexFailure{{Path: tc.graphPath, RepoPrefix: "alpha"}})
		require.Equal(t, 1, successful, "absolute paths must use the same prefixed NFC keys as the ledger")
	}
}

func TestIndexFileFailures_SelectedReaderDoesNotInheritBase(t *testing.T) {
	base, selected := graph.New(), graph.New()
	require.NoError(t, base.ReplaceFileIndexFailures("", []graph.FileIndexFailure{{Path: "base.go", Error: "permission denied", PermissionDenied: true}}))
	idx := indexer.New(base, testRegistry(), config.Default().Index, zap.NewNop())
	srv := &Server{graph: base, indexer: idx}
	require.NotNil(t, srv.indexFileFailureWarning(context.Background(), ResolvedScope{}, nil))
	ctx := withRequestView(context.Background(), &requestView{reader: selected})
	require.Nil(t, srv.indexFileFailureWarning(ctx, ResolvedScope{}, nil), "an empty selected ledger is authoritative")
	require.NoError(t, selected.ReplaceFileIndexFailures("", []graph.FileIndexFailure{{Path: "selected.go", Error: "read failed"}}))
	warning := srv.indexFileFailureWarning(ctx, ResolvedScope{}, nil)
	require.Equal(t, 1, warning["failed_file_count"])
	require.Equal(t, 0, warning["unreadable_file_count"])
	failures := warning["failed_files"].([]indexer.FileIndexFailure)
	require.Equal(t, "selected.go", failures[0].Path)
}

type unavailableIndexFailureReader struct {
	graph.Reader
	err error
}

func (r unavailableIndexFailureReader) FileIndexFailuresForRepo(string) ([]graph.FileIndexFailure, error) {
	return nil, r.err
}

func TestIndexFileFailures_ReadErrorDoesNotClaimCompleteness(t *testing.T) {
	g := graph.New()
	idx := indexer.New(g, testRegistry(), config.Default().Index, zap.NewNop())
	srv := NewServer(query.NewEngine(g), g, idx, nil, zap.NewNop(), nil)
	reader := unavailableIndexFailureReader{Reader: g, err: fmt.Errorf("ledger unavailable\nsecond line")}
	ctx := withRequestView(context.Background(), &requestView{reader: reader})
	warning := srv.indexFileFailureWarning(ctx, ResolvedScope{}, nil)
	require.NotNil(t, warning)
	require.Equal(t, 0, warning["failed_file_count"])
	require.Contains(t, warning["index_state_read_error"], "ledger unavailable")
	require.Contains(t, warning["message"], "completeness is unknown")
	result := decorateIndexFileFailureResult(mcplib.NewToolResultText("total: 0\n"), warning)
	require.Len(t, result.Content, 1)
	require.NotContains(t, result.Content[0].(mcplib.TextContent).Text, "\nsecond line", "multiline errors must remain a quoted TOON value")
	health, err := srv.buildIndexHealthPayloadCtx(ctx)
	require.NoError(t, err)
	require.Equal(t, false, health["index_complete"])
	require.Equal(t, "degraded", health["status"])
	require.Contains(t, health["index_state_read_error"], "ledger unavailable")
	require.Contains(t, compactIndexHealth(health, time.Now(), false), "status=degraded")
	require.Nil(t, srv.indexFileFailureWarning(ctx, ResolvedScope{RepoAllow: map[string]bool{"other": true}}, nil), "unselected repository read failures must not affect scoped warnings")
	idx.SetWorkspaceID("unrelated")
	idx.SetProjectID("other-project")
	require.Nil(t, srv.indexFileFailureWarning(ctx, ResolvedScope{WorkspaceID: "selected"}, nil), "another workspace's unreadable ledger must not qualify this workspace")
	require.Nil(t, srv.indexFileFailureWarning(ctx, ResolvedScope{ProjectID: "selected-project"}, nil), "project narrowing must apply before reading failure state")
	require.NotNil(t, srv.indexFileFailureWarning(ctx, ResolvedScope{WorkspaceID: "unrelated"}, nil), "an in-scope ledger error must remain visible")
}

func TestIndexFileFailures_DirectoryFailureQualifiesNestedSearch(t *testing.T) {
	g := graph.New()
	idx := indexer.New(g, testRegistry(), config.Default().Index, zap.NewNop())
	srv := NewServer(query.NewEngine(g), g, idx, nil, zap.NewNop(), nil)
	require.NoError(t, g.ReplaceFileIndexFailures("", []graph.FileIndexFailure{{Path: "restricted", Error: "permission denied", PermissionDenied: true}}))
	for _, tc := range []struct {
		path    string
		warning bool
	}{
		{"restricted/nested/file.go", true},
		{"restricted", true},
		{"restricted-other/file.go", false},
	} {
		result, err := srv.handleSearchText(context.Background(), makeReq("search_text", map[string]any{"query": "MissingNeedle", "path": tc.path, "format": "json"}))
		require.NoError(t, err)
		var payload map[string]any
		require.NoError(t, json.Unmarshal([]byte(result.Content[0].(mcplib.TextContent).Text), &payload))
		require.Equal(t, float64(0), payload["count"])
		_, warned := payload["index_warning"]
		require.Equal(t, tc.warning, warned, tc.path)
	}
	failures := []indexer.FileIndexFailure{{Path: "repo/restricted", RepoPrefix: "repo"}}
	require.Len(t, scopedIndexFileFailures(failures, ResolvedScope{}, []string{"restricted/nested/file.go"}), 1)
	require.Empty(t, scopedIndexFileFailures(failures, ResolvedScope{}, []string{"restricted-other/file.go"}))
	rootFailure := []indexer.FileIndexFailure{{Path: "repo/.", RepoPrefix: "repo"}}
	require.Len(t, scopedIndexFileFailures(rootFailure, ResolvedScope{RepoAllow: map[string]bool{"repo": true}}, []string{"pkg/file.go"}), 1)
	require.Empty(t, scopedIndexFileFailures(rootFailure, ResolvedScope{RepoAllow: map[string]bool{"other": true}}, []string{"pkg/file.go"}))
	require.Len(t, scopedIndexFileFailures([]indexer.FileIndexFailure{{Path: "."}}, ResolvedScope{}, []string{"pkg/file.go"}), 1)
}

func TestIndexFileFailures_HealthCacheRefreshesLiveLedger(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte("package app\n\nfunc Alpha() {}\n"), 0o644))
	g := graph.New()
	idx := indexer.New(g, testRegistry(), config.Default().Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)
	srv := NewServer(query.NewEngine(g), g, idx, nil, zap.NewNop(), nil)
	baseline, err := srv.buildIndexHealthBasePayloadCtx(context.Background())
	require.NoError(t, err)
	srv.indexHealth.mu.Lock()
	srv.indexHealth.payload = baseline
	srv.indexHealth.updatedAt = time.Now()
	srv.indexHealth.mu.Unlock()
	read := func(ctx context.Context) map[string]any {
		t.Helper()
		result, err := srv.handleIndexHealth(ctx, makeReq("index_health", map[string]any{"format": "json"}))
		require.NoError(t, err)
		var payload map[string]any
		require.NoError(t, json.Unmarshal([]byte(result.Content[0].(mcplib.TextContent).Text), &payload))
		return payload
	}
	healthy := read(context.Background())
	require.Equal(t, true, healthy["index_complete"])
	require.NoError(t, g.ReplaceFileIndexFailures("", []graph.FileIndexFailure{{Path: "blocked.go", Error: "permission denied", PermissionDenied: true}}))
	failed := read(context.Background())
	require.Equal(t, "degraded", failed["status"])
	require.Less(t, failed["health_score"].(float64), healthy["health_score"].(float64))
	compact, err := srv.handleIndexHealth(context.Background(), makeReq("index_health", map[string]any{"compact": true}))
	require.NoError(t, err)
	require.Contains(t, compact.Content[0].(mcplib.TextContent).Text, "status=degraded")
	require.Contains(t, compact.Content[0].(mcplib.TextContent).Text, "unreadable_files=1")
	require.NotContains(t, baseline, "failed_file_count", "response-time overlay must not mutate the shared cache")
	selected := graph.New()
	selectedCtx := withRequestView(context.Background(), &requestView{reader: selected})
	require.Equal(t, true, read(selectedCtx)["index_complete"], "a healthy selected ledger must override primary failures")
	brokenCtx := withRequestView(context.Background(), &requestView{reader: unavailableIndexFailureReader{Reader: selected, err: fmt.Errorf("selected ledger unavailable")}})
	unknown := read(brokenCtx)
	require.Equal(t, false, unknown["index_complete"])
	require.Contains(t, unknown["index_state_read_error"], "selected ledger unavailable")
	require.NoError(t, g.ReplaceFileIndexFailures("", nil))
	recovered := read(context.Background())
	require.Equal(t, "ready", recovered["status"])
	require.Equal(t, healthy["health_score"], recovered["health_score"], "recovery must restore the baseline score before cache expiry")
	require.NotContains(t, recovered, "failed_files")
	require.NotContains(t, recovered, "index_state_read_error")
	require.Equal(t, healthy["recommendation"], recovered["recommendation"])
}

type healthFailureContentSource struct {
	source.ContentSource
	denied bool
}

func (s *healthFailureContentSource) Open(path string) (io.ReadCloser, source.FileMeta, error) {
	if s.denied && filepath.Base(path) == "blocked.go" {
		return nil, source.FileMeta{}, &os.PathError{Op: "open", Path: path, Err: syscall.EPERM}
	}
	return s.ContentSource.Open(path)
}

func TestIndexFileFailures_HealthCacheClearsRecoveredParsePenalty(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "blocked.go"), []byte("package app\n\nfunc Blocked() {}\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "readable.go"), []byte("package app\n\nfunc Readable() {}\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "another.go"), []byte("package app\n\nfunc Another() {}\n"), 0o644))
	g := graph.New()
	idx := indexer.New(g, testRegistry(), config.Default().Index, zap.NewNop())
	filesystem, err := source.NewFilesystemSource(dir)
	require.NoError(t, err)
	// Release the os.Root handle before TempDir cleanup removes the directory;
	// Windows does not allow removal while the source keeps that handle open.
	t.Cleanup(func() { require.NoError(t, filesystem.Close()) })
	content := &healthFailureContentSource{ContentSource: filesystem, denied: true}
	idx.SetContentSource(content)
	_, err = idx.Index(dir)
	require.NoError(t, err)
	require.NotEmpty(t, idx.ParseErrors(), "full indexing must report the injected read failure")
	srv := NewServer(query.NewEngine(g), g, idx, nil, zap.NewNop(), nil)
	baseline, err := srv.buildIndexHealthBasePayloadCtx(context.Background())
	require.NoError(t, err)
	require.Less(t, baseline["health_score"].(float64), float64(100))
	failedHealth, err := srv.buildIndexHealthPayloadCtx(context.Background())
	require.NoError(t, err)
	require.Equal(t, 3, failedHealth["total_detected"])
	require.Equal(t, 2, failedHealth["successfully_indexed"], "one absolute parse error and its ledger entry identify the same failed file")
	require.Equal(t, float64(66.7), failedHealth["health_score"])
	content.denied = false
	_, err = idx.IncrementalReindexPaths(dir, []string{filepath.Join(dir, "blocked.go")})
	require.NoError(t, err)
	require.Empty(t, idx.ParseErrors(), "successful incremental recovery must clear the historical read failure")
	for _, ceiling := range []float64{100, 40} {
		cached := make(map[string]any, len(baseline))
		for key, value := range baseline {
			cached[key] = value
		}
		cached[indexHealthLivenessCeilingKey] = ceiling
		srv.indexHealth.mu.Lock()
		srv.indexHealth.payload, srv.indexHealth.updatedAt = cached, time.Now()
		srv.indexHealth.mu.Unlock()
		result, err := srv.handleIndexHealth(context.Background(), makeReq("index_health", map[string]any{"format": "json"}))
		require.NoError(t, err)
		var payload map[string]any
		require.NoError(t, json.Unmarshal([]byte(result.Content[0].(mcplib.TextContent).Text), &payload))
		require.Equal(t, ceiling, payload["health_score"], "recovery must clear the cached parse penalty while retaining the liveness ceiling")
		require.NotContains(t, payload, "parse_failures")
		require.NotContains(t, payload, indexHealthLivenessCeilingKey, "internal cache bookkeeping must not reach callers")
		require.Contains(t, cached, "parse_failures", "refresh must not mutate the shared cached parse error slice")
		if ceiling == 100 {
			require.NotContains(t, fmt.Sprint(payload["recommendation"]), "Health score below 80%")
		}
	}
}

func TestIndexFileFailures_EmptyHealthBaselineWithoutIndexer(t *testing.T) {
	srv := &Server{graph: graph.New()}
	baseline := map[string]any{"health_score": float64(0), "total_detected": 0, "file_node_count": 0, indexHealthLivenessCeilingKey: float64(100)}
	result := srv.refreshIndexHealthFileFailures(context.Background(), baseline)
	require.Equal(t, float64(0), result["health_score"])
	require.NotContains(t, result, indexHealthLivenessCeilingKey)
	require.Contains(t, baseline, indexHealthLivenessCeilingKey)
}

// Keep the healthy failure ledger while deliberately hiding FileMetaReader.
type healthFileMetadataStoreOnly struct{ graph.Store }

func (s healthFileMetadataStoreOnly) FileIndexFailuresForRepo(repo string) ([]graph.FileIndexFailure, error) {
	return s.Store.(graph.FileIndexFailureReader).FileIndexFailuresForRepo(repo)
}

type healthFileMetadataReadFailure struct {
	healthFileMetadataStoreOnly
	err error
}

func (s *healthFileMetadataReadFailure) FileMetasForRepo(repo string) ([]graph.FileMetaRow, error) {
	if repo == "beta" && s.err != nil {
		return nil, s.err
	}
	return s.Store.(graph.FileMetaReader).FileMetasForRepo(repo)
}

func TestIndexHealthFileMetadataUnavailable(t *testing.T) {
	for _, mode := range []string{"missing", "read failure"} {
		t.Run(mode, func(t *testing.T) {
			// Given independent healthy graph/ledger evidence and two repositories' metadata.
			g := graph.New()
			for _, repo := range []string{"alpha", "beta"} {
				file := repo + "/a.go"
				g.AddBatch([]*graph.Node{
					{ID: file, Kind: graph.KindFile, FilePath: file, RepoPrefix: repo, Language: "go"},
					{ID: file + "::A", Kind: graph.KindFunction, FilePath: file, RepoPrefix: repo, Language: "go"},
				}, []*graph.Edge{{From: file, To: file + "::A", Kind: graph.EdgeContains, FilePath: file}})
				require.NoError(t, g.SetFileMetas(repo, []graph.FileMetaRow{{FilePath: file, NodeCount: 2, Errors: `["line 1"]`}}))
			}
			var store graph.Store = healthFileMetadataStoreOnly{Store: g}
			if mode == "read failure" {
				store = &healthFileMetadataReadFailure{healthFileMetadataStoreOnly: healthFileMetadataStoreOnly{Store: g}, err: fmt.Errorf("injected file metadata failure\nsecond line")}
			}
			idx := indexer.New(g, testRegistry(), config.Default().Index, zap.NewNop())
			srv := NewServer(query.NewEngine(g), store, idx, nil, zap.NewNop(), nil)
			// When the full health payload reads absent or only partly readable metadata.
			payload, err := srv.buildIndexHealthPayloadCtx(context.Background())
			require.NoError(t, err)
			require.Equal(t, "degraded", payload["status"], "a healthy ledger cannot certify unavailable metadata")
			require.Equal(t, false, payload["index_complete"])
			require.Equal(t, false, payload["file_metadata_available"])
			require.NotEmpty(t, payload["file_metadata_error"])
			require.NotContains(t, payload, "indexed_file_count", "a partial count is not a complete total")
			require.NotContains(t, payload, "files_with_parse_errors", "partial diagnostics must not look complete")
			require.Equal(t, 4, payload["node_count"], "unrelated graph evidence stays useful")
			require.Equal(t, float64(100), payload["health_score"], "unknown metadata must not invent a parse failure")
			require.Equal(t, true, payload["language_coverage"].(map[string]bool)["go"])
			// The same error must survive response-time ledger refresh and every renderer.
			baseline, err := srv.buildIndexHealthBasePayloadCtx(context.Background())
			require.NoError(t, err)
			srv.indexHealth.mu.Lock()
			srv.indexHealth.payload, srv.indexHealth.updatedAt = baseline, time.Now()
			srv.indexHealth.mu.Unlock()
			for _, format := range []string{"json", "toon", "compact"} {
				args := map[string]any{"format": format}
				if format == "compact" {
					args["compact"] = true
				}
				result, err := srv.handleIndexHealth(context.Background(), makeReq("index_health", args))
				require.NoError(t, err)
				require.False(t, result.IsError, "independent health evidence remains available")
				body := result.Content[0].(mcplib.TextContent).Text
				if format == "compact" {
					require.Contains(t, body, "status=degraded")
					require.Contains(t, body, "file_metadata=unavailable")
				} else {
					require.Contains(t, body, "file_metadata_available")
					require.Contains(t, body, "file_metadata_error")
					require.NotContains(t, body, "\nsecond line", "multiline errors must remain encoded values")
				}
			}
			require.NotContains(t, baseline, "status", "response decoration must not mutate the shared cache")
			// After capability/read recovery and the normal cache rebuild, no warning leaks.
			srv.graph = g
			healthy, err := srv.buildIndexHealthPayloadCtx(context.Background())
			require.NoError(t, err)
			require.Equal(t, "ready", healthy["status"])
			require.Equal(t, true, healthy["index_complete"])
			require.Equal(t, true, healthy["file_metadata_available"])
			require.NotContains(t, healthy, "file_metadata_error")
			require.Equal(t, 2, healthy["indexed_file_count"])
		})
	}
}

func TestIndexHealthFileRollupPreservesErrors(t *testing.T) {
	g := graph.New()
	require.NoError(t, g.SetFileMetas("alpha", []graph.FileMetaRow{{FilePath: "alpha/a.go", Errors: `["line 1"]`}}))
	for _, cause := range []error{errors.New("injected metadata read failure"), context.Canceled} {
		// Given one readable repository and one failed metadata read.
		reader := &healthFileMetadataReadFailure{healthFileMetadataStoreOnly: healthFileMetadataStoreOnly{Store: g}, err: cause}
		srv := &Server{graph: reader}
		// When a complete rollup is requested, then no partial count/rows escape.
		rows, count, err := srv.buildIndexHealthFileRollup(map[string]struct{}{"alpha": {}, "beta": {}})
		require.ErrorIs(t, err, cause)
		require.Nil(t, rows)
		require.Zero(t, count)
	}
	// Missing support has its own stable cause, distinct from a valid empty table.
	srv := &Server{graph: healthFileMetadataStoreOnly{Store: g}}
	rows, count, err := srv.buildIndexHealthFileRollup(map[string]struct{}{"alpha": {}})
	require.ErrorIs(t, err, errors.ErrUnsupported)
	require.Nil(t, rows)
	require.Zero(t, count)
}

func TestIndexHealthFileRollupSupportedCounts(t *testing.T) {
	for _, backend := range []string{"graph", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			// Given native metadata support, even an empty table is available.
			var store graph.Store = graph.New()
			if backend == "sqlite" {
				disk, err := store_sqlite.Open(filepath.Join(t.TempDir(), "metadata.sqlite"))
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, disk.Close()) })
				store = disk
			}
			srv := &Server{graph: store}
			repos := map[string]struct{}{"": {}, "beta": {}}
			rows, count, err := srv.buildIndexHealthFileRollup(repos)
			require.NoError(t, err)
			require.Empty(t, rows)
			require.Zero(t, count)
			writer := store.(graph.FileMetaWriter)
			require.NoError(t, writer.SetFileMetas("", []graph.FileMetaRow{{FilePath: "a.go", NodeCount: 2, Errors: `["line 7","line 8"]`}}))
			require.NoError(t, writer.SetFileMetas("beta", []graph.FileMetaRow{{FilePath: "beta/b.go", NodeCount: 3, Errors: `invalid-json`}}))
			// When prefix-separated rows are read repeatedly, then existing values survive.
			for range 2 {
				rows, count, err = srv.buildIndexHealthFileRollup(repos)
				require.NoError(t, err)
				require.Equal(t, 2, count)
				require.ElementsMatch(t, []map[string]any{
					{"file": "a.go", "node_count": 2, "errors": []string{"line 7", "line 8"}},
					{"file": "beta/b.go", "node_count": 3},
				}, rows)
			}
			// No requested repositories is a supported, successful no-op.
			rows, count, err = srv.buildIndexHealthFileRollup(nil)
			require.NoError(t, err)
			require.Empty(t, rows)
			require.Zero(t, count)
		})
	}
}
