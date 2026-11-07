package storetest

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// K14 No-change restart (design-c.md 1.8, FR-5). Given complete F with
// unchanged bytes, recipes and dependency selections, when restarting, then
// routes and answers remain unchanged, with zero rebuild jobs and at most
// 1 MiB written.
//
// The measured restart is a fresh child process: the test binary run again
// with the K14 environment set, so RunStorageContract runs only the child.
// The child opens the store over files a previous process closed cleanly,
// waits until it reports no pending work, closes it, and reports the rebuild
// jobs and its own wchar from /proc/self/io, read when it starts and again
// after the close. wchar counts every byte the process passed to write and
// similar calls, to any file. It needs Linux /proc; elsewhere the byte count
// and its counter subcase skip (review rule T5) and the rebuild count still
// runs. Writes through a writable memory map, such as SQLite's -shm index,
// and writes by other processes are not counted (plan risk R14).

const (
	k14DirEnv    = "STORETEST_K14_DIR"
	k14ResultEnv = "STORETEST_K14_RESULT"
	// k14WriterEnv names an existing file the counter writer rewrites.
	k14WriterEnv = "STORETEST_K14_WRITER"
	// k14WriteLimit is the most a no-change restart may write.
	k14WriteLimit = 1 << 20
	// k14WriterBytes is what the counter writer writes twice: over the
	// limit each time.
	k14WriterBytes = k14WriteLimit + k14WriteLimit/2
	procSelfIO     = "/proc/self/io"
)

// k14Result is what a child reports.
type k14Result struct {
	ProcIO   bool
	Written  int64
	Rebuilds int
	Err      string
}

// k14Check is K14's verdict on one measured restart.
func k14Check(r k14Result) error {
	var problems []error
	if r.Err != "" {
		problems = append(problems, fmt.Errorf("the restart failed: %s", r.Err))
	}
	if r.Rebuilds != 0 {
		problems = append(problems, fmt.Errorf("the restart started %d rebuild jobs, want 0", r.Rebuilds))
	}
	if r.ProcIO && r.Written > k14WriteLimit {
		problems = append(problems, fmt.Errorf("the restart wrote %d bytes, want at most %d", r.Written, k14WriteLimit))
	}
	return errors.Join(problems...)
}

// k14IsChild reports whether this process is a K14 child.
func k14IsChild() bool { return os.Getenv(k14DirEnv) != "" }

// k14RunChild is the measured restart, run in the child process.
func k14RunChild(t *testing.T, backend Backend) {
	result := k14Result{}
	start, procIO, err := readWchar()
	if err != nil {
		result.Err = err.Error()
	}
	if result.Err == "" {
		result.ProcIO = procIO
		ctx := caseContext(t)
		inst := backend.Open(t, os.Getenv(k14DirEnv), nil)
		rebuilds, settleErr := inst.Settle(ctx)
		result.Rebuilds = rebuilds
		var writeErr error
		if path := os.Getenv(k14WriterEnv); path != "" {
			writeErr = k14CounterWriter(path)
		}
		if err := errors.Join(settleErr, writeErr, inst.Close()); err != nil {
			result.Err = err.Error()
		}
		end, _, err := readWchar()
		if err != nil {
			result.Err = err.Error()
		}
		result.Written = end - start
	}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("encode the K14 result: %v", err)
	}
	if err := os.WriteFile(os.Getenv(k14ResultEnv), data, 0o644); err != nil {
		t.Fatalf("write the K14 result: %v", err)
	}
}

// k14CounterWriter rewrites path with its own bytes and creates, syncs and
// deletes a scratch file of the same size beside it.
func k14CounterWriter(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return err
	}
	scratch, err := os.Create(path + ".scratch")
	if err != nil {
		return err
	}
	_, writeErr := scratch.Write(make([]byte, k14WriterBytes))
	return errors.Join(writeErr, scratch.Sync(), scratch.Close(), os.Remove(scratch.Name()))
}

// readWchar reads this process's wchar. ok is false when the platform has
// no /proc/self/io.
func readWchar() (int64, bool, error) {
	data, err := os.ReadFile(procSelfIO)
	if errors.Is(err, os.ErrNotExist) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		if value, found := strings.CutPrefix(scanner.Text(), "wchar:"); found {
			n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
			return n, true, err
		}
	}
	return 0, false, fmt.Errorf("%s has no wchar line", procSelfIO)
}

// k14Restart runs one measured restart of dir in a child process. contract
// is the name of the test that runs RunStorageContract.
func k14Restart(t *testing.T, contract, dir, writer string) k14Result {
	t.Helper()
	var pattern []string
	for _, part := range strings.Split(contract, "/") {
		pattern = append(pattern, "^"+regexp.QuoteMeta(part)+"$")
	}
	resultPath := filepath.Join(t.TempDir(), "k14.json")
	cmd := exec.CommandContext(caseContext(t), os.Args[0], "-test.run="+strings.Join(pattern, "/"), "-test.count=1")
	cmd.Env = append(os.Environ(), k14DirEnv+"="+dir, k14ResultEnv+"="+resultPath, k14WriterEnv+"="+writer)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the K14 child failed: %v\n%s", err, out)
	}
	data, err := os.ReadFile(resultPath)
	if err != nil {
		t.Fatalf("the K14 child wrote no result: %v", err)
	}
	var result k14Result
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("decode the K14 result: %v", err)
	}
	return result
}

// k14Published publishes every checkout of F at E0 and R1 at E1 in a new
// directory, closes the instance cleanly, and returns the publications.
func k14Published(t *testing.T, backend Backend) (string, *FixtureF, map[string]Publication) {
	t.Helper()
	ctx := caseContext(t)
	f := NewFixtureF(t)
	dir := t.TempDir()
	inst := newChainIn(t, ctx, backend, dir, nil, f, CheckoutR1, CheckoutR2, CheckoutS1)
	for _, c := range f.Checkouts {
		publishVersion(t, ctx, inst, f, c.ID, E0)
	}
	publishVersion(t, ctx, inst, f, CheckoutR1, E1)
	routes := map[string]Publication{}
	for _, c := range f.Checkouts {
		routes[c.ID] = current(t, ctx, inst, c.ID)
	}
	if err := inst.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return dir, f, routes
}

// testK14NoChangeRestart restarts F in a child process: no rebuild job, and
// on Linux at most 1 MiB written; then routes and answers are unchanged.
func testK14NoChangeRestart(t *testing.T, backend Backend, contract string) {
	dir, f, routes := k14Published(t, backend)
	result := k14Restart(t, contract, dir, "")
	if err := k14Check(result); err != nil {
		t.Errorf("no-change restart: %v", err)
	}
	ctx := caseContext(t)
	inst := backend.Open(t, dir, nil)
	for _, c := range f.Checkouts {
		if got := current(t, ctx, inst, c.ID); got != routes[c.ID] {
			t.Fatalf("%s: the restart moved the publication from %+v to %+v", c.ID, routes[c.ID], got)
		}
	}
	assertServesF(t, ctx, openView(t, ctx, inst, CheckoutR1), f, CheckoutR1, E1)
	assertServesF(t, ctx, openView(t, ctx, inst, CheckoutR2), f, CheckoutR2, E0)
	assertServesF(t, ctx, openView(t, ctx, inst, CheckoutS1), f, CheckoutS1, E0)
	if !result.ProcIO {
		t.Skipf("no %s on this platform: the write count runs on Linux (review rule T5)", procSelfIO)
	}
}

// testK14CounterSeesWrites is the permanent counter subcase: during the
// measured restart a test writer rewrites an existing file of more than
// 1 MiB with identical bytes and creates, syncs and deletes a scratch file of
// the same size. K14's check must report the restart as over its limit.
func testK14CounterSeesWrites(t *testing.T, backend Backend, contract string) {
	if _, err := os.Stat(procSelfIO); errors.Is(err, os.ErrNotExist) {
		t.Skipf("no %s on this platform: the write count runs on Linux (review rule T5)", procSelfIO)
	}
	dir, _, _ := k14Published(t, backend)
	existing := filepath.Join(t.TempDir(), "existing.bin")
	if err := os.WriteFile(existing, bytes.Repeat([]byte{0x5a}, k14WriterBytes), 0o644); err != nil {
		t.Fatalf("write the existing file: %v", err)
	}
	result := k14Restart(t, contract, dir, existing)
	if !result.ProcIO {
		t.Fatalf("the child read no %s although the parent has it", procSelfIO)
	}
	// Exactly the writer's bytes is valid: the store may write nothing.
	if !k14CountsWriterBytes(result.Written) {
		t.Errorf("the child counted %d bytes; the writer alone wrote %d", result.Written, 2*k14WriterBytes)
	}
	if k14Check(result) == nil {
		t.Errorf("K14's check passed a restart that wrote %d bytes", result.Written)
	}
}

func k14CountsWriterBytes(written int64) bool {
	return written >= 2*k14WriterBytes
}
