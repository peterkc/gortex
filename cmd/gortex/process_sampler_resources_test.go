//go:build acceptance

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestProcessSamplerResourcesHelper(t *testing.T) {
	if os.Getenv("GX_PROCESS_RESOURCES_HELPER") != "1" {
		return
	}
	memory := make([]byte, 64<<20)
	for i := 0; i < len(memory); i += 4096 {
		memory[i] = 1
	}
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		t.Fatal(err)
	}
	start := time.Duration(usage.Utime.Sec)*time.Second + time.Duration(usage.Utime.Usec)*time.Microsecond
	for {
		for i := 0; i < len(memory); i += 4096 {
			memory[i]++
		}
		if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
			t.Fatal(err)
		}
		user := time.Duration(usage.Utime.Sec)*time.Second + time.Duration(usage.Utime.Usec)*time.Microsecond
		if user-start >= 200*time.Millisecond {
			break
		}
	}
	fmt.Println("resources ready")
	var stop [1]byte
	if _, err := os.Stdin.Read(stop[:]); err != nil {
		t.Fatal(err)
	}
	runtime.KeepAlive(memory)
}

func TestProcessSamplerResourcesFinalUsage(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("final resource sampler supports Linux and Darwin")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestProcessSamplerResourcesHelper$")
	cmd.Env = append(os.Environ(), "GX_PROCESS_RESOURCES_HELPER=1")
	cmd.Stderr = os.Stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		if cmd.ProcessState == nil {
			_ = cmd.Wait()
		}
	})
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "resources ready\n" {
		t.Fatalf("helper readiness = %q, %v", line, err)
	}
	f := &issue767Fixture{run: 1}
	f.setChild(cmd, nil, nil, nil)
	if runtime.GOOS == "darwin" {
		missing := f.processResources()[0]
		if missing.PeakFootprintBytes != nil || !strings.HasPrefix(missing.Unavailable["PeakFootprintBytes"], "unavailable: ") {
			t.Fatalf("missing footprint sample produced a measurement: %+v", missing)
		}
		if _, err := f.sampleProcessIO(ctx, cmd.Process.Pid); err != nil {
			t.Fatalf("footprint sample: %v", err)
		}
	}
	if _, err := stdin.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("helper exit: %v", err)
	}
	usage := issue767FinalProcessResources(cmd.ProcessState)
	if usage.UserCPUNS == nil || *usage.UserCPUNS < int64(100*time.Millisecond) {
		t.Fatalf("user CPU must be at least 100 ms: %+v", usage)
	}
	if usage.SystemCPUNS == nil || *usage.SystemCPUNS < 0 {
		t.Fatalf("system CPU missing or negative: %+v", usage)
	}
	if usage.PeakRSSBytes == nil || *usage.PeakRSSBytes < 64<<20 {
		t.Fatalf("peak RSS must be at least 64 MiB: %+v", usage)
	}
	if len(usage.Unavailable) != 0 {
		t.Fatalf("final usage unavailable: %v", usage.Unavailable)
	}
	t.Logf("UserCPUNS=%d SystemCPUNS=%d PeakRSSBytes=%d", *usage.UserCPUNS, *usage.SystemCPUNS, *usage.PeakRSSBytes)
	if runtime.GOOS == "darwin" {
		footprint := f.processResources()[0]
		if footprint.PeakFootprintBytes == nil || *footprint.PeakFootprintBytes < 64<<20 {
			t.Fatalf("physical-footprint high-water mark missing or too small: %+v", footprint)
		}
		t.Logf("PeakFootprintBytes=%d (physical-footprint high-water mark, not RSS)", *footprint.PeakFootprintBytes)
	}
}

func TestProcessSamplerResourcesUnavailable(t *testing.T) {
	for _, state := range []*os.ProcessState{nil, {}} {
		usage := issue767FinalProcessResources(state)
		if usage.UserCPUNS != nil || usage.SystemCPUNS != nil || usage.PeakRSSBytes != nil || len(usage.Unavailable) != 3 {
			t.Fatalf("missing process state or SysUsage produced measurements: %+v", usage)
		}
	}
	usage := issue767FinalProcessResources(nil)
	if usage.UserCPUNS != nil || usage.SystemCPUNS != nil || usage.PeakRSSBytes != nil {
		t.Fatalf("missing process state produced measured values: %+v", usage)
	}
	for _, field := range []string{"UserCPUNS", "SystemCPUNS", "PeakRSSBytes"} {
		if !strings.HasPrefix(usage.Unavailable[field], "unavailable: ") {
			t.Errorf("%s has no unavailable reason: %+v", field, usage)
		}
	}
	raw, err := json.Marshal(usage)
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]json.RawMessage
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"UserCPUNS", "SystemCPUNS", "PeakRSSBytes", "PeakFootprintBytes"} {
		if _, ok := record[field]; ok {
			t.Errorf("unavailable %s emitted a value: %s", field, raw)
		}
	}
}

func TestProcessSamplerResourcesRunReport(t *testing.T) {
	root := t.TempDir()
	usage := issue767FinalProcessResources(nil)
	usage.Run, usage.PID = 1, 12345
	user, system, rss := int64(200*time.Millisecond), int64(10*time.Millisecond), int64(64<<20)
	measured := issue767ProcessResources{Run: 2, PID: 12346, UserCPUNS: &user, SystemCPUNS: &system, PeakRSSBytes: &rss}
	if runtime.GOOS == "darwin" {
		footprint := uint64(70 << 20)
		measured.PeakFootprintBytes = &footprint
	}
	f := &issue767Fixture{t: t, root: root, run: 2, resources: map[int]issue767ProcessResources{1: usage, 2: measured}}
	run := &sustainedIORun{t: t, f: f, artifactDir: t.TempDir(), sampler: newSustainedIOSampler(nil, time.Second)}
	run.finish(sustainedIOManifest{RunID: "resources"})
	var report struct {
		Resources []issue767ProcessResources `json:"process_resources"`
	}
	raw, err := os.ReadFile(filepath.Join(run.artifactDir, "report.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &report); err != nil {
		t.Fatal(err)
	}
	if len(report.Resources) != 2 || report.Resources[0].PID != usage.PID || len(report.Resources[0].Unavailable) != 3 {
		t.Fatalf("run report lost process resources: %s", raw)
	}
	got := report.Resources[1]
	if got.PID != measured.PID || got.UserCPUNS == nil || *got.UserCPUNS != user || got.SystemCPUNS == nil || *got.SystemCPUNS != system || got.PeakRSSBytes == nil || *got.PeakRSSBytes != rss {
		t.Fatalf("run report lost measured values: %s", raw)
	}
	if runtime.GOOS == "darwin" && (got.PeakFootprintBytes == nil || *got.PeakFootprintBytes != *measured.PeakFootprintBytes) {
		t.Fatalf("run report lost the separate physical-footprint high-water mark: %s", raw)
	}
}
