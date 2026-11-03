package runner

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"gxstudy/adapter"
	"gxstudy/conformance"
	"gxstudy/dataset"
	"gxstudy/oracle"
	"gxstudy/workload"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func testCgroupRoot(t *testing.T) string {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("kernel cgroup-v2 tests require Linux")
	}
	root := os.Getenv("GXSTUDY_TEST_CGROUP_ROOT")
	if root == "" {
		t.Fatal("GXSTUDY_TEST_CGROUP_ROOT is required for Linux hard-limit acceptance tests")
	}
	return root
}
func fixtureTier(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "tier")
	d := conformance.Fixture()
	if _, err := dataset.Write(filepath.Join(root, d.Repo), d); err != nil {
		t.Fatal(err)
	}
	qs := []workload.Query{{ID: "point", Op: "id", Repo: d.Repo, Key: "a"}, {ID: "edge", Op: "in", Repo: d.Repo, Key: "a", Kind: "calls"}, {ID: "symbol", Op: "symbols", Repo: d.Repo, Key: "graph", K: 10}, {ID: "vector", Op: "vector", Repo: d.Repo, Vec: d.Vectors[0].Values, K: 2}}
	var want []workload.Expected
	for _, q := range qs {
		a, err := workload.Evaluate(context.Background(), map[string]adapter.Segment{d.Repo: oracle.New(d)}, nil, q)
		if err != nil {
			t.Fatal(err)
		}
		want = append(want, workload.Expected{QueryID: q.ID, Answer: a})
	}
	if err := dataset.WriteJSON(filepath.Join(root, "queries.json"), qs); err != nil {
		t.Fatal(err)
	}
	if err := dataset.WriteJSON(filepath.Join(root, "expected.json"), want); err != nil {
		t.Fatal(err)
	}
	if err := dataset.TierManifest(root, []string{d.Repo}, dataset.Manifest{Version: dataset.GeneratorVersion}); err != nil {
		t.Fatal(err)
	}
	return root
}
func fixtureBinding(t *testing.T, data string, args []string) RunBinding {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	h, err := dataset.Hash(exe)
	if err != nil {
		t.Fatal(err)
	}
	dataHash := ""
	if data != "" {
		dataHash, err = dataset.Hash(filepath.Join(data, "manifest.json"))
		if err != nil {
			t.Fatal(err)
		}
	} else {
		dataHash = fmt.Sprintf("%x", sha256.Sum256([]byte("catalog-fixture")))
	}
	host, err := os.Hostname()
	if err != nil {
		t.Fatal(err)
	}
	return RunBinding{SourceRevision: strings.Repeat("a", 40), HarnessSHA256: h, DataSHA256: dataHash, RecipeSHA256: fmt.Sprintf("%x", sha256.Sum256([]byte("synthetic-fixture"))), Host: host, Command: args, Limits: MemoryLimits{Runner: 256 << 20, Checker: 256 << 20, Engine: 256 << 20}}
}
func TestActualCLIProcessesAndFullProtocol(t *testing.T) {
	root := testCgroupRoot(t)
	data := fixtureTier(t)
	out := filepath.Join(t.TempDir(), "results")
	bindingPath := filepath.Join(t.TempDir(), "binding.json")
	original := os.Args
	t.Cleanup(func() { os.Args = original })
	os.Args = []string{original[0], "-data", data, "-out", out, "-runs", "1", "-warmup=false", "-crash-trials", "1", "-binding", bindingPath, "-cgroup-root", root}
	binding := fixtureBinding(t, data, os.Args[1:])
	if err := dataset.WriteJSON(bindingPath, binding); err != nil {
		t.Fatal(err)
	}
	if err := RunCLI(oracle.Backend{}, oracle.NewCatalog); err != nil {
		t.Fatal(err)
	}
	var receipt componentReceipt
	if err := dataset.ReadJSON(filepath.Join(out, "components-01.json"), &receipt); err != nil {
		t.Fatal(err)
	}
	if !receipt.Success || len(receipt.Outputs) == 0 {
		t.Fatal("no complete hash-bound receipt", receipt)
	}
	pids := map[int]bool{}
	for _, name := range []string{"runner", "checker", "engine"} {
		component := receipt.Components[name]
		if component.PID <= 0 || pids[component.PID] || component.PeakBytes <= 0 || component.PeakRSSBytes <= 0 || component.Status != "completed" {
			t.Fatal("component accounting not isolated", name, component)
		}
		pids[component.PID] = true
	}
	var report Result
	if err := dataset.ReadJSON(filepath.Join(out, "oracle-tier-01.json"), &report); err != nil {
		t.Fatal(err)
	}
	if !report.Success || len(report.DeltaChecks) == 0 || len(report.Concurrent.CommitRaceWindows) != len(stages) || len(report.Crash) != 2 {
		t.Fatal("full protocol did not run", report.Failures)
	}
	if len(report.Builds) == 0 || report.Builds[0].CPU <= 0 || report.Delta.CPU <= 0 {
		t.Fatal("engine construction CPU missing", report.Builds, report.Delta.CPU)
	}
	if report.PeakRSSBytes != receipt.Components["engine"].PeakRSSBytes {
		t.Fatal("reported RSS is not engine-only")
	}
	var runnerSelf ComponentMemory
	if err := dataset.ReadJSON(filepath.Join(out, "runner-01.memory.json"), &runnerSelf); err != nil {
		t.Fatal(err)
	}
	if receipt.Components["runner"].PeakRSSBytes != runnerSelf.PeakRSSBytes || receipt.Components["runner"].RSSSource != "RUSAGE_SELF" {
		t.Fatal("runner receipt did not use its own high-water", receipt.Components["runner"], runnerSelf)
	}
	// A repeat cannot reuse the output and certify an old result.
	if err := RunCLI(oracle.Backend{}, oracle.NewCatalog); err == nil {
		t.Fatal("restart reused an existing run directory")
	}
}
func TestHardLimitFailurePreservesEvidenceForEachComponent(t *testing.T) {
	root := testCgroupRoot(t)
	data := fixtureTier(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"runner", "checker", "engine"} {
		t.Run(name, func(t *testing.T) {
			out := t.TempDir()
			binding := fixtureBinding(t, data, nil)
			switch name {
			case "runner":
				binding.Limits.Runner = 1 << 20
			case "checker":
				binding.Limits.Checker = 1 << 20
			case "engine":
				binding.Limits.Engine = 1 << 20
			}
			err := controlledRun(context.Background(), exe, "__measure_run", root, runRequest{Data: data, Out: out, Run: 1, Trials: 1, SafetyTimeout: defaultSafetyTimeout, TimingOnly: true, Binding: binding})
			if err == nil {
				t.Fatal("hard memory limit did not reject", name)
			}
			var receipt componentReceipt
			if e := dataset.ReadJSON(filepath.Join(out, "components-01.json"), &receipt); e != nil {
				t.Fatal(e)
			}
			if receipt.Success || receipt.Components[name].OOMKills == 0 || receipt.Components[name].PeakBytes == 0 {
				t.Fatal("lost component failure evidence", name, receipt)
			}
			if _, e := os.Stat(filepath.Join(out, "request-01.json")); e != nil {
				t.Fatal("failed request erased", e)
			}
			assertRunScopesRemoved(t, out)
		})
	}
}
func TestCLIRejectsMissingOrMismatchedBindingsBeforeOutput(t *testing.T) {
	data := fixtureTier(t)
	for _, kind := range []string{"missing", "digest", "host", "command", "limit", "alignment"} {
		t.Run(kind, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "out")
			path := filepath.Join(t.TempDir(), "binding.json")
			original := os.Args
			t.Cleanup(func() { os.Args = original })
			os.Args = []string{original[0], "-data", data, "-out", out, "-binding", path, "-cgroup-root", "/sys/fs/cgroup/not-used"}
			binding := fixtureBinding(t, data, os.Args[1:])
			switch kind {
			case "digest":
				binding.HarnessSHA256 = strings.Repeat("0", 64)
			case "host":
				binding.Host = "wrong"
			case "command":
				binding.Command = nil
			case "limit":
				binding.Limits.Engine = 0
			case "alignment":
				binding.Limits.Engine++
			}
			if kind != "missing" {
				if err := dataset.WriteJSON(path, binding); err != nil {
					t.Fatal(err)
				}
			}
			if err := RunCLI(oracle.Backend{}, oracle.NewCatalog); err == nil {
				t.Fatal("accepted invalid binding", kind)
			}
			if _, err := os.Stat(out); !os.IsNotExist(err) {
				t.Fatal("rejected binding created output", err)
			}
		})
	}
}
func TestDiskDeltaAndChangedAnswersMatchFixtureCallers(t *testing.T) {
	data := fixtureTier(t)
	ds, qs, want, _, err := loadTier(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(ds[0].Nodes) != 0 || ds[0].Source == nil || len(want[0].Sources) == 0 {
		t.Fatal("runtime loaded a dataset")
	}
	diskChange, err := MakeChange(ds[0])
	if err != nil {
		t.Fatal(err)
	}
	memoryChange, err := MakeChange(conformance.Fixture())
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(diskChange)
	b, _ := json.Marshal(memoryChange)
	if string(a) != string(b) {
		t.Fatal("delta selection differs", string(a), string(b))
	}
	changed, err := adapter.ChangeSource(ds[0], diskChange)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := changedAnswers(context.Background(), ds, changed, qs)
	if err != nil {
		t.Fatal(err)
	}
	memory := adapter.ApplyChange(conformance.Fixture(), memoryChange)
	for i, q := range qs {
		a, err := workload.Evaluate(context.Background(), map[string]adapter.Segment{memory.Repo: oracle.New(memory)}, nil, q)
		if err != nil {
			t.Fatal(err)
		}
		q = withPolicy(q, oracle.Backend{}.Capabilities())
		check, err := checkedAnswer(context.Background(), q, expected[i], a, nil)
		if err != nil || check.Mismatch {
			t.Fatal("changed caller parity", q.ID, check, err)
		}
	}
}

type noPublicationBackend struct {
	oracle.Backend
	builds int
}

func (b *noPublicationBackend) Build(ctx context.Context, d dataset.Dataset, path string) (adapter.SegmentRef, adapter.BuildStats, error) {
	b.builds++
	return b.Backend.Build(ctx, d, path)
}
func TestRejectedDiskTierNeverBuildsCandidate(t *testing.T) {
	for _, kind := range []string{"duplicate", "cross-repo-duplicate", "digest", "alias", "symlink", "partial", "query-id"} {
		t.Run(kind, func(t *testing.T) {
			tier := fixtureTier(t)
			switch kind {
			case "duplicate":
				d := conformance.Fixture()
				d.Nodes = append(d.Nodes, d.Nodes[0])
				if _, err := dataset.Write(filepath.Join(tier, d.Repo), d); err != nil {
					t.Fatal(err)
				}
				if err := dataset.TierManifest(tier, []string{d.Repo}, dataset.Manifest{Version: dataset.GeneratorVersion}); err != nil {
					t.Fatal(err)
				}
			case "cross-repo-duplicate":
				d := conformance.Fixture()
				d.Repo = "second"
				if _, err := dataset.Write(filepath.Join(tier, d.Repo), d); err != nil {
					t.Fatal(err)
				}
				if err := dataset.TierManifest(tier, []string{"fixture", "second"}, dataset.Manifest{Version: dataset.GeneratorVersion}); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				path := filepath.Join(tier, "fixture", "nodes.jsonl")
				bytes, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				external := filepath.Join(t.TempDir(), "nodes")
				if err = os.WriteFile(external, bytes, 0600); err != nil {
					t.Fatal(err)
				}
				if err = os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err = os.Symlink(external, path); err != nil {
					t.Fatal(err)
				}
			case "digest":
				if err := os.WriteFile(filepath.Join(tier, "fixture", "nodes.jsonl"), []byte("{}\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "alias":
				var meta dataset.Manifest
				if err := dataset.ReadJSON(filepath.Join(tier, "manifest.json"), &meta); err != nil {
					t.Fatal(err)
				}
				meta.Repos = append(meta.Repos, "fixture")
				if err := dataset.WriteJSON(filepath.Join(tier, "manifest.json"), meta); err != nil {
					t.Fatal(err)
				}
			case "partial":
				if err := os.WriteFile(filepath.Join(tier, "expected.json"), []byte("["), 0600); err != nil {
					t.Fatal(err)
				}
				if err := dataset.TierManifest(tier, []string{"fixture"}, dataset.Manifest{Version: dataset.GeneratorVersion}); err != nil {
					t.Fatal(err)
				}
			case "query-id":
				if err := dataset.WriteJSON(filepath.Join(tier, "queries.json"), []workload.Query{{ID: "wrong", Op: "id", Repo: "fixture", Key: "a"}}); err != nil {
					t.Fatal(err)
				}
				if err := dataset.TierManifest(tier, []string{"fixture"}, dataset.Manifest{Version: dataset.GeneratorVersion}); err != nil {
					t.Fatal(err)
				}
			}
			b := &noPublicationBackend{}
			out := filepath.Join(t.TempDir(), "out")
			if err := runChild(b, oracle.NewCatalog, runRequest{Data: tier, Out: out, Run: 1, Trials: 1}); err == nil {
				t.Fatal("invalid tier accepted", kind)
			}
			if b.builds != 0 {
				t.Fatal("invalid input reached candidate publication")
			}
		})
	}
}
func TestDiskDistributionMatchesExactInMemorySummary(t *testing.T) {
	values := []int64{19, 3, 1, 2, 17, 7, 7, 9, 6, 4, 13, 11, 5, 8, 12, 10, 16, 14, 15, 18}
	distribution, err := newDiskDistribution(t.TempDir(), "fixture")
	if err != nil {
		t.Fatal(err)
	}
	defer distribution.close()
	for _, ns := range values {
		if err = distribution.add(ns); err != nil {
			t.Fatal(err)
		}
	}
	got, err := distribution.finish()
	if err != nil {
		t.Fatal(err)
	}
	if want := Summarize(values); got != want {
		t.Fatal("disk summary changed percentiles", got, want)
	}
}

type namedCatalogBackend struct{ oracle.Backend }

func (namedCatalogBackend) Name() string { return "catalog-refs" }

type blockedBuildBackend struct{ oracle.Backend }

func (blockedBuildBackend) Build(ctx context.Context, d dataset.Dataset, path string) (adapter.SegmentRef, adapter.BuildStats, error) {
	ready, err := os.OpenFile(os.Getenv("GXSTUDY_TEST_BUILD_READY"), os.O_WRONLY, 0600)
	if err != nil {
		return adapter.SegmentRef{}, adapter.BuildStats{}, err
	}
	_, writeErr := ready.Write([]byte{1})
	closeErr := ready.Close()
	if writeErr != nil {
		return adapter.SegmentRef{}, adapter.BuildStats{}, writeErr
	}
	if closeErr != nil {
		return adapter.SegmentRef{}, adapter.BuildStats{}, closeErr
	}
	<-ctx.Done()
	return adapter.SegmentRef{}, adapter.BuildStats{}, ctx.Err()
}
func testProcessBackend() adapter.Backend {
	if os.Getenv("GXSTUDY_TEST_BUILD_READY") != "" {
		return blockedBuildBackend{}
	}
	if os.Getenv("GXSTUDY_TEST_CATALOG") == "1" {
		return namedCatalogBackend{}
	}
	return oracle.Backend{}
}
func TestActualCatalogEntrypointUsesLimitedEngine(t *testing.T) {
	root := testCgroupRoot(t)
	t.Setenv("GXSTUDY_TEST_CATALOG", "1")
	out := filepath.Join(t.TempDir(), "catalog")
	bindingPath := filepath.Join(t.TempDir(), "binding.json")
	args := []string{"-candidate", "catalog-refs", "-entries", "1", "-repeats", "1", "-seed", "20261025", "-out", out}
	original := os.Args
	t.Cleanup(func() { os.Args = original })
	os.Args = append([]string{original[0], "catalog-bench", "-binding", bindingPath, "-cgroup-root", root, "--"}, args...)
	binding := fixtureBinding(t, "", os.Args[1:])
	var err error
	binding.DataSHA256, err = catalogInputDigest(args)
	if err != nil {
		t.Fatal(err)
	}
	if err = dataset.WriteJSON(bindingPath, binding); err != nil {
		t.Fatal(err)
	}
	if err = RunLimitedCatalog(namedCatalogBackend{}, oracle.NewCatalog, os.Args[2:]); err != nil {
		t.Fatal(err)
	}
	var receipt componentReceipt
	if err = dataset.ReadJSON(out+".control/components-01.json", &receipt); err != nil {
		t.Fatal(err)
	}
	if !receipt.Success || receipt.Components["engine"].PeakRSSBytes == 0 {
		t.Fatal("catalog accepted without engine accounting", receipt)
	}
	var result map[string]any
	if err = dataset.ReadJSON(filepath.Join(out, "result.json"), &result); err != nil {
		t.Fatal(err)
	}
	if result["success"] != true {
		t.Fatal("catalog result failed", result)
	}
}
func TestLegacyUnboundedEntrypointsRejectBeforeOutput(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out")
	original := os.Args
	t.Cleanup(func() { os.Args = original })
	os.Args = []string{original[0], "-data", "unused", "-out", out}
	if err := RunCLI(oracle.Backend{}, oracle.NewCatalog); err == nil || !strings.Contains(err.Error(), "binding and cgroup-root required") {
		t.Fatal("legacy unbounded run did not reject visibly", err)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatal("legacy invocation wrote output", err)
	}
	if err := RunLimitedCatalog(namedCatalogBackend{}, oracle.NewCatalog, []string{"-candidate", "catalog-refs", "-out", out}); err == nil {
		t.Fatal("legacy catalog invocation accepted")
	}
}

func TestCheckerExitBeforeHandshakeReturnsItsExitStatus(t *testing.T) {
	root := testCgroupRoot(t)
	scope, err := newMemoryScope(root, "checker-startup-exit", 256<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := scope.close(); err != nil {
			t.Error(err)
		}
	}()
	t.Setenv("GXSTUDY_TEST_CHECKER_EXIT", "1")
	ctx, cancel := context.WithTimeout(context.Background(), defaultSafetyTimeout)
	defer cancel()
	client, err := startProcess(ctx, scope.path, filepath.Join(t.TempDir(), "checker.log"), true)
	if client != nil {
		client.close()
		t.Fatal("early-exit checker was accepted")
	}
	if err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("checker exit was hidden until the RPC deadline", err)
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 23 {
		t.Fatal("lost checker exit status", err)
	}
}

func assertRunScopesRemoved(t *testing.T, out string) {
	t.Helper()
	var req runRequest
	if err := dataset.ReadJSON(filepath.Join(out, "request-01.json"), &req); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{"runner": req.RunnerScope, "checker": req.CheckerScope, "engine": req.EngineScope} {
		if path == "" {
			t.Fatal("missing owned scope identity", name)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("owned scope leaked after termination", name, path, err)
		}
	}
}

func cliFixtureRequest(t *testing.T, extra ...string) runRequest {
	t.Helper()
	root := testCgroupRoot(t)
	data := fixtureTier(t)
	out := filepath.Join(t.TempDir(), "out")
	bindingPath := filepath.Join(t.TempDir(), "binding.json")
	previous := os.Args
	t.Cleanup(func() { os.Args = previous })
	os.Args = append([]string{previous[0], "-data", data, "-out", out, "-runs", "1", "-warmup=false", "-crash-trials", "1", "-binding", bindingPath, "-cgroup-root", root}, extra...)
	if err := dataset.WriteJSON(bindingPath, fixtureBinding(t, data, os.Args[1:])); err != nil {
		t.Fatal(err)
	}
	if err := RunCLI(oracle.Backend{}, oracle.NewCatalog); err != nil {
		t.Fatal(err)
	}
	var req runRequest
	if err := dataset.ReadJSON(filepath.Join(out, "request-01.json"), &req); err != nil {
		t.Fatal(err)
	}
	return req
}

func TestCheckerOwnsOneReferenceViewOutsideEngineMeasurements(t *testing.T) {
	ds, _, _, _, err := loadTier(fixtureTier(t))
	if err != nil {
		t.Fatal(err)
	}
	backend := &noPublicationBackend{}
	service := &ProcessService{backend: backend, checker: true}
	t.Cleanup(func() {
		if err := service.closeReference(); err != nil {
			t.Error(err)
		}
	})
	var described ProcessReply
	if err = service.Call(ProcessRequest{Op: "describe"}, &described); err != nil || described.Error != "" || service.referenceView != nil {
		t.Fatal("handshake constructed an index", err, described.Error)
	}
	q := workload.Query{ID: "edge", Op: "in", Repo: ds[0].Repo, Key: "a", Kind: "calls"}
	expected, err := oracle.DiskReference(context.Background(), ds, q, nil)
	if err != nil {
		t.Fatal(err)
	}
	request := ProcessRequest{Op: "check", Data: ds, Query: q, Answer: expected.Answer}
	for repeat := 0; repeat < 2; repeat++ {
		var reply ProcessReply
		previous := service.referenceView
		if err = service.Call(request, &reply); err != nil || reply.Error != "" || reply.Check.Mismatch {
			t.Fatal("reference check failed", err, reply.Error, reply.Check)
		}
		if service.referenceView == nil || repeat > 0 && previous != service.referenceView {
			t.Fatal("checker rebuilt identical view")
		}
		if backend.builds != 0 || reply.Elapsed != 0 || reply.CPU != 0 || !reply.Start.IsZero() || !reply.End.IsZero() {
			t.Fatal("checker entered native construction/timing", reply)
		}
	}
	owned := service.referenceView
	if err = service.closeReference(); err != nil {
		t.Fatal(err)
	}
	if service.referenceView != nil || owned.Validate(context.Background()) == nil {
		t.Fatal("checker shutdown left view open")
	}
	// Non-checker callers retain the independently scanned reference path.
	engine := &ProcessService{backend: backend}
	var reply ProcessReply
	if err = engine.Call(request, &reply); err != nil || reply.Error != "" || reply.Check.Mismatch || engine.referenceView != nil {
		t.Fatal("engine acquired checker index", err, reply)
	}
}

func TestCheckerReferenceDescriptorOverlayAndTierIsolation(t *testing.T) {
	ds, _, _, _, err := loadTier(fixtureTier(t))
	if err != nil {
		t.Fatal(err)
	}
	service := &ProcessService{checker: true}
	defer service.closeReference()
	q := workload.Query{ID: "edge", Op: "in", Repo: ds[0].Repo, Key: "a", Kind: "calls"}
	if _, err = service.checkReference(context.Background(), ds, q, nil); err != nil {
		t.Fatal(err)
	}
	original := service.referenceView
	change, err := MakeChange(ds[0])
	if err != nil {
		t.Fatal(err)
	}
	changed, err := adapter.ChangeSource(ds[0], change)
	if err != nil {
		t.Fatal(err)
	}
	overlay := []dataset.Dataset{changed}
	want, err := oracle.DiskReference(context.Background(), overlay, q, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := service.checkReference(context.Background(), overlay, q, nil)
	a, _ := json.Marshal(want)
	b, _ := json.Marshal(got)
	if err != nil || string(a) != string(b) || service.referenceView == original || original.Validate(context.Background()) == nil {
		t.Fatal("overlay reused or changed the original index", err, string(a), string(b))
	}
	// Removing and re-adding a descriptor creates a new owned view, not stale
	// offsets retained from before the overlay.
	if _, err = service.checkReference(context.Background(), ds, q, nil); err != nil {
		t.Fatal(err)
	}
	immutable := service.referenceView
	ds[0].Meta.Seed++
	if _, err = service.checkReference(context.Background(), ds, q, nil); err == nil {
		t.Fatal("same-path changed manifest descriptor reused index")
	}
	if service.referenceView != nil || immutable.Validate(context.Background()) == nil {
		t.Fatal("failed replacement retained open view")
	}
	other, _, _, _, err := loadTier(fixtureTier(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = service.checkReference(context.Background(), other, q, nil); err != nil {
		t.Fatal("different tier path did not get its own view", err)
	}
}

func TestCheckerCancelledRequestDoesNotRetainView(t *testing.T) {
	ds, _, _, _, err := loadTier(fixtureTier(t))
	if err != nil {
		t.Fatal(err)
	}
	service := &ProcessService{checker: true}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = service.checkReference(ctx, ds, workload.Query{Op: "in"}, nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if service.referenceView != nil {
		t.Fatal("cancelled construction retained a reference view")
	}
}
