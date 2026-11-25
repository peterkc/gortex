package resolver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func frontierFixture(t *testing.T, lines int) (context.Context, *store_pebble.BuildStore, store_pebble.RebuildLinkOptions) {
	t.Helper()
	b, err := store_pebble.OpenBuild(filepath.Join(t.TempDir(), "build"), store_pebble.FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	b.AddNode(&graph.Node{ID: "initial", Name: "Run", QualName: "pkg.Run", RepoPrefix: "repo"})
	for line := lines; line > 0; line-- {
		b.AddEdge(&graph.Edge{From: "caller", Kind: graph.EdgeCalls, To: "unresolved::Run", Line: line})
	}
	lease := t.TempDir()
	keep := filepath.Join(lease, "sibling", "keep")
	require.NoError(t, os.Mkdir(filepath.Dir(keep), 0700))
	require.NoError(t, os.WriteFile(keep, []byte("unowned"), 0600))
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx, b, store_pebble.RebuildLinkOptions{
		ScratchParent: lease, SpillBytes: 1 << 20,
	}
}

func checkFrontierScratch(t *testing.T, lease string) {
	t.Helper()
	entries, err := os.ReadDir(lease)
	require.NoError(t, err)
	require.Len(t, entries, 1, "candidate scratch survived return")
	data, err := os.ReadFile(filepath.Join(lease, "sibling", "keep"))
	require.NoError(t, err)
	require.Equal(t, "unowned", string(data))
}

func TestRebuildLinkFrontierPages(t *testing.T) {
	ctx, b, opts := frontierFixture(t, resolvePendingPageRows+1)
	b.AddNode(&graph.Node{ID: "outside", Name: "Run", QualName: "pkg.Run", RepoPrefix: "z-repo"})
	var sizes []int
	rows := 0
	err := runRebuildLinkFrontier(ctx, b, opts, nil, func(f *rebuildLinkFrontier, page []*graph.Edge) error {
		sizes = append(sizes, len(page))
		for _, e := range page {
			rows++
			require.Equal(t, rows, e.Line) // last component of the complete old key
		}
		if len(sizes) == 1 {
			b.AddNode(&graph.Node{ID: "added", Name: "Run", QualName: "pkg.Run", RepoPrefix: "repo"})
			b.AddEdge(&graph.Edge{From: "later", Kind: graph.EdgeCalls, To: "unresolved::Later"})
		}
		requests := []store_pebble.LinkRequest{
			{Kind: store_pebble.LinkName, Key: "Run", RepoPrefix: "repo"},
			{Kind: store_pebble.LinkQualName, Key: "pkg.Run", AllRepos: true},
			{Kind: store_pebble.LinkName, Key: "Run", AllRepos: true},
		}
		var visits []string
		stats, err := f.candidates(ctx, requests, func(pos int, n *graph.Node) error {
			visits = append(visits, fmt.Sprintf("%d:%s", pos, n.ID))
			return nil
		})
		if err != nil {
			return err
		}
		require.Equal(t, 6, stats.ResultRows)
		require.Equal(t, []string{"0:initial", "1:initial", "1:outside", "2:added", "2:initial", "2:outside"}, visits)
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, []int{resolvePendingPageRows, 1}, sizes)
	checkFrontierScratch(t, opts.ScratchParent)
}

func TestRebuildLinkFrontierEmpty(t *testing.T) {
	ctx, b, opts := frontierFixture(t, 0)
	b.AddEdge(&graph.Edge{From: "caller", Kind: graph.EdgeCalls, To: "initial"})
	opts.SpillBytes = 0 // invalid if the reader starts
	noPage := func(*rebuildLinkFrontier, []*graph.Edge) error { return errors.New("unexpected pending page") }
	require.NoError(t, runRebuildLinkFrontier(ctx, b, opts, nil, noPage))
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	err := runRebuildLinkFrontier(canceled, b, opts, nil, noPage)
	require.ErrorIs(t, err, store_pebble.ErrRebuildView)
	require.ErrorIs(t, err, context.Canceled)
	checkFrontierScratch(t, opts.ScratchParent)
}

func TestRebuildLinkFrontierFailure(t *testing.T) {
	for _, mode := range []string{"cancel", "reducer"} {
		t.Run(mode, func(t *testing.T) {
			ctx, b, opts := frontierFixture(t, 2*resolvePendingPageRows+1)
			b.AddBatch([]*graph.Node{{ID: "a", Name: "A"}, {ID: "b", Name: "B"}}, nil)
			reject := errors.New("refused private page")
			pages, visits := 0, 0
			err := runRebuildLinkFrontier(ctx, b, opts, nil, func(f *rebuildLinkFrontier, _ []*graph.Edge) error {
				pages++
				if pages == 2 && mode == "reducer" {
					return reject
				}
				requests := []store_pebble.LinkRequest{{Kind: store_pebble.LinkName, Key: "A"}}
				readCtx := ctx
				if pages == 2 {
					var cancel context.CancelFunc
					readCtx, cancel = context.WithCancel(ctx)
					cancel()
					requests = append(requests, store_pebble.LinkRequest{Kind: store_pebble.LinkName, Key: "B"})
				}
				_, err := f.candidates(readCtx, requests, func(_ int, _ *graph.Node) error {
					visits++
					return nil
				})
				if err == nil {
					b.AddNode(&graph.Node{ID: "provisional", Name: "Private"})
				}
				return err
			})
			require.ErrorIs(t, err, store_pebble.ErrRebuildView)
			if mode == "cancel" {
				require.ErrorIs(t, err, store_pebble.ErrRebuildLink)
				require.ErrorIs(t, err, context.Canceled)
			} else {
				require.ErrorIs(t, err, reject)
			}
			require.Equal(t, 2, pages, "third pending page must not run")
			require.Equal(t, 1, visits)
			_, err = b.OpenSnapshot()
			require.ErrorIs(t, err, store_pebble.ErrUnpublishedBuild)
			checkFrontierScratch(t, opts.ScratchParent)
		})
	}
}
