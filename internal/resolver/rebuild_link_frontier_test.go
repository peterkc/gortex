package resolver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// Re-exec the same entry protocol as __rebuild-link-worker, without the Go
// test runner writing status text into the worker's stdout frames.
func TestRebuildLinkFrontierChild(t *testing.T) {
	i := slices.Index(os.Args, "--")
	if i < 0 {
		return
	}
	args := os.Args[i+1:]
	if len(args) != 3 || args[0] != "__rebuild-link-worker" {
		os.Exit(2)
	}
	spill, err := strconv.Atoi(args[2])
	if err == nil && os.Getenv("GORTEX_TEST_FRONTIER_CORRUPT") == "1" && filepath.Base(filepath.Dir(args[1])) == "partition-0" {
		// Damage a data block, not the index/footer used during startup.
		var f *os.File
		if f, err = os.OpenFile(args[1], os.O_WRONLY, 0); err == nil {
			_, err = f.WriteAt(bytes.Repeat([]byte{0xff}, 8), 0)
			err = errors.Join(err, f.Close())
		}
	}
	if err == nil {
		err = store_pebble.RunRebuildLinkWorker(context.Background(), args[1], spill, os.Stdin, os.Stdout)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func frontierFixture(t *testing.T, workers, lines int) (context.Context, *store_pebble.BuildStore, store_pebble.RebuildLinkOptions) {
	t.Helper()
	b, err := store_pebble.OpenBuild(filepath.Join(t.TempDir(), "build"), store_pebble.FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	b.AddNode(&graph.Node{ID: "initial", Name: "Run", QualName: "pkg.Run", RepoPrefix: "repo"})
	for line := lines; line > 0; line-- {
		b.AddEdge(&graph.Edge{From: "caller", Kind: graph.EdgeCalls, To: "unresolved::Run", Line: line})
	}
	exe, err := os.Executable()
	require.NoError(t, err)
	lease := t.TempDir()
	keep := filepath.Join(lease, "sibling", "keep")
	require.NoError(t, os.Mkdir(filepath.Dir(keep), 0700))
	require.NoError(t, os.WriteFile(keep, []byte("unowned"), 0600))
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)
	return ctx, b, store_pebble.RebuildLinkOptions{
		Binary: exe, WorkerArgs: []string{"-test.run=^TestRebuildLinkFrontierChild$", "--", "__rebuild-link-worker"},
		ScratchParent: lease, Partitions: workers, SpillBytes: 1 << 20, LegacyCandidates: true,
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
	for _, workers := range []int{1, 2} {
		t.Run(fmt.Sprintf("workers=%d", workers), func(t *testing.T) {
			ctx, b, opts := frontierFixture(t, workers, resolvePendingPageRows+1)
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
		})
	}
}

func TestRebuildLinkFrontierEmpty(t *testing.T) {
	ctx, b, opts := frontierFixture(t, 1, 0)
	b.AddEdge(&graph.Edge{From: "caller", Kind: graph.EdgeCalls, To: "initial"})
	opts.Binary = filepath.Join(opts.ScratchParent, "must-not-start")
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
	for _, mode := range []string{"child-read", "reducer"} {
		t.Run(mode, func(t *testing.T) {
			if mode == "child-read" {
				t.Setenv("GORTEX_TEST_FRONTIER_CORRUPT", "1")
			}
			ctx, b, opts := frontierFixture(t, 2, 2*resolvePendingPageRows+1)
			b.AddBatch([]*graph.Node{{ID: "a", Name: "A"}, {ID: "b", Name: "B"}}, nil)
			reject := errors.New("refused private page")
			pages, visits := 0, 0
			err := runRebuildLinkFrontier(ctx, b, opts, nil, func(f *rebuildLinkFrontier, _ []*graph.Edge) error {
				pages++
				if pages == 2 && mode == "reducer" {
					return reject
				}
				// As in TestRebuildLinkReapsOnFailure, A hits healthy
				// partition 1; B hits partition 0 with a damaged data block.
				requests := []store_pebble.LinkRequest{{Kind: store_pebble.LinkName, Key: "A"}}
				if pages == 2 {
					requests = append(requests, store_pebble.LinkRequest{Kind: store_pebble.LinkName, Key: "B"})
				}
				_, err := f.candidates(ctx, requests, func(_ int, _ *graph.Node) error {
					visits++
					return nil
				})
				if err == nil {
					b.AddNode(&graph.Node{ID: "provisional", Name: "Private"})
				}
				return err
			})
			require.ErrorIs(t, err, store_pebble.ErrRebuildView)
			if mode == "child-read" {
				require.ErrorIs(t, err, store_pebble.ErrRebuildLink)
				require.ErrorContains(t, err, "read candidate partition")
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
