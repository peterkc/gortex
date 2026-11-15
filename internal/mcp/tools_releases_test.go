package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// seedReleasesGraph populates the graph with a KindRelease timeline
// and a couple of file nodes whose meta.added_in maps onto the
// releases. Mirrors what releases.EnrichGraphForBranch would have
// written; lets the read-side handler be tested without a real git
// repo.
func seedReleasesGraph(t *testing.T) *Server {
	t.Helper()
	g := graph.New()
	g.AddNode(&graph.Node{
		ID:   "release::v0.1",
		Kind: graph.KindRelease,
		Name: "v0.1",
		Meta: map[string]any{
			"tag":        "v0.1",
			"file_count": 1,
			"order":      0,
		},
	})
	g.AddNode(&graph.Node{
		ID:   "release::v0.2",
		Kind: graph.KindRelease,
		Name: "v0.2",
		Meta: map[string]any{
			"tag":        "v0.2",
			"file_count": 2,
			"order":      1,
		},
	})
	g.AddNode(&graph.Node{
		ID: "a.go", Kind: graph.KindFile, FilePath: "a.go",
		Meta: map[string]any{"added_in": "v0.1"},
	})
	g.AddNode(&graph.Node{
		ID: "b.go", Kind: graph.KindFile, FilePath: "b.go",
		Meta: map[string]any{"added_in": "v0.2"},
	})
	return &Server{
		graph:      g,
		session:    newSessionState(),
		tokenStats: &tokenStats{},
		symHistory: &symbolHistory{entries: make(map[string][]SymbolModification)},
		sessions:   newSessionMap(),
		toolScopes: newScopeRegistry(),
	}
}

func callAnalyzeReleases(t *testing.T, s *Server, args map[string]any) map[string]any {
	t.Helper()
	req := mcp.CallToolRequest{}
	req.Params.Arguments = args
	res, err := s.handleAnalyzeReleases(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, res)
	tc, ok := res.Content[0].(mcp.TextContent)
	require.True(t, ok)
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(tc.Text), &m))
	return m
}

func TestAnalyzeReleases_Timeline(t *testing.T) {
	s := seedReleasesGraph(t)
	out := callAnalyzeReleases(t, s, map[string]any{})
	releases, _ := out["releases"].([]any)
	require.Len(t, releases, 2)
	first := releases[0].(map[string]any)
	assert.Equal(t, "v0.1", first["tag"], "ordered by Meta.order asc — oldest first")
	assert.EqualValues(t, 0, first["order"])
	assert.EqualValues(t, 1, first["file_count"])
}

func TestAnalyzeReleases_TagFilterReturnsFiles(t *testing.T) {
	s := seedReleasesGraph(t)
	out := callAnalyzeReleases(t, s, map[string]any{"tag": "v0.2"})
	releases, _ := out["releases"].([]any)
	require.Len(t, releases, 1)
	first := releases[0].(map[string]any)
	files, _ := first["files"].([]any)
	require.Len(t, files, 1)
	assert.Equal(t, "b.go", files[0])
	assert.EqualValues(t, 1, out["file_hits"])
}

func TestAnalyzeReleases_TagFilterUnknownTag(t *testing.T) {
	s := seedReleasesGraph(t)
	out := callAnalyzeReleases(t, s, map[string]any{"tag": "v99"})
	require.NotEmpty(t, out["error"])
	assert.Equal(t, "enrich_releases", out["suggestion"])
}

func TestAnalyzeReleases_ErrorsWhenNoMeta(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "x.go", Kind: graph.KindFile, FilePath: "x.go"})
	s := &Server{
		graph:      g,
		session:    newSessionState(),
		tokenStats: &tokenStats{},
		symHistory: &symbolHistory{entries: make(map[string][]SymbolModification)},
		sessions:   newSessionMap(),
		toolScopes: newScopeRegistry(),
	}
	out := callAnalyzeReleases(t, s, map[string]any{})
	require.NotEmpty(t, out["error"])
	assert.Equal(t, "enrich_releases", out["suggestion"])
}

type releaseRowsCountingStore struct {
	graph.Store
	reads int
}

func (s *releaseRowsCountingStore) ReleaseRows(repo string) []graph.ReleaseEnrichment {
	s.reads++
	return s.Store.(graph.ReleaseEnrichmentReader).ReleaseRows(repo)
}

func TestAnalyzeReleases_LoadsSidecarOnlyWhenNeeded(t *testing.T) {
	for _, tc := range []struct {
		name          string
		args          map[string]any
		emptyTimeline bool
		wantReads     int
	}{
		{name: "timeline", wantReads: 0},
		{name: "unknown tag", args: map[string]any{"tag": "v99"}, wantReads: 0},
		{name: "matched tag", args: map[string]any{"tag": "v0.2"}, wantReads: 1},
		{name: "empty timeline", emptyTimeline: true, wantReads: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Given supported release enrichment, when only the node timeline
			// is needed, then no sidecar load occurs; row-consuming paths load once.
			srv := seedReleasesGraph(t)
			if tc.emptyTimeline {
				srv.graph = graph.New()
			} else {
				require.NoError(t, srv.graph.(graph.ReleaseEnrichmentWriter).BulkSetReleases("", []graph.ReleaseEnrichment{{NodeID: "a.go", AddedIn: "v0.2"}}))
			}
			store := &releaseRowsCountingStore{Store: srv.graph}
			srv.graph = store
			out := callAnalyzeReleases(t, srv, tc.args)
			assert.Equal(t, tc.wantReads, store.reads)
			assert.NotContains(t, out, "enrichment_unavailable", "the cheap support check must preserve supported availability")
			switch tc.name {
			case "timeline":
				assert.EqualValues(t, 2, out["total"])
			case "unknown tag", "empty timeline":
				assert.NotEmpty(t, out["error"])
				assert.Equal(t, "enrich_releases", out["suggestion"])
			case "matched tag":
				require.Len(t, out["releases"].([]any), 1)
				row := out["releases"].([]any)[0].(map[string]any)
				assert.Equal(t, []any{"a.go", "b.go"}, row["files"], "the loaded sidecar must override a.go's older node-local tag")
				assert.EqualValues(t, 2, out["file_hits"])
			}
		})
	}
}

func TestAnalyzeReleases_UnavailableTagFilter(t *testing.T) {
	for _, tag := range []string{"v0.2", "absent"} {
		t.Run(tag, func(t *testing.T) {
			// Given no release sidecar, when a tag is requested, then retain
			// the KindRelease existence check and disclose unavailable file enrichment.
			srv := seedReleasesGraph(t)
			srv.graph = struct{ graph.Store }{Store: srv.graph}
			out := callAnalyzeReleases(t, srv, map[string]any{"tag": tag})
			assert.Equal(t, []any{"release"}, out["enrichment_unavailable"])
			switch tag {
			case "v0.2":
				assert.NotContains(t, out, "error")
				assert.NotContains(t, out, "suggestion")
				require.Len(t, out["releases"].([]any), 1)
				row := out["releases"].([]any)[0].(map[string]any)
				assert.Equal(t, []any{"b.go"}, row["files"], "retain node-local file metadata")
			default:
				assert.Equal(t, "no KindRelease node for tag \"absent\"; run `enrich_releases` first", out["error"])
				assert.Equal(t, "enrich_releases", out["suggestion"])
				assert.Empty(t, out["releases"])
				assert.EqualValues(t, 0, out["total"])
			}
		})
	}
}
