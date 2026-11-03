package oracle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"gxstudy/adapter"
	"gxstudy/dataset"
	"gxstudy/workload"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"
)

func indexedFixture(t *testing.T, reverse bool) (dataset.Dataset, *CheckerView) {
	t.Helper()
	d := tiny()
	d.Nodes = append(d.Nodes, dataset.Node{ID: "zero", Name: "Zero"})
	edge := dataset.Edge{From: "b", To: "a", Kind: "calls", FilePath: "b.go", Line: 7, Confidence: .7, Payload: map[string]json.RawMessage{"opaque": json.RawMessage(`{"x":[1,null]}`)}}
	d.Edges = []dataset.Edge{edge, edge, {From: "a", To: "a", Kind: "calls"}, {From: "external", To: "a", Kind: "returns", FilePath: "external.go", Line: 42, Confidence: 1, CrossRepo: true}, {From: "a", To: "dangling", Kind: "calls"}}
	for i := 0; i < workload.FanoutCap+5; i++ {
		id := fmt.Sprintf("n%02d", i)
		d.Edges = append(d.Edges, dataset.Edge{From: id, To: "a", Kind: "calls"}, dataset.Edge{From: "g" + id, To: id, Kind: "calls"}, dataset.Edge{From: "gg" + id, To: "g" + id, Kind: "calls"})
	}
	if reverse {
		slices.Reverse(d.Edges)
	}
	dir := t.TempDir()
	if _, err := dataset.Write(dir, d); err != nil {
		t.Fatal(err)
	}
	source, err := dataset.OpenSource(dir)
	if err != nil {
		t.Fatal(err)
	}
	view, err := NewCheckerView(context.Background(), []dataset.Dataset{source})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := view.Close(); err != nil {
			t.Error(err)
		}
	})
	return source, view
}

// Each traversal frontier independently compares full ordered edge payloads.
// A missing index entry must fail here even if a final ID set looks plausible.
type frontierParity struct {
	scan, indexed *DiskSegment
	calls         int
}

func (p *frontierParity) InEdges(ctx context.Context, id, kind string) ([]dataset.Edge, error) {
	want, err := p.scan.InEdges(ctx, id, kind)
	if err != nil {
		return nil, err
	}
	got, err := p.indexed.InEdges(ctx, id, kind)
	if err != nil {
		return nil, err
	}
	p.calls++
	if !reflect.DeepEqual(want, got) {
		return nil, fmt.Errorf("frontier parity failed: %s %s", id, kind)
	}
	return got, nil
}

func TestIndexedEdgesAndEveryBFSFrontierMatchScan(t *testing.T) {
	ctx := context.Background()
	for _, reverse := range []bool{false, true} {
		t.Run(fmt.Sprint(reverse), func(t *testing.T) {
			source, view := indexedFixture(t, reverse)
			scan := &DiskSegment{Data: source}
			indexed := view.segments[source.Repo].(*DiskSegment)
			endpoints := map[string]bool{"unknown": true, "zero": true}
			for e, err := range source.EdgeRows() {
				if err != nil {
					t.Fatal(err)
				}
				endpoints[e.From] = true
				endpoints[e.To] = true
			}
			for id := range endpoints {
				for _, kind := range []string{"", "any", "calls", "returns", "absent"} {
					for _, incoming := range []bool{false, true} {
						scanCall, indexedCall := scan.OutEdges, indexed.OutEdges
						if incoming {
							scanCall, indexedCall = scan.InEdges, indexed.InEdges
						}
						want, err := scanCall(ctx, id, kind)
						if err != nil {
							t.Fatal(err)
						}
						got, err := indexedCall(ctx, id, kind)
						if err != nil {
							t.Fatal(err)
						}
						if !reflect.DeepEqual(want, got) {
							t.Fatalf("endpoint=%s kind=%s incoming=%v: %v != %v", id, kind, incoming, got, want)
						}
					}
				}
			}
			for _, depth := range []int{2, 3} {
				parity := &frontierParity{scan: scan, indexed: indexed}
				got, err := workload.Traverse(ctx, parity, "a", depth)
				if err != nil {
					t.Fatal(err)
				}
				want, err := workload.Traverse(ctx, scan, "a", depth)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(want, got) || parity.calls < workload.FanoutCap {
					t.Fatal("missing frontier coverage", depth, parity.calls, got, want)
				}
				if slices.Contains(got, "n24") || slices.Contains(got, "a") {
					t.Fatal("fanout cap or self-loop changed", got)
				}
			}
		})
	}
}

func TestDroppedIndexEntryFailsFrontierParity(t *testing.T) {
	source, view := indexedFixture(t, false)
	indexed := view.segments[source.Repo].(*DiskSegment)
	delete(indexed.index.incoming, "a")
	parity := &frontierParity{scan: &DiskSegment{Data: source}, indexed: indexed}
	if _, err := workload.Traverse(context.Background(), parity, "a", 3); err == nil {
		t.Fatal("dropped index entry escaped scan parity")
	}
}

func TestIndexedBFSReadsOnlyMatchingSourceRows(t *testing.T) {
	source, view := indexedFixture(t, false)
	segment := view.segments[source.Repo].(*DiskSegment)
	index := segment.index
	if index.decodedRows != source.Meta.Counts.Edges {
		t.Fatal("construction was not one full source pass", index.decodedRows)
	}
	for repeat := 0; repeat < 3; repeat++ {
		before := index.decodedRows
		if _, err := workload.Traverse(context.Background(), segment, "a", 3); err != nil {
			t.Fatal(err)
		}
		// Every edge is returned at most once during this BFS. A whole-file scan
		// for each of its >20 frontiers necessarily exceeds this exact row bound.
		if decoded := index.decodedRows - before; decoded <= 0 || decoded > source.Meta.Counts.Edges {
			t.Fatal("BFS reparsed unrelated source rows", decoded)
		}
	}
}

func TestEdgeIndexRejectsMalformedAndUnterminatedSourceLikeScan(t *testing.T) {
	for _, mode := range []string{"json", "utf8", "no-newline", "checksum", "count"} {
		t.Run(mode, func(t *testing.T) {
			source, _ := indexedFixture(t, false)
			path := filepath.Join(source.Source.Dir, "edges.jsonl")
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "json":
				raw = []byte("{broken}\n")
			case "utf8":
				raw = []byte{0xff, '\n'}
			case "no-newline":
				raw = raw[:len(raw)-1]
			case "checksum":
				source.Meta.SHA256["edges.jsonl"] = "bad"
			case "count":
				source.Meta.Counts.Edges++
			}
			if err = os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			if mode != "checksum" {
				source.Meta.SHA256["edges.jsonl"], err = dataset.Hash(path)
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err = newEdgeIndex(context.Background(), source); err == nil {
				t.Fatal("invalid index admitted", mode)
			}
			if mode == "json" || mode == "utf8" || mode == "no-newline" {
				if _, err = (&DiskSegment{Data: source}).InEdges(context.Background(), "a", "any"); err == nil {
					t.Fatal("scan failed to reject malformed source", mode)
				}
			}
		})
	}
}

func TestCheckerViewRejectsChangedSourceAndDescriptor(t *testing.T) {
	for _, mode := range []string{"truncate", "same-size", "replace", "manifest", "descriptor", "symlink"} {
		t.Run(mode, func(t *testing.T) {
			source, view := indexedFixture(t, false)
			path := filepath.Join(source.Source.Dir, "edges.jsonl")
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "truncate":
				err = os.Truncate(path, 1)
			case "same-size":
				raw[2] = 'X'
				err = os.WriteFile(path, raw, 0600)
				if err == nil {
					err = os.Chtimes(path, time.Unix(1, 0), time.Unix(1, 0))
				}
			case "replace":
				err = os.Remove(path)
				if err == nil {
					err = os.WriteFile(path, raw, 0600)
				}
			case "manifest":
				err = os.WriteFile(filepath.Join(source.Source.Dir, "manifest.json"), []byte("{}\n"), 0600)
			case "descriptor":
				source.Meta.Seed++
				if _, err := NewCheckerView(context.Background(), []dataset.Dataset{source}); err == nil {
					t.Fatal("changed descriptor admitted")
				}
				return
			case "symlink":
				external := filepath.Join(t.TempDir(), "edges")
				err = os.WriteFile(external, raw, 0600)
				if err == nil {
					err = os.Remove(path)
				}
				if err == nil {
					err = os.Symlink(external, path)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			q := workload.Query{ID: "changed", Op: "in", Repo: source.Repo, Key: "unknown", Kind: "any"}
			if _, err = view.Reference(context.Background(), q, nil); err == nil {
				t.Fatal("changed source gave a false empty pass", mode)
			}
		})
	}
}

func TestCheckerViewCancellationClosureAndOverlayFallback(t *testing.T) {
	source, view := indexedFixture(t, false)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewCheckerView(cancelled, []dataset.Dataset{source}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := view.indexes[0].edges(cancelled, "unknown", "any", true); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := view.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := view.indexes[0].file.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatal("index handle left open", err)
	}
	if err := view.Validate(context.Background()); err == nil {
		t.Fatal("closed view accepted")
	}
	changed, err := adapter.ChangeSource(source, adapter.FileChange{Path: "a.go", Replacement: dataset.Dataset{Repo: source.Repo, Edges: []dataset.Edge{{From: "new", To: "a", Kind: "calls"}}}})
	if err != nil {
		t.Fatal(err)
	}
	overlay, err := NewCheckerView(context.Background(), []dataset.Dataset{changed})
	if err != nil {
		t.Fatal(err)
	}
	defer overlay.Close()
	if len(overlay.indexes) != 0 {
		t.Fatal("overlay reused immutable index")
	}
	memory, err := NewCheckerView(context.Background(), []dataset.Dataset{tiny()})
	if err != nil {
		t.Fatal(err)
	}
	defer memory.Close()
	if len(memory.indexes) != 0 {
		t.Fatal("memory data created a disk index")
	}
	q := workload.Query{ID: "overlay", Op: "in", Repo: source.Repo, Key: "a", Kind: "any"}
	want, err := DiskReference(context.Background(), []dataset.Dataset{changed}, q, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := overlay.Reference(context.Background(), q, nil)
	if err != nil || !reflect.DeepEqual(want, got) {
		t.Fatal("overlay scan differs", err, want, got)
	}
}

func TestCheckerReferenceMatchesAllUnchangedCallers(t *testing.T) {
	ctx := context.Background()
	var sources []dataset.Dataset
	for _, repo := range []string{"one", "two"} {
		d := dataset.Derive(tiny(), repo, 1, 7)
		dir := t.TempDir()
		if _, err := dataset.Write(dir, d); err != nil {
			t.Fatal(err)
		}
		source, err := dataset.OpenSource(dir)
		if err != nil {
			t.Fatal(err)
		}
		sources = append(sources, source)
	}
	view, err := NewCheckerView(ctx, sources)
	if err != nil {
		t.Fatal(err)
	}
	defer view.Close()
	for _, op := range []string{"id", "name", "file", "out", "in", "traverse2", "traverse3", "symbols", "content", "vector", "multi_name", "multi_in", "multi_symbols", "native_multi_symbols", "multi_vector", "multi_traverse2", "multi_traverse3"} {
		q := workload.Query{ID: op, Op: op, Repo: "one", Key: "one::a", Kind: "any", K: 2, Depth: 2, Vec: tiny().Vectors[0].Values}
		switch op {
		case "name", "multi_name":
			q.Key = "Alpha"
		case "file":
			q.Key = "a.go"
		case "symbols", "content", "multi_symbols", "native_multi_symbols":
			q.Key = "graph"
		case "traverse3", "multi_traverse3":
			q.Depth = 3
		}
		if len(op) >= 6 && op[:6] == "multi_" || op == "native_multi_symbols" {
			q.Repo = ""
		}
		ids := []string{"one::a", "two::a", "unknown"}
		want, err := DiskReference(ctx, sources, q, ids)
		if err != nil {
			t.Fatal(op, err)
		}
		got, err := view.Reference(ctx, q, ids)
		if err != nil || !reflect.DeepEqual(want, got) {
			t.Fatal(op, err, want, got)
		}
	}
}

// Cancels synchronously at a particular context check. Done/Err still come from
// a real cancellable context, without scheduling or wall-time assumptions.
type cancelAtCheck struct {
	context.Context
	cancel context.CancelFunc
	checks int
}

func (c *cancelAtCheck) Err() error {
	c.checks++
	if c.checks == 4 {
		c.cancel()
	}
	return c.Context.Err()
}
func TestEdgeIndexCancellationDuringConstructionAndLookup(t *testing.T) {
	source, view := indexedFixture(t, false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	building := &cancelAtCheck{Context: ctx, cancel: cancel}
	if _, err := newEdgeIndex(building, source); !errors.Is(err, context.Canceled) || building.checks != 4 {
		t.Fatal("construction ignored mid-stream cancellation", err, building.checks)
	}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	reading := &cancelAtCheck{Context: ctx, cancel: cancel}
	before := view.indexes[0].decodedRows
	rows, err := view.indexes[0].edges(reading, "a", "any", true)
	if !errors.Is(err, context.Canceled) || rows != nil || view.indexes[0].decodedRows-before != 2 {
		t.Fatal("lookup returned partial rows or ignored cancellation", err, rows)
	}
}
