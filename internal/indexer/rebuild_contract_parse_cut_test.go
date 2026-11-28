package indexer

import (
	"context"
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
