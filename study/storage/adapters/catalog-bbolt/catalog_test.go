package main

import (
	"context"
	"errors"
	"gxstudy/adapter"
	"gxstudy/conformance"
	"gxstudy/dataset"
	"gxstudy/oracle"
	"gxstudy/runner"
	"gxstudy/workload"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && strings.HasPrefix(os.Args[1], "__") {
		runner.Main(backend{}, factory)
		os.Exit(0)
	}
	os.Exit(m.Run())
}
func TestConformance(t *testing.T) {
	conformance.CheckBackend(t, backend{})
	conformance.CheckCatalog(t, backend{}, factory)
}
func TestSnapshotAndSetRejection(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	b := backend{}
	d := conformance.Fixture()
	first, _, err := b.Build(ctx, d, filepath.Join(dir, "first"))
	if err != nil {
		t.Fatal(err)
	}
	d.Repo = "peer"
	second, _, err := b.Build(ctx, d, filepath.Join(dir, "second"))
	if err != nil {
		t.Fatal(err)
	}
	cat, err := factory(filepath.Join(dir, "catalog"))
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	if _, _, err = cat.Pin(ctx, []string{"missing"}); !errors.Is(err, adapter.ErrNotFound) {
		t.Fatal(err)
	}
	if _, err = cat.Previous(ctx, "missing"); !errors.Is(err, adapter.ErrNotFound) {
		t.Fatal(err)
	}
	initial := adapter.View{first.Repo: first, second.Repo: second}
	if err = cat.PublishSet(ctx, initial, nil); err != nil {
		t.Fatal(err)
	}
	pin, release, err := cat.Pin(ctx, []string{first.Repo, second.Repo})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	wrong := first
	wrong.Digest = "not-same"
	if err = cat.PublishSet(ctx, initial, adapter.View{first.Repo: wrong, second.Repo: second}); !errors.Is(err, adapter.ErrConflict) {
		t.Fatal("failed set CAS was accepted", err)
	}
	if err = cat.Publish(ctx, first.Repo, first, &first); err != nil {
		t.Fatal(err)
	}
	if pin[first.Repo] != first || pin[second.Repo] != second {
		t.Fatal("old pin mutated")
	}
	for i := 0; i < 2; i++ {
		if err = cat.Recover(ctx); err != nil {
			t.Fatal(err)
		}
	}
	// A standalone update may detach one member; a later set must still work.
	if err = cat.PublishSet(ctx, initial, initial); err != nil {
		t.Fatal(err)
	}
}

// This tiny package fixture runs the actual harness's six-window readers and
// default 12 checkpoint + 12 external SIGKILL trials. It is not corpus tier1.
func TestHarnessFixtureProtocol(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	data := filepath.Join(dir, "fixture")
	d := conformance.Fixture()
	if _, err := dataset.Write(filepath.Join(data, d.Repo), d); err != nil {
		t.Fatal(err)
	}
	queries := workload.Generate([]dataset.Dataset{d}, 7)
	segments := map[string]adapter.Segment{d.Repo: oracle.New(d)}
	var expected []workload.Expected
	for _, q := range queries {
		answer, err := workload.Evaluate(ctx, segments, nil, q)
		if err != nil {
			t.Fatal(err)
		}
		expected = append(expected, workload.Expected{QueryID: q.ID, Answer: answer})
	}
	if err := dataset.WriteJSON(filepath.Join(data, "queries.json"), queries); err != nil {
		t.Fatal(err)
	}
	if err := dataset.WriteJSON(filepath.Join(data, "expected.json"), expected); err != nil {
		t.Fatal(err)
	}
	if err := dataset.TierManifest(data, []string{d.Repo}, dataset.Manifest{Version: dataset.GeneratorVersion}); err != nil {
		t.Fatal(err)
	}
	oldArgs := os.Args
	defer func() { os.Args = oldArgs }()
	out := filepath.Join(dir, "out")
	os.Args = []string{candidate, "-data", data, "-out", out, "-runs", "1", "-warmup=false", "-crash-trials", "12"}
	err := runner.RunCLI(backend{}, factory)
	resultPath := filepath.Join(out, candidate+"-fixture-01.json")
	resultData, readErr := os.ReadFile(resultPath)
	if readErr == nil {
		if e := os.MkdirAll("verification", 0755); e != nil {
			t.Fatal(e)
		}
		if e := os.WriteFile("verification/fixture-result.json", resultData, 0600); e != nil {
			t.Fatal(e)
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	if readErr != nil {
		t.Fatal(readErr)
	}
	var result runner.Result
	if err = dataset.ReadJSON(resultPath, &result); err != nil {
		t.Fatal(err)
	}
	if !result.Success || len(result.Crash) != 24 {
		t.Fatal("incomplete protocol", result.Success, len(result.Crash))
	}
	for _, trial := range result.Crash {
		if !trial.OK || !trial.Killed {
			t.Fatal(trial)
		}
	}
	for _, stage := range []string{"before-write", "after-write", "after-sync", "before-rename", "after-rename", "after-dirsync"} {
		if result.Concurrent.CommitRaceWindows[stage] < 2 {
			t.Fatal(stage, "reader coverage")
		}
	}
}

func TestLinkedSetCheckpointViews(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	b := backend{}
	d := conformance.Fixture()
	a, _, err := b.Build(ctx, d, filepath.Join(dir, "a"))
	if err != nil {
		t.Fatal(err)
	}
	d.Repo = "peer"
	p, _, err := b.Build(ctx, d, filepath.Join(dir, "p"))
	if err != nil {
		t.Fatal(err)
	}
	nextData := conformance.Fixture()
	nextData.Nodes[0].Name = "changed"
	na, _, err := b.Build(ctx, nextData, filepath.Join(dir, "na"))
	if err != nil {
		t.Fatal(err)
	}
	nextData.Repo = "peer"
	np, _, err := b.Build(ctx, nextData, filepath.Join(dir, "np"))
	if err != nil {
		t.Fatal(err)
	}
	cat, err := factory(filepath.Join(dir, "catalog"))
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	old := adapter.View{a.Repo: a, p.Repo: p}
	next := adapter.View{a.Repo: na, p.Repo: np}
	priorHook := adapter.Checkpoint
	defer func() { adapter.Checkpoint = priorHook }()
	repos := []string{a.Repo, p.Repo}
	stages := []string{"before-write", "after-write", "after-sync", "before-rename", "after-rename", "after-dirsync"}
	for pass := 0; pass < 2; pass++ {
		index := 0
		var firstError error
		desired, expected := old, adapter.View{}
		if pass == 1 {
			desired, expected = next, old
		}
		adapter.Checkpoint = func(stage string) {
			if index >= len(stages) || stages[index] != stage {
				firstError = errors.New("checkpoint order")
				return
			}
			index++
			got, release, err := cat.Pin(ctx, repos)
			after := stage == "after-rename" || stage == "after-dirsync"
			if pass == 0 && !after {
				if !errors.Is(err, adapter.ErrNotFound) {
					firstError = errors.New("initial linked set visible before commit")
				}
				return
			}
			if err != nil {
				firstError = err
				return
			}
			release()
			release()
			want := expected
			if after {
				want = desired
			}
			for _, repo := range repos {
				if got[repo] != want[repo] {
					firstError = errors.New("mixed or wrong-side linked set")
				}
			}
		}
		if err = cat.PublishSet(ctx, desired, expected); err != nil {
			t.Fatal(err)
		}
		if firstError != nil {
			t.Fatal(firstError)
		}
		if index != 6 {
			t.Fatal("missing hooks", index)
		}
	}
}
