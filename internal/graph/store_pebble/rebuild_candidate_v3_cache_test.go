package store_pebble

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
)

// Use the actual node codec and reserve before decoding, as the ordered reader
// does. The physical frame/key bytes in this fixture are a fixed small header.
func v3FixtureLoad(t *testing.T, n *graph.Node, exceptional bool) func(func(int, bool) error) (*v3Piece, error) {
	t.Helper()
	raw, err := encodeNode(n)
	if err != nil {
		t.Fatal(err)
	}
	return func(reserve func(int, bool) error) (*v3Piece, error) {
		encoded := len(raw) + 32
		if err := reserve(encoded, exceptional); err != nil {
			return nil, err
		}
		decoded, err := decodeNode(raw)
		if err != nil {
			return nil, err
		}
		return &v3Piece{encoded: encoded, unbounded: exceptional, rows: []v3CachedRow{{node: decoded, rawBytes: len(raw), canonicalBytes: len(raw), orderBytes: len(raw) + 16}}}, nil
	}
}

func TestV3CacheEvictionReload(t *testing.T) {
	const budget = 16 << 10
	c := newV3Cache(budget, 3)
	n := &graph.Node{ID: "fixture", Meta: map[string]any{"nested": []any{map[string]any{"payload": strings.Repeat("x", 4096)}}}}
	load := v3FixtureLoad(t, n, false)
	first, err := c.get("a", 0, load)
	if err != nil {
		t.Fatal(err)
	}
	if first.charge <= 0 || first.charge > c.half || v3ReserveBytes(first.encoded) <= budget {
		t.Fatalf("fixture did not separate retention and reserve: charge=%d", first.charge)
	}
	if hit, err := c.get("a", 0, func(func(int, bool) error) (*v3Piece, error) { t.Fatal("cache hit reloaded"); return nil, nil }); err != nil || hit != first {
		t.Fatalf("hit=%p err=%v, want %p", hit, err, first)
	}
	for i, key := range []string{"b", "c"} {
		if _, err := c.get(key, uint64(i+1), load); err != nil {
			t.Fatal(err)
		}
	}
	reloaded, err := c.get("a", 0, load)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded == first || reloaded.rows[0].node.ID != first.rows[0].node.ID {
		t.Fatal("evicted piece did not reload its original data")
	}
	if c.slot(1) != nil || c.slot(0) == nil || c.slot(2) == nil || v3RetainedCharge(c) > budget {
		t.Fatalf("cache retention, charge=%d", v3RetainedCharge(c))
	}
	if err := c.close(); err != nil {
		t.Fatal(err)
	}
}

func TestV3CacheMutation(t *testing.T) {
	n := &graph.Node{Meta: map[string]any{"nested": []any{map[string]any{"payload": strings.Repeat("x", 4096)}}}}
	load := v3FixtureLoad(t, n, false)
	mutate := func(p *v3Piece) { p.rows[0].node.Meta["nested"].([]any)[0].(map[string]any)["payload"] = "changed" }
	for _, closeOnly := range []bool{false, true} {
		c := newV3Cache(16<<10, 3)
		p, err := c.get("a", 0, load)
		if err != nil {
			t.Fatal(err)
		}
		mutate(p)
		if closeOnly {
			err = c.close()
		} else {
			if _, err := c.get("b", 1, load); err != nil {
				t.Fatal(err)
			}
			_, err = c.get("c", 2, load)
		}
		if err == nil || !strings.Contains(err.Error(), "mutated") {
			t.Fatalf("closeOnly=%t: mutation not reported: %v", closeOnly, err)
		}
		_ = c.close()
	}
}

func TestV3CacheUnretained(t *testing.T) {
	for _, fixture := range []struct {
		name        string
		node        *graph.Node
		budget      int64
		exceptional bool
	}{
		{"budget", &graph.Node{ID: "x"}, 1, false},
		{"oversized", &graph.Node{ID: strings.Repeat("x", v3CachePieceBytes)}, 512 << 20, false},
		{"exception", &graph.Node{Meta: map[string]any{"large": v3CacheMap(v3SingleTableEntries + 1)}}, 512 << 20, true},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			c := newV3Cache(fixture.budget, 1)
			load := v3FixtureLoad(t, fixture.node, fixture.exceptional)
			first, err := c.get("k", 0, load)
			if err != nil {
				t.Fatal(err)
			}
			p, err := c.get("k", 0, load)
			if err != nil {
				t.Fatal(err)
			}
			if p == first || c.gens[0].charge != 0 || c.gens[1].charge != 0 || c.slot(0) != nil || c.borrowed == nil {
				t.Fatal("unretained piece entered a generation")
			}
			p.rows[0].node.ID = "mutated unretained row"
			if err := c.close(); err == nil || !strings.Contains(err.Error(), "mutated") {
				t.Fatalf("unretained mutation not reported: %v", err)
			}
		})
	}
	if got := v3ReserveBytes(math.MaxInt); got != math.MaxInt64 {
		t.Fatalf("overflow reserve=%d", got)
	}
	c := newV3Cache(0, 1)
	_, err := c.get("x", 0, func(reserve func(int, bool) error) (*v3Piece, error) { return nil, reserve(math.MaxInt, false) })
	if err == nil || !strings.Contains(err.Error(), "reserve overflow") {
		t.Fatalf("overflow=%v", err)
	}
	if err := c.close(); err != nil {
		t.Fatal(err)
	}
}

func TestV3CacheNextKey(t *testing.T) {
	c := newV3Cache(1<<20, 1)
	next := strings.Repeat("x", v3CachePieceBytes)
	p, err := c.get("k", 0, func(reserve func(int, bool) error) (*v3Piece, error) {
		encoded := len(next) + 100
		if err := reserve(encoded, false); err != nil {
			return nil, err
		}
		return &v3Piece{encoded: encoded, next: next, rows: []v3CachedRow{{node: &graph.Node{ID: "a"}}, {node: &graph.Node{ID: "b"}}}}, nil
	})
	if err != nil || p.next != next || p.encoded != len(next)+100 {
		t.Fatalf("next key falsely classified as oversized row: err=%v", err)
	}
	if err := c.close(); err != nil {
		t.Fatal(err)
	}
}

func TestV3CacheReserveAndDeferredError(t *testing.T) {
	c := newV3Cache(1<<20, 1)
	_, err := c.get("k", 0, func(func(int, bool) error) (*v3Piece, error) { return &v3Piece{}, nil })
	if err == nil {
		t.Fatal("load without reserve succeeded")
	}
	_, err = c.get("k", 0, func(reserve func(int, bool) error) (*v3Piece, error) {
		if err := reserve(32, false); err != nil {
			return nil, err
		}
		_ = reserve(32, false) // The cache detects a broken loader even if it ignores the second error.
		return &v3Piece{encoded: 32}, nil
	})
	if err == nil {
		t.Fatal("load with duplicate reserve succeeded")
	}
	wanted := errors.New("deferred codec error")
	p, err := c.get("k", 0, func(reserve func(int, bool) error) (*v3Piece, error) {
		if err := reserve(32, false); err != nil {
			return nil, err
		}
		return &v3Piece{encoded: 32, rows: []v3CachedRow{{node: &graph.Node{ID: "light"}, err: wanted}}}, nil
	})
	if err != nil || p.rows[0].err != wanted || c.slot(0) != nil {
		t.Fatalf("deferred row converted into cache failure: %v", err)
	}
	if err := c.close(); err != nil {
		t.Fatal(err)
	}
}

func v3CacheMap(n int) map[string]any {
	m := make(map[string]any, n)
	for i := range n {
		m[fmt.Sprint(i)] = nil
	}
	return m
}

type v3JSONMap map[string]any // unmodeled type deliberately exercises metaTagJSON

func TestV3CacheMetaTags(t *testing.T) {
	long := strings.Repeat("z", 8192)
	meta := map[string]any{
		"nil": nil, "string": long, "bool": true, "int": int(-3), "int64": int64(-4), "uint64": uint64(7), "float64": math.Inf(1),
		"ints": []int{1, -2}, "int64s": []int64{1, -4}, "uint64s": []uint64{1, 2}, "floats": []float64{1, math.NaN()}, "bools": []bool{true, false},
		"strings": []string{"a", long}, "intmap": map[string]int{"x": 5}, "map": map[string]any{"nested": long}, "maps": []map[string]any{{"nested": long}},
		"any":   []any{[]any{map[string]any{"nested": long}}, nil, []float64{math.NaN()}},
		"shape": &contracts.Shape{Kind: "struct", Fields: []contracts.ShapeField{{Name: "X", Type: long, JSONTag: "x", Required: true, Comment: long}}, Notes: []string{long}},
		"json":  v3JSONMap{"nested": []any{map[string]any{"payload": long}}, "value": json.Number("3")},
	}
	raw, err := encodeNode(&graph.Node{Meta: meta})
	if err != nil {
		t.Fatal(err)
	}
	if large, err := v3UnboundedRecord(raw); err != nil || large {
		t.Fatalf("closed native/JSON tags misclassified: large=%t err=%v", large, err)
	}
	n, err := decodeNode(raw)
	if err != nil {
		t.Fatal(err)
	}
	// Nine separately decoded long strings, not just their map entry headers.
	charge := v3NodeCharge(n)
	if charge < 9*int64(len(long)) || charge > v3ReserveBytes(len(raw)) {
		t.Fatalf("decoded charge=%d raw=%d", charge, len(raw))
	}
	first, err := v3Fingerprint(n)
	if err != nil {
		t.Fatal(err)
	}
	second, err := v3Fingerprint(n)
	if err != nil || second != first {
		t.Fatal("unchanged decoded tags hashed differently")
	}
	n.Meta["shape"].(*contracts.Shape).Fields[0].Type = "changed"
	third, err := v3Fingerprint(n)
	if err != nil || third == first {
		t.Fatal("nested Shape mutation missed")
	}
	if got := v3MetaValueCharge(make([]int, 1, 1024)); got < 8192 {
		t.Fatalf("slice capacity undercharged: %d", got)
	}
}

func TestV3CachePreflight(t *testing.T) {
	for _, count := range []int{v3SingleTableEntries, v3SingleTableEntries + 1} {
		m := v3CacheMap(count)
		im := make(map[string]int, count)
		for k := range m {
			im[k] = 1
		}
		for _, value := range []any{m, im, []map[string]any{m}, []any{m}, v3JSONMap(m), v3JSONMap{"nested": m}} {
			raw, err := encodeNode(&graph.Node{Meta: map[string]any{"x": value}})
			if err != nil {
				t.Fatal(err)
			}
			large, err := v3UnboundedRecord(raw)
			if err != nil || large != (count > v3SingleTableEntries) {
				t.Fatalf("type=%T count=%d large=%t err=%v", value, count, large, err)
			}
			n, err := decodeNode(raw)
			if err != nil {
				t.Fatal(err)
			}
			if got := v3NodeCharge(n); (got == math.MaxInt64) != (count > v3SingleTableEntries) {
				t.Fatalf("type=%T count=%d charge=%d", value, count, got)
			}
			if large, err := v3UnboundedRecord(raw[:len(raw)-1]); err == nil || !large {
				t.Fatalf("truncated raw not isolated: large=%t err=%v", large, err)
			}
		}
	}
	for _, depth := range []int{1000, 1001} {
		raw := []byte(strings.Repeat("[", depth) + "null" + strings.Repeat("]", depth))
		if large, err := v3LargeJSONObject(raw); err != nil || large != (depth > 1000) {
			t.Fatalf("deep JSON depth=%d large=%t err=%v", depth, large, err)
		}
	}
	// Colons in escaped JSON strings must not count as object members.
	if large, err := v3LargeJSONObject([]byte(`{"x":"a\"` + strings.Repeat(":", 1000) + `"}`)); err != nil || large {
		t.Fatalf("quoted colon count: large=%t err=%v", large, err)
	}
}

func TestV3CacheIndexSlots(t *testing.T) {
	const budget = 8 << 10
	c := newV3Cache(budget, 1<<30)
	if len(c.index) == 0 || uint64(len(c.index)) >= c.pieceCount {
		t.Fatal("bounded slots not selected")
	}
	load := v3FixtureLoad(t, &graph.Node{ID: "first"}, false)
	first, err := c.get("first", 0, load)
	if err != nil || c.slot(0) == nil {
		t.Fatalf("initial retention: %v", err)
	}
	collision := uint64(len(c.index))
	other, err := c.get("other", collision, v3FixtureLoad(t, &graph.Node{ID: "other"}, false))
	if err != nil || other.rows[0].node.ID != "other" || c.borrowed == nil || c.slot(collision).id != 0 {
		t.Fatalf("collision: %v", err)
	}
	got, err := c.get("first", 0, func(func(int, bool) error) (*v3Piece, error) { t.Fatal("resident lost on collision"); return nil, nil })
	if err != nil || first != got || v3RetainedCharge(c) > budget {
		t.Fatalf("got %p first %p charge %d error %v", got, first, v3RetainedCharge(c), err)
	}
	if err := c.close(); err != nil {
		t.Fatal(err)
	}
}
