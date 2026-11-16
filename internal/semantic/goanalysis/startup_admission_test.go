package goanalysis

import (
	"context"
	"go/types"
	"path/filepath"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"

	"github.com/zzet/gortex/internal/semantic"
)

func startupAdmissionFixture(t *testing.T) (string, []*packages.Package) {
	t.Helper()
	root := resolvedTempDir(t)
	writeGoMod(t, root, "example.com/admission")
	var roots []*packages.Package
	for _, name := range []string{"first", "second"} {
		writeFile(t, root, name+"/a.go", "package "+name+"\ntype Value struct{}\nvar Binding = Value{}\n")
		file := filepath.Join(root, name, "a.go")
		roots = append(roots, &packages.Package{
			ID: "example.com/admission/" + name, PkgPath: "example.com/admission/" + name,
			Name: name, Dir: filepath.Dir(file), GoFiles: []string{file}, CompiledGoFiles: []string{file},
			Module:     &packages.Module{Path: "example.com/admission", Main: true, GoVersion: "1.21"},
			TypesSizes: types.SizesFor("gc", "arm64"),
		})
	}
	return root, roots
}

func TestStartupGoCachedPassDoesNotWaitForQueuedTarget(t *testing.T) {
	root, roots := startupAdmissionFixture(t)
	synctest.Test(t, func(t *testing.T) {
		p := newTestProvider(t)
		p.compilerGC = func() {}
		t.Cleanup(func() { require.NoError(t, p.Close()) })
		end := p.BeginStartupWarmup()
		defer end()
		// Given a pass already holding the startup lease, before a forced touched
		// warm-up reaches its first of two targeted packages.
		lease, err := p.acquireHeavy(t.Context(), false)
		require.NoError(t, err)
		defer lease()
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		ctx = context.WithValue(ctx, startupCompilerAdmission{}, p)
		parent, stop := context.WithCancel(t.Context())
		p.warm.ctx, p.warm.stop = parent, stop
		activity := &fakeForeground{}
		activity.touch(root)
		activity.set("checkout_build")
		p.warm.activity = activity
		p.warm.maxDeferral = time.Second
		e := &checkoutWarmup{
			loadDir: root, want: goManifestDigest(root), running: true, planned: true,
			queue: []string{roots[0].PkgPath, roots[1].PkgPath}, targeted: 2,
			targetDirs:    map[string]string{roots[0].PkgPath: roots[0].Dir, roots[1].PkgPath: roots[1].Dir},
			pending:       map[string]bool{roots[0].Dir: true, roots[1].Dir: true},
			deferredSince: time.Now().Add(-2 * time.Second),
		}
		p.warm.entries = map[string]*checkoutWarmup{root: e}
		var calls atomic.Int32
		p.packagesLoad = func(cfg *packages.Config, _ ...string) ([]*packages.Package, error) {
			calls.Add(1)
			return roots[:1], nil
		}
		warmDone := make(chan struct{})
		go func() { p.runWarmup(e); close(warmDone) }()
		synctest.Wait()
		p.warm.mu.Lock()
		inflight, forced, touched := e.inflight[roots[0].Dir], e.status.Forced, e.status.Touched
		p.warm.mu.Unlock()
		require.True(t, inflight)
		require.True(t, forced)
		require.True(t, touched)
		require.Zero(t, calls.Load(), "warm-up is queued behind the pass's lease")
		plan := planHandleRoots(root, root, "", map[string]struct{}{"first/a.go": {}}, cachedScope)
		stats := &semantic.CompilerCacheStats{}
		// When the pass encounters a cold cached handle root, it must list under
		// its own lease, not wait for the warm-up that needs that same lease.
		program, err := p.loadCheckoutProgramCached(ctx, root, goManifestDigest(root), plan, cachedScope, stats)
		if program.release != nil {
			program.release()
		}
		stop()
		<-warmDone
		// Then it completes before its deadline with a real typechecked root.
		require.NoError(t, err)
		require.Empty(t, program.bypass)
		require.Len(t, program.program.pkgs, 1)
		require.NotNil(t, program.program.pkgs[0].Types.Scope().Lookup("Value"))
		require.Empty(t, stats.TargetedWait)
		require.EqualValues(t, 1, calls.Load())
	})
}

func TestStartupGoListingAdmissionDoesNotConsumeStallTime(t *testing.T) {
	root, roots := startupAdmissionFixture(t)
	synctest.Test(t, func(t *testing.T) {
		p := newTestProvider(t)
		t.Cleanup(func() { require.NoError(t, p.Close()) })
		end := p.BeginStartupWarmup()
		defer end()
		lease, err := p.acquireHeavy(t.Context(), false)
		require.NoError(t, err)
		p.warm.stall = time.Second
		parent, stop := context.WithCancel(t.Context())
		p.warm.ctx, p.warm.stop = parent, stop
		defer stop()
		e := &checkoutWarmup{loadDir: root, want: goManifestDigest(root), running: true, planned: true, queue: []string{roots[0].PkgPath}}
		p.warm.entries = map[string]*checkoutWarmup{root: e}
		var calls atomic.Int32
		dispatched := make(chan struct{})
		p.packagesLoad = func(cfg *packages.Config, _ ...string) ([]*packages.Package, error) {
			calls.Add(1)
			close(dispatched)
			<-cfg.Context.Done()
			return nil, cfg.Context.Err()
		}
		done := make(chan struct{})
		go func() { p.runWarmup(e); close(done) }()
		synctest.Wait()
		// Given a background batch queued behind an admitted committed pass.
		// When admission waits longer than the background stall interval.
		waited := make(chan struct{})
		time.AfterFunc(3*p.warm.stall, func() { close(waited) })
		<-waited
		synctest.Wait()
		p.warm.mu.Lock()
		escalations := e.status.Escalations
		p.warm.mu.Unlock()
		before := calls.Load()
		lease()
		<-dispatched
		stop()
		<-done
		// Then only running compilation can consume stall time or escalate.
		require.Zero(t, before)
		require.Zero(t, escalations)
		require.EqualValues(t, 1, calls.Load())
	})
}
