package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"gxstudy/adapter"
	"gxstudy/conformance"
	"gxstudy/dataset"
	"gxstudy/oracle"
	"gxstudy/workload"
	"io"
	"math"
	"net/rpc"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
)

func fixtureProcess(t *testing.T, checker bool) *processClient {
	t.Helper()
	scope, err := newMemoryScope(testCgroupRoot(t), "boundary", 256<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := scope.close(); err != nil {
			t.Error(err)
		}
	})
	client, err := startProcess(context.Background(), scope.path, filepath.Join(t.TempDir(), "component.log"), checker)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := client.close(); err != nil {
			t.Error(err)
		}
	})
	return client
}
func TestCheckerRPCPreservesZeroAndAbsentOverlaps(t *testing.T) {
	checker := fixtureProcess(t, true)
	data := fixtureTier(t)
	sources, _, _, _, err := loadTier(data)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(context.Background(), checkerContextKey{}, checker)
	q := workload.Query{ID: "miss", Op: "symbols", Repo: sources[0].Repo, Key: "graph", K: 2}
	expected := workload.Expected{Sources: sources}
	miss, err := checkedAnswer(ctx, q, expected, workload.Answer{Hits: []adapter.Hit{{ID: "missing", Score: 1}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []*float64{miss.Overlap, miss.LocalOverlap, miss.GlobalOverlap} {
		if value == nil || *value != 0 {
			t.Fatal("zero overlap lost on RPC", miss)
		}
	}
	reference, err := oracle.DiskReference(ctx, sources, q, nil)
	if err != nil {
		t.Fatal(err)
	}
	hit, err := checkedAnswer(ctx, q, expected, reference.Answer, nil)
	if err != nil {
		t.Fatal(err)
	}
	if hit.Overlap == nil || *hit.Overlap != 1 {
		t.Fatal("nonzero overlap lost", hit)
	}
	point := q
	point.ID = "point"
	point.Op = "id"
	point.Key = "a"
	fixture := conformance.Fixture()
	unranked, err := checkedAnswer(ctx, point, expected, workload.Answer{Node: &fixture.Nodes[0]}, nil)
	if err != nil || unranked.Overlap != nil || unranked.LocalOverlap != nil || unranked.GlobalOverlap != nil {
		t.Fatal("absent overlap became present", unranked, err)
	}
	r := Result{Checks: []workload.Check{miss, hit}}
	summarizeOverlap(&r)
	for _, summary := range []OverlapSummary{r.Overlap[q.Op], r.LocalQuality[q.Op], r.GlobalQuality[q.Op]} {
		if summary.Count != 2 || summary.Min != 0 || summary.Mean != .5 || summary.P50 != 0 {
			t.Fatal("summary dropped total miss", summary)
		}
	}
}
func TestEngineRPCPreservesEmptyAndNilPayloads(t *testing.T) {
	client := fixtureProcess(t, false)
	b, _, err := remoteCandidate(context.Background(), client)
	if err != nil {
		t.Fatal(err)
	}
	d := conformance.Fixture()
	d.Nodes[0].Payload = map[string]json.RawMessage{}
	d.Nodes[1].Payload = nil
	ref, _, err := b.Build(context.Background(), d, filepath.Join(t.TempDir(), "segment"))
	if err != nil {
		t.Fatal(err)
	}
	segment, err := b.Open(context.Background(), ref.Path, ref)
	if err != nil {
		t.Fatal(err)
	}
	defer segment.Close()
	for _, original := range d.Nodes[:2] {
		got, err := segment.NodeByID(context.Background(), original.ID)
		if err != nil || !reflect.DeepEqual(got.Payload, original.Payload) {
			t.Fatal("payload shape changed on RPC", original, got, err)
		}
	}
}
func TestTimedPublishDoesNotForwardUnarmedCheckpoints(t *testing.T) {
	client := fixtureProcess(t, false)
	b, factory, err := remoteCandidate(context.Background(), client)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	d := conformance.Fixture()
	old, _, err := b.Build(ctx, d, filepath.Join(t.TempDir(), "old"))
	if err != nil {
		t.Fatal(err)
	}
	change, err := MakeChange(d)
	if err != nil {
		t.Fatal(err)
	}
	next, _, err := b.BuildDelta(ctx, old, change, filepath.Join(t.TempDir(), "next"))
	if err != nil {
		t.Fatal(err)
	}
	cat, err := factory(filepath.Join(t.TempDir(), "catalog"))
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	previous := adapter.Checkpoint
	defer func() { adapter.Checkpoint = previous }()
	callbacks := 0
	adapter.Checkpoint = func(string) { callbacks++ }
	if err := cat.Publish(ctx, old.Repo, old, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := measurePublication(ctx, b, cat, next, old); err != nil {
		t.Fatal(err)
	}
	if callbacks != 0 {
		t.Fatal("timed publication forwarded callbacks", callbacks)
	}
	windows, _, err := commitRace(ctx, b, factory, d, next, old, filepath.Join(t.TempDir(), "race"), defaultSafetyTimeout)
	if err != nil {
		t.Fatal(err)
	}
	for _, stage := range stages {
		if windows[stage] < MinimumCommitRaceSamples {
			t.Fatal("armed callback was lost", stage, windows)
		}
	}
}
func TestDiskCheckerMultiReferenceDecisionsMatchMemory(t *testing.T) {
	checker := fixtureProcess(t, true)
	ctx := context.WithValue(context.Background(), checkerContextKey{}, checker)
	var ds, sources []dataset.Dataset
	segments := map[string]adapter.Segment{}
	for _, repo := range []string{"one", "two"} {
		d := dataset.Dataset{Repo: repo}
		for i := 0; i < 10; i++ {
			id := fmt.Sprintf("%s%d", repo, i)
			text := "graph other words text node"
			if repo == "one" && i == 0 {
				text = "graph graph graph graph graph"
			}
			if repo == "two" {
				text = "other words text node tokens"
				if i == 0 {
					text = "graph"
				}
			}
			d.Nodes = append(d.Nodes, dataset.Node{ID: id, Name: "Alpha", FilePath: "a.go"})
			d.Symbols = append(d.Symbols, dataset.Symbol{NodeID: id, Tokens: text})
			d.Vectors = append(d.Vectors, dataset.MakeVector(id, 7))
			if i < 3 {
				d.Edges = append(d.Edges, dataset.Edge{From: id, To: "one0", Kind: "calls"})
			}
		}
		d.Vectors[1].Values = slices.Clone(d.Vectors[0].Values)
		d.Edges = append(d.Edges, d.Edges[0])
		ds = append(ds, d)
		segments[repo] = oracle.New(d)
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
	global := oracle.NewMulti(ds, segments)
	for _, op := range []string{"multi_name", "multi_in", "multi_symbols", "multi_vector", "multi_traverse2", "multi_traverse3"} {
		base := workload.Query{ID: op, Op: op, Key: "graph", K: 2, Depth: 2, Vec: ds[0].Vectors[0].Values, Kind: "calls"}
		if op == "multi_symbols" {
			base.K = 1
		}
		if op == "multi_name" {
			base.Key = "Alpha"
		}
		if op == "multi_in" || strings.Contains(op, "traverse") {
			base.Key = "one0"
		}
		if strings.HasSuffix(op, "3") {
			base.Depth = 3
		}
		answer, err := workload.Evaluate(ctx, segments, global, base)
		if err != nil {
			t.Fatal(err)
		}
		references, err := oracle.ReferenceAnswers(ds, []workload.Query{base}, []workload.Expected{{QueryID: base.ID, Answer: answer}})
		if err != nil {
			t.Fatal(err)
		}
		if op == "multi_symbols" && (references[0].Global == nil || references[0].Answer.Hits[0].ID == references[0].Global.Hits[0].ID) {
			t.Fatal("fixture did not distinguish local/global scoring")
		}
		for _, native := range []bool{false, true} {
			for _, nativeScoring := range []bool{false, true} {
				if nativeScoring && !workload.Lexical(base.Op) {
					continue
				}
				q, w := base, references[0]
				if native {
					q.Op = "native_" + q.Op
					w = globalExpected(w)
				}
				q.NativeScoring = nativeScoring
				var inputs []adapter.SearchInput
				if nativeScoring && !native {
					for _, d := range ds {
						hits, err := segments[d.Repo].SearchSymbols(ctx, q.Key, q.K)
						if err != nil {
							t.Fatal(err)
						}
						inputs = append(inputs, adapter.SearchInput{Repo: d.Repo, Hits: hits})
					}
				}
				variants := []workload.Answer{w.Answer, {}, {Hits: []adapter.Hit{{ID: "outside", Score: 1}}}}
				if len(w.Answer.Hits) > 0 {
					duplicate := w.Answer
					duplicate.Hits = []adapter.Hit{w.Answer.Hits[0], w.Answer.Hits[0]}
					reverse := w.Answer
					reverse.Hits = slices.Clone(reverse.Hits)
					slices.Reverse(reverse.Hits)
					variants = append(variants, duplicate, reverse)
				}
				for _, got := range variants {
					want := compareRankedAnswer(q, w, got, inputs)
					have, err := checkedAnswer(ctx, q, workload.Expected{Sources: sources}, got, inputs)
					if err != nil {
						t.Fatal(q.Op, err)
					}
					a, _ := json.Marshal(want)
					b, _ := json.Marshal(have)
					if string(a) != string(b) {
						t.Fatalf("%s native=%v native-score=%v checker decision differs: %s != %s", op, native, nativeScoring, b, a)
					}
				}
			}
		}
	}
	q := workload.Query{ID: "tie", Op: "multi_vector", Vec: ds[0].Vectors[0].Values, K: 1, ExactRank: true}
	answer, err := workload.Evaluate(ctx, segments, global, q)
	if err != nil {
		t.Fatal(err)
	}
	refs, err := oracle.ReferenceAnswers(ds, []workload.Query{q}, []workload.Expected{{QueryID: q.ID, Answer: answer}})
	if err != nil {
		t.Fatal(err)
	}
	got := workload.Answer{Hits: []adapter.Hit{{ID: "one1", Score: answer.Hits[0].Score}}}
	want := compareRankedAnswer(q, refs[0], got, nil)
	have, err := checkedAnswer(ctx, q, workload.Expected{Sources: sources}, got, nil)
	if err != nil || want.Mismatch || !want.CutoffTie || !reflect.DeepEqual(have, want) {
		t.Fatal("disk checker lost an accepted cutoff tie", want, have, err)
	}

}
func TestRunnerRSSExcludesReapedLargerChild(t *testing.T) {
	scope, err := newMemoryScope(testCgroupRoot(t), "self-rss", 256<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := scope.close(); err != nil {
			t.Error(err)
		}
	}()
	req := runRequest{Out: t.TempDir(), Run: 1, RunnerScope: scope.path, Binding: RunBinding{Limits: MemoryLimits{Runner: 256 << 20}}}
	request := filepath.Join(req.Out, "request.json")
	if err := dataset.WriteJSON(request, req); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), defaultSafetyTimeout)
	defer cancel()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd, err := runLimitedCommand(ctx, exe, "__rss_parent", request, scope.path, filepath.Join(req.Out, "runner.log"))
	if err != nil {
		t.Fatal(err)
	}
	memory, err := runnerMemory(req, cmd, nil)
	if err != nil {
		t.Fatal(err)
	}
	var own ComponentMemory
	if err := dataset.ReadJSON(filepath.Join(req.Out, "runner-01.memory.json"), &own); err != nil {
		t.Fatal(err)
	}
	wait := cmd.ProcessState.SysUsage().(*syscall.Rusage).Maxrss * 1024
	if wait <= own.PeakRSSBytes || memory.PeakRSSBytes != own.PeakRSSBytes || memory.RSSSource != "RUSAGE_SELF" {
		t.Fatal("runner inherited child peak", memory, own, wait)
	}
	if err := os.Remove(filepath.Join(req.Out, "runner-01.memory.json")); err != nil {
		t.Fatal(err)
	}
	missing, err := runnerMemory(req, cmd, nil)
	if err == nil || missing.PeakRSSBytes != 0 || missing.Status != "failed" {
		t.Fatal("missing self record relabelled wait RSS", missing, err)
	}
}

type nonfiniteSegment struct{ adapter.Segment }

func (nonfiniteSegment) SearchSymbols(context.Context, string, int) ([]adapter.Hit, error) {
	return []adapter.Hit{{ID: "bad", Score: math.NaN()}}, nil
}
func TestInvalidNativeReplyBecomesAnEncodableError(t *testing.T) {
	service := ProcessService{backend: oracle.Backend{}, segments: map[int]adapter.Segment{1: nonfiniteSegment{}}}
	var reply ProcessReply
	err := service.Call(ProcessRequest{Op: "query", Handles: map[string]int{"r": 1}, Query: workload.Query{Op: "symbols", Repo: "r", Key: "graph", K: 1}}, &reply)
	if err != nil || reply.Error == "" {
		t.Fatal("invalid native reply did not fail visibly", reply, err)
	}
	if _, err := json.Marshal(reply); err != nil {
		t.Fatal("error reply cannot cross the RPC", err)
	}
}

type gatedReplyCodec struct {
	requests                          chan rpc.Request
	ready, release, inputDone, closed chan struct{}
	once                              sync.Once
}

func (c *gatedReplyCodec) WriteRequest(req *rpc.Request, _ any) error {
	select {
	case c.requests <- *req:
		return nil
	case <-c.closed:
		return io.EOF
	}
}
func (c *gatedReplyCodec) ReadResponseHeader(reply *rpc.Response) error {
	select {
	case req := <-c.requests:
		reply.Seq = req.Seq
		reply.ServiceMethod = req.ServiceMethod
		return nil
	case <-c.closed:
		close(c.inputDone)
		return io.EOF
	}
}
func (c *gatedReplyCodec) ReadResponseBody(value any) error {
	*value.(*ProcessReply) = ProcessReply{Name: "partially decoded"}
	close(c.ready)
	<-c.release
	return nil
}
func (c *gatedReplyCodec) Close() error { c.once.Do(func() { close(c.closed) }); return nil }
func TestCancellationDoesNotExposeReplyBeingDecoded(t *testing.T) {
	codec := &gatedReplyCodec{requests: make(chan rpc.Request, 1), ready: make(chan struct{}), release: make(chan struct{}), inputDone: make(chan struct{}), closed: make(chan struct{})}
	client := &processClient{rpc: rpc.NewClientWithCodec(codec)}
	defer client.rpc.Close()
	released := false
	defer func() {
		if !released {
			close(codec.release)
		}
		client.rpc.Close()
		<-codec.inputDone
	}()
	ctx, cancel := context.WithTimeout(context.Background(), defaultSafetyTimeout)
	defer cancel()
	type result struct {
		reply ProcessReply
		err   error
	}
	completed := make(chan result, 1)
	go func() {
		reply, err := client.call(ctx, ProcessRequest{Op: "describe"})
		completed <- result{reply, err}
	}()
	select {
	case <-codec.ready:
	case <-ctx.Done():
		t.Fatal("decoder did not reach the cancellation boundary", ctx.Err())
	}
	cancel()
	got := <-completed
	if !errors.Is(got.err, context.Canceled) || !reflect.DeepEqual(got.reply, ProcessReply{}) {
		t.Fatal("cancel returned a concurrently owned reply", got)
	}
	close(codec.release)
	released = true
}
