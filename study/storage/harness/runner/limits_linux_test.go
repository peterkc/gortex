//go:build linux

package runner

import (
	"context"
	"gxstudy/dataset"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestCancellationCannotCompleteOrDeleteUnownedScope(t *testing.T) {
	root := testCgroupRoot(t)
	unowned, err := newMemoryScope(root, "unowned", 32<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := unowned.close(); err != nil {
			t.Error(err)
		}
	}()
	out := t.TempDir()
	data := fixtureTier(t)
	binding := fixtureBinding(t, data, nil)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := controlledRun(ctx, exe, "__measure_run", root, runRequest{Data: data, Out: out, Run: 1, Binding: binding}); err == nil {
		t.Fatal("cancelled run completed")
	}
	var receipt componentReceipt
	if err := dataset.ReadJSON(filepath.Join(out, "components-01.json"), &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.Success {
		t.Fatal("false complete receipt")
	}
	if _, err := os.Stat(unowned.path); err != nil {
		t.Fatal("unowned scope removed", err)
	}
}
func TestCleanupRefusesReplacedScope(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()
	info, err := os.Stat(second)
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(first, "unowned")
	if err = os.WriteFile(marker, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	scope := memoryScope{path: first, info: info}
	if err = scope.close(); err == nil {
		t.Fatal("replaced ownership accepted")
	}
	if _, err = os.Stat(marker); err != nil {
		t.Fatal("unowned file deleted", err)
	}
}

func TestCancellationDuringEngineBuildPreservesFailureAndCleanup(t *testing.T) {
	root := testCgroupRoot(t)
	data := fixtureTier(t)
	out := t.TempDir()
	fifo := filepath.Join(t.TempDir(), "ready")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	pipe, err := os.OpenFile(fifo, os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer pipe.Close()
	t.Setenv("GXSTUDY_TEST_BUILD_READY", fifo)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), defaultSafetyTimeout)
	defer cancel()
	binding := fixtureBinding(t, data, nil)
	finished := make(chan error, 1)
	go func() {
		finished <- controlledRun(ctx, exe, "__measure_run", root, runRequest{Adapter: "oracle", Data: data, Out: out, Run: 1, Trials: 1, TimingOnly: true, SafetyTimeout: defaultSafetyTimeout, Binding: binding})
	}()
	ready := make(chan error, 1)
	go func() { var token [1]byte; _, err := pipe.Read(token[:]); ready <- err }()
	select {
	case err = <-ready:
		if err != nil {
			cancel()
			<-finished
			t.Fatal(err)
		}
	case earlyErr := <-finished:
		pipe.Close()
		<-ready
		t.Fatalf("run ended before the engine-build cancellation handshake: %v", earlyErr)
	case <-ctx.Done():
		pipe.Close()
		<-ready
		<-finished
		t.Fatal("engine build never reached the cancellation handshake")
	}
	cancel()
	select {
	case err = <-finished:
	case <-time.After(defaultSafetyTimeout):
		t.Fatal("cancelled process tree did not terminate")
	}
	if err == nil {
		t.Fatal("cancelled active build completed")
	}
	var receipt componentReceipt
	if err = dataset.ReadJSON(filepath.Join(out, "components-01.json"), &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.Success || receipt.Components["engine"].PID <= 0 || receipt.Components["engine"].PeakBytes <= 0 {
		t.Fatal("cancelled engine evidence lost", receipt)
	}
	if _, err = os.Stat(filepath.Join(out, "oracle-tier-01.json")); !os.IsNotExist(err) {
		t.Fatal("cancelled build published a complete result", err)
	}
	assertRunScopesRemoved(t, out)
}

func TestMemoryScopeSetsAndReadsKernelOOMPolicy(t *testing.T) {
	scope, err := newMemoryScope(testCgroupRoot(t), "kernel-policy", 256<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := scope.close(); err != nil {
			t.Error(err)
		}
	}()
	for name, want := range map[string]string{"memory.swap.max": "0", "memory.oom.group": "1"} {
		got, err := os.ReadFile(filepath.Join(scope.path, name))
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(string(got)) != want {
			t.Fatal("kernel OOM policy mismatch", name, string(got), want)
		}
	}
}
