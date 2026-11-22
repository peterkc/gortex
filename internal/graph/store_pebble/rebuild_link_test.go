package store_pebble

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cockroachdb/pebble/v2/sstable"
	"github.com/zzet/gortex/internal/graph"
)

func startLinkFixture(t *testing.T, opts RebuildLinkOptions, mode string, nodes []*graph.Node) (*RebuildLink, context.Context) {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	opts.Binary = exe
	opts.WorkerArgs = []string{"-test.run=^TestRebuildLinkChild$", "--", mode}
	opts.ScratchParent = t.TempDir()
	if err := os.WriteFile(filepath.Join(opts.ScratchParent, "keep"), []byte("owner data"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)
	pass, err := StartRebuildLink(ctx, opts, func(yield func(*graph.Node) error) error {
		for _, node := range nodes {
			if err := yield(node); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pass.Close() })
	return pass, ctx
}

func assertLinkReaped(t *testing.T, pass *RebuildLink) {
	t.Helper()
	if entries, err := os.ReadDir(filepath.Dir(pass.dir)); err != nil || len(entries) != 1 || entries[0].Name() != "keep" {
		t.Fatalf("parent scratch entries %v: %v", entries, err)
	}
	for i, w := range pass.workers[:pass.parts] {
		if w.cmd.ProcessState == nil {
			t.Errorf("partition %d was not reaped", i)
		}
	}
}

func TestRebuildLinkSourceOrder(t *testing.T) {
	makeNode := func(id, name, qual, repo, lang string) *graph.Node {
		return &graph.Node{ID: id, Kind: graph.KindFunction, Name: name, QualName: qual, RepoPrefix: repo, Language: lang}
	}
	// Source order differs from both ID order and language order.
	z := makeNode("z", "A", "pkg.A", "one", "go")
	a := makeNode("a", "A", "pkg.A", "one", "python")
	x := makeNode("x", "A", "other.A", "two", "go")
	b := makeNode("b", "B", "pkg.B", "one", "go")
	escaped := makeNode("escaped", "A\x00\xff", "", "one", "go")
	nodes := []*graph.Node{z, b, a, x, z, escaped}
	requests := []LinkRequest{
		{Kind: LinkName, Key: "A", RepoPrefix: "one", Languages: []string{"go", "go"}},
		{Kind: LinkName, Key: "B", RepoPrefix: "one"},
		{Kind: LinkName, Key: "A", AllRepos: true},
		{Kind: LinkQualName, Key: "pkg.A", AllRepos: true},
		{Kind: LinkName, Key: "missing", AllRepos: true},
		{Kind: LinkName, Key: "A", RepoPrefix: "one", Languages: []string{"go"}},
		{Kind: LinkName, Key: "A\x00\xff", AllRepos: true},
		{Kind: LinkQualName, Key: "pkg.A", RepoPrefix: "two", Languages: []string{"go"}},
		{Kind: LinkName, Key: "A", RepoPrefix: ""},
	}
	type hit struct {
		position int
		node     *graph.Node
	}
	want := []hit{{0, z}, {0, z}, {1, b}, {2, z}, {2, a}, {2, x}, {2, z}, {3, z}, {3, a}, {3, z}, {5, z}, {5, z}, {6, escaped}}
	for _, tc := range []struct {
		parts   int
		reverse bool
	}{{1, false}, {2, false}, {2, true}} {
		t.Run(fmt.Sprintf("parts=%d/reverse=%t", tc.parts, tc.reverse), func(t *testing.T) {
			pass, ctx := startLinkFixture(t, RebuildLinkOptions{Partitions: tc.parts, SpillBytes: 1 << 20, reverse: tc.reverse}, "ordinary", nodes)
			parts, _, err := pass.routePage(requests)
			if err != nil {
				t.Fatal(err)
			}
			if parts[0] != parts[2] || parts[0] != parts[5] || parts[0] != parts[8] || (tc.parts == 2 && parts[0] == parts[1]) {
				t.Fatalf("partition ownership %v", parts)
			}
			var got []hit
			stats, err := pass.SelectPage(ctx, requests, func(pos int, n *graph.Node) error {
				got = append(got, hit{pos, n})
				return nil
			})
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("got %#v, want %#v: %v", got, want, err)
			}
			if stats.ScannedRows != 24 || stats.ResultRows != len(want) || stats.ResultBytes <= 0 || stats.SpillRows != 0 || stats.SpillBytes != 0 {
				t.Fatalf("unexpected saved work counts: %+v", stats)
			}
			if err := pass.Close(); err != nil {
				t.Fatal(err)
			}
			assertLinkReaped(t, pass)
		})
	}
}

func TestRebuildLinkSpills(t *testing.T) {
	const rows = 384
	payload := strings.Repeat("payload#", 512) // 4096 bytes/row, over 1 MiB per page
	nodes := make([]*graph.Node, 0, rows)
	for i := range rows {
		nodes = append(nodes, &graph.Node{ID: strconv.Itoa(rows - i), Kind: graph.KindFunction, Name: "spill",
			RepoPrefix: "repo", Meta: map[string]any{"payload": payload}})
	}
	var first LinkPageStats
	for _, limit := range []int{1, 1 << 20} {
		pass, ctx := startLinkFixture(t, RebuildLinkOptions{Partitions: 2, SpillBytes: limit}, "ordinary", nodes)
		visits := 0
		stats, err := pass.SelectPage(ctx, []LinkRequest{{Kind: LinkName, Key: "spill", RepoPrefix: "repo"}}, func(pos int, n *graph.Node) error {
			if pos != 0 || visits >= len(nodes) || !reflect.DeepEqual(nodes[visits], n) {
				return fmt.Errorf("candidate differs at ordinal %d", visits)
			}
			visits++
			return nil
		})
		t.Logf("threshold=%d results=%+v", limit, stats)
		if err != nil || visits != rows || stats.ScannedRows != rows || stats.ResultRows != rows || stats.ResultBytes <= 1<<20 || stats.SpillRows != rows || stats.SpillBytes <= 0 {
			t.Fatalf("threshold %d produced %d visits and %+v: %v", limit, visits, stats, err)
		}
		if limit == 1 {
			first = stats
		} else if stats != first {
			t.Fatalf("thresholds produced different counts: %+v vs %+v", first, stats)
		}
		if err := pass.Close(); err != nil {
			t.Fatal(err)
		}
		assertLinkReaped(t, pass)
	}
}

func TestRebuildLinkRequestIDs(t *testing.T) {
	for _, tc := range []struct {
		mode    string
		clients int
	}{{"ordinary", 16}, {"reverse-replies", 2}} {
		t.Run(tc.mode, func(t *testing.T) {
			nodes := make([]*graph.Node, tc.clients)
			for i := range nodes {
				name := strconv.Itoa(i)
				nodes[i] = &graph.Node{ID: "node-" + name, Name: name}
			}
			pass, ctx := startLinkFixture(t, RebuildLinkOptions{Partitions: 1, SpillBytes: 1}, tc.mode, nodes)
			start := make(chan struct{})
			results := make(chan error, len(nodes))
			for _, node := range nodes {
				go func() {
					<-start
					var got []string
					stats, err := pass.SelectPage(ctx, []LinkRequest{{Kind: LinkName, Key: node.Name, AllRepos: true}}, func(pos int, n *graph.Node) error {
						if pos != 0 {
							return fmt.Errorf("unexpected position %d", pos)
						}
						got = append(got, n.ID)
						return nil
					})
					if err == nil && (!slices.Equal(got, []string{node.ID}) || stats.ResultRows != 1 || stats.SpillBytes <= 0) {
						err = fmt.Errorf("request %s saw %v, counts %+v", node.ID, got, stats)
					}
					results <- err
				}()
			}
			close(start)
			for range nodes {
				if err := <-results; err != nil {
					t.Error(err)
				}
			}
			if err := pass.Close(); err != nil {
				t.Fatal(err)
			}
			assertLinkReaped(t, pass)
		})
	}
}

func TestRebuildLinkReapsOnFailure(t *testing.T) {
	for _, tc := range []struct{ mode, message string }{
		{"bad-id", "unknown link reply ID 101"},
		{"corrupt-partition", "read candidate partition"},
		{"ordinary", "context canceled"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			nodes := []*graph.Node{{ID: "a", Name: "A"}, {ID: "b", Name: "B"}}
			pass, ctx := startLinkFixture(t, RebuildLinkOptions{Partitions: 2, SpillBytes: 1}, tc.mode, nodes)
			// B is on partition 0, A on partition 1. Do real work on the
			// unaffected partition before the next parallel lookup fails.
			good := LinkRequest{Kind: LinkName, Key: "A", AllRepos: true}
			bad := LinkRequest{Kind: LinkName, Key: "B", AllRepos: true}
			stats, err := pass.SelectPage(ctx, []LinkRequest{good}, func(_ int, n *graph.Node) error {
				if n.ID != "a" {
					return fmt.Errorf("first page hit %q", n.ID)
				}
				return nil
			})
			if err != nil || stats.ResultRows != 1 || stats.SpillBytes <= 0 {
				t.Fatalf("positive page %+v: %v", stats, err)
			}
			pageCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			visits := 0
			stats, err = pass.SelectPage(pageCtx, []LinkRequest{bad, good}, func(_ int, _ *graph.Node) error {
				visits++
				if tc.mode == "ordinary" {
					cancel()
				}
				return nil
			})
			if !errors.Is(err, ErrRebuildLink) || !strings.Contains(err.Error(), tc.message) || stats != (LinkPageStats{}) {
				t.Fatalf("got stats=%+v error=%v", stats, err)
			}
			if tc.mode == "ordinary" {
				if !errors.Is(err, context.Canceled) || visits != 1 {
					t.Fatalf("cancellation error %v after %d visits", err, visits)
				}
			} else if visits != 0 {
				t.Fatalf("visited %d after partition failure", visits)
			}
			assertLinkReaped(t, pass)
		})
	}
}

func TestRebuildLinkChild(t *testing.T) {
	index := slices.Index(os.Args, "--")
	if index < 0 {
		return
	}
	args := os.Args[index+1:]
	if len(args) != 3 {
		fmt.Fprintln(os.Stderr, "expected test mode, partition path and spill threshold")
		os.Exit(2)
	}
	limit, err := strconv.Atoi(args[2])
	if err == nil {
		err = runLinkTestChild(args[0], args[1], limit)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0) // do not write the testing package's text to the pipe
}

func runLinkTestChild(mode, path string, limit int) error {
	ctx := context.Background()
	firstPart := filepath.Base(filepath.Dir(path)) == "partition-0"
	if mode == "corrupt-partition" && firstPart {
		// Only touch the first data block. The footer and index still open;
		// the later lookup, not the startup handshake, must detect failure.
		f, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err != nil {
			return err
		}
		_, err = f.WriteAt(bytes.Repeat([]byte{0xff}, 8), 0)
		if err := errors.Join(err, f.Close()); err != nil {
			return err
		}
	}
	if mode == "ordinary" || mode == "corrupt-partition" || (mode == "bad-id" && !firstPart) {
		return RunRebuildLinkWorker(ctx, path, limit, os.Stdin, os.Stdout)
	}
	if err := writeLinkReply(os.Stdout, 0, LinkPageStats{}, nil); err != nil {
		return err
	}
	if mode == "bad-id" {
		id, _, err := readLinkFrame(os.Stdin)
		if err != nil {
			return err
		}
		if err := writeLinkReply(os.Stdout, id+100, LinkPageStats{}, nil); err != nil {
			return err
		}
	} else if err := writeLinkRepliesInReverse(ctx, path, limit); err != nil {
		return err
	}
	_, err := io.Copy(io.Discard, os.Stdin)
	return err
}

func writeLinkRepliesInReverse(ctx context.Context, path string, limit int) (err error) {
	table, err := openLinkTable(ctx, path)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, table.Close()) }()
	it, err := table.NewIter(sstable.NoTransforms, nil, nil, sstable.TableBlobContext{})
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, it.Close()) }()
	var ids [2]uint64
	var stats [2]LinkPageStats
	for i := range ids {
		id, raw, err := readLinkFrame(os.Stdin)
		if err != nil {
			return err
		}
		if id != uint64(i+1) {
			return fmt.Errorf("request ID %d at pipe position %d", id, i)
		}
		requests, err := readLinkRequests(raw)
		if err != nil {
			return err
		}
		result, err := selectLinkPartition(ctx, it, linkReplyPath(path, id), limit, requests)
		if err != nil {
			return err
		}
		ids[i], stats[i] = id, result
	}
	for i := len(ids) - 1; i >= 0; i-- {
		if err := writeLinkReply(os.Stdout, ids[i], stats[i], nil); err != nil {
			return err
		}
	}
	return nil
}
