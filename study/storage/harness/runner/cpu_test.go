package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"gxstudy/adapter"
	"gxstudy/conformance"
	"gxstudy/dataset"
	"gxstudy/oracle"
	"gxstudy/workload"
	"os"
	"path/filepath"
	"testing"
)

func TestBuildCPUFieldAndMissingRejection(t *testing.T) {
	var record BuildRecord
	if err := json.Unmarshal([]byte(`{"artifact_bytes":1}`), &record); err != nil {
		t.Fatal(err)
	}
	if checkBuildCPU(record.CPU, record.ArtifactBytes) == nil {
		t.Fatal("missing CPU accepted")
	}
	for _, ns := range []int64{0, -1} {
		if checkBuildCPU(ns, 1) == nil {
			t.Fatal("invalid CPU accepted", ns)
		}
	}
	record.CPU = 123
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if string(fields["cpu_ns"]) != "123" {
		t.Fatal("CPU not serialized", string(raw))
	}
	if err := checkBuildCPU(record.CPU, 1); err != nil {
		t.Fatal(err)
	}
}

func TestQueryLatencySeparatesHitsAndMissesAndRejectsMissing(t *testing.T) {
	for _, missing := range []bool{false, true} {
		p := pendingCheck{NS: 123, Query: workload.Query{ID: "lookup", Op: "id"}, Answer: workload.Answer{Missing: missing}}
		sample, err := latencyFromPending(p, workload.Check{Status: "passed"})
		if err != nil || sample.Missing != missing || sample.NS != 123 {
			t.Fatal("latency not recoverable", sample, err)
		}
	}
	var missing pendingCheck
	if err := json.Unmarshal([]byte(`{"Query":{"id":"missing","op":"id"}}`), &missing); err != nil {
		t.Fatal(err)
	}
	if _, err := latencyFromPending(missing, workload.Check{Status: "passed"}); err == nil {
		t.Fatal("missing elapsed_ns accepted")
	}
}

func TestQueryLatencySpoolPreservesActualHitMissSamples(t *testing.T) {
	d := conformance.Fixture()
	segment := oracle.New(d)
	queries := []workload.Query{{ID: "hit", Op: "id", Repo: d.Repo, Key: "a"}, {ID: "miss", Op: "id", Repo: d.Repo, Key: "missing"}}
	var expected []workload.Expected
	for _, q := range queries {
		answer, err := workload.Evaluate(context.Background(), map[string]adapter.Segment{d.Repo: segment}, nil, q)
		if err != nil {
			t.Fatal(err)
		}
		expected = append(expected, workload.Expected{QueryID: q.ID, Answer: answer})
	}
	dir := t.TempDir()
	result := &Result{}
	ctx := context.WithValue(context.Background(), queryLatencyKey{}, queryLatencyOutput{dir: dir, result: result})
	samples, checks, err := measuredQueries(ctx, map[string]adapter.Segment{d.Repo: segment}, nil, queries, expected, oracle.Backend{}.Capabilities())
	if err != nil || len(samples["id"]) != 2 || len(checks) != 2 || len(result.QueryLatencies) != 1 {
		t.Fatal("query timing was not preserved", samples, checks, result.QueryLatencies, err)
	}
	for name, pin := range result.QueryLatencies {
		path := filepath.Join(dir, name)
		actual, err := dataset.Hash(path)
		if err != nil || actual != pin {
			t.Fatal("latency spool is not hash-bound", err)
		}
		file, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		decoder := json.NewDecoder(file)
		for _, missing := range []bool{false, true} {
			var sample queryLatency
			if err := decoder.Decode(&sample); err != nil {
				t.Fatal(err)
			}
			if sample.Missing != missing || sample.NS <= 0 {
				t.Fatal("hit/miss sample changed", sample)
			}
		}
	}
}

type operationErrorSegment struct{ adapter.Segment }

func (operationErrorSegment) NodeByID(context.Context, string) (dataset.Node, error) {
	return dataset.Node{}, fmt.Errorf("injected I/O failure")
}
func TestSemanticAndOperationErrorKindsDoNotMix(t *testing.T) {
	query := workload.Query{ID: "q", Op: "id", Repo: "fixture", Key: "a"}
	d := conformance.Fixture()
	reference := oracle.New(d)
	want, err := workload.Evaluate(context.Background(), map[string]adapter.Segment{d.Repo: reference}, nil, query)
	if err != nil {
		t.Fatal(err)
	}
	wrong := workload.Compare(query, want, workload.Answer{Missing: true})
	if !wrong.Mismatch || wrong.FailureKind != "semantic" {
		t.Fatal("answer mismatch kind missing", wrong)
	}
	segments := map[string]adapter.Segment{d.Repo: operationErrorSegment{reference}}
	_, checks, err := measuredQueries(context.Background(), segments, nil, []workload.Query{query}, []workload.Expected{{QueryID: query.ID, Answer: want}}, oracle.Backend{}.Capabilities())
	if err != nil || len(checks) != 1 || checks[0].FailureKind != "operation_error" {
		t.Fatal("operation error kind missing", checks, err)
	}
	if checks[0].FailureKind == "semantic" {
		t.Fatal("operation error became a format miss")
	}
}
func TestFixedMissRepeatsAndSemanticMemoKey(t *testing.T) {
	d := conformance.Fixture()
	segment := oracle.New(d)
	query := workload.Query{ID: "miss", Op: "id", Repo: d.Repo, Key: "missing"}
	expected := []workload.Expected{{QueryID: query.ID, Answer: workload.Answer{Missing: true}}}
	ctx := context.WithValue(context.Background(), lookupRepeatKey{}, 20)
	samples, checks, err := measuredQueries(ctx, map[string]adapter.Segment{d.Repo: segment}, nil, []workload.Query{query}, expected, oracle.Backend{}.Capabilities())
	if err != nil || len(samples["id"]) != 20 || len(checks) != 20 {
		t.Fatal("fixed repeats not collected", len(samples["id"]), len(checks), err)
	}
	memo, err := newReaderCheckMemo(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer memo.close()
	pending := pendingCheck{Query: query, Expected: expected[0], Answer: workload.Answer{Missing: true}}
	check, err := memo.check(context.Background(), pending)
	if err != nil || check.Mismatch {
		t.Fatal(check, err)
	}
	pending.Answer = workload.Answer{Node: &d.Nodes[0]}
	check, err = memo.check(context.Background(), pending)
	if err != nil || !check.Mismatch || check.FailureKind != "semantic" {
		t.Fatal("changed answer reused old pass", check, err)
	}
}

func TestMissingComparisonTraceIsNotSemantic(t *testing.T) {
	_, queries, want, caps := nativeScoreFixture(t)
	query := withPolicy(queries[2], caps)
	answer := workload.Answer{Hits: []adapter.Hit{{ID: "b1", Score: 12}}}
	if check := compareRankedAnswer(query, want[2], answer, nil); !check.Mismatch || check.FailureKind != "operation_error" {
		t.Fatal("missing trace was a format miss", check)
	}
	inputs := []adapter.SearchInput{{Repo: "a", Hits: []adapter.Hit{{ID: "a1", Score: 9}}}, {Repo: "b", Hits: []adapter.Hit{{ID: "b1", Score: 12}}}}
	answer.Hits[0].Score = 11
	if check := compareRankedAnswer(query, want[2], answer, inputs); !check.Mismatch || check.FailureKind != "semantic" {
		t.Fatal("wrong own-score merge lacks semantic kind", check)
	}
}

func TestUnmeasuredBuildCPULeftOutOfJSON(t *testing.T) {
	raw, err := json.Marshal(BuildRecord{ArtifactBytes: 1})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if _, exists := fields["cpu_ns"]; exists {
		t.Fatal("unmeasured zero presented as a CPU observation", string(raw))
	}
}
func TestActualBytesGuardCoversUnderreportedStats(t *testing.T) {
	stats := adapter.BuildStats{}
	if err := checkBuildCPU(0, stats.Bytes); err != nil {
		t.Fatal("transport cannot infer unreported bytes", err)
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "artifact"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	actual, err := localBytes(root)
	if err != nil {
		t.Fatal(err)
	}
	if actual != 1 || checkBuildCPU(0, actual) == nil {
		t.Fatal("actual non-empty artifact escaped zero-CPU guard", actual)
	}
}
