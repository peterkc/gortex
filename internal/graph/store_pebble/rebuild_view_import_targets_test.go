package store_pebble

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cockroachdb/pebble/v2"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildViewFileImportTargets(t *testing.T) {
	ctx := t.Context()
	b := newBuild(t, FullProfile)
	v := b.RebuildView()
	const file = "repo/app/main.go"
	const bound = "not-a-file-ID"
	const unresolved = "repo::unresolved::import::pkg"
	const external = "external::pkg"
	b.AddBatch([]*graph.Node{
		{ID: "file-id", Kind: graph.KindFile, FilePath: file},
		{ID: "fixture", Kind: graph.KindFunction, FilePath: file},
		{ID: "other", FilePath: file + ".other"},
		{ID: bound, FilePath: "repo/old/target.go"},
		{ID: external, FilePath: "unexpected/external.go"},
		{ID: "pathless"},
	}, []*graph.Edge{
		{From: "fixture", To: bound, Kind: graph.EdgeImports, FilePath: "different.go"},
		{From: "fixture", To: unresolved, Kind: graph.EdgeImports},
		{From: "file-id", To: external, Kind: graph.EdgeImports},
		{From: "file-id", To: "absent", Kind: graph.EdgeImports},
		{From: "file-id", To: "pathless", Kind: graph.EdgeImports},
		{From: "other", To: "foreign", Kind: graph.EdgeImports, FilePath: file},
		{From: "fixture", To: "nonimport", Kind: graph.EdgeReferences, FilePath: file},
	})
	collect := func(mutate bool) map[string]string {
		t.Helper()
		got := map[string]string{}
		require.NoError(t, v.VisitFileImportTargets(ctx, file, func(id, path string) error {
			if mutate && len(got) == 0 {
				// The first callback precedes the fixture's bound target lookup.
				b.AddNode(&graph.Node{ID: bound, FilePath: "repo/new/target.go"})
				b.AddEdge(&graph.Edge{From: "fixture", To: "new", Kind: graph.EdgeImports})
			}
			require.NotContains(t, got, id)
			got[id] = path
			return nil
		}))
		return got
	}
	want := map[string]string{bound: "repo/old/target.go", unresolved: "", external: "", "absent": "", "pathless": ""}
	require.Equal(t, want, collect(true))
	want[bound], want["new"] = "repo/new/target.go", ""
	require.Equal(t, want, collect(false))
}

func TestRebuildViewFileImportTargetsRefusals(t *testing.T) {
	const file = "repo/main.go"
	const id = "source"
	visit := func(_, _ string) error { return nil }
	for _, tc := range []struct {
		name    string
		corrupt func(*testing.T, *BuildStore)
		want    error
	}{
		{"index identity", func(t *testing.T, b *BuildStore) {
			require.NoError(t, b.pending.Set(keyStrings([]byte{familyFile}, file, id), []byte("wrong"), pebble.NoSync))
		}, errInvalidRecord},
		{"source ownership", func(t *testing.T, b *BuildStore) {
			raw, err := encodeNode(&graph.Node{ID: id, FilePath: "other.go"})
			require.NoError(t, err)
			require.NoError(t, b.pending.Set(pendingNodeKey(id), raw, pebble.NoSync))
		}, errInvalidRecord},
		{"edge identity", func(t *testing.T, b *BuildStore) {
			e := &graph.Edge{From: id, To: "a", Kind: graph.EdgeImports}
			key := pendingEdgeKey(e)
			e.From = "different"
			raw, err := encodeEdge(e)
			require.NoError(t, err)
			require.NoError(t, b.pending.Set(key, raw, pebble.NoSync))
		}, errInvalidRecord},
		{"target identity", func(t *testing.T, b *BuildStore) {
			b.AddEdge(&graph.Edge{From: id, To: "target", Kind: graph.EdgeImports})
			raw, err := encodeNode(&graph.Node{ID: "different", FilePath: "other.go"})
			require.NoError(t, err)
			require.NoError(t, b.pending.Set(pendingNodeKey("target"), raw, pebble.NoSync))
		}, errInvalidRecord},
		{"malformed target", func(t *testing.T, b *BuildStore) {
			b.AddEdge(&graph.Edge{From: id, To: "target", Kind: graph.EdgeImports})
			require.NoError(t, b.pending.Set(pendingNodeKey("target"), []byte{0xff}, pebble.NoSync))
		}, errInvalidRecord},
		{"byte limit", func(t *testing.T, b *BuildStore) {
			b.AddEdge(&graph.Edge{From: id, To: "target", Kind: graph.EdgeImports, Context: strings.Repeat("x", rebuildDirectImportReadBytes)})
		}, ErrRebuildLinkInputSize},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBuild(t, FullProfile)
			b.AddNode(&graph.Node{ID: id, FilePath: file})
			tc.corrupt(t, b)
			err := b.RebuildView().VisitFileImportTargets(t.Context(), file, visit)
			require.ErrorIs(t, err, ErrRebuildView)
			require.ErrorIs(t, err, tc.want)
		})
	}
	b := newBuild(t, FullProfile)
	b.AddNode(&graph.Node{ID: id, FilePath: file})
	b.AddEdge(&graph.Edge{From: id, To: "target", Kind: graph.EdgeImports})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, b.RebuildView().VisitFileImportTargets(ctx, file, visit), context.Canceled)
	reject := errors.New("stop imports")
	require.ErrorIs(t, b.RebuildView().VisitFileImportTargets(t.Context(), file, func(_, _ string) error { return reject }), reject)
}
