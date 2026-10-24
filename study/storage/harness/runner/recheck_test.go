package runner

import (
	"context"
	"fmt"
	"gxstudy/adapter"
	"gxstudy/conformance"
	"gxstudy/dataset"
	"gxstudy/oracle"
	"gxstudy/workload"
	"path/filepath"
	"testing"
	"time"
)

func TestHarnessFloorsAndNativePolicies(t *testing.T) {
	caps := adapter.Capabilities{Ranking: map[string]adapter.RankPolicy{"symbols": {MinOverlap: 0}, "native_multi_symbols": {Exact: true}}}
	q := withPolicy(workload.Query{Op: "symbols"}, caps)
	if q.MinOverlap != workload.MinimumRankedOverlap {
		t.Fatal("adapter lowered floor", q)
	}
	native := withPolicy(workload.Query{Op: "native_multi_symbols"}, caps)
	if !native.ExactRank {
		t.Fatal("native policy ignored")
	}
	local := withPolicy(workload.Query{Op: "multi_symbols"}, caps)
	baseline := withPolicy(workload.Query{Op: "multi_symbols"}, adapter.Capabilities{})
	if local.ExactRank != baseline.ExactRank || local.MinOverlap != baseline.MinOverlap {
		t.Fatal("native policy changed local gate")
	}
	if unsupported(adapter.Capabilities{Unsupported: map[string]string{"symbols": "absent"}}, workload.Query{Op: "multi_symbols"}) == "" {
		t.Fatal("default multi failed to inherit base unsupported")
	}
}
func TestLocalGateAndGlobalQualityAreSeparate(t *testing.T) {
	a, b := dataset.Dataset{Repo: "a"}, dataset.Dataset{Repo: "b"}
	for i := 0; i < 10; i++ {
		text := "graph other words text node"
		if i == 0 {
			text = "graph graph graph graph graph"
		}
		a.Symbols = append(a.Symbols, dataset.Symbol{NodeID: fmt.Sprintf("a%d", i), Tokens: text})
		text = "other words text node tokens"
		if i == 0 {
			text = "graph"
		}
		b.Symbols = append(b.Symbols, dataset.Symbol{NodeID: fmt.Sprintf("b%d", i), Tokens: text})
	}
	ds := []dataset.Dataset{a, b}
	segments := map[string]adapter.Segment{"a": oracle.New(a), "b": oracle.New(b)}
	globalMulti := oracle.NewMulti(ds, segments)
	q := workload.Query{ID: "q", Op: "multi_symbols", Key: "graph", K: 1}
	g, e := workload.Evaluate(context.Background(), segments, globalMulti, q)
	if e != nil {
		t.Fatal(e)
	}
	want, e := oracle.ReferenceAnswers(ds, []workload.Query{q}, []workload.Expected{{QueryID: "q", Answer: g}})
	if e != nil {
		t.Fatal(e)
	}
	_, checks, e := measuredQueries(context.Background(), segments, adapter.Fanout{Segments: segments}, []workload.Query{q}, want, adapter.Capabilities{Ranking: map[string]adapter.RankPolicy{"native_multi_symbols": {Exact: true}}})
	if e != nil || checks[0].Mismatch || checks[0].GlobalOverlap == nil || *checks[0].GlobalOverlap != 0 {
		t.Fatal("local gate was global quality gate", checks, e)
	}
	q.Op = "native_multi_symbols"
	_, nativeChecks, e := measuredQueries(context.Background(), segments, globalMulti, []workload.Query{q}, []workload.Expected{globalExpected(want[0])}, adapter.Capabilities{Ranking: map[string]adapter.RankPolicy{"native_multi_symbols": {Exact: true}}})
	if e != nil || nativeChecks[0].Mismatch {
		t.Fatal(nativeChecks, e)
	}
	r := Result{Checks: append(checks, nativeChecks...)}
	summarizeOverlap(&r)
	if r.Overlap["multi_symbols"].Min != 1 || r.GlobalQuality["multi_symbols"].Min != 0 || r.GlobalQuality["native_multi_symbols"].Min != 1 {
		t.Fatal(r.Overlap, r.GlobalQuality)
	}
}

type warmSegment struct {
	adapter.Segment
	calls int
}

func (s *warmSegment) NativeTraverse(context.Context, string, int, int, int) ([]string, error) {
	s.calls++
	return []string{"visited"}, nil
}

type warmMulti struct {
	adapter.MultiRepo
	calls int
}

func (m *warmMulti) SearchSymbols(context.Context, string, int) ([]adapter.Hit, error) {
	m.calls++
	return []adapter.Hit{{ID: "hit", Score: 1}}, nil
}
func TestNativePathsHaveDiscardedWarmup(t *testing.T) {
	s := &warmSegment{}
	q := workload.Query{Op: "native_traverse2", Repo: "r", Depth: 2}
	_, checks, e := warmedQueries(context.Background(), map[string]adapter.Segment{"r": s}, nil, []workload.Query{q}, []workload.Expected{{Answer: workload.Answer{IDs: []string{"visited"}}}}, adapter.Capabilities{}, 7)
	if e != nil || s.calls != 2 || checks[0].Mismatch {
		t.Fatal(s.calls, checks, e)
	}
	m := &warmMulti{}
	q = workload.Query{Op: "native_multi_symbols", Key: "x", K: 1}
	_, checks, e = warmedQueries(context.Background(), nil, m, []workload.Query{q}, []workload.Expected{{Answer: workload.Answer{Hits: []adapter.Hit{{ID: "hit", Score: 1}}}}}, adapter.Capabilities{}, 8)
	if e != nil || m.calls != 2 || checks[0].Mismatch {
		t.Fatal(m.calls, checks, e)
	}
}

type openCounter struct {
	oracle.Backend
	calls    int
	lastOpen time.Time
}

func (b *openCounter) Open(ctx context.Context, dir string, ref adapter.SegmentRef) (adapter.Segment, error) {
	b.calls++
	s, e := b.Backend.Open(ctx, dir, ref)
	b.lastOpen = time.Now()
	return s, e
}
func TestReferenceVerificationIsOutsideCommit(t *testing.T) {
	ctx := context.Background()
	b := &openCounter{}
	dir := t.TempDir()
	d := conformance.Fixture()
	old, _, e := b.Build(ctx, d, filepath.Join(dir, "old"))
	if e != nil {
		t.Fatal(e)
	}
	change, e := MakeChange(d)
	if e != nil {
		t.Fatal(e)
	}
	next, _, e := b.BuildDelta(ctx, old, change, filepath.Join(dir, "next"))
	if e != nil {
		t.Fatal(e)
	}
	cat, e := ReferenceCatalog(b)(filepath.Join(dir, "catalog"))
	if e != nil {
		t.Fatal(e)
	}
	defer cat.Close()
	if e = cat.Publish(ctx, d.Repo, old, nil); e != nil {
		t.Fatal(e)
	}
	before := b.calls
	times, e := measurePublication(ctx, b, cat, next, old)
	if e != nil {
		t.Fatal(e)
	}
	if b.calls-before != 1 || !times.Begin.After(b.lastOpen) || !times.End.After(times.Begin) || times.OpenNS <= 0 {
		t.Fatal("verification reopened within commit", b.calls-before, times)
	}
}
func TestCommitRaceChecksEveryWindow(t *testing.T) {
	ctx := context.Background()
	b := oracle.Backend{}
	base := conformance.Fixture()
	dir := t.TempDir()
	old, _, e := b.Build(ctx, base, filepath.Join(dir, "old"))
	if e != nil {
		t.Fatal(e)
	}
	change, e := MakeChange(base)
	if e != nil {
		t.Fatal(e)
	}
	next, _, e := b.BuildDelta(ctx, old, change, filepath.Join(dir, "next"))
	if e != nil {
		t.Fatal(e)
	}
	windows, checks, e := commitRace(ctx, b, ReferenceCatalog(b), base, next, old, filepath.Join(dir, "catalog"), defaultSafetyTimeout)
	if e != nil {
		t.Fatal(e)
	}
	for _, stage := range stages {
		if windows[stage] < MinimumCommitRaceSamples {
			t.Fatal(stage, windows)
		}
	}
	if len(checks) != len(stages)*MinimumCommitRaceSamples {
		t.Fatal("unchecked windows", len(checks))
	}
}

type missingHooksCatalog struct{ adapter.Catalog }

func (c missingHooksCatalog) Publish(ctx context.Context, repo string, next adapter.SegmentRef, old *adapter.SegmentRef) error {
	saved := adapter.Checkpoint
	adapter.Checkpoint = func(string) {}
	defer func() { adapter.Checkpoint = saved }()
	return c.Catalog.Publish(ctx, repo, next, old)
}
func TestMissingCommitRaceSamplesFail(t *testing.T) {
	ctx := context.Background()
	b := oracle.Backend{}
	base := conformance.Fixture()
	dir := t.TempDir()
	old, _, e := b.Build(ctx, base, filepath.Join(dir, "old"))
	if e != nil {
		t.Fatal(e)
	}
	c, e := MakeChange(base)
	if e != nil {
		t.Fatal(e)
	}
	next, _, e := b.BuildDelta(ctx, old, c, filepath.Join(dir, "new"))
	if e != nil {
		t.Fatal(e)
	}
	factory := func(dir string) (adapter.Catalog, error) {
		cat, e := oracle.NewCatalog(dir)
		return missingHooksCatalog{cat}, e
	}
	if _, _, e = commitRace(ctx, b, factory, base, next, old, filepath.Join(dir, "catalog"), defaultSafetyTimeout); e == nil {
		t.Fatal("missing checked windows passed")
	}
}

type staleVersionBackend struct {
	oracle.Backend
	path string
	old  dataset.Dataset
}

func (b staleVersionBackend) Open(ctx context.Context, dir string, ref adapter.SegmentRef) (adapter.Segment, error) {
	if ref.Path == b.path {
		return oracle.New(b.old), nil
	}
	return b.Backend.Open(ctx, dir, ref)
}
func TestCommitRaceRejectsStaleNewVersionAnswers(t *testing.T) {
	ctx := context.Background()
	plain := oracle.Backend{}
	base := conformance.Fixture()
	dir := t.TempDir()
	old, _, e := plain.Build(ctx, base, filepath.Join(dir, "old"))
	if e != nil {
		t.Fatal(e)
	}
	change, e := MakeChange(base)
	if e != nil {
		t.Fatal(e)
	}
	next, _, e := plain.BuildDelta(ctx, old, change, filepath.Join(dir, "new"))
	if e != nil {
		t.Fatal(e)
	}
	b := staleVersionBackend{path: next.Path, old: base}
	windows, _, e := commitRace(ctx, b, ReferenceCatalog(b), base, next, old, filepath.Join(dir, "catalog"), defaultSafetyTimeout)
	if e == nil || windows["after-rename"] >= MinimumCommitRaceSamples {
		t.Fatal("stale new-version content passed", windows, e)
	}
}
