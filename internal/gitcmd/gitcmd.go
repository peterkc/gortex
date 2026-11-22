// Package gitcmd is the single chokepoint every git shell-out routes
// through. A repository scan can fan out dozens of `git` invocations at
// once (per-file blame, per-tag ls-tree, per-commit log); left
// unbounded they thrash the disk and starve CPU. A package-global
// weighted semaphore caps the number of concurrent git subprocesses so
// the rest of the indexer keeps making progress.
//
// The limiter is process-wide on purpose: it bounds the total git
// concurrency across every caller (churn, blame, releases, the index
// poller and git watcher), not per-package. Callers acquire a slot
// before spawning and release it when the subprocess exits.
//
// Run captures stdout and stderr separately and, on a non-nil exec
// error, wraps git's own stderr into the returned error so the failure
// reason survives. Callers that previously ignored git errors keep
// doing so by ignoring the returned err.
package gitcmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zzet/gortex/internal/platform"
	"golang.org/x/sync/semaphore"
)

var (
	semMu sync.Mutex
	// sem is the package-global limiter, swapped under semMu by
	// SetConcurrency. Its default weight is min(NumCPU, 8), or
	// GORTEX_GIT_CONCURRENCY.
	sem *semaphore.Weighted = semaphore.NewWeighted(defaultConcurrency())
)

// concurrencyEnv overrides the default limiter weight (a positive integer,
// capped at maxDefaultConcurrency*4).
const concurrencyEnv = "GORTEX_GIT_CONCURRENCY"

const maxDefaultConcurrency = 8

// defaultConcurrency returns the default semaphore weight: min(NumCPU, 8),
// unless GORTEX_GIT_CONCURRENCY names another positive weight.
//
// It follows the host's CPUs, not GOMAXPROCS: a git child is a separate
// process the kernel schedules on any CPU, and GOMAXPROCS bounds only this
// process's Go threads. A daemon run at GOMAXPROCS=1 used to get a weight of
// 1, so every git call in it — an edit's working-copy sample included —
// queued behind whichever other git child was running.
func defaultConcurrency() int64 {
	if raw := strings.TrimSpace(os.Getenv(concurrencyEnv)); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			return int64(min(n, maxDefaultConcurrency*4))
		}
	}
	return int64(max(1, min(runtime.NumCPU(), maxDefaultConcurrency)))
}

// SetConcurrency resizes the global git limiter, called once at
// daemon/CLI init. A value < 1 is clamped to 1. The swap is done under
// semMu; in-flight Run calls that already hold a slot are unaffected.
func SetConcurrency(n int) {
	if n < 1 {
		n = 1
	}
	semMu.Lock()
	sem = semaphore.NewWeighted(int64(n))
	semMu.Unlock()
}

// currentSem returns the live limiter under semMu so a concurrent
// SetConcurrency can swap the package var without racing the read.
func currentSem() *semaphore.Weighted {
	semMu.Lock()
	s := sem
	semMu.Unlock()
	return s
}

// Run acquires the global semaphore (ctx-cancellable), runs
// `git [-C dir] args...`, and on error wraps git's own stderr into the
// returned error. The acquire aborts before spawning the subprocess
// when ctx is already cancelled, returning ctx.Err().
//
// On a non-nil exec error, Run returns
// fmt.Errorf("git %s: %w: %s", args[0], err, bytes.TrimSpace(stderr)).
// When ctx has ended by the time git fails (as it has when its cancellation
// killed git), the error also wraps ctx.Err(). The captured stdout is always
// returned, even on error.
func Run(ctx context.Context, dir string, args ...string) ([]byte, error) {
	return run(ctx, dir, nil, nil, args...)
}

// RunIndexRefresh has Run's limiter, environment and output contract. On Unix,
// cancellation first lets Git remove its own index lock before bounded force
// termination. Windows retains the existing CommandContext termination policy.
func RunIndexRefresh(ctx context.Context, dir string, args ...string) ([]byte, error) {
	return run(ctx, dir, nil, configureIndexRefreshCancellation, args...)
}

// RunNoLazy has Run's semaphore, context, output, and error contract, but
// forces Git's local-only read environment. It is for plumbing that walks
// immutable object graphs where a promisor lookup must fail locally instead
// of fetching from a remote.
func RunNoLazy(ctx context.Context, dir string, args ...string) ([]byte, error) {
	return run(ctx, dir, noLazyGitEnv(), nil, args...)
}

// ErrOutputLimit says the command exceeded a caller's local capture bound.
var ErrOutputLimit = errors.New("git output exceeds capture limit")

// RunNoLazyBounded has RunNoLazy's local-only process and limiter. It caps
// stdout at maxStdout bytes and stderr at 16 KiB. If either overflows, no
// stdout is usable, even if Git exited successfully after writing it.
func RunNoLazyBounded(ctx context.Context, dir string, maxStdout int64, args ...string) ([]byte, error) {
	out, _, err := runNoLazyBounded(ctx, dir, maxStdout, nil, args...)
	return out, err
}

// RunNoLazyBoundedStdin runs the same bounded capture with caller-bounded
// stdin. It reports whether Git wrote to stderr, even with a zero exit status.
// Supply a context with a deadline to bound the process and semaphore wait.
func RunNoLazyBoundedStdin(ctx context.Context, dir string, stdin []byte, maxStdout int64, args ...string) (out []byte, wroteStderr bool, err error) {
	return runNoLazyBounded(ctx, dir, maxStdout, bytes.NewReader(stdin), args...)
}

func runNoLazyBounded(ctx context.Context, dir string, maxStdout int64, stdin io.Reader, args ...string) ([]byte, bool, error) {
	if maxStdout < 0 {
		return nil, false, ErrOutputLimit
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stdout := boundedOutput{remaining: maxStdout, cancel: cancel}
	const maxStderr = 16 << 10
	stderr := boundedOutput{remaining: maxStderr, cancel: cancel}
	out, err := run(ctx, dir, noLazyGitEnv(), func(cmd *exec.Cmd) {
		stdout.dst, stderr.dst = cmd.Stdout, cmd.Stderr
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if stdin != nil {
			cmd.Stdin = stdin
			cmd.WaitDelay = time.Second // Bound pipe teardown after cancellation or Git exit.
		}
	}, args...)
	wroteStderr := stderr.remaining != maxStderr || stderr.overflow
	if stdout.overflow || stderr.overflow {
		return nil, wroteStderr, ErrOutputLimit
	}
	return out, wroteStderr, err
}

type boundedOutput struct {
	dst       io.Writer
	remaining int64
	cancel    context.CancelFunc
	overflow  bool
}

func (w *boundedOutput) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		w.overflow = true
		w.cancel()
		return 0, ErrOutputLimit
	}
	n, err := w.dst.Write(p)
	w.remaining -= int64(n)
	return n, err
}

func run(ctx context.Context, dir string, env []string, configure func(*exec.Cmd), args ...string) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	// Abort before spawning if ctx is already cancelled.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s := currentSem()
	if err := s.Acquire(ctx, 1); err != nil {
		// ctx cancelled while (or before) waiting for a slot — no
		// subprocess was spawned.
		return nil, err
	}
	defer s.Release(1)

	full := args
	if dir != "" {
		full = append([]string{"-C", dir}, args...)
	}
	cmd := exec.CommandContext(ctx, "git", full...)
	if env != nil {
		cmd.Env = env
	}
	platform.ConfigureBackgroundCommand(cmd)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if configure != nil {
		configure(cmd)
	}
	if err := cmd.Run(); err != nil {
		name := "git"
		if len(args) > 0 {
			name = args[0]
		}
		// A child the context killed exits on the signal, and exec reports
		// that exit status, not ctx.Err(). Wrap both, so a caller can tell its
		// own cancellation from a git failure and still read the exit status.
		if ctxErr := ctx.Err(); ctxErr != nil && !errors.Is(err, ctxErr) {
			return stdout.Bytes(), fmt.Errorf("git %s: %w: %s (%w)", name, err, bytes.TrimSpace(stderr.Bytes()), ctxErr)
		}
		return stdout.Bytes(), fmt.Errorf("git %s: %w: %s", name, err, bytes.TrimSpace(stderr.Bytes()))
	}
	return stdout.Bytes(), nil
}

var fixedNoLazyGitEnv = []string{
	"GIT_NO_LAZY_FETCH=1",
	"GIT_TERMINAL_PROMPT=0",
	"GIT_OPTIONAL_LOCKS=0",
}

func noLazyGitEnv() []string {
	base := os.Environ()
	env := make([]string, 0, len(base)+len(fixedNoLazyGitEnv))
	for _, entry := range base {
		key, _, ok := strings.Cut(entry, "=")
		if ok && isFixedNoLazyGitEnvKey(key) {
			continue
		}
		env = append(env, entry)
	}
	return append(env, fixedNoLazyGitEnv...)
}

func isFixedNoLazyGitEnvKey(key string) bool {
	for _, entry := range fixedNoLazyGitEnv {
		fixedKey, _, _ := strings.Cut(entry, "=")
		if key == fixedKey {
			return true
		}
	}
	return false
}

// Output is the one-shot convenience: it runs Run and returns
// strings.TrimSpace(stdout) for callers that ignore stderr framing.
func Output(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := Run(ctx, dir, args...)
	return string(bytes.TrimSpace(out)), err
}
