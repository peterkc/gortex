package store_pebble

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"testing"

	"github.com/cockroachdb/pebble/v2"
	"github.com/cockroachdb/pebble/v2/sstable"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestRebuildViewCanonicalOrder(t *testing.T) {
	ctx := context.Background()
	for _, reverse := range []bool{false, true} {
		t.Run(fmt.Sprintf("reverse=%t", reverse), func(t *testing.T) {
			b := newBuild(t, FullProfile)
			v := b.RebuildView()
			nodes := []*graph.Node{{ID: "b", Name: "original", RepoPrefix: "r"}, {ID: "a"}, {ID: "z"}}
			edges := []*graph.Edge{
				{From: "b", To: "a", Kind: graph.EdgeCalls, FilePath: "a.go", Line: 2},
				{From: "a", To: "a", Kind: graph.EdgeReferences, FilePath: "a.go", Line: 1},
				{From: "a", To: "z", Kind: graph.EdgeCalls, FilePath: "a.go", Line: 1},
				{From: "a", To: "b", Kind: graph.EdgeCalls, FilePath: "z.go", Line: 0},
				{From: "a", To: "b", Kind: graph.EdgeCalls, FilePath: "a.go", Line: 10},
				{From: "a", To: "b", Kind: graph.EdgeCalls, FilePath: "a.go", Line: 2},
			}
			if reverse {
				slices.Reverse(nodes)
				slices.Reverse(edges)
			}
			b.AddBatch(nodes, edges)
			b.AddNode(&graph.Node{ID: "b", Name: "replacement"})
			b.AddEdge(&graph.Edge{From: "a", To: "b", Kind: graph.EdgeCalls, FilePath: "a.go", Line: 2, Origin: "fresh"})
			n, err := v.Node(ctx, "b")
			if err != nil || n == nil || n.Name != "replacement" || n.RepoPrefix != "r" {
				t.Fatalf("replacement point = %+v, %v", n, err)
			}
			if n, err := v.Node(ctx, "missing"); n != nil || err != nil {
				t.Fatalf("missing point = %+v, %v", n, err)
			}
			assertRebuildNodeIDs(t, func(yield func(*graph.Node) error) error { return v.VisitNodes(ctx, yield) }, "a", "b", "z")
			var got []string
			err = v.VisitEdges(ctx, func(e *graph.Edge) error {
				got = append(got, fmt.Sprintf("%s/%s/%s/%s/%d/%s", e.From, e.Kind, e.To, e.FilePath, e.Line, e.Origin))
				return nil
			})
			want := []string{"a/calls/b/a.go/2/fresh", "a/calls/b/a.go/10/", "a/calls/b/z.go/0/", "a/calls/z/a.go/1/", "a/references/a/a.go/1/", "b/calls/a/a.go/2/"}
			if err != nil || !slices.Equal(got, want) {
				t.Fatalf("saved edges = %v, %v; want %v", got, err, want)
			}
			if b.GetNode("b") != nil || len(b.AllNodes()) != 0 || len(b.AllEdges()) != 0 {
				t.Fatal("legacy build reads served working rows")
			}
		})
	}
}

func TestRebuildViewLinkPendingPages(t *testing.T) {
	ctx := t.Context()
	b := newBuild(t, FullProfile)
	sql, err := store_sqlite.Open(filepath.Join(t.TempDir(), "oracle.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := sql.Close(); err != nil {
			t.Error(err)
		}
	})
	a := &graph.Edge{From: "a", Kind: graph.EdgeCalls, To: "unresolved::A", FilePath: "one.go", Line: 1}
	c := &graph.Edge{From: "c", Kind: graph.EdgeCalls, To: "repo::UNRESOLVED::C", FilePath: "two.go", Line: 1}
	d := &graph.Edge{From: "d", Kind: graph.EdgeCalls, To: "unresolved::D", FilePath: "two.go", Line: 2}
	edges := []*graph.Edge{
		d, c, a,
		{From: "e", Kind: graph.EdgeCalls, To: "unresolved::fnvalue::E"},
		{From: "e", Kind: graph.EdgeCalls, To: "repo::unresolved::FNVALUE::F"},
		{From: "e", Kind: graph.EdgeCalls, To: "UNRESOLVED::G"},
		{From: "e", Kind: graph.EdgeCalls, To: "target"},
	}
	b.AddBatch(nil, edges)
	if err := sql.AddBatchChecked(nil, edges); err != nil {
		t.Fatal(err)
	}
	scan, err := sql.BeginUnresolvedEdgeScan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	oracle, err := sql.ReadUnresolvedEdgePage(ctx, scan, 0, 16, 16<<20)
	if err != nil || !oracle.Exhausted {
		t.Fatalf("read native frontier: %+v, %v", oracle, err)
	}
	keyOf := func(e *graph.Edge) string { return string(pendingEdgeKey(e)) }
	var want []string
	for _, e := range oracle.Edges {
		want = append(want, keyOf(e))
	}
	slices.Sort(want)
	if !slices.Equal(want, []string{keyOf(a), keyOf(c), keyOf(d)}) {
		t.Fatalf("native frontier fixture = %q", want)
	}
	rowBytes := func(e *graph.Edge) int {
		t.Helper()
		raw, err := encodeEdge(e)
		if err != nil {
			t.Fatal(err)
		}
		return len(raw) + len(pendingEdgeKey(e))
	}
	newEdge := &graph.Edge{From: "z", Kind: graph.EdgeCalls, To: "unresolved::New"}
	var got []string
	var counts []int
	err = b.RebuildView().VisitLinkPendingPages(ctx, 2, 16<<20, func(page []*graph.Edge) error {
		if len(counts) == 0 {
			b.AddEdge(newEdge)
		}
		counts = append(counts, len(page))
		for _, e := range page {
			got = append(got, keyOf(e))
		}
		return nil
	})
	if err != nil || !slices.Equal(counts, []int{2, 1}) || !slices.Equal(got, want) {
		t.Fatalf("pinned row pages: %v %q, %v", counts, got, err)
	}
	got = nil
	counts = nil
	err = b.RebuildView().VisitLinkPendingPages(ctx, 16, max(rowBytes(a), rowBytes(c), rowBytes(d), rowBytes(newEdge)), func(page []*graph.Edge) error {
		counts = append(counts, len(page))
		for _, e := range page {
			got = append(got, keyOf(e))
		}
		return nil
	})
	if err != nil || !slices.Equal(counts, []int{1, 1, 1, 1}) || !slices.Equal(got, append(want, keyOf(newEdge))) {
		t.Fatalf("fresh byte pages: %v %q, %v", counts, got, err)
	}
}

func TestRebuildViewLinkPendingPageFailures(t *testing.T) {
	ctx := t.Context()
	b := newBuild(t, FullProfile)
	v := b.RebuildView()
	a := &graph.Edge{From: "a", To: "unresolved::Run", Kind: graph.EdgeCalls}
	b.AddEdge(a)
	reject := errors.New("stop link page")
	err := v.VisitLinkPendingPages(ctx, 16, 16<<20, func([]*graph.Edge) error { return reject })
	if !errors.Is(err, ErrRebuildView) || !errors.Is(err, reject) {
		t.Fatalf("callback refusal = %v", err)
	}
	noVisit := func([]*graph.Edge) error {
		t.Fatal("oversize or unreadable frontier reached a reducer")
		return nil
	}
	if err := v.VisitLinkPendingPages(ctx, 16, 1, noVisit); !errors.Is(err, ErrRebuildView) || !errors.Is(err, ErrRebuildLinkInputSize) {
		t.Fatalf("oversize refusal = %v", err)
	}
	if err := b.pending.Set(pendingEdgeKey(a), []byte{0xff}, pebble.NoSync); err != nil {
		t.Fatal(err)
	}
	if err := v.VisitLinkPendingPages(ctx, 16, 16<<20, noVisit); !errors.Is(err, ErrRebuildView) || !errors.Is(err, errInvalidRecord) {
		t.Fatalf("corrupt pending row refusal = %v", err)
	}
}

func TestApplyLinkReindexesKeepsNativePage(t *testing.T) {
	ctx := t.Context()
	b := newBuild(t, FullProfile)
	sql, err := store_sqlite.Open(filepath.Join(t.TempDir(), "oracle.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := sql.Close(); err != nil {
			t.Error(err)
		}
	})
	var edges []*graph.Edge
	var moves []graph.EdgeReindex
	appendMove := func(oldTo, newTo string, kind graph.EdgeKind) {
		old := &graph.Edge{From: "caller", To: oldTo, Kind: graph.EdgeCalls, FilePath: "a.go", Line: 5}
		newEdge := *old
		newEdge.To, newEdge.Kind = newTo, kind
		edges = append(edges, old)
		moves = append(moves, graph.EdgeReindex{Edge: &newEdge, OldTo: oldTo, OldKind: old.Kind})
	}
	// Native first frees every moving row in this page. At 256 rows, the
	// first move would lose to the old Z and the following page would erase Z.
	appendMove("unresolved::A", "unresolved::Z", graph.EdgeCalls)
	for i := range 255 {
		appendMove(fmt.Sprintf("unresolved::N%03d", i), fmt.Sprintf("target:%03d", i), graph.EdgeCalls)
	}
	appendMove("unresolved::Z", "callee", graph.EdgeReferences)
	b.AddBatch(nil, edges)
	if err := sql.AddBatchChecked(nil, edges); err != nil {
		t.Fatal(err)
	}
	if !sql.ReindexEdgesApplied(moves) {
		t.Fatal("native reindex did not apply")
	}
	if err := b.ApplyLinkReindexes(ctx, moves); err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, e := range sql.AllEdges() {
		want = append(want, string(pendingEdgeKey(e)))
	}
	slices.Sort(want)
	if len(want) != len(moves) || !slices.Contains(want, string(pendingEdgeKey(moves[0].Edge))) ||
		!slices.Contains(want, string(pendingEdgeKey(moves[len(moves)-1].Edge))) {
		t.Fatalf("native fixture did not retain the incoming Z and the promoted reference: %d/%d rows", len(want), len(moves))
	}
	var got []string
	if err := b.RebuildView().VisitEdges(ctx, func(e *graph.Edge) error {
		got = append(got, string(pendingEdgeKey(e)))
		return nil
	}); err != nil || !slices.Equal(got, want) {
		t.Fatalf("private versus native saved identity mismatch: %d/%d rows, %v", len(got), len(want), err)
	}
}

func TestApplyLinkReindexesRefusesStalePage(t *testing.T) {
	ctx := t.Context()
	b := newBuild(t, FullProfile)
	old := graph.Edge{From: "a", To: "unresolved::A", Kind: graph.EdgeCalls}
	b.AddEdge(&old)
	moved := old
	moved.To = "resolved"
	stale := moved
	stale.From = "missing"
	err := b.ApplyLinkReindexes(ctx, []graph.EdgeReindex{{Edge: &moved, OldTo: old.To}, {Edge: &stale, OldTo: old.To}})
	if !errors.Is(err, ErrRebuildLink) || !errors.Is(err, errInvalidRecord) {
		t.Fatalf("stale edge refusal = %v", err)
	}
	if row, err := b.pendingEdge(pendingEdgeKey(&old)); err != nil || row == nil {
		t.Fatalf("old edge lost before validating the whole page: %+v, %v", row, err)
	}
	if row, err := b.pendingEdge(pendingEdgeKey(&moved)); err != nil || row != nil {
		t.Fatalf("new edge saved after a stale page: %+v, %v", row, err)
	}
}

func TestRebuildViewLiveCandidates(t *testing.T) {
	ctx := context.Background()
	b := newBuild(t, FullProfile)
	v := b.RebuildView()
	name := func(name string, languages []string, want ...string) {
		t.Helper()
		assertRebuildNodeIDs(t, func(yield func(*graph.Node) error) error {
			return v.VisitNameCandidates(ctx, name, languages, yield)
		}, want...)
	}
	qual := func(key string, want ...string) {
		t.Helper()
		assertRebuildNodeIDs(t, func(yield func(*graph.Node) error) error {
			return v.VisitQualNameCandidates(ctx, key, yield)
		}, want...)
	}
	b.AddBatch([]*graph.Node{
		{ID: "a-0-py", Name: "Run", QualName: "p.Run", RepoPrefix: "a", Language: "python"},
		{ID: "a-go-z", Name: "Run", QualName: "p.Run", RepoPrefix: "a", Language: "go"},
		{ID: "0", Name: "Run", QualName: "p.Run", RepoPrefix: "b", Language: "go", FilePath: "old.go"},
		{ID: "a-go-a", Name: "Run", QualName: "p.Run", RepoPrefix: "a", Language: "go"},
		{ID: "case", Name: "run", QualName: "p.run", RepoPrefix: "a", Language: "go"},
		{ID: "suffix", Name: "RunExtra", QualName: "p.RunExtra", RepoPrefix: "a", Language: "go"},
	}, nil)
	name("Run", []string{"python", "go", "go"}, "a-go-a", "a-go-z", "a-0-py", "0")
	name("Run", []string{"go"}, "a-go-a", "a-go-z", "0")
	name("Run", nil, "a-go-a", "a-go-z", "a-0-py", "0")
	qual("p.Run", "0", "a-0-py", "a-go-a", "a-go-z")

	b.AddNode(&graph.Node{ID: "synthetic", Name: "Run", QualName: "p.Run", Language: "go", FilePath: "new.go", RepoPrefix: "b", Meta: map[string]any{"synthetic": true}})
	name("Run", []string{"go"}, "a-go-a", "a-go-z", "0", "synthetic")
	// Rename a later row during a visit. The index and point dereference must
	// keep the same snapshot; a new call must see the committed replacement.
	assertRebuildNodeIDs(t, func(yield func(*graph.Node) error) error {
		return v.VisitNameCandidates(ctx, "Run", []string{"go"}, func(n *graph.Node) error {
			if n.ID == "a-go-a" {
				b.AddNode(&graph.Node{ID: "0", Name: "Moved", QualName: "q.Moved", RepoPrefix: "c", Language: "rust", FilePath: "moved.rs"})
			}
			return yield(n)
		})
	}, "a-go-a", "a-go-z", "0", "synthetic")
	name("Run", []string{"go"}, "a-go-a", "a-go-z", "synthetic")
	qual("p.Run", "a-0-py", "a-go-a", "a-go-z", "synthetic")
	name("Moved", nil, "0")
	qual("q.Moved", "0")
	if nodes, edges := b.EvictFile("old.go"); nodes != 0 || edges != 0 {
		t.Fatalf("old file eviction counted %d nodes, %d edges", nodes, edges)
	}
	if nodes, edges := b.EvictFile("new.go"); nodes != 1 || edges != 0 {
		t.Fatalf("file eviction counted %d nodes, %d edges", nodes, edges)
	}
	if nodes, edges := b.EvictRepo("c"); nodes != 1 || edges != 0 {
		t.Fatalf("repo eviction counted %d nodes, %d edges", nodes, edges)
	}
	name("Run", []string{"go"}, "a-go-a", "a-go-z")
	qual("p.Run", "a-0-py", "a-go-a", "a-go-z")
	name("Moved", nil)
	qual("q.Moved")
}

func assertRebuildNodeIDs(t *testing.T, read func(func(*graph.Node) error) error, want ...string) {
	t.Helper()
	var got []string
	err := read(func(n *graph.Node) error { got = append(got, n.ID); return nil })
	if err != nil || !slices.Equal(got, want) {
		t.Fatalf("working node IDs = %v, %v; want %v", got, err, want)
	}
}

func TestRebuildViewReadFailures(t *testing.T) {
	ctx := context.Background()
	b := newBuild(t, FullProfile)
	v := b.RebuildView()
	b.AddNode(&graph.Node{ID: "a", Name: "Run", QualName: "p.Run", RepoPrefix: "r", Language: "go"})
	b.AddNode(&graph.Node{ID: "b"})
	assertTyped := func(err, cause error) {
		t.Helper()
		if !errors.Is(err, ErrRebuildView) || !errors.Is(err, cause) {
			t.Fatalf("expected working read error with cause %v, got %v", cause, err)
		}
	}
	noVisit := func(*graph.Node) error { t.Fatal("unexpected visit"); return nil }
	stop := errors.New("writer refused")
	assertTyped(v.VisitNodes(ctx, func(*graph.Node) error { return stop }), stop)

	canceled, cancel := context.WithCancel(ctx)
	defer cancel()
	var visits int
	err := v.VisitNodes(canceled, func(*graph.Node) error { visits++; cancel(); return nil })
	assertTyped(err, context.Canceled)
	if visits != 1 {
		t.Fatalf("canceled stream visited %d rows, want 1", visits)
	}
	n, err := v.Node(canceled, "a")
	assertTyped(err, context.Canceled)
	if n != nil {
		t.Fatalf("canceled point returned %+v", n)
	}
	assertTyped(v.VisitQualNameCandidates(canceled, "", noVisit), context.Canceled)

	// A later dangling ID must fail the whole name lookup, not silently skip
	// that row or report the already visited prefix as complete.
	if err := b.pending.Set(keyStrings([]byte{familyName, nameRebuildScoped}, "Run", "r", "go", "z"), []byte("z"), pebble.NoSync); err != nil {
		t.Fatal(err)
	}
	visits = 0
	err = v.VisitNameCandidates(ctx, "Run", []string{"go"}, func(*graph.Node) error { visits++; return nil })
	assertTyped(err, errInvalidRecord)
	if visits != 1 {
		t.Fatalf("name stream visited %d rows before the bad entry, want 1", visits)
	}
	if err := b.pending.Set(keyStrings([]byte{familyName, nameQualified}, "stale", "a"), []byte("a"), pebble.NoSync); err != nil {
		t.Fatal(err)
	}
	assertTyped(v.VisitQualNameCandidates(ctx, "stale", noVisit), errInvalidRecord)
	if err := b.pending.Set(pendingNodeKey("bad"), []byte{0xff}, pebble.NoSync); err != nil {
		t.Fatal(err)
	}
	bad, err := v.Node(ctx, "bad")
	assertTyped(err, errInvalidRecord)
	if bad != nil {
		t.Fatalf("invalid point returned %+v", bad)
	}

	// Void legacy mutations retain failure. Stop a live stream at the first
	// failure and surface the same failure on subsequent reads, even misses.
	visits = 0
	err = v.VisitNodes(ctx, func(*graph.Node) error {
		visits++
		b.AddNode(&graph.Node{ID: "unsupported", Meta: map[string]any{"bad": func() {}}})
		return nil
	})
	assertTyped(err, errInvalidRecord)
	if visits != 1 {
		t.Fatalf("failed mutation let the view visit %d rows, want 1", visits)
	}
	assertTyped(v.VisitNameCandidates(ctx, "missing", nil, noVisit), errInvalidRecord)
	assertTyped(v.VisitNodes(ctx, noVisit), errInvalidRecord)
	assertTyped(v.VisitEdges(ctx, func(*graph.Edge) error { t.Fatal("unexpected edge visit"); return nil }), errInvalidRecord)
	_, err = v.Node(ctx, "missing")
	assertTyped(err, errInvalidRecord)
}

func TestRebuildViewStreamsLinkPartition(t *testing.T) {
	ctx := context.Background()
	b := newBuild(t, FullProfile)
	b.AddBatch([]*graph.Node{{ID: "b", Name: "Run"}, {ID: "a", Name: "Run"}}, nil)
	dir := t.TempDir()
	if err := buildLinkTables(ctx, dir, 1, func(yield func(*graph.Node) error) error {
		return b.RebuildView().VisitNodes(ctx, yield)
	}); err != nil {
		t.Fatal(err)
	}
	b.AddNode(&graph.Node{ID: "a", Name: "Moved"})
	assertRebuildNodeIDs(t, func(yield func(*graph.Node) error) error {
		return b.RebuildView().VisitNameCandidates(ctx, "Run", nil, yield)
	}, "b")
	table, err := openLinkTable(ctx, linkPartitionPath(dir, 0))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := table.Close(); err != nil {
			t.Error(err)
		}
	}()
	it, err := table.NewIter(sstable.NoTransforms, nil, nil, sstable.TableBlobContext{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := it.Close(); err != nil {
			t.Error(err)
		}
	}()
	assertRebuildNodeIDs(t, func(yield func(*graph.Node) error) error {
		prefix := linkPrefix(LinkName, "Run")
		for kv := it.SeekGE(prefix, 0); kv != nil && bytes.HasPrefix(kv.K.UserKey, prefix); kv = it.Next() {
			raw, _, err := kv.Value(nil)
			if err != nil {
				return err
			}
			n, err := decodeNode(raw)
			if err != nil {
				return err
			}
			if err := yield(n); err != nil {
				return err
			}
		}
		return it.Error()
	}, "a", "b")
}

func TestApplyLinkArgumentRefresh(t *testing.T) {
	ctx := t.Context()
	b := newBuild(t, FullProfile)
	old := graph.Edge{From: "caller", To: "callee", Kind: graph.EdgeArgOf, FilePath: "a.go", Line: 6}
	stationary := old
	stationary.To, stationary.Origin = "callee#param:x", graph.OriginASTResolved
	b.AddEdge(&old)
	b.AddEdge(&stationary)
	move := old
	move.To = stationary.To
	if err := b.applyRebuildLinkReindexes(ctx, []graph.EdgeReindex{{Edge: &move, OldTo: old.To,
		RefreshIdentity: true, OldFrom: old.From, OldFilePath: old.FilePath, OldLine: old.Line}}, graph.EdgeArgOf); err != nil {
		t.Fatal(err)
	}
	if got, err := b.pendingEdge(pendingEdgeKey(&old)); err != nil || got != nil {
		t.Fatalf("old edge survived: %+v, %v", got, err)
	}
	if got, err := b.pendingEdge(pendingEdgeKey(&stationary)); err != nil || got == nil || got.Origin != stationary.Origin {
		t.Fatalf("stationary payload changed: %+v, %v", got, err)
	}
	if count, err := pendingGet(b.pending, revisionKey, decodeCount); err != nil || count != 1 {
		t.Fatalf("losing refresh has revision count %d, %v", count, err)
	}
}
