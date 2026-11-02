//go:build linux

package runner

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"gxstudy/adapter"
	"gxstudy/conformance"
	"gxstudy/dataset"
	"gxstudy/oracle"
)

func TestCPUAccountingChild(t *testing.T) {
	if os.Getenv("GXSTUDY_CPU_CHILD") != "1" {
		return
	}
	before, err := ProcessCPU()
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	var value uint64 = 1
	for {
		for range 100000 {
			value = value*6364136223846793005 + 1
		}
		now, err := ProcessCPU()
		if err != nil {
			t.Fatal(err)
		}
		if now-before >= 300000000 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("child failed to consume bounded CPU")
		}
	}
	if value == 0 {
		t.Fatal("impossible child state")
	}
}

type childCPUBackend struct {
	oracle.Backend
	childCPU int64
}

func (b *childCPUBackend) Build(ctx context.Context, d dataset.Dataset, path string) (adapter.SegmentRef, adapter.BuildStats, error) {
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCPUAccountingChild$")
	command.Env = append(os.Environ(), "GXSTUDY_CPU_CHILD=1")
	if output, err := command.CombinedOutput(); err != nil {
		return adapter.SegmentRef{}, adapter.BuildStats{}, fmt.Errorf("CPU child: %w: %s", err, output)
	}
	usage := command.ProcessState.SysUsage().(*syscall.Rusage)
	b.childCPU = usage.Utime.Nano() + usage.Stime.Nano()
	return b.Backend.Build(ctx, d, path)
}
func TestBuildCPUIncludesWaitedChild(t *testing.T) {
	backend := &childCPUBackend{}
	service := ProcessService{backend: backend}
	var reply ProcessReply
	if err := service.Call(ProcessRequest{Op: "build", Dataset: conformance.Fixture(), Path: filepath.Join(t.TempDir(), "version")}, &reply); err != nil {
		t.Fatal(err)
	}
	if reply.Error != "" {
		t.Fatal(reply.Error)
	}
	if backend.childCPU < 300000000 || reply.CPU < backend.childCPU {
		t.Fatal("joined child CPU was excluded", reply.CPU, backend.childCPU)
	}
}
