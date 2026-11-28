package indexer

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func TestRebuildEndpointConstantReduction(t *testing.T) {
	ctx := t.Context()
	b, err := store_pebble.OpenBuild(t.TempDir(), store_pebble.FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	native := graph.New()
	for _, file := range []struct {
		path, repo string
		values     map[string]string
	}{
		{path: "repo/site.go", repo: "repo", values: map[string]string{"ROUTE": "/local", "MISSING": ""}},
		{path: "repo/other.go", repo: "repo", values: map[string]string{"ROUTE": "/other", "SHARED": "/same"}},
		{path: "repo/third.go", repo: "repo", values: map[string]string{"SHARED": "/same", "MISSING": "/present"}},
		{path: "foreign/site.go", repo: "foreign", values: map[string]string{"SHARED": "/foreign"}},
	} {
		parsed := store_pebble.ParseFacts{
			File:              graph.FileMetaRow{FilePath: file.path},
			EffectiveLanguage: "go",
			Nodes:             []*graph.Node{{ID: file.path, Name: file.path, FilePath: file.path, Kind: graph.KindFile, Language: "go", RepoPrefix: file.repo}},
		}
		for name, value := range file.values {
			id := file.path + "::" + name
			parsed.Nodes = append(parsed.Nodes, &graph.Node{ID: id, Kind: graph.KindConstant, Name: name, FilePath: file.path, Language: "go", RepoPrefix: file.repo})
			if value != "" {
				parsed.Constants = append(parsed.Constants, graph.ConstantValueRow{NodeID: id, FilePath: file.path, Value: value})
			}
		}
		parsed.File.NodeCount = len(parsed.Nodes)
		require.NoError(t, b.AddParsedFile(file.repo, parsed))
		native.AddBatch(parsed.Nodes, nil)
		require.NoError(t, native.BulkSetConstantValues(file.repo, parsed.Constants))
	}
	constants := rebuildEndpointConstants{ctx: ctx, view: b.RebuildView()}
	for _, tc := range []struct {
		name, file, repo, want string
	}{
		{name: " ROUTE ", file: "repo/site.go", repo: "repo", want: "/local"},
		{name: "ROUTE", file: "repo/third.go", repo: "repo"},
		{name: "SHARED", repo: "repo", want: "/same"},
		{name: "SHARED"},
		{name: "MISSING", file: "repo/site.go", repo: "repo", want: "/present"},
		{name: "UNKNOWN", repo: "repo"},
	} {
		q := contracts.EndpointExpression{Terms: []contracts.EndpointTerm{{Name: tc.name}}}
		want, ok := q.Resolve(tc.file, tc.repo, native, false)
		require.Equal(t, tc.want, want)
		require.Equal(t, tc.want != "", ok)
		require.Equal(t, want, constants.lookup(tc.name, tc.file, tc.repo))
		require.NoError(t, constants.err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	constants = rebuildEndpointConstants{ctx: canceled, view: b.RebuildView()}
	require.Empty(t, constants.lookup("ROUTE", "repo/site.go", "repo"))
	require.ErrorIs(t, constants.err, context.Canceled)
}

func TestRebuildEndpointConstantCandidateLimit(t *testing.T) {
	ctx := t.Context()
	b, err := store_pebble.OpenBuild(t.TempDir(), store_pebble.FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	const name, file, id = "ROUTE", "repo/site.go", "repo/site.go::ROUTE"
	require.NoError(t, b.AddParsedFile("repo", store_pebble.ParseFacts{
		File:              graph.FileMetaRow{FilePath: file, NodeCount: 2},
		EffectiveLanguage: "go",
		Nodes: []*graph.Node{
			{ID: file, Name: file, FilePath: file, Kind: graph.KindFile, Language: "go", RepoPrefix: "repo"},
			{ID: id, Name: name, FilePath: file, Kind: graph.KindConstant, Language: "go", RepoPrefix: "repo"},
		},
		Constants: []graph.ConstantValueRow{{NodeID: id, FilePath: file, Value: "/local"}},
	}))
	// These rows count toward the native name limit, but cannot vote.
	for i := range graph.ContractProjectionRowLimit - 1 {
		b.AddNode(&graph.Node{ID: fmt.Sprintf("foreign/site.go::%d", i), Name: name, FilePath: "foreign/site.go", Kind: graph.KindFunction, Language: "go", RepoPrefix: "foreign"})
	}
	constants := rebuildEndpointConstants{ctx: ctx, view: b.RebuildView()}
	require.Equal(t, "/local", constants.lookup(name, file, "repo"))
	require.NoError(t, constants.err)

	b.AddNode(&graph.Node{ID: "foreign/site.go::extra", Name: name, FilePath: "foreign/site.go", Kind: graph.KindConstant, Language: "go", RepoPrefix: "foreign"})
	require.Empty(t, constants.lookup(name, file, "repo"))
	require.ErrorIs(t, constants.err, graph.ErrContractProjectionLimit)

	// Reuse the over-limit name bucket to test a read failure in the early
	// pass itself. Keep the store and receipt valid up to the lookup.
	idx := newTestIndexer(graph.New())
	t.Cleanup(idx.Close)
	const routeFile = "routes.go"
	src := []byte("package p\nfunc Register(r Router) { r.GET(ROUTE, Handle) }\n")
	ext, ok := idx.registry.GetByLanguage("go")
	require.True(t, ok)
	parsed, facts, err := idx.extractPreparedRebuildFileWithContractFacts(ctx, routeFile, ext, src, src)
	require.NoError(t, err)
	require.Len(t, facts.Questions, 1)
	wire, err := store_pebble.EncodeContractParseFacts(parsed, facts)
	require.NoError(t, err)
	input := rebuildInput{Path: routeFile, Language: "go", Size: int64(len(src))}
	job := rebuildFileStages{inputs: []rebuildInput{input}, recipes: map[string]rebuildDigest{"go": {1}}}
	store := rebuildParseStore{dir: t.TempDir(), limit: 1 << 20}
	require.NoError(t, store.write(input.parseKey(idx.rebuildExtractorRecipe(job.recipes[input.Language], input)), wire))
	census := rebuildExtractorCensus{admitted: 1, byPath: map[string]string{routeFile: "go"}, originByGraphPath: map[string]string{routeFile: routeFile}}
	got, err := idx.readRebuildEarlyContractFacts(ctx, b.RebuildView(), job, store, &census, nil, nil)
	require.Nil(t, got)
	require.ErrorIs(t, err, store_pebble.ErrRebuildView)
	require.ErrorIs(t, err, graph.ErrContractProjectionLimit)
	require.ErrorContains(t, err, `resolve endpoint constants for "routes.go"`)
}
