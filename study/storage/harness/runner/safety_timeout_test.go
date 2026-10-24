package runner

import (
	"context"
	"errors"
	"gxstudy/adapter"
	"gxstudy/conformance"
	"gxstudy/dataset"
	"gxstudy/oracle"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func safetyRequest(t *testing.T, option string) runRequest {
	t.Helper()
	args := os.Args
	t.Cleanup(func() { os.Args = args })
	out := filepath.Join(t.TempDir(), "out")
	os.Args = []string{args[0], "-data", "unused", "-out", out, "-runs", "1", "-warmup=false", "-crash-trials", "1"}
	if option != "" {
		os.Args = append(os.Args, "-safety-timeout", option)
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

type deadlineCatalog struct {
	adapter.Catalog
	deadline time.Time
	stop     error
}

func (c *deadlineCatalog) Publish(ctx context.Context, _ string, _ adapter.SegmentRef, _ *adapter.SegmentRef) error {
	c.deadline, _ = ctx.Deadline()
	return c.stop
}
func (c *deadlineCatalog) Close() error { return nil }

func TestSafetyTimeoutReachesCommitGuard(t *testing.T) {
	for _, tc := range []struct {
		name, option string
		want         time.Duration
	}{{"default", "", 30 * time.Second}, {"custom", "15m", 15 * time.Minute}} {
		t.Run(tc.name, func(t *testing.T) {
			req := safetyRequest(t, tc.option)
			if req.SafetyTimeout != tc.want {
				t.Fatalf("serialized safety timeout = %s, want %s", req.SafetyTimeout, tc.want)
			}
			stop := errors.New("deadline captured")
			cat := &deadlineCatalog{stop: stop}
			factory := func(string) (adapter.Catalog, error) { return cat, nil }
			before := time.Now()
			_, _, err := commitRace(context.Background(), oracle.Backend{}, factory, dataset.Dataset{}, adapter.SegmentRef{}, adapter.SegmentRef{}, t.TempDir(), req.SafetyTimeout)
			after := time.Now()
			if !errors.Is(err, stop) {
				t.Fatalf("commit-race error = %v, want probe error", err)
			}
			if cat.deadline.Before(before.Add(tc.want)) || cat.deadline.After(after.Add(tc.want)) {
				t.Fatalf("guard deadline %s is outside configured %s interval", cat.deadline, tc.want)
			}
		})
	}
}

func TestSafetyTimeoutReachesCrashGuard(t *testing.T) {
	req := safetyRequest(t, "1ns")
	ctx := context.Background()
	b := oracle.Backend{}
	base := conformance.Fixture()
	dir := t.TempDir()
	old, _, err := b.Build(ctx, base, filepath.Join(dir, "old"))
	if err != nil {
		t.Fatal(err)
	}
	change, err := MakeChange(base)
	if err != nil {
		t.Fatal(err)
	}
	next, _, err := b.BuildDelta(ctx, old, change, filepath.Join(dir, "new"))
	if err != nil {
		t.Fatal(err)
	}
	trials, err := crashTests(ctx, b, oracle.NewCatalog, base, old, next, filepath.Join(dir, "crash"), 1, req.SafetyTimeout)
	if !errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
		t.Fatalf("crash guard error = %v, parent error = %v", err, ctx.Err())
	}
	if len(trials) != 1 || trials[0].OK || trials[0].Error != context.DeadlineExceeded.Error() {
		t.Fatalf("expired guard trials = %+v", trials)
	}
}

func TestSafetyTimeoutRejectsNonPositive(t *testing.T) {
	for _, value := range []string{"0", "-1s"} {
		t.Run(value, func(t *testing.T) {
			args := os.Args
			t.Cleanup(func() { os.Args = args })
			out := filepath.Join(t.TempDir(), "out")
			os.Args = []string{args[0], "-data", "unused", "-out", out, "-safety-timeout", value}
			if err := RunCLI(oracle.Backend{}, oracle.NewCatalog); err == nil {
				t.Fatal("nonpositive safety timeout accepted")
			}
			if _, err := os.Stat(out); !os.IsNotExist(err) {
				t.Fatalf("invalid option created output: %v", err)
			}
		})
	}
}
