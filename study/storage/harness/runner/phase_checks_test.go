package runner

import (
	"context"
	"gxstudy/adapter"
	"gxstudy/conformance"
	"gxstudy/dataset"
	"gxstudy/oracle"
	"gxstudy/workload"
	"net"
	"net/rpc"
	"net/rpc/jsonrpc"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func fixtureRPCClient(t *testing.T, service any) *processClient {
	t.Helper()
	parent, child := net.Pipe()
	server := rpc.NewServer()
	if err := server.RegisterName("Process", service); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); server.ServeCodec(jsonrpc.NewServerCodec(child)) }()
	client := &processClient{rpc: jsonrpc.NewClient(parent)}
	t.Cleanup(func() { client.rpc.Close(); <-done })
	return client
}

type phaseChecker struct {
	operations *atomic.Int64
	minimum    int64
	active     *atomic.Bool
	calls      atomic.Int64
	wrong      bool
	delegate   *ProcessService
}

func (c *phaseChecker) Call(req ProcessRequest, out *ProcessReply) error {
	c.calls.Add(1)
	if c.active != nil && c.active.Load() {
		out.Error = "checker ran during sampling"
		return nil
	}
	if c.operations != nil && c.operations.Load() < c.minimum {
		out.Error = "checker ran between sampled queries"
		return nil
	}
	if c.delegate != nil {
		return c.delegate.Call(req, out)
	}
	out.Check = workload.Check{QueryID: req.Query.ID, Operation: req.Query.Op, Status: "ok", NativeScoring: req.NativeScoring}
	if c.wrong {
		out.Check.Mismatch = true
		out.Check.Status = "failed"
		out.Check.Detail = "wrong sampled answer"
	}
	return nil
}

type phaseSegment struct {
	adapter.Segment
	operations *atomic.Int64
	observed   chan struct{}
	node       dataset.Node
}

func (s *phaseSegment) NodeByID(context.Context, string) (dataset.Node, error) {
	s.operations.Add(1)
	if s.observed != nil {
		select {
		case s.observed <- struct{}{}:
		default:
		}
	}
	return s.node, nil
}
func TestQueryChecksRunAfterWarmAndMeasuredOperations(t *testing.T) {
	for _, warm := range []bool{false, true} {
		t.Run(map[bool]string{false: "cold", true: "warm"}[warm], func(t *testing.T) {
			for _, wrong := range []bool{false, true} {
				var operations atomic.Int64
				minimum := int64(2)
				if warm {
					minimum = 4
				}
				checker := &phaseChecker{operations: &operations, minimum: minimum, wrong: wrong}
				client := fixtureRPCClient(t, checker)
				ctx := context.WithValue(context.Background(), checkerContextKey{}, client)
				node := dataset.Node{ID: "a"}
				segments := map[string]adapter.Segment{"r": &phaseSegment{operations: &operations, node: node}}
				qs := []workload.Query{{ID: "one", Op: "id", Repo: "r", Key: "a"}, {ID: "two", Op: "id", Repo: "r", Key: "a"}}
				want := []workload.Expected{{Sources: []dataset.Dataset{{Repo: "r"}}}, {Sources: []dataset.Dataset{{Repo: "r"}}}}
				var samples map[string][]int64
				var checks []workload.Check
				var err error
				if warm {
					samples, checks, err = warmedQueries(ctx, segments, nil, qs, want, adapter.Capabilities{}, 7)
				} else {
					samples, checks, err = measuredQueries(ctx, segments, nil, qs, want, adapter.Capabilities{})
				}
				if err != nil || operations.Load() != minimum || checker.calls.Load() != minimum || len(samples["id"]) != 2 {
					t.Fatal("checking disturbed query phase", operations.Load(), checker.calls.Load(), samples, err)
				}
				if len(checks) < 2 || checks[0].Mismatch != wrong || checks[1].Mismatch != wrong {
					t.Fatal("deferred mismatch was lost", checks)
				}
				if checks[0].QueryID != qs[0].ID && !warm {
					t.Fatal("query order changed", checks)
				}
				if warm && wrong && len(checks) != 4 {
					t.Fatal("warm-up mismatch was dropped", checks)
				}
			}
		})
	}
}
func TestDiskReadersCheckOnlyAfterStoppingAndRejectBadSamples(t *testing.T) {
	var active atomic.Bool
	active.Store(true)
	checker := &phaseChecker{active: &active}
	client := fixtureRPCClient(t, checker)
	ctx := context.WithValue(context.Background(), checkerContextKey{}, client)
	ctx = context.WithValue(ctx, sampleDirectoryKey{}, t.TempDir())
	d := conformance.Fixture()
	b := oracle.Backend{}
	ref, _, err := b.Build(ctx, d, filepath.Join(t.TempDir(), "base"))
	if err != nil {
		t.Fatal(err)
	}
	cat, err := oracle.NewCatalog(filepath.Join(t.TempDir(), "catalog"))
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	if err := cat.Publish(ctx, d.Repo, ref, nil); err != nil {
		t.Fatal(err)
	}
	var operations atomic.Int64
	observed := make(chan struct{}, 1)
	segment := &phaseSegment{operations: &operations, observed: observed, node: d.Nodes[0]}
	segments := map[string]adapter.Segment{d.Repo: segment}
	cache := &readCache{segments: map[adapter.SegmentRef]adapter.Segment{ref: segment}, b: b}
	qs := []workload.Query{{ID: "reader", Op: "id", Repo: d.Repo, Key: d.Nodes[0].ID}}
	want := []workload.Expected{{Sources: []dataset.Dataset{{Repo: d.Repo}}}}
	control := startReaders(ctx, b, cat, cache, segments, ref, qs, want, want)
	<-observed
	stream, err := finishReaderStream(control)
	active.Store(false)
	if err != nil || checker.calls.Load() != 0 {
		t.Fatal("checker ran in reader phase", checker.calls.Load(), err)
	}
	memo := fixtureReaderMemo(t)
	count := 0
	for sample, err := range stream.rows() {
		if err != nil {
			t.Fatal(err)
		}
		if sample.pending == nil {
			t.Fatal("sample lost its unverified answer")
		}
		checked, err := memo.checkReaderSample(ctx, sample)
		if err != nil || checked.pending != nil {
			t.Fatal("post-phase check failed", checked, err)
		}
		bad := *sample.pending
		node := *bad.Answer.Node
		node.Name += "-wrong"
		bad.Answer.Node = &node
		sample.pending = &bad
		checker.wrong = true
		failed, err := memo.checkReaderSample(ctx, sample)
		if err == nil || !failed.check.Mismatch {
			t.Fatal("bad recorded answer was accepted", failed, err)
		}
		checker.wrong = false
		count++
	}
	if count == 0 || checker.calls.Load() != 2 {
		t.Fatal("identical spooled answers were rechecked", count, checker.calls.Load())
	}
}

type nativeDurationService struct{ node dataset.Node }

func (s nativeDurationService) Call(req ProcessRequest, out *ProcessReply) error {
	out.Answer = workload.Answer{Node: &s.node}
	out.Start = time.Unix(100, 0)
	out.End = time.Unix(200, 0)
	out.Elapsed = 7
	out.Publication = publicationTimes{Begin: out.Start, End: out.End, ElapsedNS: 11}
	return nil
}
func TestNativeDurationsDoNotUseDecodedWallClockSpans(t *testing.T) {
	node := dataset.Node{ID: "a"}
	client := fixtureRPCClient(t, nativeDurationService{node: node})
	segments := map[string]adapter.Segment{"r": &remoteSegment{client: client, id: 1}}
	qs := []workload.Query{{ID: "point", Op: "id", Repo: "r", Key: "a"}}
	samples, checks, err := measuredQueries(context.Background(), segments, nil, qs, []workload.Expected{{Answer: workload.Answer{Node: &node}}}, adapter.Capabilities{})
	if err != nil || len(checks) != 1 || checks[0].Mismatch || len(samples["id"]) != 1 || samples["id"][0] != 7 {
		t.Fatal("query used a decoded wall-clock span", samples, checks, err)
	}
	next := adapter.SegmentRef{Repo: "r"}
	times, err := measurePublication(context.Background(), nil, &remoteCatalog{client: client, id: 1}, next, next)
	if err != nil || times.ElapsedNS != 11 {
		t.Fatal("publication lost native duration", times, err)
	}
}

func (s *phaseSegment) SearchSymbols(context.Context, string, int) ([]adapter.Hit, error) {
	s.operations.Add(1)
	return []adapter.Hit{{ID: "a", Score: 1}}, nil
}
func TestSpooledNativePolicySurvivesRuntimeOnlyJSONFields(t *testing.T) {
	var operations atomic.Int64
	checker := &phaseChecker{operations: &operations, minimum: 1}
	client := fixtureRPCClient(t, checker)
	ctx := context.WithValue(context.Background(), checkerContextKey{}, client)
	segments := map[string]adapter.Segment{"r": &phaseSegment{operations: &operations}}
	q := workload.Query{ID: "native-score", Op: "symbols", Repo: "r", Key: "graph", K: 1}
	_, checks, err := measuredQueries(ctx, segments, nil, []workload.Query{q}, []workload.Expected{{Sources: []dataset.Dataset{{Repo: "r"}}}}, adapter.Capabilities{Ranking: map[string]adapter.RankPolicy{"symbols": {Mode: "native"}}})
	if err != nil || len(checks) != 1 || !checks[0].NativeScoring {
		t.Fatal("runtime scoring policy was dropped by the spool or RPC", checks, err)
	}
}
