package goanalysis

import (
	"context"
	"encoding/json"
	"errors"
	"go/token"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/semantic"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/tools/go/packages"
)

type preparedBindingFailure struct {
	*startupBindingStore
	err error
}

var _ graph.SemanticBindingTypeWriter = (*preparedBindingFailure)(nil)

func (s *preparedBindingFailure) ReplaceSemanticBindingTypesForFiles(string, []string, []graph.SemanticBindingType) error {
	return s.err
}

func TestStartupFileExportPreparation(t *testing.T) {
	t.Run("preparation deadline includes admission", func(t *testing.T) {
		root := typecheckCacheFixture(t)
		synctest.Test(t, func(t *testing.T) {
			p := newTestProvider(t)
			end := p.BeginStartupWarmup()
			defer end()
			held, err := p.acquireHeavy(t.Context(), false)
			require.NoError(t, err)
			p.packagesLoad = func(*packages.Config, ...string) ([]*packages.Package, error) {
				t.Fatal("expired queued preparation dispatched")
				return nil, nil
			}
			m := semantic.NewManager(semantic.Config{Enabled: true, TimeoutSeconds: 2}, zap.NewNop())
			m.RegisterProvider(p)
			defer m.Close()
			started := time.Now()
			_, err = m.EnrichStartupFilesContext(t.Context(), graph.New(), "", root, "go", []string{"api/api.go"})
			require.ErrorIs(t, err, context.DeadlineExceeded)
			require.Equal(t, 480*time.Second, time.Since(started))
			held()
			next, err := p.acquireHeavy(t.Context(), false)
			require.NoError(t, err)
			next()
		})
	})
	t.Run("source dependency closure needs no exports", func(t *testing.T) {
		t.Setenv("GORTEX_GOTYPES_NEEDDEPS", "1")
		p := newTestProvider(t)
		defer p.Close()
		p.packagesLoad = func(*packages.Config, ...string) ([]*packages.Package, error) {
			t.Fatal("source dependency closure must not prepare unused exports")
			return nil, nil
		}
		require.NoError(t, p.PrepareFileExportsContext(t.Context(), "", typecheckCacheFixture(t), []string{"api/api.go"}))
	})
	t.Run("two repositories keep admission through binding", func(t *testing.T) {
		roots := []string{typecheckCacheFixture(t), typecheckCacheFixture(t)}
		synctest.Test(t, func(t *testing.T) {
			p := newTestProvider(t)
			p.compilerGC = func() {}
			end := p.BeginStartupWarmup()
			defer end()
			entered, release := make(chan struct{}), make(chan struct{})
			var bound atomic.Int32
			p.packagesLoad = func(cfg *packages.Config, _ ...string) ([]*packages.Package, error) {
				if cfg.Mode&packages.NeedTypes != 0 {
					deadline, ok := cfg.Context.Deadline()
					require.True(t, ok)
					require.Equal(t, 2*time.Second, time.Until(deadline))
					bound.Add(1)
					return startupFakeMetadata(cfg), nil
				}
				if cfg.Dir == roots[0] {
					close(entered)
					select {
					case <-release:
					case <-cfg.Context.Done():
					}
				} else {
					<-time.After(3 * time.Second)
				}
				return startupFakeMetadata(cfg), nil
			}
			m := semantic.NewManager(semantic.Config{Enabled: true, TimeoutSeconds: 2}, zap.NewNop())
			m.RegisterProvider(p)
			defer m.Close()
			done := make(chan error, 2)
			start := func(root string) {
				_, err := m.EnrichStartupFilesContext(t.Context(), graph.New(), "", root, "go", []string{"api/api.go"})
				done <- err
			}
			go start(roots[0])
			<-entered
			go start(roots[1])
			synctest.Wait()
			close(release)
			for range 2 {
				require.NoError(t, <-done)
			}
			require.EqualValues(t, 2, bound.Load())
		})
	})
	t.Run("driver dispatch is debug", func(t *testing.T) {
		p := newTestProvider(t)
		defer p.Close()
		core, logs := observer.New(zap.DebugLevel)
		p.logger = zap.New(core)
		p.packagesLoad = func(*packages.Config, ...string) ([]*packages.Package, error) { return nil, nil }
		_, err := p.loadCompilerPackages(&packages.Config{Dir: t.TempDir()}, "./api")
		require.NoError(t, err)
		entries := logs.FilterMessage("go-types: package driver dispatch").All()
		require.Len(t, entries, 1)
		require.Equal(t, zap.DebugLevel, entries[0].Level)
		require.NotContains(t, entries[0].ContextMap(), "stage", "generic listings are not startup preparation")
	})
	t.Run("invalid inherited parallelism", func(t *testing.T) {
		root := typecheckCacheFixture(t)
		t.Setenv("GOFLAGS", "-p=invalid")
		p := newTestProvider(t)
		defer p.Close()
		require.Error(t, p.PrepareFileExportsContext(t.Context(), "", root, []string{"api/api.go"}))
	})
	t.Run("binding store error", func(t *testing.T) {
		root := typecheckCacheFixture(t)
		p := newTestProvider(t)
		defer p.Close()
		failure := errors.New("binding store failed")
		require.NoError(t, p.PrepareFileExportsContext(t.Context(), "", root, []string{"api/api.go"}))
		result, err := p.EnrichFilesContext(t.Context(), &preparedBindingFailure{startupBindingStore: &startupBindingStore{Store: graph.New()}, err: failure}, "", root, []string{"api/api.go"})
		require.ErrorIs(t, err, failure)
		require.Nil(t, result)
	})
	t.Run("native preparation chunks", func(t *testing.T) {
		root := typecheckCacheFixture(t)
		p := newTestProvider(t)
		defer p.Close()
		core, logs := observer.New(zap.InfoLevel)
		p.logger = zap.New(core)
		var files []string
		for i := 0; i < 300; i++ {
			files = append(files, strings.Repeat("x", 80)+strconv.Itoa(i)+"/a.go")
		}
		p.packagesLoad = func(*packages.Config, ...string) ([]*packages.Package, error) { return nil, nil }
		require.NoError(t, p.PrepareFileExportsContext(t.Context(), "", root, files))
		entries := logs.FilterMessage("go-types: package load uses native driver chunks").All()
		require.Len(t, entries, 2)
		for _, entry := range entries {
			require.Greater(t, entry.ContextMap()["driver_chunks"], int64(1))
		}
	})
	t.Run("native cold and warm cache", startupPreparedCgo)
	t.Run("canonical cache input parity", startupPreparationInputs)
	t.Run("six special directories and follow-on changes", startupPreparationSpecialFiles)
	t.Run("cross module admission", startupPreparationAdmission)
	t.Run("preempts and joins listing", startupPreparationPreemption)
	t.Run("joined owned compiler", startupPreparationOwnedChild)
	t.Run("cancel before admission and nominal success", func(t *testing.T) {
		root := typecheckCacheFixture(t)
		synctest.Test(t, func(t *testing.T) {
			p := newTestProvider(t)
			defer p.Close()
			end := p.BeginStartupWarmup()
			defer end()
			held, err := p.acquireHeavy(t.Context(), false)
			require.NoError(t, err)
			ctx, cancel := context.WithCancel(t.Context())
			var calls atomic.Int32
			p.packagesLoad = func(*packages.Config, ...string) ([]*packages.Package, error) { calls.Add(1); return nil, nil }
			done := make(chan error, 1)
			go func() { done <- p.PrepareFileExportsContext(ctx, "", root, []string{"api/api.go"}) }()
			synctest.Wait()
			cancel()
			held()
			require.ErrorIs(t, <-done, context.Canceled)
			require.Zero(t, calls.Load())
			ctx, cancel = context.WithCancel(t.Context())
			p.packagesLoad = func(*packages.Config, ...string) ([]*packages.Package, error) { cancel(); return nil, nil }
			require.ErrorIs(t, p.PrepareFileExportsContext(ctx, "", root, []string{"api/api.go"}), context.Canceled)
			next, err := p.acquireHeavy(t.Context(), false)
			require.NoError(t, err)
			next()
		})
	})
	t.Run("top level ordinary and fallback errors", func(t *testing.T) {
		for _, fallback := range []bool{false, true} {
			root := typecheckCacheFixture(t)
			p := newTestProvider(t)
			defer p.Close()
			failure := errors.New("native driver failed")
			p.packagesLoad = func(_ *packages.Config, patterns ...string) ([]*packages.Package, error) {
				if fallback && !strings.HasPrefix(patterns[0], "file=") {
					return nil, nil
				}
				return nil, failure
			}
			require.ErrorIs(t, p.PrepareFileExportsContext(t.Context(), "", root, []string{"api/api.go"}), failure)
		}
	})
}

func startupPreparationInputs(t *testing.T) {
	root := typecheckCacheFixture(t)
	link := filepath.Join(resolvedTempDir(t), "link")
	if err := os.Symlink(root, link); err != nil {
		t.Skipf("symlink fixture unavailable: %v", err)
	}
	for _, procs := range []string{"1", "2"} {
		t.Run(procs, func(t *testing.T) {
			t.Setenv("GOMAXPROCS", procs)
			t.Setenv("GOFLAGS", "-tags=startup_tag -mod=readonly")
			t.Setenv("CGO_ENABLED", "1")
			p := newTestProvider(t)
			defer p.Close()
			p.includeTest = true
			var configs []*packages.Config
			var queries [][]string
			p.packagesLoad = func(cfg *packages.Config, patterns ...string) ([]*packages.Package, error) {
				copy := *cfg
				configs = append(configs, &copy)
				queries = append(queries, slices.Clone(patterns))
				return startupFakeMetadata(cfg), nil
			}
			overlay := map[string][]byte{filepath.Join(root, "api/api.go"): []byte("package api\n")}
			ctx := semantic.WithCheckoutCompilerScope(t.Context(), semantic.CheckoutCompilerScope{Committed: true, GoWorkOff: true, Overlay: overlay})
			originalOverlay := slices.Clone(overlay[filepath.Join(root, "api/api.go")])
			files := []string{"r/api/api.go", "r/api/./api.go", "r/api/api.go"}
			original := slices.Clone(files)
			require.NoError(t, p.PrepareFileExportsContext(ctx, "r", link, files))
			_, err := p.EnrichFilesContext(ctx, graph.New(), "r", link, files)
			require.NoError(t, err)
			require.Equal(t, originalOverlay, overlay[filepath.Join(root, "api/api.go")])
			require.Equal(t, original, files)
			require.Len(t, configs, 2)
			require.Equal(t, queries[0], queries[1])
			require.Equal(t, []string{"./api"}, queries[0])
			require.Equal(t, configs[0].Dir, configs[1].Dir)
			require.Equal(t, configs[0].BuildFlags, configs[1].BuildFlags)
			require.Equal(t, configs[0].Env, configs[1].Env)
			require.Equal(t, configs[0].Overlay, configs[1].Overlay)
			require.Equal(t, configs[0].Tests, configs[1].Tests)
			require.Equal(t, procs, startupEnvValue(configs[0].Env, "GOMAXPROCS"))
			require.Equal(t, "-tags=startup_tag -mod=readonly", startupEnvValue(configs[0].Env, "GOFLAGS"))
			require.Zero(t, configs[0].Mode&(packages.NeedTypes|packages.NeedTypesInfo|packages.NeedSyntax))
			require.NotZero(t, configs[0].Mode&packages.NeedExportFile)
			require.NotContains(t, configs[0].BuildFlags, "-trimpath")
		})
	}
}

func startupPreparedCgo(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Cleanup(func() {
		files, err := filepath.Glob(filepath.Join(home, "prepared-*.json"))
		require.NoError(t, err)
		require.Empty(t, files, "native test must not write evidence into HOME")
	})
	root := resolvedTempDir(t)
	writeGoMod(t, root, "example.com/prepared")
	writeFile(t, root, "api/api.go", "package api\n/*\nint answer(void) { return 42; }\n*/\nimport \"C\"\ntype Number int\nvar Value = Number(C.answer())\n")
	writeFile(t, root, "api/sibling.go", "package api\nvar Sibling = Number(2)\n")
	writeFile(t, root, "api/external_test.go", "package api_test\nimport \"example.com/prepared/api\"\nvar External=api.Number(1)\n")
	log := startupDriverLog(t)
	t.Setenv("GOCACHE", resolvedTempDir(t))
	cc, err := exec.LookPath("cc")
	require.NoError(t, err)
	binary, err := os.Executable()
	require.NoError(t, err)
	tools := resolvedTempDir(t)
	prefix := "#!/bin/sh\nexec \"" + binary + "\" -test.run='^TestStartupGoDriverHelper$' -- "
	tool := filepath.Join(tools, "tool")
	require.NoError(t, os.WriteFile(tool, []byte(prefix+"startup-tool \"$@\"\n"), 0700))
	ccShim := filepath.Join(tools, "cc")
	require.NoError(t, os.WriteFile(ccShim, []byte(prefix+"startup-cc \""+cc+"\" \"$@\"\n"), 0700))
	t.Setenv("CC", ccShim)
	p := newTestProvider(t)
	defer p.Close()
	p.includeTest = true
	var membership []startupPackageMembership
	p.packagesLoad = func(cfg *packages.Config, patterns ...string) ([]*packages.Package, error) {
		cfg.BuildFlags = append(slices.Clone(cfg.BuildFlags), "-toolexec="+tool)
		pkgs, err := packages.Load(cfg, patterns...)
		if cfg.Mode&packages.NeedTypes != 0 {
			membership = startupMembership(pkgs, cfg.Fset, root)
		}
		return pkgs, err
	}
	evidence := t.TempDir()
	var rows []graph.SemanticBindingType
	var members []startupPackageMembership
	for _, phase := range []string{"cold prepare", "cold bind", "warm prepare", "warm bind"} {
		require.NoError(t, os.WriteFile(log, nil, 0600))
		store := &startupBindingStore{Store: graph.New()}
		if strings.HasSuffix(phase, "prepare") {
			require.NoError(t, p.PrepareFileExportsContext(t.Context(), "", root, []string{"api/api.go", "api/external_test.go"}))
		} else {
			_, err := p.EnrichFilesContext(t.Context(), store, "", root, []string{"api/api.go", "api/external_test.go"})
			require.NoError(t, err)
			require.NotEmpty(t, store.rows)
			if rows == nil {
				rows = slices.Clone(store.rows)
				members = membership
			} else {
				require.Equal(t, rows, store.rows)
				require.Equal(t, members, membership)
			}
		}
		records := startupDriverRecords(t, log)
		data, err := json.Marshal(records)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(evidence, "prepared-"+strings.ReplaceAll(phase, " ", "-")+".json"), data, 0600))
		compile, active, peak, drivers := 0, 0, 0, 0
		started := map[int]bool{}
		for _, record := range records {
			expensive := record.Kind == "go" && slices.Contains(record.Args, "-export=true")
			if record.Event == "start" {
				started[record.PID] = true
				if expensive {
					drivers++
					active++
					peak = max(peak, active)
					require.Contains(t, record.Args, "-p=1")
				}
				if record.Kind == "tool" && filepath.Base(record.Command) == "compile" && !slices.Contains(record.Args, "-V=full") {
					compile++
				}
			} else {
				delete(started, record.PID)
				if expensive {
					active--
				}
			}
		}
		require.Empty(t, started)
		require.Zero(t, active)
		require.Equal(t, 1, peak)
		require.Equal(t, 1, drivers)
		if phase == "cold prepare" {
			require.Positive(t, compile)
		} else {
			require.Zero(t, compile, "binding and retained preparation must reuse exports")
		}
	}
	baseSet := token.NewFileSet()
	basePkgs, err := packages.Load(&packages.Config{Context: t.Context(), Dir: root, Tests: true, Fset: baseSet, Mode: packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles | packages.NeedImports | packages.NeedTypes | packages.NeedTypesInfo | packages.NeedSyntax | packages.NeedModule}, "file="+filepath.Join(root, "api/api.go"), "file="+filepath.Join(root, "api/external_test.go"))
	require.NoError(t, err)
	require.Equal(t, buildSemanticBindingTypes(basePkgs, baseSet, root, ""), rows)
	loaded, loadedSet, err := p.loadPackagesContext(t.Context(), root, "file="+filepath.Join(root, "api/api.go"), "file="+filepath.Join(root, "api/external_test.go"))
	require.NoError(t, err)
	require.Equal(t, startupMembership(basePkgs, baseSet, root), startupMembership(loaded, loadedSet, root))
	// A changed input is not hidden by any retained preparation flag.
	writeFile(t, root, "api/sibling.go", "package api\ntype Text string\nvar Sibling = Text(\"changed\")\n")
	store := &startupBindingStore{Store: graph.New()}
	_, err = p.EnrichFilesContext(t.Context(), store, "", root, []string{"api/api.go", "api/external_test.go"})
	require.NoError(t, err)
	var sibling []graph.SemanticBindingType
	for _, row := range store.rows {
		if row.Site.Name == "Sibling" {
			sibling = append(sibling, row)
		}
	}
	require.Equal(t, []graph.SemanticBindingType{{Site: graph.SemanticBindingSite{FilePath: "api/sibling.go", Line: 3, Name: "Sibling"}, TypeName: "Text"}}, sibling)
}

func startupPreparationSpecialFiles(t *testing.T) {
	root := typecheckCacheFixture(t)
	files := []string{"api/api.go"}
	for _, name := range []string{"excluded1", "excluded2", "excluded3", "excluded4"} {
		file := name + "/a.go"
		writeFile(t, root, file, "//go:build special_tag\n\npackage special\ntype Value int\nvar V=Value(1)\n")
		files = append(files, file)
	}
	for _, name := range []string{"nested1", "nested2"} {
		writeGoMod(t, filepath.Join(root, name), "example.com/"+name)
		file := name + "/a.go"
		writeFile(t, root, file, "package special\ntype Value int\nvar V=Value(1)\n")
		files = append(files, file)
	}
	p := newTestProvider(t)
	defer p.Close()
	for _, event := range []string{"initial", "reversed aliases", "tag included", "removed", "readded"} {
		if event == "tag included" {
			t.Setenv("GOFLAGS", "-tags=special_tag -p=1")
		}
		if event == "removed" {
			require.NoError(t, os.Remove(filepath.Join(root, "nested1/a.go")))
			files = slices.DeleteFunc(files, func(f string) bool { return f == "nested1/a.go" })
		}
		if event == "readded" {
			writeFile(t, root, "nested1/a.go", "package special\ntype Value int\nvar V=Value(2)\n")
			files = append(files, "nested1/a.go")
		}
		inputs := slices.Clone(files)
		if event == "reversed aliases" {
			slices.Reverse(inputs)
			inputs = append(inputs, "api/./api.go")
		}
		_, _, _, patterns, dir, err := fileCompilerInputs("", root, inputs)
		require.NoError(t, err)
		baseSet := token.NewFileSet()
		basePkgs, err := loadFilePackages(&packages.Config{Context: t.Context(), Dir: dir, Fset: baseSet, Mode: packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles | packages.NeedImports | packages.NeedTypes | packages.NeedTypesInfo | packages.NeedSyntax | packages.NeedModule}, packages.Load, patterns)
		require.NoError(t, err)
		want := buildSemanticBindingTypes(basePkgs, baseSet, root, "")
		require.NoError(t, p.PrepareFileExportsContext(t.Context(), "", root, inputs))
		store := &startupBindingStore{Store: graph.New()}
		_, err = p.EnrichFilesContext(t.Context(), store, "", root, inputs)
		require.NoError(t, err)
		require.Equal(t, want, store.rows, event)
	}
	for _, file := range []string{"nested1/a.go", "excluded1/a.go"} {
		require.NoError(t, p.PrepareFileExportsContext(t.Context(), "", root, []string{file}))
		store := &startupBindingStore{Store: graph.New()}
		_, err := p.EnrichFilesContext(t.Context(), store, "", root, []string{file})
		require.NoError(t, err)
		require.NotEmpty(t, store.rows)
	}
}

func startupPreparationAdmission(t *testing.T) {
	routes := []string{"full", "checkout", "committed", "background", "targeted"}
	roots := []string{typecheckCacheFixture(t)}
	for range routes {
		roots = append(roots, typecheckCacheFixture(t))
	}
	synctest.Test(t, func(t *testing.T) {
		p := newTestProvider(t)
		defer p.Close()
		p.heavyGate = make(chan struct{}, 2)
		p.compilerGC = func() {}
		end := p.BeginStartupWarmup()
		defer end()
		release := make(chan struct{})
		var active, peak atomic.Int32
		p.packagesLoad = func(cfg *packages.Config, _ ...string) ([]*packages.Package, error) {
			if cfg.Mode&(packages.NeedTypes|packages.NeedExportFile) == 0 {
				return startupFakeMetadata(cfg), nil
			}
			n := active.Add(1)
			peak.Store(max(peak.Load(), n))
			select {
			case <-release:
			case <-cfg.Context.Done():
			}
			active.Add(-1)
			return nil, cfg.Context.Err()
		}
		done := make(chan error, len(routes)+1)
		go func() { done <- p.PrepareFileExportsContext(t.Context(), "", roots[0], []string{"api/api.go"}) }()
		synctest.Wait()
		for i, route := range routes {
			go func() { done <- startupRunRoute(t.Context(), p, route, roots[i+1]) }()
		}
		synctest.Wait()
		require.EqualValues(t, 1, active.Load())
		close(release)
		for range len(routes) + 1 {
			require.NoError(t, <-done)
		}
		require.EqualValues(t, 1, peak.Load())
	})
}

func startupPreparationOwnedChild(t *testing.T) {
	for _, mode := range []string{"cancel", "close", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			root := typecheckCacheFixture(t)
			log := startupDriverLog(t)
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			defer listener.Close()
			require.NoError(t, listener.(*net.TCPListener).SetDeadline(time.Now().Add(10*time.Second)))
			t.Setenv("STARTUP_DRIVER_CONTROL", listener.Addr().String())
			p := newTestProvider(t)
			defer p.Close()
			end := p.BeginStartupWarmup()
			defer end()
			ctx, cancel := context.WithCancel(t.Context())
			if mode == "deadline" {
				cancel()
				ctx, cancel = context.WithTimeout(t.Context(), 2*time.Second)
			}
			m := semantic.NewManager(semantic.Config{Enabled: true, TimeoutSeconds: 120}, p.logger)
			m.RegisterProvider(p)
			defer m.Close()
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := m.EnrichStartupFilesContext(ctx, graph.New(), "", root, "go", []string{"api/api.go"})
				done <- err
			}()
			conn, err := listener.Accept()
			if err != nil {
				cancel()
				require.Error(t, <-done)
				require.NoError(t, err)
			}
			defer conn.Close()
			require.NoError(t, conn.SetDeadline(time.Now().Add(10*time.Second)))
			var ready [1]byte
			_, err = io.ReadFull(conn, ready[:])
			require.NoError(t, err)
			switch mode {
			case "close":
				require.NoError(t, m.Close())
			case "cancel":
				cancel()
			}
			select {
			case err := <-done:
				if mode == "deadline" {
					require.ErrorIs(t, err, context.DeadlineExceeded)
				} else {
					require.ErrorIs(t, err, context.Canceled)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("preparation failed to join owned compiler")
			}
			started := map[int]bool{}
			for _, record := range startupDriverRecords(t, log) {
				if record.Event == "start" {
					started[record.PID] = true
				} else {
					delete(started, record.PID)
				}
			}
			require.Empty(t, started)
			if mode != "close" {
				next, err := p.acquireHeavy(t.Context(), false)
				require.NoError(t, err)
				next()
			}
		})
	}
}

func startupPreparationPreemption(t *testing.T) {
	roots := []string{typecheckCacheFixture(t), typecheckCacheFixture(t)}
	synctest.Test(t, func(t *testing.T) {
		p := newTestProvider(t)
		p.heavyGate = make(chan struct{}, 2)
		t.Cleanup(func() { require.NoError(t, p.Close()) })
		end := p.BeginStartupWarmup()
		defer end()
		bgCtx, cancel := context.WithCancel(t.Context())
		defer cancel()
		e := &checkoutWarmup{loadDir: roots[0], cancel: cancel}
		p.warm.entries = map[string]*checkoutWarmup{roots[0]: e}
		p.warm.active = e
		cleanup := make(chan struct{})
		childJoined := make(chan struct{})
		var foreground, early atomic.Bool
		p.packagesLoad = func(cfg *packages.Config, _ ...string) ([]*packages.Package, error) {
			if cfg.Mode&(packages.NeedTypes|packages.NeedExportFile) == 0 {
				return startupFakeMetadata(cfg), nil
			}
			if cfg.Dir == roots[0] {
				<-cfg.Context.Done()
				<-cleanup
				close(childJoined)
				return nil, cfg.Context.Err()
			}
			foreground.Store(true)
			select {
			case <-childJoined:
			default:
				early.Store(true)
			}
			return nil, nil
		}
		background := make(chan error, 1)
		go func() {
			_, err := p.runListing(bgCtx, roots[0], []string{"./api"}, warmupBuildFlags())
			background <- err
		}()
		synctest.Wait()
		front := make(chan error, 1)
		go func() { front <- p.PrepareFileExportsContext(t.Context(), "", roots[1], []string{"api/api.go"}) }()
		synctest.Wait()
		cancelled := bgCtx.Err() != nil
		ranBeforeJoin := foreground.Load()
		// Complete cleanup even on the flags-only/base behavior so the failed
		// assertion cannot strand the fixture goroutines.
		cancel()
		close(cleanup)
		require.ErrorIs(t, <-background, context.Canceled)
		require.NoError(t, <-front)
		require.True(t, cancelled, "foreground must preempt the different module's listing")
		require.False(t, ranBeforeJoin, "foreground must wait for owned child cleanup")
		require.False(t, early.Load())
		require.True(t, foreground.Load())
	})
}
