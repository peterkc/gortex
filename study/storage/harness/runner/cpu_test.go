package runner

import (
	"context"
	"encoding/json"
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
