package store_pebble

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"testing"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/graph"
)

func newBuild(t *testing.T, profile Profile) *BuildStore {
	t.Helper()
	b, err := OpenBuild(filepath.Join(t.TempDir(), "graph"), profile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := b.Close(); err != nil {
			t.Error(err)
		}
	})
	return b
}

func snapshot(t *testing.T, b *BuildStore) *Snapshot {
	t.Helper()
	s, err := b.OpenSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}

func seedBuild(b *BuildStore) {
	var nodes []*graph.Node
	for _, id := range []string{"a", "b", "c", "d"} {
		nodes = append(nodes, &graph.Node{ID: id, Kind: graph.KindFunction, Name: id, QualName: "p." + id, FilePath: "a.go", RepoPrefix: "repo", Language: "go", Meta: map[string]any{"id": id}})
	}
	b.AddBatch(nodes, []*graph.Edge{
		{From: "a", To: "b", Kind: graph.EdgeCalls, Line: 30, FilePath: "a.go"},
		{From: "a", To: "c", Kind: graph.EdgeReferences, Line: 10, FilePath: "a.go"},
		{From: "b", To: "d", Kind: graph.EdgeCalls, Line: 40, FilePath: "a.go"},
		{From: "c", To: "d", Kind: graph.EdgeCalls, Line: 50, FilePath: "a.go"},
		{From: "d", To: "a", Kind: graph.EdgeCalls, Line: 60, FilePath: "a.go"},
		{From: "a", To: "b", Kind: graph.EdgeContains, Line: 20, FilePath: "a.go"},
	})
}

func TestNativeGoldAndSnapshot(t *testing.T) {
	b := newBuild(t, FullProfile)
	if s, err := b.OpenSnapshot(); s != nil || !errors.Is(err, ErrUnpublishedBuild) {
		t.Fatalf("unpublished snapshot = %v, %v", s, err)
	}
	seedBuild(b)
	if err := b.Publish(t.Context()); err != nil {
		t.Fatal(err)
	}
	s := snapshot(t, b)
	if got := s.Stats(); got.TotalNodes != 4 || got.TotalEdges != 6 || got.ByLanguage["go"] != 4 {
		t.Fatalf("saved stats = %+v", got)
	}
	levels := b.db.Metrics().Levels
	for i, level := range levels {
		if (i == goldLevel && level.TablesCount != 1) || (i != goldLevel && level.TablesCount != 0) {
			t.Errorf("L%d contains %d tables after one native ingest", i, level.TablesCount)
		}
	}
	if got := s.GetFileNodes("a.go"); len(got) != 4 || got[0].Meta["id"] != "a" {
		t.Fatalf("file scope lost saved rows: %v", got)
	}
	for _, read := range []func([]string) map[string][]*graph.Edge{s.GetInEdgesByNodeIDs, s.GetOutEdgesByNodeIDs} {
		if empty := read([]string{"", ""}); empty != nil {
			t.Fatalf("empty batch IDs returned %v, want nil", empty)
		}
		if got := read([]string{"a", "", "a", "missing"}); len(got) != 1 || len(got["a"]) == 0 {
			t.Fatalf("batch IDs returned %v, want only the adjacency for a", got)
		}
	}
	kinds := []graph.EdgeKind{graph.EdgeCalls, graph.EdgeReferences}
	rows, capped, err := s.ReadKindAdjacencyBatch(t.Context(), []string{"a", "b"}, graph.DirectionForward, kinds, 1)
	if err != nil || !capped || rows["a"].Total != 3 || len(rows["a"].Edges) != 1 || rows["a"].Edges[0].To != "c" {
		t.Fatalf("mixed-kind cap did not preserve single-read order: %v, %t, %v", rows, capped, err)
	}
	for _, dir := range []graph.Direction{graph.DirectionForward, graph.DirectionBackward, graph.DirectionBoth} {
		for _, depth := range []int{0, 1, 3} {
			got, err := s.Walk(t.Context(), []string{"a"}, dir, kinds, depth, 0, 0)
			want := walkOracle(s, []string{"a"}, dir, kinds, depth)
			if err != nil || !reflect.DeepEqual(got.Levels, want) {
				t.Fatalf("direction %d depth %d: levels %v vs %v: %v", dir, depth, got.Levels, want, err)
			}
		}
	}
	for _, tc := range []struct {
		nodes, hops int
		cap         graph.WalkCap
	}{{2, 0, graph.WalkNodeCap}, {0, 1, graph.WalkHopCap}} {
		got, err := s.Walk(t.Context(), []string{"a"}, graph.DirectionForward, kinds, 3, tc.nodes, tc.hops)
		if err != nil || got.StoppedBy != tc.cap || len(got.Levels) != 1 || len(got.Levels[0]) != 1 || got.Levels[0][0].Neighbor.ID != "c" {
			t.Fatalf("cap stop did not match first admitted hop: %v, %v", got, err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if got, err := s.Walk(ctx, []string{"a"}, graph.DirectionForward, kinds, 3, 0, 0); !errors.Is(err, context.Canceled) || got.Levels != nil {
		t.Fatalf("cancelled walk = %v, %v", got, err)
	}
	if err := s.ReadError(); err != nil {
		t.Fatal(err)
	}
	if err := b.Publish(t.Context()); err != nil {
		t.Fatalf("no-change publication: %v", err)
	}
}

// D38: legacy BuildStore queries may not leak pending state. Readers
// hold the previous publication across pending writes and the next ingest.
func TestBuildPublicationVisibility(t *testing.T) {
	b := newBuild(t, FullProfile)
	seedBuild(b)
	if n := b.GetNode("a"); n != nil || !errors.Is(b.ReadError(), ErrUnpublishedBuild) {
		t.Fatalf("pending lookup returned %+v, %v", n, b.ReadError())
	}
	if err := b.pending.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := b.Publish(t.Context()); err != nil {
		t.Fatal(err)
	}
	old := snapshot(t, b)
	assertOld := func() {
		n := old.GetNode("a")
		if n == nil || n.Name != "a" || old.NodeCount() != 4 || old.EdgeCount() != 6 || len(old.GetOutEdges("a")) != 3 || old.ReadError() != nil {
			t.Errorf("previous snapshot changed: a=%+v nodes=%d edges=%d error=%v", n, old.NodeCount(), old.EdgeCount(), old.ReadError())
		}
	}
	var wg sync.WaitGroup
	started := make(chan struct{}, 8)
	stop := make(chan struct{})
	for range 8 {
		wg.Go(func() {
			assertOld()
			started <- struct{}{}
			for range 64 {
				select {
				case <-stop:
					return
				default:
					assertOld()
				}
			}
		})
	}
	t.Cleanup(func() { close(stop); wg.Wait() })
	for range 8 {
		<-started
	}
	if !b.RemoveEdge("a", "b", graph.EdgeCalls) || b.RemoveEdge("a", "b", graph.EdgeCalls) {
		t.Fatal("removal did not find the spilled base edge exactly once")
	}
	b.AddNode(&graph.Node{ID: "e", Name: "new", Kind: graph.KindFunction, FilePath: "new.go", RepoPrefix: "repo"})
	b.AddEdge(&graph.Edge{From: "a", To: "e", Kind: graph.EdgeCalls, Line: 12})
	middle := snapshot(t, b)
	if middle.GetNode("e") != nil || middle.EdgeCount() != 6 || middle.ReadError() != nil {
		t.Fatalf("snapshot before next publish saw pending changes: %v", middle.ReadError())
	}
	if err := b.Publish(t.Context()); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	assertOld()
	next := snapshot(t, b)
	if got := next.GetNode("e"); got == nil || got.Name != "new" || next.NodeCount() != 5 || next.EdgeCount() != 6 {
		t.Fatalf("fresh snapshot did not see the new head: e=%+v nodes=%d edges=%d", got, next.NodeCount(), next.EdgeCount())
	}
	if e := next.GetOutEdges("a"); len(e) != 3 || e[0].To != "c" || slices.ContainsFunc(e, func(e *graph.Edge) bool { return e.Kind == graph.EdgeCalls && e.To == "b" }) {
		t.Fatalf("fresh adjacency did not apply its removal: %+v", e)
	}
	if err := next.ReadError(); err != nil {
		t.Fatal(err)
	}
	if nodes, edges := b.EvictRepo("repo"); nodes != 5 || edges != 6 {
		t.Fatalf("eviction of published state returned %d nodes / %d edges", nodes, edges)
	}
	if err := b.Publish(t.Context()); err != nil {
		t.Fatal(err)
	}
	empty := snapshot(t, b)
	if empty.NodeCount() != 0 || empty.EdgeCount() != 0 || empty.GetNode("a") != nil || empty.ReadError() != nil {
		t.Fatalf("eviction left visible data: %v", empty.ReadError())
	}
}

func walkOracle(s graph.Reader, frontier []string, dir graph.Direction, kinds []graph.EdgeKind, depth int) [][]graph.FrontierHop {
	seen := map[string]bool{"a": true}
	var out [][]graph.FrontierHop
	for len(frontier) != 0 && depth > 0 {
		depth--
		var next []string
		var level []graph.FrontierHop
		for _, id := range frontier {
			add := func(edges []*graph.Edge, reverse bool) {
				for _, e := range edges {
					if !slices.Contains(kinds, e.Kind) {
						continue
					}
					target := e.To
					if reverse {
						target = e.From
					}
					n := s.GetNode(target)
					if n == nil {
						continue
					}
					level = append(level, graph.FrontierHop{Edge: e, Neighbor: n})
					if !seen[target] {
						seen[target] = true
						next = append(next, target)
					}
				}
			}
			if dir != graph.DirectionBackward {
				add(s.GetOutEdges(id), false)
			}
			if dir != graph.DirectionForward {
				add(s.GetInEdges(id), true)
			}
		}
		if len(level) != 0 {
			out = append(out, level)
		}
		frontier = next
	}
	return out
}

func TestSavedCountsWhenGroupIsOff(t *testing.T) {
	b := newBuild(t, DefaultProfile)
	seedBuild(b)
	if err := b.Publish(t.Context()); err != nil {
		t.Fatal(err)
	}
	s := snapshot(t, b)
	if got := s.EdgeCount(); got != 5 {
		t.Fatalf("saved edges = %d", got)
	}
	got, err := s.ReadKindAdjacency(t.Context(), "a", graph.DirectionForward, []graph.EdgeKind{graph.EdgeCalls})
	if err != nil || got.Total != 3 || len(got.Edges) != 1 {
		t.Fatalf("saved counts lost the off group: %v, %v", got, err)
	}
	got, err = s.ReadKindAdjacency(t.Context(), "a", graph.DirectionForward, []graph.EdgeKind{graph.EdgeContains})
	if !errors.Is(err, ErrGroupOff) || len(got.Edges) != 0 {
		t.Fatalf("off group returned apparent success: %v, %v", got, err)
	}
}

func TestExactSymbolPathScope(t *testing.T) {
	b := newBuild(t, FullProfile)
	for _, row := range []struct{ id, name, path string }{
		{"a-out", "main", "repo/other/main.go"},
		{"b-in", "main", "repo/cmd/main.go"},
		{"c-fts", "launch", "repo/tools/launch.go"},
	} {
		b.AddNode(&graph.Node{ID: row.id, Kind: graph.KindFunction, Name: row.name, FilePath: row.path, RepoPrefix: "repo"})
	}
	if err := b.UpsertSymbolFTS("c-fts", "main"); err != nil {
		t.Fatal(err)
	}
	if err := b.Publish(t.Context()); err != nil {
		t.Fatal(err)
	}
	s := snapshot(t, b)
	read := func(t *testing.T, paths []string, limit int) []graph.SymbolHit {
		t.Helper()
		hits, err := s.search(t.Context(), "main", []string{"repo"}, paths, limit)
		if err != nil {
			t.Fatal(err)
		}
		return hits
	}
	out := graph.SymbolHit{NodeID: "a-out", Score: 100}
	in := graph.SymbolHit{NodeID: "b-in", Score: 100}
	t.Run("scoped-before-cap", func(t *testing.T) {
		if got := read(t, []string{"cmd/main.go"}, 1); !slices.Equal(got, []graph.SymbolHit{in}) {
			t.Fatalf("scoped exact hits = %v, want [%v]", got, in)
		}
	})
	t.Run("unscoped-retains-both", func(t *testing.T) {
		if got := read(t, nil, 2); !slices.Equal(got, []graph.SymbolHit{out, in}) {
			t.Fatalf("unscoped exact hits = %v, want [%v %v]", got, out, in)
		}
		if got := read(t, []string{"cmd"}, 2); !slices.Equal(got, []graph.SymbolHit{in}) {
			t.Fatalf("directory-scoped exact hits = %v, want [%v]", got, in)
		}
	})
	t.Run("all-exact-hits-out-of-scope", func(t *testing.T) {
		if got := read(t, []string{"missing"}, 2); len(got) != 0 {
			t.Fatalf("disjoint path scope returned %v", got)
		}
	})
	t.Run("empty-exact-tier-runs-fts", func(t *testing.T) {
		if got := read(t, []string{"tools"}, 2); len(got) != 1 || got[0].NodeID != "c-fts" || got[0].Score == 100 {
			t.Fatalf("FTS tier hits = %v, want c-fts only at a non-exact score", got)
		}
	})
}

func TestDeferredContentSearchRefuses(t *testing.T) {
	b := newBuild(t, FullProfile)
	seedBuild(b)
	if err := b.Publish(t.Context()); err != nil {
		t.Fatal(err)
	}
	s := snapshot(t, b)
	if hits, err := s.SearchContent("needle", "repo", 3); hits != nil || !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("deferred content search = %v, %v; want no hits and a typed refusal", hits, err)
	}
	if _, ok := any(s).(graph.ContentSearcher); ok {
		t.Fatal("published snapshot implements the content write interface")
	}
}

func TestVectorCapabilitiesRefuse(t *testing.T) {
	b := newBuild(t, FullProfile)
	v := graph.VectorSearcher(b)
	c := graph.AtomicVectorCorpusInstaller(b)
	_, similar := v.SimilarTo([]float32{1}, 1)
	_, install := c.ReplaceVectorCorpus(t.Context(), "repo", 1, []graph.VectorCorpusItem{{NodeID: "a", Vec: []float32{1}}})
	_, stats := c.VectorCorpusStats(t.Context(), 1)
	_, repoStats := c.VectorCorpusStatsForRepo(t.Context(), "repo", 1)
	for _, err := range []error{v.UpsertEmbedding("a", []float32{1}), v.BulkUpsertEmbeddings(nil), v.BuildVectorIndex(1), similar, install, stats, repoStats} {
		if !errors.Is(err, errors.ErrUnsupported) {
			t.Fatalf("unbuilt feature did not return typed refusal: %v", err)
		}
	}
	if got := v.GetEmbeddings([]string{"a"}); len(got) != 0 || !errors.Is(b.ReadError(), errors.ErrUnsupported) {
		t.Fatalf("value-only vector refusal silent: %v, %v", got, b.ReadError())
	}
}

func TestSnapshotRetainsFirstReadError(t *testing.T) {
	b := newBuild(t, FullProfile)
	seedBuild(b)
	if err := b.Publish(t.Context()); err != nil {
		t.Fatal(err)
	}
	key := keyStrings([]byte{familyNode}, "b")
	if err := b.db.Set(key, []byte{0xff}, pebble.Sync); err != nil {
		t.Fatal(err)
	}
	s := snapshot(t, b)
	if n := s.GetNode("a"); n == nil || n.ID != "a" || s.ReadError() != nil {
		t.Fatalf("did not read the saved prefix of the request: %v, %v", n, s.ReadError())
	}
	if n := s.GetNode("b"); n != nil || !errors.Is(s.ReadError(), errInvalidRecord) {
		t.Fatalf("corruption appeared successful: %v, %v", n, s.ReadError())
	}
	first := s.ReadError()
	s.retain(errors.New("second error"))
	if s.ReadError() != first {
		t.Fatal("later error replaced the typed first error")
	}
}

func TestSealRefusesMisfiledSecondary(t *testing.T) {
	b := newBuild(t, FullProfile)
	seedBuild(b)
	scratch := t.TempDir()
	format := b.db.FormatMajorVersion().MaxTableFormat()
	rows := sortedRuns{dir: scratch, name: "original", options: b.options, format: format, limit: 128}
	seal := goldSeal{refs: sortedRuns{dir: scratch, name: "refs", options: b.options, format: format, limit: 128}, owner: b.keptOwner, edgeOwner: b.rebuildEdgeSourceOwner}
	if err := b.writeRows(t.Context(), &rows, StageSearch); err != nil {
		t.Fatal(err)
	}
	broken := sortedRuns{dir: scratch, name: "bad", options: b.options, format: format, limit: 128}
	replaced := 0
	if err := rows.merge(t.Context(), func(r row) error {
		if len(r.key) > 0 && r.key[0] == familyFile && replaced == 0 {
			replaced++
			return broken.add(keyStrings([]byte{familyFile}, "repo", "wrong.go", "a"), bytes.Clone(r.value))
		}
		return broken.add(bytes.Clone(r.key), bytes.Clone(r.value))
	}); err != nil {
		t.Fatal(err)
	}
	if replaced != 1 || len(rows.paths) < 2 {
		t.Fatalf("fixture did not exercise a cross-run secondary mismatch: %d rows, %d paths", replaced, len(rows.paths))
	}
	if err := publishGold(t.Context(), b.db, &broken, &seal); !errors.Is(err, errInvalidRecord) {
		t.Fatalf("bad rows were not rejected: %v", err)
	}
	for i, level := range b.db.Metrics().Levels {
		if level.TablesCount != 0 {
			t.Fatalf("refusal published %d tables into L%d", level.TablesCount, i)
		}
	}
}

func TestLayoutASymbolHits(t *testing.T) {
	b := newBuild(t, FullProfile)
	// Insert z first, but a and z have equal rank. A canonical writer must
	// assign a the earlier ID independent of the order of these calls.
	for _, id := range []string{"z", "other", "a"} {
		b.AddNode(&graph.Node{ID: id, Name: "name" + id, Kind: graph.KindFunction, RepoPrefix: "r", FilePath: "r/x.go"})
		tokens := "needle item"
		if id == "other" {
			tokens = "haystack"
		}
		if err := b.UpsertSymbolFTS(id, tokens); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.Publish(t.Context()); err != nil {
		t.Fatal(err)
	}
	s := snapshot(t, b)
	stats, err := point(s, searchKey("stats"), searchReadStats)
	if err != nil || stats.Rows != 3 || stats.Tokens != 5 {
		t.Fatalf("saved corpus = %+v, %v", stats, err)
	}
	hits, err := s.SearchSymbols("needle item", 2)
	if err != nil || len(hits) != 2 || hits[0].NodeID != "a" || hits[1].NodeID != "z" || hits[0].Score <= 0 || hits[0].Score != hits[1].Score {
		t.Fatalf("ordered equal-score hits = %v, %v", hits, err)
	}
	hits[0].NodeID = "caller-owned"
	hits, err = s.SearchSymbols("needle item", 2)
	if err != nil || len(hits) != 2 || hits[0].NodeID != "a" || len(s.searchMemo) != 1 {
		t.Fatalf("cached hit changed or was not retained: %v, %v", hits, err)
	}
}

// goldFaultCursor returns valid rows, then fails instead of reaching clean EOF.
type goldFaultCursor struct {
	keys   []string
	at     int
	fault  error
	closed bool
}

func (c *goldFaultCursor) First() bool { c.at = 0; return c.valid() }
func (c *goldFaultCursor) Next() bool  { c.at++; return c.valid() }
func (c *goldFaultCursor) valid() bool { return c.at < len(c.keys) }
func (c *goldFaultCursor) Key() []byte { return []byte(c.keys[c.at]) }
func (c *goldFaultCursor) ValueAndErr() ([]byte, error) {
	return []byte("value"), nil
}
func (c *goldFaultCursor) Error() error {
	if c.valid() {
		return nil
	}
	return c.fault
}
func (c *goldFaultCursor) Close() error { c.closed = true; return nil }

func TestWriteCheckedGoldDiffReturnsCursorFailure(t *testing.T) {
	for _, tc := range []struct {
		name    string
		old     []string
		fresh   []string
		changes []string
	}{
		{name: "first", fresh: []string{"B"}},
		{name: "merge deletion", old: []string{"A"}, fresh: []string{"B"}, changes: []string{"A"}},
		{name: "equal row", old: []string{"A"}, fresh: []string{"A", "B"}},
		{name: "trailing deletion", old: []string{"A"}, changes: []string{"A"}},
		{name: "witness skip", old: []string{string(publicationWitnessKey)}, fresh: []string{"B"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBuild(t, FullProfile)
			runs := sortedRuns{dir: t.TempDir(), options: b.options, format: b.db.FormatMajorVersion().MaxTableFormat(), limit: goldBufferBytes}
			for _, key := range tc.fresh {
				if err := runs.add([]byte(key), []byte("value")); err != nil {
					t.Fatal(err)
				}
			}
			fault := errors.New("predecessor read failed")
			cursor := &goldFaultCursor{keys: tc.old, fault: fault}
			var changes []string
			paths, err := writeCheckedGoldDiff(t.Context(), cursor, &runs, 1,
				func(row) error { return nil },
				func(context.Context) error { t.Fatal("failed diff reached finish"); return nil },
				func(r row) error {
					if cursor.Error() != nil {
						t.Fatalf("classified %q after cursor failure", r.key)
					}
					changes = append(changes, string(r.key))
					return nil
				})
			if !errors.Is(err, fault) || len(paths) != 0 || !cursor.closed || !slices.Equal(changes, tc.changes) {
				t.Fatalf("diff = %v, %v; closed = %t; changes = %v, want %v", paths, err, cursor.closed, changes, tc.changes)
			}
		})
	}
}
