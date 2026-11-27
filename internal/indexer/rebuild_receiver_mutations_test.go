package indexer

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func rebuildReceiverFixture(sites int) ([]*graph.Node, []*graph.Edge) {
	method := func(id, name string) *graph.Node {
		return &graph.Node{ID: id, Name: name, Kind: graph.KindMethod, Meta: map[string]any{"receiver": "T"}}
	}
	field := func(id, name string) *graph.Node {
		return &graph.Node{ID: id, Name: name, Kind: graph.KindField, Meta: map[string]any{"receiver": "T"}}
	}
	const a, b, c, d, g, f = "p/t.go::A", "p/t.go::B", "p/t.go::C", "p/t.go::D", "p/t.go::G", "q/t.go::F"
	const counter, duplicate, guard, foreign = "p/a.go::counter", "p/z.go::counter", "p/a.go::guard", "q/a.go::counter"
	nodes := []*graph.Node{
		method(a, "A"), method(b, "B"), method(c, "C"), method(d, "D"), method(g, "G"), method(f, "0Foreign"),
		field(counter, "counter"), field(duplicate, "counter"), field(guard, "guard"), field(foreign, "counter"),
	}
	call := func(from, to, file string, line int, field string, self bool) *graph.Edge {
		return &graph.Edge{From: from, To: to, Kind: graph.EdgeCalls, FilePath: file, Line: line,
			Meta: map[string]any{"recv_field": field, "recv_self": self}}
	}
	edges := []*graph.Edge{
		{From: c, To: duplicate, Kind: graph.EdgeWrites, FilePath: "p/t.go", Line: 3},
		{From: c, To: counter, Kind: graph.EdgeWrites, FilePath: "p/t.go", Line: 4},
		{From: f, To: foreign, Kind: graph.EdgeWrites, FilePath: "q/t.go", Line: 3},
		// wrong owner
		{From: f, To: guard, Kind: graph.EdgeWrites, FilePath: "q/t.go", Line: 4},
		call(a, b, "p/t.go", 10, "", true),
		call(a, b, "p/t.go", 9, "", true),
		// path sorts before line
		call(a, b, "p/z.go", 1, "", true),
		call(b, c, "p/t.go", 5, "", true),
		// receiver cycle
		call(c, a, "p/t.go", 2, "", true),
		call(d, f, "p/t.go", 12, "guard", false),
		// field beats self; the later "0Foreign" call replaces "A" at this site
		call(d, a, "p/t.go", 12, "guard", true),
		// foreign self cannot add or emit
		call(d, f, "p/t.go", 13, "", true),
		call(d, "unresolved::Lock", "p/t.go", 14, "guard", false),
		// missing field does not fall through to self
		call(d, f, "p/t.go", 15, "missing", true),
	}
	for j := range sites {
		file := "p/t.go"
		if sites > 1 {
			file = "p/" + strings.Repeat("w", 4096) + ".go"
		}
		edges = append(edges, call(g, "unresolved::Lock", file, j+1, "guard", false))
	}
	return nodes, edges
}

func TestRebuildReceiverMutationsNativeRows(t *testing.T) {
	for _, spillAt := range []int{1, 1 << 20} {
		for _, reverse := range []bool{false, true} {
			t.Run(fmt.Sprintf("spill=%d/reverse=%t", spillAt, reverse), func(t *testing.T) {
				nodes, edges := rebuildReceiverFixture(320)
				if reverse {
					slices.Reverse(nodes)
					slices.Reverse(edges)
				}
				b := openRebuildReceiverFixture(t, nodes, edges)
				native, err := store_sqlite.Open(filepath.Join(t.TempDir(), "native.sqlite"))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := native.Close(); err != nil {
						t.Error(err)
					}
				})
				if err := native.AddBatchChecked(nodes, edges); err != nil {
					t.Fatal(err)
				}
				if _, _, _, err := synthesizeCapabilityEdges(native); err != nil {
					t.Fatal(err)
				}
				var want []string
				for e := range native.EdgesByKind(graph.EdgeAccessesField) {
					if e.Meta["indirect"] == true {
						want = append(want, rebuildReceiverRow(t, e))
					}
				}
				if len(want) != 6 {
					t.Fatalf("native saved %d indirect rows, want 6", len(want))
				}
				scratch := t.TempDir()
				var tentative []*graph.Edge
				stats, err := b.RebuildView().VisitReceiverMutations(t.Context(), scratch, spillAt, rebuildReceiverRules(), func(e *graph.Edge) error {
					tentative = append(tentative, e)
					return nil
				})
				if err != nil || stats.Emitted != 6 || stats.Facts != 6 || stats.Rounds != 3 || stats.RoundBound != 12 || stats.SpillBytes == 0 {
					t.Fatalf("receiver stats = %+v, %v", stats, err)
				}
				assertRebuildReceiverScratchEmpty(t, scratch)
				b.AddBatch(nil, tentative)
				var got []string
				if err := b.RebuildView().VisitEdges(t.Context(), func(e *graph.Edge) error {
					if e.Kind == graph.EdgeAccessesField {
						got = append(got, rebuildReceiverRow(t, e))
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				slices.Sort(got)
				slices.Sort(want)
				if !slices.Equal(got, want) {
					t.Fatalf("complete saved indirect rows differ:\n got: %q\nwant: %q", got, want)
				}
			})
		}
	}
}

func rebuildReceiverRow(t *testing.T, e *graph.Edge) string {
	t.Helper()
	// The public Edge JSON excludes these two saved fields.
	raw, err := json.Marshal(struct {
		*graph.Edge
		Confidence float64
		Meta       map[string]any
	}{e, e.Confidence, e.Meta})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func openRebuildReceiverFixture(t *testing.T, nodes []*graph.Node, edges []*graph.Edge) *store_pebble.BuildStore {
	t.Helper()
	b, err := store_pebble.OpenBuild(filepath.Join(t.TempDir(), "working"), store_pebble.FullProfile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := b.Close(); err != nil {
			t.Error(err)
		}
	})
	b.AddBatch(nodes, edges)
	return b
}

func assertRebuildReceiverScratchEmpty(t *testing.T, scratch string) {
	t.Helper()
	entries, err := os.ReadDir(scratch)
	if err != nil || len(entries) != 0 {
		t.Fatalf("receiver scratch = %v, %v", entries, err)
	}
}

func TestRebuildReceiverMutationsRefusesAndCleans(t *testing.T) {
	consumerErr := errors.New("planted consumer failure")
	for _, mode := range []string{"ceiling", "read", "write", "consumer"} {
		t.Run(mode, func(t *testing.T) {
			lastGood := &graph.Node{ID: "old.go::saved", Name: "saved", Kind: graph.KindFunction}
			b := openRebuildReceiverFixture(t, []*graph.Node{lastGood}, nil)
			if err := b.Publish(t.Context()); err != nil {
				t.Fatal(err)
			}
			readPublished := func() []*graph.Node {
				t.Helper()
				snap, err := b.OpenSnapshot()
				if err != nil {
					t.Fatal(err)
				}
				rows := snap.AllNodes()
				if edges := snap.AllEdges(); len(edges) != 0 {
					t.Fatalf("last good published edges = %v, want none", edges)
				}
				if err := errors.Join(snap.ReadError(), snap.Close()); err != nil {
					t.Fatal(err)
				}
				return rows
			}
			before := readPublished()
			if len(before) != 1 || before[0].ID != lastGood.ID {
				t.Fatalf("missing last good row: %v", before)
			}
			nodes, edges := rebuildReceiverFixture(1)
			b.AddBatch(nodes, edges)
			scratch := t.TempDir()
			rules := rebuildReceiverRules()
			wantCalls := 0
			var want error
			planted := false
			switch mode {
			case "ceiling":
				rules.RoundLimit = 1
				want = store_pebble.ErrReceiverMutationNonconvergent
			case "read", "write":
				rules.OwnerKey = func(id, receiver string) string {
					if planted {
						return receiverOwnerKey(id, receiver)
					}
					dirs, err := os.ReadDir(scratch)
					if err != nil || len(dirs) != 1 {
						t.Fatalf("private scratch was not opened: %v, %v", dirs, err)
					}
					dir := filepath.Join(scratch, dirs[0].Name())
					if mode == "write" {
						path := filepath.Join(dir, "owners-0000.sst")
						if err := os.Mkdir(path, 0700); err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(filepath.Join(path, "block-remove"), nil, 0600); err != nil {
							t.Fatal(err)
						}
						planted = true
					} else if err := os.Remove(filepath.Join(dir, "owners.part-0.sst")); err == nil {
						planted = true
					} else if !errors.Is(err, os.ErrNotExist) {
						t.Fatal(err)
					}
					return receiverOwnerKey(id, receiver)
				}
				if mode == "read" {
					want = os.ErrNotExist
				}
			case "consumer":
				want, wantCalls = consumerErr, 1
			}
			calls := 0
			stats, err := b.RebuildView().VisitReceiverMutations(t.Context(), scratch, 1, rules, func(*graph.Edge) error {
				calls++
				return consumerErr
			})
			if !errors.Is(err, store_pebble.ErrRebuildReceiverMutation) || (want != nil && !errors.Is(err, want)) || calls != wantCalls || stats != (store_pebble.ReceiverMutationStats{}) {
				t.Fatalf("expected refusal %v with %d calls; stats=%+v calls=%d error=%v", want, wantCalls, stats, calls, err)
			}
			if mode == "write" || mode == "read" {
				var pathErr *os.PathError
				if !planted || !errors.As(err, &pathErr) || !strings.Contains(pathErr.Path, "owners") {
					t.Fatalf("did not fail on the planted scratch %s: %v", mode, err)
				}
			}
			assertRebuildReceiverScratchEmpty(t, scratch)
			if after := readPublished(); !reflect.DeepEqual(after, before) {
				t.Fatalf("last good published nodes changed: before=%+v after=%+v", before, after)
			}
		})
	}
}
