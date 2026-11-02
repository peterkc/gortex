package runner

import (
	"context"
	"encoding/json"
	"errors"
	"gxstudy/adapter"
	"gxstudy/conformance"
	"gxstudy/dataset"
	"gxstudy/workload"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func fixtureReaderMemo(t *testing.T) *readerCheckMemo {
	t.Helper()
	memo, err := newReaderCheckMemo(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := memo.close(); err != nil {
			t.Error(err)
		}
	})
	return memo
}

func TestReaderMemoChecksIdenticalSpoolOnceAndRejectsChangedAnswer(t *testing.T) {
	sources, _, _, _, err := loadTier(fixtureTier(t))
	if err != nil {
		t.Fatal(err)
	}
	checker := &phaseChecker{delegate: &ProcessService{checker: true}}
	ctx := context.WithValue(context.Background(), checkerContextKey{}, fixtureRPCClient(t, checker))
	memo := fixtureReaderMemo(t)
	node := conformance.Fixture().Nodes[0]
	pending := pendingCheck{Sources: sources, Query: workload.Query{ID: "reader", Op: "id", Repo: sources[0].Repo, Key: node.ID}, Answer: workload.Answer{Node: &node}}
	path := filepath.Join(t.TempDir(), "samples.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	encoder := json.NewEncoder(f)
	for i := 0; i < 64; i++ {
		if err := encoder.Encode(storedSample{Pending: &pending, NS: int64(i)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	count := 0
	var first workload.Check
	for sample, err := range (sampleStream{files: []string{path}}).rows() {
		if err != nil {
			t.Fatal(err)
		}
		checked, err := memo.checkReaderSample(ctx, sample)
		if err != nil || checked.pending != nil || checked.check.Mismatch {
			t.Fatal(checked, err)
		}
		if count == 0 {
			first = checked.check
		}
		if !reflect.DeepEqual(first, checked.check) {
			t.Fatal("cached result changed", first, checked.check)
		}
		count++
	}
	if count != 64 || checker.calls.Load() != 1 {
		t.Fatal("identical rows rescanned", count, checker.calls.Load())
	}
	bad := pending
	wrongNode := node
	wrongNode.Name += "-wrong"
	bad.Answer.Node = &wrongNode
	prior := errors.New("recorded sample error")
	for i := 0; i < 2; i++ {
		failed, err := memo.checkReaderSample(ctx, readerSample{pending: &bad, err: prior})
		if !errors.Is(err, prior) || !failed.check.Mismatch || failed.pending != nil {
			t.Fatal("changed answer accepted or prior error lost", failed, err)
		}
	}
	if checker.calls.Load() != 2 {
		t.Fatal("changed answer was not checked once", checker.calls.Load())
	}
	fresh := fixtureReaderMemo(t)
	if _, err := fresh.check(ctx, pending); err != nil || checker.calls.Load() != 3 {
		t.Fatal("memo leaked across runs", checker.calls.Load(), err)
	}
}

func TestReaderMemoKeyCoversCompleteObservation(t *testing.T) {
	base := pendingCheck{
		Sources:       []dataset.Dataset{{Repo: "r", Meta: dataset.Manifest{Version: "v1", SHA256: map[string]string{"nodes.jsonl": "one"}}, Source: &dataset.DiskSource{Dir: "base", OmitFile: "old.go", RemovedIDs: []string{"old"}, Replacement: &dataset.Dataset{Repo: "r", Nodes: []dataset.Node{{ID: "replacement"}}}}}},
		NativeScoring: true, MatchRepos: map[string]string{"a": "r", "b": "s"},
		Query:    workload.Query{ID: "q", Op: "symbols", Repo: "r", Key: "name", Kind: "calls", K: 2, Depth: 1, Vec: []float32{1}, MinOverlap: 0.5},
		Expected: workload.Expected{QueryID: "q", Answer: workload.Answer{IDs: []string{"a"}}, Local: &workload.Answer{IDs: []string{"a"}}, Global: &workload.Answer{IDs: []string{"a"}}, MatchRepos: map[string]string{"a": "r"}, Ties: []adapter.Hit{{ID: "a", Score: 1}}, GlobalTies: []adapter.Hit{{ID: "a", Score: 1}}},
		Answer:   workload.Answer{Node: &dataset.Node{ID: "a", Payload: map[string]json.RawMessage{}}, IDs: []string{"a", "b"}, Hits: []adapter.Hit{{ID: "a", Score: 1}, {ID: "b", Score: 0.5}}},
		Inputs:   []adapter.SearchInput{{Repo: "r", Hits: []adapter.Hit{{ID: "a", Score: 1}, {ID: "b", Score: 0.5}}}, {Repo: "s", Hits: []adapter.Hit{{ID: "c", Score: 0.25}}}},
	}
	for _, tc := range []struct {
		name   string
		change func(*pendingCheck)
	}{
		{"query-id", func(p *pendingCheck) { p.Query.ID = "other" }},
		{"query-operation", func(p *pendingCheck) { p.Query.Op = "content" }},
		{"query-repo", func(p *pendingCheck) { p.Query.Repo = "s" }},
		{"query-key", func(p *pendingCheck) { p.Query.Key = "other" }},
		{"query-kind", func(p *pendingCheck) { p.Query.Kind = "uses" }},
		{"query-k", func(p *pendingCheck) { p.Query.K++ }},
		{"query-depth", func(p *pendingCheck) { p.Query.Depth++ }},
		{"query-vector", func(p *pendingCheck) { p.Query.Vec[0] = 0.5 }},
		{"overlap-policy", func(p *pendingCheck) { p.Query.MinOverlap = 1 }},
		{"exact-policy", func(p *pendingCheck) { p.Query.ExactRank = true }},
		{"required-id", func(p *pendingCheck) { p.Query.RequiredIDs = []string{"a"} }},
		{"forbidden-id", func(p *pendingCheck) { p.Query.ForbiddenIDs = []string{"b"} }},
		{"cutoff-policy", func(p *pendingCheck) { p.Query.CutoffTies = []adapter.Hit{{ID: "b", Score: 1}} }},
		{"native-policy", func(p *pendingCheck) { p.NativeScoring = false }},
		{"membership-policy", func(p *pendingCheck) { p.MatchRepos["a"] = "s" }},
		{"source-repo", func(p *pendingCheck) { p.Sources[0].Repo = "s" }},
		{"source-version", func(p *pendingCheck) { p.Sources[0].Meta.Version = "v2" }},
		{"source-hash", func(p *pendingCheck) { p.Sources[0].Meta.SHA256["nodes.jsonl"] = "two" }},
		{"source-directory", func(p *pendingCheck) { p.Sources[0].Source.Dir = "alias/base" }},
		{"source-omitted-file", func(p *pendingCheck) { p.Sources[0].Source.OmitFile = "other.go" }},
		{"source-removed-id", func(p *pendingCheck) { p.Sources[0].Source.RemovedIDs[0] = "other" }},
		{"source-replacement", func(p *pendingCheck) { p.Sources[0].Source.Replacement.Nodes[0].Name = "new" }},
		{"nested-source-replacement", func(p *pendingCheck) { p.Sources[0].Source.Replacement.Source = &dataset.DiskSource{Dir: "nested"} }},
		{"answer-node", func(p *pendingCheck) { p.Answer.Node.Name = "wrong" }},
		{"answer-payload", func(p *pendingCheck) { p.Answer.Node.Payload["x"] = json.RawMessage(`1`) }},
		{"nil-payload", func(p *pendingCheck) { p.Answer.Node.Payload = nil }},
		{"answer-missing", func(p *pendingCheck) { p.Answer.Missing = true }},
		{"answer-order", func(p *pendingCheck) { p.Answer.IDs[0], p.Answer.IDs[1] = p.Answer.IDs[1], p.Answer.IDs[0] }},
		{"answer-multiplicity", func(p *pendingCheck) { p.Answer.IDs = append(p.Answer.IDs, "a") }},
		{"hit-order", func(p *pendingCheck) { p.Answer.Hits[0], p.Answer.Hits[1] = p.Answer.Hits[1], p.Answer.Hits[0] }},
		{"hit-score", func(p *pendingCheck) { p.Answer.Hits[0].Score = 2 }},
		{"input-repo", func(p *pendingCheck) { p.Inputs[0].Repo = "s" }},
		{"input-hit", func(p *pendingCheck) { p.Inputs[0].Hits[0].ID = "other" }},
		{"input-score", func(p *pendingCheck) { p.Inputs[0].Hits[0].Score = 2 }},
		{"input-hit-order", func(p *pendingCheck) {
			p.Inputs[0].Hits[0], p.Inputs[0].Hits[1] = p.Inputs[0].Hits[1], p.Inputs[0].Hits[0]
		}},
		{"input-order", func(p *pendingCheck) { p.Inputs[0], p.Inputs[1] = p.Inputs[1], p.Inputs[0] }},
		{"expected-query", func(p *pendingCheck) { p.Expected.QueryID = "other" }},
		{"expected-answer", func(p *pendingCheck) { p.Expected.Answer.IDs[0] = "b" }},
		{"expected-local", func(p *pendingCheck) { p.Expected.Local.IDs[0] = "b" }},
		{"expected-global", func(p *pendingCheck) { p.Expected.Global.IDs[0] = "b" }},
		{"expected-membership", func(p *pendingCheck) { p.Expected.MatchRepos["a"] = "s" }},
		{"expected-ties", func(p *pendingCheck) { p.Expected.Ties[0].Score = 2 }},
		{"expected-global-ties", func(p *pendingCheck) { p.Expected.GlobalTies[0].Score = 2 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checker := &phaseChecker{}
			ctx := context.WithValue(context.Background(), checkerContextKey{}, fixtureRPCClient(t, checker))
			memo := fixtureReaderMemo(t)
			if _, err := memo.check(ctx, base); err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal(base)
			if err != nil {
				t.Fatal(err)
			}
			var changed pendingCheck
			if err := json.Unmarshal(encoded, &changed); err != nil {
				t.Fatal(err)
			}
			tc.change(&changed)
			if _, err := memo.check(ctx, changed); err != nil {
				t.Fatal(err)
			}
			// Map iteration/insertion order must not prevent reuse.
			same := base
			same.MatchRepos = map[string]string{"b": "s", "a": "r"}
			if _, err := memo.check(ctx, same); err != nil {
				t.Fatal(err)
			}
			if checker.calls.Load() != 2 {
				t.Fatal("different observation reused a check", checker.calls.Load())
			}
		})
	}
}

func TestReaderMemoErrorsRemainVisible(t *testing.T) {
	memo := fixtureReaderMemo(t)
	zero := 0.0
	ready := pendingCheck{Ready: &workload.Check{QueryID: "zero", Status: "passed", Overlap: &zero}}
	first, err := memo.check(context.Background(), ready)
	if err != nil {
		t.Fatal(err)
	}
	again, err := memo.check(context.Background(), ready)
	if err != nil || !reflect.DeepEqual(first, again) || again.Overlap == nil || *again.Overlap != 0 {
		t.Fatal("cached zero/absent fields changed", first, again, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := memo.check(ctx, ready); !errors.Is(err, context.Canceled) {
		t.Fatal("cache masked cancellation", err)
	}
	entries, err := os.ReadDir(memo.dir)
	if err != nil || len(entries) != 1 {
		t.Fatal(entries, err)
	}
	if err := os.WriteFile(filepath.Join(memo.dir, entries[0].Name()), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := memo.check(context.Background(), ready); err == nil {
		t.Fatal("corrupt cached result was accepted")
	}
	if err := memo.close(); err != nil {
		t.Fatal(err)
	}
	if _, err := memo.check(context.Background(), ready); err == nil {
		t.Fatal("memo write failure was accepted")
	}

	checker := &phaseChecker{delegate: &ProcessService{checker: true}}
	ctx = context.WithValue(context.Background(), checkerContextKey{}, fixtureRPCClient(t, checker))
	missing := pendingCheck{Sources: []dataset.Dataset{{Repo: "r", Source: &dataset.DiskSource{Dir: filepath.Join(t.TempDir(), "missing")}}}, Query: workload.Query{ID: "missing", Op: "id", Repo: "r", Key: "a"}}
	memo = fixtureReaderMemo(t)
	for i := 0; i < 2; i++ {
		if _, err := memo.check(ctx, missing); err == nil {
			t.Fatal("checker error was accepted")
		}
	}
	if checker.calls.Load() != 2 {
		t.Fatal("checker error was cached", checker.calls.Load())
	}
	entries, err = os.ReadDir(memo.dir)
	if err != nil || len(entries) != 0 {
		t.Fatal("checker error left a success entry", entries, err)
	}
}

func TestReaderMemoIncludesInMemoryReference(t *testing.T) {
	memo := fixtureReaderMemo(t)
	q := workload.Query{ID: "point", Op: "id", Key: "a"}
	node := dataset.Node{ID: "a"}
	p := pendingCheck{Query: q, Expected: workload.Expected{Answer: workload.Answer{Node: &node}}, Answer: workload.Answer{Node: &node}}
	if check, err := memo.check(context.Background(), p); err != nil || check.Mismatch {
		t.Fatal(check, err)
	}
	wrong := dataset.Node{ID: "b"}
	p.Expected.Answer.Node = &wrong
	if check, err := memo.check(context.Background(), p); err != nil || !check.Mismatch {
		t.Fatal("reference change reused a pass", check, err)
	}
}
