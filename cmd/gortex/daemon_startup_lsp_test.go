package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/lspuri"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/semantic/lsp"
)

const startupLSPChildEnv = "GORTEX_STARTUP_LSP_TEST_CHILD"

// This uses the portable test-binary peer pattern from the router shutdown tests:
// answer initialize/definition, then exit only after the owned exit frame.
func startupLSPChild() {
	reader := bufio.NewReader(os.Stdin)
	root, trace := "", os.Getenv("GORTEX_STARTUP_LSP_TEST_TRACE")
	for {
		size := 0
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				os.Exit(2)
			}
			if line == "\r\n" {
				break
			}
			if strings.HasPrefix(line, "Content-Length:") {
				size, _ = strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "Content-Length:")))
			}
		}
		body := make([]byte, size)
		if _, err := io.ReadFull(reader, body); err != nil {
			os.Exit(2)
		}
		var req struct {
			ID     json.RawMessage
			Method string
			Params json.RawMessage
		}
		if json.Unmarshal(body, &req) != nil {
			os.Exit(2)
		}
		var result any
		switch req.Method {
		case "initialize":
			var params lsp.InitializeParams
			if json.Unmarshal(req.Params, &params) != nil {
				os.Exit(2)
			}
			root = lspuri.URIToAbsPath(params.RootURI)
			if os.WriteFile(filepath.Join(trace, fmt.Sprint(os.Getpid())+".started"), []byte(root), 0o600) != nil {
				os.Exit(2)
			}
			result = map[string]any{"capabilities": map[string]any{"definitionProvider": true}}
		case "textDocument/definition":
			result = []lsp.Location{{URI: lspuri.PathToURI(filepath.Join(root, "main.ts")), Range: lsp.Range{Start: lsp.Position{Line: 0}}}}
		case "test/pid":
			result = os.Getpid()
		case "exit":
			status := 0
			if raw := os.Getenv("GORTEX_STARTUP_LSP_TEST_EXIT"); raw != "" {
				var err error
				status, err = strconv.Atoi(raw)
				if err != nil {
					os.Exit(2)
				}
			}
			if os.WriteFile(filepath.Join(trace, fmt.Sprint(os.Getpid())+".exited"), []byte(strconv.Itoa(status)), 0o600) != nil {
				os.Exit(2)
			}
			os.Exit(status)
		}
		if len(req.ID) != 0 {
			payload, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
			if err != nil {
				os.Exit(2)
			}
			if _, err := fmt.Fprintf(os.Stdout, "Content-Length: %d\r\n\r\n%s", len(payload), payload); err != nil {
				os.Exit(2)
			}
		}
	}
}

func startupLSPTraceCounts(t *testing.T, trace string) (int, int) {
	t.Helper()
	entries, err := os.ReadDir(trace)
	require.NoError(t, err)
	started, exited := 0, 0
	for _, entry := range entries {
		switch filepath.Ext(entry.Name()) {
		case ".started":
			started++
		case ".exited":
			exited++
			status, err := os.ReadFile(filepath.Join(trace, entry.Name()))
			require.NoError(t, err)
			assert.Equal(t, "0", string(status))
		}
	}
	return started, exited
}

func assertStartupLSPReaped(t *testing.T, client *lsp.Client, trace string, pid int) {
	t.Helper()
	select {
	case <-client.Done():
		// Client.Shutdown caches the real transport Stop/Wait result. Calling
		// it only after the router's joining close must return that result.
		require.NoError(t, client.Shutdown(), "owned child must have a successful actual Wait")
		status, err := os.ReadFile(filepath.Join(trace, fmt.Sprint(pid)+".exited"))
		require.NoError(t, err)
		assert.Equal(t, "0", string(status))
	default:
		t.Error("startup child still answers after cleanup should have joined it")
	}
}

func TestStartupLSPReleaseLifecycle(t *testing.T) {
	if os.Getenv(startupLSPChildEnv) == "1" {
		startupLSPChild()
		return
	}
	for _, mode := range []string{"normal", "resolve_failure", "canceled", "pool"} {
		t.Run(mode, func(t *testing.T) {
			// Given two startup workspaces plus an unrelated live workspace.
			base := t.TempDir()
			trace := filepath.Join(base, "children")
			require.NoError(t, os.Mkdir(trace, 0o700))
			roots := []string{filepath.Join(base, "first"), filepath.Join(base, "second"), filepath.Join(base, "unrelated")}
			cm, err := config.NewConfigManager(filepath.Join(base, "config.yaml"))
			require.NoError(t, err)
			for i, root := range roots {
				require.NoError(t, os.Mkdir(root, 0o700))
				require.NoError(t, os.WriteFile(filepath.Join(root, "main.ts"), []byte("target();\n"), 0o600))
				require.NoError(t, os.WriteFile(filepath.Join(root, "tsconfig.json"), []byte("{}"), 0o600))
				require.NoError(t, os.WriteFile(filepath.Join(root, "main.go"), []byte("package fixture\nfunc Caller() { missing() }\n"), 0o600))
				if i < 2 {
					require.NoError(t, cm.Global().AddRepo(config.RepoEntry{Path: root + string(os.PathSeparator), Name: filepath.Base(root)}))
				}
			}
			poolSize := "1"
			if mode == "pool" {
				poolSize = "2"
			}
			t.Setenv("GORTEX_LSP_POOL_SIZE", poolSize)
			t.Setenv("GORTEX_ENRICH_OVERLAP", "0")
			exe, err := os.Executable()
			require.NoError(t, err)
			// The startup factory chooses the registered TS spec; no host server
			// or operator setting is consulted or changed by this fixture.
			spec := lsp.SpecByName("tsgo")
			savedSpec := *spec
			t.Cleanup(func() { *spec = savedSpec })
			spec.Command, spec.Args = exe, []string{"-test.run=^TestStartupLSPReleaseLifecycle$"}
			// Keep race detection enabled, but exclude its artificial exit sleep
			// from the owned child's graceful-stop budget, as in the router tests.
			spec.Env = []string{startupLSPChildEnv + "=1", "GORTEX_STARTUP_LSP_TEST_TRACE=" + trace,
				"GORACE=" + os.Getenv("GORACE") + " atexit_sleep_ms=0"}
			spec.AlternativeCommands = nil
			router := lsp.NewRouter(roots[0], zap.NewNop())
			router.RegisterSpec(spec)
			registry := lsp.NewResolverHelperRegistry()
			t.Cleanup(func() { require.NoError(t, registry.Close()); require.NoError(t, router.Close()) })
			backend, err := store_sqlite.Open(filepath.Join(base, "store.sqlite"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, backend.Close()) })
			g := &warmupResolveFailureStore{Store: backend, resolveErr: errors.New("injected startup resolve failure")}
			reg := parser.NewRegistry()
			reg.Register(languages.NewGoExtractor())
			idx := indexer.New(g, reg, config.Default().Index, zap.NewNop())
			mi := indexer.NewMultiIndexer(g, reg, idx.Search(), cm, zap.NewNop())
			mi.SetResolverLSPHelper(registry)
			t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
			state := &daemonState{graph: g, indexer: idx, multiIndexer: mi, configManager: cm, lspRouter: router, resolverLSPRegistry: registry}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			other, err := router.ForSpecWorkspace(spec, roots[2])
			require.NoError(t, err)
			var otherPID int
			require.NoError(t, other.Client().Call("test/pid", nil, &otherPID))
			var first, second *lsp.Provider
			var firstPID, secondPID int
			var heldRelease func()
			seeded, boundary, laterWork := false, false, false
			core, logs := observer.New(zap.InfoLevel)
			logger := zap.New(core, zap.Hooks(func(entry zapcore.Entry) error {
				fields := logs.All()[logs.Len()-1].ContextMap()
				switch entry.Message {
				case "daemon: resolve-time LSP helpers registered":
					seeded = true
					for _, prefix := range []string{"first", "second"} {
						path, line, ok := registry.Definition(prefix+"/main.ts", 1, "target")
						require.True(t, ok)
						assert.Equal(t, prefix+"/main.ts", path)
						assert.Equal(t, 1, line)
					}
					first, err = router.ForSpecWorkspace(spec, roots[0])
					require.NoError(t, err)
					require.NoError(t, first.Client().Call("test/pid", nil, &firstPID))
					if mode != "pool" {
						second, err = router.ForSpecWorkspace(spec, roots[1])
						require.NoError(t, err)
						require.NoError(t, second.Client().Call("test/pid", nil, &secondPID))
					}
				case "daemon: resolve readiness phase published":
					if mode == "resolve_failure" {
						g.failScan.Store(true)
					}
				case "daemon: warmup phase done":
					if fields["phase"] != "resolve" {
						break
					}
					boundary = true
					if mode != "pool" {
						// Foreground acquisition occurs after startup's last use
						// but before the cleanup; the same provider must stay usable.
						provider, release, acquireErr := router.AcquireSpecWorkspace(spec, roots[1]+string(os.PathSeparator))
						require.NoError(t, acquireErr)
						require.Same(t, second, provider)
						heldRelease = release
					}
					if mode == "canceled" {
						cancel()
					}
				case "daemon: warmup phase start":
					if fields["phase"] != "deferred_passes_all" {
						break
					}
					laterWork = true
					if mode != "pool" {
						assertStartupLSPReaped(t, first.Client(), trace, firstPID)
					}
				}
				return nil
			}))
			// When warmup completes its resolver boundary normally or by failure/cancellation.
			watcher, timings := warmupDaemonStateContext(ctx, state, logger, nil, func(context.Context, *indexer.MultiWatcher) error { return nil })
			if watcher != nil {
				require.NoError(t, watcher.Stop())
			}
			require.True(t, seeded)
			require.True(t, boundary)
			assert.Equal(t, poolSize, os.Getenv("GORTEX_LSP_POOL_SIZE"))
			if mode == "resolve_failure" {
				assert.ErrorIs(t, timings.resolveErr, g.resolveErr)
			}
			if mode == "canceled" {
				assert.ErrorIs(t, ctx.Err(), context.Canceled)
				assert.False(t, laterWork)
			} else {
				assert.True(t, laterWork)
			}
			// Then the unrelated workspace and held foreground call still answer.
			require.NoError(t, other.Client().Call("test/ping", nil, nil))
			if mode == "pool" {
				started, exited := startupLSPTraceCounts(t, trace)
				assert.Equal(t, 6, started)
				assert.Zero(t, exited)
				require.NoError(t, first.Client().Call("test/ping", nil, nil), "pool mode must not run the new close even for an unpinned router provider at the same root")
				path, line, ok := registry.Definition("first/main.ts", 1, "target")
				assert.True(t, ok)
				assert.Equal(t, "first/main.ts", path)
				assert.Equal(t, 1, line)
				require.NoError(t, registry.Close())
				_, exited = startupLSPTraceCounts(t, trace)
				assert.Equal(t, 4, exited, "only existing helper shutdown may close owned pools")
				require.NoError(t, first.Client().Call("test/ping", nil, nil))
			} else {
				require.NotNil(t, heldRelease)
				defer heldRelease()
				assertStartupLSPReaped(t, first.Client(), trace, firstPID)
				require.NoError(t, second.Client().Call("test/ping", nil, nil))
				assert.Zero(t, router.CloseCheckoutWorkspace("typescript", roots[0]+string(os.PathSeparator)), "repeated close is safe")
				// A later query/edit uses the same registry and lazily recovers.
				path, line, ok := registry.Definition("first/main.ts", 1, "target")
				require.True(t, ok)
				assert.Equal(t, "first/main.ts", path)
				assert.Equal(t, 1, line)
				recreated, err := router.ForSpecWorkspace(spec, roots[0])
				require.NoError(t, err)
				assert.NotSame(t, first, recreated)
				require.NoError(t, recreated.PushSimulatedContent(filepath.Join(roots[0], "main.ts"), "target();\n"))
				heldRelease()
				assert.Equal(t, 1, router.CloseCheckoutWorkspace("typescript", roots[1]))
				assertStartupLSPReaped(t, second.Client(), trace, secondPID)
				assert.Equal(t, 1, router.CloseCheckoutWorkspace("typescript", roots[0]))
			}
			// Shutdown joins every remaining owned child and closes acquisition.
			require.NoError(t, registry.Close())
			require.NoError(t, router.Close())
			assertStartupLSPReaped(t, other.Client(), trace, otherPID)
			if mode == "pool" {
				assertStartupLSPReaped(t, first.Client(), trace, firstPID)
			}
			_, release, err := router.AcquireSpecWorkspace(spec, roots[0])
			require.Error(t, err)
			assert.Nil(t, release)
			started, exited := startupLSPTraceCounts(t, trace)
			assert.Equal(t, started, exited)
		})
	}
	t.Run("shutdown_retains_actual_exit_status", func(t *testing.T) {
		// A child that exits nonzero proves the lifecycle oracle reads the
		// transport's actual Wait outcome, not merely a close notification.
		root, trace := t.TempDir(), t.TempDir()
		exe, err := os.Executable()
		require.NoError(t, err)
		spec := &lsp.ServerSpec{Name: "exit-status-fixture", Command: exe,
			Args: []string{"-test.run=^TestStartupLSPReleaseLifecycle$"}, Languages: []string{"typescript"},
			Env: []string{startupLSPChildEnv + "=1", "GORTEX_STARTUP_LSP_TEST_TRACE=" + trace, "GORTEX_STARTUP_LSP_TEST_EXIT=7",
				"GORACE=" + os.Getenv("GORACE") + " atexit_sleep_ms=0"}}
		router := lsp.NewRouter(root, zap.NewNop())
		router.RegisterSpec(spec)
		t.Cleanup(func() { _ = router.Close() })
		provider, err := router.ForSpecWorkspace(spec, root)
		require.NoError(t, err)
		assert.Equal(t, 1, router.CloseCheckoutWorkspace("typescript", root))
		var exit *exec.ExitError
		require.ErrorAs(t, provider.Client().Shutdown(), &exit)
		assert.Equal(t, 7, exit.ExitCode())
		require.NoError(t, router.Close()) // The prior checkout-close result stays on its captured client.
	})
	for _, empty := range []bool{false, true} {
		t.Run("no_provider_"+strconv.FormatBool(empty), func(t *testing.T) {
			// Given no available provider, with either no startup work or a tracked repo.
			base := t.TempDir()
			cm, err := config.NewConfigManager(filepath.Join(base, "config.yaml"))
			require.NoError(t, err)
			if !empty {
				require.NoError(t, os.WriteFile(filepath.Join(base, "main.go"), []byte("package fixture\n"), 0o600))
				require.NoError(t, cm.Global().AddRepo(config.RepoEntry{Path: base}))
			}
			g := graph.New()
			reg := parser.NewRegistry()
			reg.Register(languages.NewGoExtractor())
			idx := indexer.New(g, reg, config.Default().Index, zap.NewNop())
			mi := indexer.NewMultiIndexer(g, reg, idx.Search(), cm, zap.NewNop())
			t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
			router := lsp.NewRouter(base, zap.NewNop())
			t.Cleanup(func() { require.NoError(t, router.Close()) })
			state := &daemonState{graph: g, indexer: idx, multiIndexer: mi, configManager: cm, lspRouter: router, resolverLSPRegistry: lsp.NewResolverHelperRegistry()}
			watcher, _ := warmupDaemonStateContext(t.Context(), state, zap.NewNop(), nil, func(context.Context, *indexer.MultiWatcher) error { return nil })
			if watcher != nil {
				require.NoError(t, watcher.Stop())
			}
			assert.Empty(t, router.Stats())
			assert.Zero(t, router.TimingStats().Starts)
		})
	}
}
