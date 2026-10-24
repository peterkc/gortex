package runner

import (
	"context"
	"gxstudy/adapter"
	"gxstudy/dataset"
	"gxstudy/oracle"
	"gxstudy/workload"
	"slices"
	"testing"
	"time"
)

type nativeScoreSegment struct {
	adapter.Segment
	symbols, content []adapter.Hit
}

func (s nativeScoreSegment) SearchSymbols(context.Context, string, int) ([]adapter.Hit, error) {
	return slices.Clone(s.symbols), nil
}
func (s nativeScoreSegment) SearchContent(context.Context, string, int) ([]adapter.Hit, error) {
	return slices.Clone(s.content), nil
}
func nativeScoreFixture(t *testing.T) (map[string]adapter.Segment, []workload.Query, []workload.Expected, adapter.Capabilities) {
	t.Helper()
	ds := []dataset.Dataset{{Repo: "a", Symbols: []dataset.Symbol{{NodeID: "a0", Tokens: "graph graph graph graph"}, {NodeID: "a1", Tokens: "graph extra padding lowers this formula score"}, {NodeID: "no-match", Tokens: "other"}}, Content: []dataset.Content{{ChunkID: "a0#0", Body: "graph graph graph graph"}, {ChunkID: "a1#0", Body: "graph extra padding lowers this formula score"}}}, {Repo: "b", Symbols: []dataset.Symbol{{NodeID: "b0", Tokens: "graph graph graph graph"}, {NodeID: "b1", Tokens: "graph extra padding lowers this formula score"}}}}
	qs := []workload.Query{{ID: "single", Op: "symbols", Repo: "a", Key: "GRAPH absent", K: 1}, {ID: "content", Op: "content", Repo: "a", Key: "graph", K: 1}, {ID: "multi", Op: "multi_symbols", Key: "graph", K: 1}}
	gold := map[string]adapter.Segment{}
	for _, d := range ds {
		gold[d.Repo] = oracle.New(d)
	}
	multi := oracle.NewMulti(ds, gold)
	var want []workload.Expected
	for _, q := range qs {
		a, e := workload.Evaluate(context.Background(), gold, multi, q)
		if e != nil {
			t.Fatal(e)
		}
		want = append(want, workload.Expected{QueryID: q.ID, Answer: a})
	}
	var e error
	want, e = oracle.ReferenceAnswers(ds, qs, want)
	if e != nil {
		t.Fatal(e)
	}
	segments := map[string]adapter.Segment{"a": nativeScoreSegment{Segment: gold["a"], symbols: []adapter.Hit{{ID: "a1", Score: 9}}, content: []adapter.Hit{{ID: "a1#0", Score: 9}}}, "b": nativeScoreSegment{Segment: gold["b"], symbols: []adapter.Hit{{ID: "b1", Score: 1}}}}
	caps := adapter.Capabilities{Ranking: map[string]adapter.RankPolicy{"symbols": {Mode: "native"}, "content": {Mode: "native"}, "native_multi_symbols": {Mode: "native"}}}
	return segments, qs, want, caps
}
func TestNativeRankQualityAndMatchGate(t *testing.T) {
	segments, qs, want, caps := nativeScoreFixture(t)
	_, checks, e := measuredQueries(context.Background(), segments, adapter.Fanout{Segments: segments}, qs, want, caps)
	if e != nil || len(checks) != 3 {
		t.Fatal(checks, e)
	}
	for _, c := range checks {
		if c.Mismatch || !c.NativeScoring || c.LocalOverlap == nil || *c.LocalOverlap != 0 || c.GlobalOverlap == nil || *c.GlobalOverlap != 0 {
			t.Fatal("valid native matches must pass at zero formula overlap", c)
		}
	}
	result := Result{Checks: checks}
	summarizeOverlap(&result)
	for _, q := range qs {
		if result.LocalQuality[q.Op].Count != 1 || result.LocalQuality[q.Op].Mean != 0 || result.GlobalQuality[q.Op].Mean != 0 {
			t.Fatal("missing quality-only summaries", result.LocalQuality, result.GlobalQuality)
		}
	}
	// A native multi API has no harness merge input trace; both quality references
	// still survive the switch to the global reference answer.
	q := qs[2]
	q.Op = "native_multi_symbols"
	_, checks, e = measuredQueries(context.Background(), segments, adapter.Fanout{Segments: segments}, []workload.Query{q}, []workload.Expected{globalExpected(want[2])}, caps)
	if e != nil || checks[0].Mismatch || *checks[0].LocalOverlap != 0 || *checks[0].GlobalOverlap != 0 {
		t.Fatal(checks, e)
	}
	q = withPolicy(qs[0], caps)
	for _, hits := range [][]adapter.Hit{{{ID: "no-match", Score: 1}}, nil} {
		if c := compareExpected(q, want[0], workload.Answer{Hits: hits}); !c.Mismatch {
			t.Fatal("nonmatching/empty native answer passed", c)
		}
	}
	formula := caps
	formula.Ranking = map[string]adapter.RankPolicy{"symbols": {Mode: "harness", Exact: true}}
	if c := compareExpected(withPolicy(qs[0], formula), want[0], workload.Answer{Hits: []adapter.Hit{{ID: "a1", Score: 9}}}); !c.Mismatch || c.NativeScoring {
		t.Fatal("formula exactness was relaxed", c)
	}
	marker := qs[0]
	marker.ExactRank = true
	if c := compareExpected(withPolicy(marker, caps), want[0], workload.Answer{Hits: []adapter.Hit{{ID: "a1", Score: 9}}}); !c.Mismatch {
		t.Fatal("query-specific deterministic exactness was lost")
	}
}
func TestNativeFanoutChecksOwnScores(t *testing.T) {
	_, qs, want, caps := nativeScoreFixture(t)
	q := withPolicy(qs[2], caps)
	inputs := []adapter.SearchInput{{Repo: "a", Hits: []adapter.Hit{{ID: "a1", Score: 9}}}, {Repo: "b", Hits: []adapter.Hit{{ID: "b1", Score: 12}}}}
	good := workload.Answer{Hits: []adapter.Hit{{ID: "b1", Score: 12}}}
	if c := compareRankedAnswer(q, want[2], good, inputs); c.Mismatch {
		t.Fatal("native-score top-k was checked against formula ranking", c)
	}
	for _, got := range []workload.Answer{{Hits: []adapter.Hit{{ID: "a1", Score: 9}}}, {Hits: []adapter.Hit{{ID: "b1", Score: 11}}}} {
		if c := compareRankedAnswer(q, want[2], got, inputs); !c.Mismatch {
			t.Fatal("wrong own-score merge passed", c)
		}
	}
	inputs[0].Hits = []adapter.Hit{{ID: "no-match", Score: 1}}
	if c := compareRankedAnswer(q, want[2], good, inputs); !c.Mismatch {
		t.Fatal("hidden nonmatching source hit passed")
	}
}

type nativeReaderBackend struct {
	oracle.Backend
	caps adapter.Capabilities
}

func (b nativeReaderBackend) Capabilities() adapter.Capabilities { return b.caps }

type nativeReaderCatalog struct {
	adapter.Catalog
	view adapter.View
}

func (c nativeReaderCatalog) Pin(context.Context, []string) (adapter.View, func(), error) {
	return c.view, func() {}, nil
}
func TestNativeRankConcurrentReaderPath(t *testing.T) {
	segments, qs, want, caps := nativeScoreFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	old := adapter.SegmentRef{Repo: "a", Version: "old"}
	next := old
	next.Version = "new"
	cache := &readCache{segments: map[adapter.SegmentRef]adapter.Segment{next: segments["a"]}}
	control := startReaders(ctx, nativeReaderBackend{caps: caps}, nativeReaderCatalog{view: adapter.View{"a": next}}, cache, segments, old, qs[2:], want[2:], want[2:])
	control.published.Store(true)
	enough := func() bool {
		for i := 0; i < 8; i++ {
			if control.newCount[i].Load() < 2 {
				return false
			}
		}
		return true
	}
	for !enough() {
		select {
		case <-control.progress:
		case <-ctx.Done():
			finishReaders(control)
			t.Fatal(ctx.Err())
		}
	}
	samples, e := finishReaders(control)
	if e != nil {
		t.Fatal(e)
	}
	for _, sample := range samples {
		if sample.check.Mismatch || !sample.check.NativeScoring || sample.check.LocalOverlap == nil || *sample.check.LocalOverlap != 0 || *sample.check.GlobalOverlap != 0 {
			t.Fatal("reader did not use the native policy/merge gate", sample.check)
		}
	}
}
