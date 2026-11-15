package goanalysis

import (
	"context"
	"encoding/json"
	"errors"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/semantic"
)

type startupDriverRecord struct {
	Kind       string
	Event      string
	Command    string
	Args       []string
	GOMAXPROCS string
	PID        int
	ExitCode   int
}

// TestStartupGoDriverHelper forwards the real command and records its owned
// child through exit. These records observe the actual driver/tool boundary.
func TestStartupGoDriverHelper(t *testing.T) {
	if os.Getenv("STARTUP_GO_DRIVER_HELPER") != "1" {
		return
	}
	var args []string
	for i, arg := range os.Args {
		if arg == "--" {
			args = os.Args[i+1:]
			break
		}
	}
	kind, command := "go", os.Getenv("STARTUP_REAL_GO")
	if len(args) > 1 {
		switch args[0] {
		case "startup-tool":
			kind, command, args = "tool", args[1], args[2:]
		case "startup-cc":
			kind, command, args = "cc", args[1], args[2:]
		}
	}
	record := startupDriverRecord{Kind: kind, Event: "start", Command: command, Args: args, GOMAXPROCS: os.Getenv("GOMAXPROCS"), PID: os.Getpid()}
	startupWriteDriverRecord(record)
	cmd := exec.Command(command, args...)
	cmd.Env = os.Environ()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		code = 94
		if exit, ok := err.(*exec.ExitError); ok {
			code = exit.ExitCode()
		}
	}
	record.Event, record.ExitCode = "exit", code
	startupWriteDriverRecord(record)
	os.Exit(code)
}

func startupWriteDriverRecord(record startupDriverRecord) {
	data, err := json.Marshal(record)
	if err != nil {
		os.Exit(91)
	}
	f, err := os.OpenFile(os.Getenv("STARTUP_GO_DRIVER_LOG"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		os.Exit(92)
	}
	data = append(data, '\n')
	n, err := f.Write(data)
	closeErr := f.Close()
	if err != nil || closeErr != nil || n != len(data) {
		os.Exit(93)
	}
}

func startupDriverLog(t *testing.T) string {
	t.Helper()
	realGo, err := exec.LookPath("go")
	require.NoError(t, err)
	binary, err := os.Executable()
	require.NoError(t, err)
	dir := resolvedTempDir(t)
	log := filepath.Join(dir, "driver.jsonl")
	shim := "#!/bin/sh\nexec " + "\"" + binary + "\" -test.run='^TestStartupGoDriverHelper$' -- \"$@\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "go"), []byte(shim), 0o700))
	t.Setenv("STARTUP_GO_DRIVER_HELPER", "1")
	t.Setenv("STARTUP_GO_DRIVER_LOG", log)
	t.Setenv("STARTUP_REAL_GO", realGo)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GOPACKAGESDRIVER", "off")
	// Keep the fail-before fixture bounded too. AC-2 separately checks that
	// production Config supplies its limits without this fixture setting.
	t.Setenv("GOFLAGS", "-p=1")
	return log
}

func startupDriverRecords(t *testing.T, log string) []startupDriverRecord {
	t.Helper()
	data, err := os.ReadFile(log)
	require.NoError(t, err)
	var records []startupDriverRecord
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var record startupDriverRecord
		require.NoError(t, json.Unmarshal([]byte(line), &record))
		records = append(records, record)
	}
	return records
}

func startupDriverQueries(t *testing.T, log string) [][]string {
	t.Helper()
	var queries [][]string
	for _, record := range startupDriverRecords(t, log) {
		args := record.Args
		if record.Kind != "go" || record.Event != "start" || len(args) == 0 || args[0] != "list" {
			continue
		}
		for i, arg := range args {
			if arg == "--" {
				queries = append(queries, args[i+1:])
				break
			}
		}
	}
	return queries
}

type startupBindingStore struct {
	graph.Store
	files []string
	rows  []graph.SemanticBindingType
}

func (s *startupBindingStore) ReplaceSemanticBindingTypes(_ string, rows []graph.SemanticBindingType) error {
	s.rows = slices.Clone(rows)
	return nil
}

func (s *startupBindingStore) DeleteSemanticBindingTypesByFiles(_ string, _ []string) error {
	return nil
}

func (s *startupBindingStore) ReplaceSemanticBindingTypesForFiles(_ string, files []string, rows []graph.SemanticBindingType) error {
	s.files = slices.Clone(files)
	s.rows = slices.Clone(rows)
	return nil
}

type startupPackageMembership struct {
	ID            string
	GoFiles       []string
	CompiledFiles []string
	SyntaxFiles   []string
}

func startupMembership(pkgs []*packages.Package, fset *token.FileSet, root string) []startupPackageMembership {
	var result []startupPackageMembership
	for _, pkg := range pkgs {
		if pkg.TypesInfo == nil {
			continue
		}
		member := startupPackageMembership{ID: pkg.ID}
		for _, file := range pkg.GoFiles {
			member.GoFiles = append(member.GoFiles, relativePath(file, root))
		}
		for _, file := range pkg.CompiledGoFiles {
			member.CompiledFiles = append(member.CompiledFiles, relativePath(file, root))
		}
		for _, file := range pkg.Syntax {
			member.SyntaxFiles = append(member.SyntaxFiles, relativePath(fset.Position(file.Pos()).Filename, root))
		}
		slices.Sort(member.GoFiles)
		slices.Sort(member.CompiledFiles)
		slices.Sort(member.SyntaxFiles)
		result = append(result, member)
	}
	slices.SortFunc(result, func(a, b startupPackageMembership) int { return strings.Compare(a.ID, b.ID) })
	return result
}

func TestStartupGoQueryGrouping(t *testing.T) {
	// Given ordinary packages, siblings, both test roots and special files.
	root := resolvedTempDir(t)
	writeGoMod(t, root, "example.com/startup")
	files := map[string]string{
		"alpha/a.go":             "package alpha\ntype A struct{}\nfunc F() { a := A{}; _ = a }\n",
		"alpha/b.go":             "package alpha\nvar B = A{}\n",
		"alpha/sibling.go":       "package alpha\nvar Sibling = A{}\n",
		"alpha/a_test.go":        "package alpha\nvar Internal = A{}\n",
		"alpha/external_test.go": "package alpha_test\nimport \"example.com/startup/alpha\"\nvar External = alpha.A{}\n",
		"beta/b.go":              "package beta\ntype B struct{}\nvar Value = B{}\n",
		"beta/excluded.go":       "//go:build startup_excluded\n\npackage beta\nvar Excluded = B{}\n",
		"nested/go.mod":          "module example.com/nested\n\ngo 1.21\n",
		"nested/n.go":            "package nested\ntype N struct{}\nvar Value = N{}\n",
		"adhoc/a.go":             "//go:build ignore\n\npackage main\ntype Adhoc struct{}\nvar Value = Adhoc{}\n",
	}
	for name, content := range files {
		writeFile(t, root, name, content)
	}
	combined := []string{"alpha/a.go", "alpha/b.go", "alpha/a_test.go", "alpha/external_test.go", "beta/b.go", "beta/excluded.go", "nested/n.go", "adhoc/a.go"}
	log := startupDriverLog(t)
	for _, tc := range []struct {
		name    string
		files   []string
		alias   bool
		reverse bool
		binding string
	}{
		{name: "ordinary and special", files: combined},
		{name: "reversed aliases", files: combined, reverse: true},
		{name: "symlinked root", files: combined, alias: true},
		{name: "unrequested external test", files: []string{"alpha/a.go", "alpha/b.go", "beta/b.go"}},
		{name: "only excluded ordinary file", files: []string{"beta/excluded.go"}},
		{name: "ad-hoc alone", files: []string{"adhoc/a.go"}, binding: "Adhoc"},
		{name: "nested module alone", files: []string{"nested/n.go"}, binding: "N"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			loadRoot := root
			if tc.alias {
				loadRoot = filepath.Join(resolvedTempDir(t), "linked")
				require.NoError(t, os.Symlink(root, loadRoot))
			}
			order := slices.Clone(tc.files)
			if tc.reverse {
				slices.Reverse(order)
			}
			inputs := []string{"repo/" + order[0], "repo/" + filepath.ToSlash(filepath.Dir(order[0])) + "/./" + filepath.Base(order[0])}
			baselinePatterns := make([]string, 0, len(tc.files))
			for _, name := range order {
				inputs = append(inputs, "repo/"+name)
			}
			for _, name := range tc.files {
				baselinePatterns = append(baselinePatterns, "file="+filepath.Join(loadRoot, name))
			}
			slices.Sort(baselinePatterns)
			// This is the pinned base's Config and ungrouped driver call, not the
			// candidate grouping helper. The combined case intentionally retains the
			// base's mixed command-line-arguments package and missing N rows (gtx-0m7).
			baseSet := token.NewFileSet()
			basePkgs, err := packages.Load(&packages.Config{
				Context: context.Background(), Dir: loadRoot, Tests: true, Fset: baseSet,
				Mode: packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles | packages.NeedImports | packages.NeedTypes | packages.NeedTypesInfo | packages.NeedSyntax | packages.NeedModule,
			}, baselinePatterns...)
			require.NoError(t, err)
			wantRows := buildSemanticBindingTypes(basePkgs, baseSet, loadRoot, "repo")
			if tc.binding != "" {
				require.Len(t, wantRows, 2)
				for _, row := range wantRows {
					require.Equal(t, tc.binding, row.TypeName)
				}
			}
			wantMembership := startupMembership(basePkgs, baseSet, loadRoot)
			wantFiles := map[string]bool{}
			for _, name := range tc.files {
				wantFiles["repo/"+name] = true
			}
			for _, pkg := range basePkgs {
				if pkg.TypesInfo == nil {
					continue
				}
				for _, syntax := range pkg.Syntax {
					rel := relativePath(baseSet.Position(syntax.Pos()).Filename, loadRoot)
					if rel != "" {
						wantFiles["repo/"+rel] = true
					}
				}
			}
			var expectedFiles []string
			for name := range wantFiles {
				expectedFiles = append(expectedFiles, name)
			}
			slices.Sort(expectedFiles)
			candidate := newTestProvider(t)
			candidate.includeTest = true
			t.Cleanup(func() { require.NoError(t, candidate.Close()) })
			canonicalRoot, err := filepath.EvalSymlinks(loadRoot)
			require.NoError(t, err)
			var canonicalPatterns []string
			for _, name := range tc.files {
				canonicalPatterns = append(canonicalPatterns, "file="+filepath.Join(canonicalRoot, name))
			}
			slices.Sort(canonicalPatterns)
			actualPkgs, actualSet, err := candidate.loadPackagesContext(context.Background(), canonicalRoot, canonicalPatterns...)
			require.NoError(t, err)
			require.Equal(t, wantMembership, startupMembership(actualPkgs, actualSet, canonicalRoot), "complete root membership and file multiplicity must match, without generated test-main or unrequested external-test roots")
			require.NoError(t, os.WriteFile(log, nil, 0o600))
			var patternsSeen []string
			candidate.packagesLoad = func(cfg *packages.Config, patterns ...string) ([]*packages.Package, error) {
				patternsSeen = append(patternsSeen, patterns...)
				return packages.Load(cfg, patterns...)
			}
			store := &startupBindingStore{Store: graph.New()}
			// When the file-scoped production route handles duplicates and aliases.
			_, err = candidate.EnrichFilesContext(context.Background(), store, "repo", loadRoot, inputs)
			require.NoError(t, err)
			// Then every persisted binding and refreshed sibling matches the base.
			require.Equal(t, wantRows, store.rows)
			require.Equal(t, expectedFiles, store.files)
			queries := startupDriverQueries(t, log)
			ordinary := map[string]int{}
			expectedOrdinary := map[string]int{}
			for _, file := range tc.files {
				directory := filepath.Dir(file)
				if directory == "alpha" || directory == "beta" {
					expectedOrdinary[directory] = 1
				}
			}
			for _, query := range queries {
				for _, pattern := range query {
					switch filepath.Base(pattern) {
					case "alpha", "beta":
						ordinary[filepath.Base(pattern)]++
					}
				}
			}
			require.Equal(t, expectedOrdinary, ordinary, "each ordinary directory reaches go list once")
			var fallback []string
			var directoryPatterns []string
			for _, pattern := range patternsSeen {
				require.NotEqual(t, "./...", pattern)
				if strings.HasPrefix(pattern, "file=") {
					fallback = append(fallback, strings.TrimPrefix(pattern, "file="))
				} else {
					directoryPatterns = append(directoryPatterns, pattern)
				}
			}
			var expectedFallback []string
			directories := map[string]bool{}
			for _, name := range tc.files {
				directories[filepath.Join(canonicalRoot, filepath.Dir(name))] = true
				switch filepath.Dir(name) {
				case "adhoc", "nested":
					expectedFallback = append(expectedFallback, filepath.Join(canonicalRoot, name))
				}
			}
			slices.Sort(expectedFallback)
			require.Equal(t, expectedFallback, fallback, "only uncovered special files use file=, together in base order")
			var expectedDirectories []string
			for dir := range directories {
				expectedDirectories = append(expectedDirectories, dir)
			}
			slices.Sort(expectedDirectories)
			require.Equal(t, expectedDirectories, directoryPatterns)
		})
	}
}

func startupEnvValue(env []string, key string) string {
	var value string
	for _, entry := range env {
		name, v, ok := strings.Cut(entry, "=")
		if ok && name == key {
			value = v
		}
	}
	return value
}

func TestStartupGoSubprocessBudget(t *testing.T) {
	t.Run("warmup reservation spans apply barrier", startupWarmupApplyBarrier)

	t.Run("interrupted startup join does not wait for publication", startupInterruptedJoin)

	t.Run("shared warmup admission", startupWarmupAdmission)
	t.Run("foreground preempts another module and joins", startupWarmupPreemption)
	t.Run("joined owner releases and restores outside behavior", startupWarmupTerminal)

	t.Run("preserve explicit flags and caller slices", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			flags []string
			want  []string
		}{
			{"strict", []string{"-p=1", "-tags=keep"}, []string{"-p=1", "-tags=keep", "-trimpath"}},
			{"separate", []string{"-p", "1", "-tags=keep"}, []string{"-p", "1", "-tags=keep", "-trimpath"}},
			{"larger", []string{"-p=8", "-tags=keep"}, []string{"-p=1", "-tags=keep", "-trimpath"}},
			{"larger separate", []string{"-p", "8", "-tags=keep"}, []string{"-p", "1", "-tags=keep", "-trimpath"}},
			{"invalid stays invalid", []string{"-p=0", "-tags=keep"}, []string{"-p=0", "-tags=keep", "-trimpath"}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				p := newTestProvider(t)
				t.Cleanup(func() { require.NoError(t, p.Close()) })
				before := slices.Clone(tc.flags)
				var got []string
				p.packagesLoad = func(cfg *packages.Config, _ ...string) ([]*packages.Package, error) {
					got = slices.Clone(cfg.BuildFlags)
					return nil, errors.New("captured")
				}
				_, err := p.runListing(t.Context(), t.TempDir(), []string{"."}, tc.flags)
				require.Error(t, err)
				require.Equal(t, tc.want, got)
				require.Equal(t, before, tc.flags)
			})
		}
	})
	t.Run("cold and warm cgo process boundary", startupCgoBoundary)

	t.Run("bounded Config on every route", func(t *testing.T) {
		root := typecheckCacheFixture(t)
		t.Setenv("GOFLAGS", "-mod=readonly -tags=startup_budget")
		t.Setenv("STARTUP_UNRELATED", "keep-me")
		for _, procs := range []string{"2", "1"} {
			t.Run("GOMAXPROCS="+procs, func(t *testing.T) {
				t.Setenv("GOMAXPROCS", procs)
				for _, route := range []string{"partial", "full", "checkout", "committed", "background", "targeted"} {
					t.Run(route, func(t *testing.T) {
						p := newTestProvider(t)
						t.Cleanup(func() { require.NoError(t, p.Close()) })
						ctx := t.Context()
						if route == "committed" {
							ctx = semantic.WithCheckoutCompilerScope(ctx, semantic.CheckoutCompilerScope{HandleRoots: true, Committed: true, GoWorkOff: true, Preempt: func() {}})
						}
						var configs []packages.Config
						var metadata []packages.Config
						failed := errors.New("startup config boundary reached")
						p.packagesLoad = func(cfg *packages.Config, _ ...string) ([]*packages.Package, error) {
							captured := *cfg
							captured.BuildFlags = slices.Clone(cfg.BuildFlags)
							captured.Env = slices.Clone(cfg.Env)
							if cfg.Mode&(packages.NeedTypes|packages.NeedExportFile) == 0 {
								metadata = append(metadata, captured)
								return []*packages.Package{{ID: "example.com/scope", PkgPath: "example.com/scope", Name: "scope", GoFiles: []string{filepath.Join(root, "api/api.go")}, Module: &packages.Module{Main: true, Path: "example.com/scope"}}}, nil
							}
							configs = append(configs, captured)
							return nil, failed
						}
						// Given each production path with unrelated environment/build flags.
						// When its real compiler/export Config reaches the driver boundary.
						switch route {
						case "partial":
							_, _ = p.EnrichFilesContext(ctx, graph.New(), "", root, []string{"api/api.go"})
						case "full":
							_, _ = p.EnrichRepoContext(ctx, scopeHandleGraph(), "", root, nil)
						case "checkout":
							_, _ = p.EnrichRepoContext(semantic.WithCheckoutCompilerScope(ctx, cachedScope), scopeHandleGraph(), "", root, nil)
						case "committed":
							_, _ = p.EnrichRepoContext(ctx, scopeHandleGraph(), "", root, nil)
						case "background":
							_, _ = p.runListing(ctx, root, []string{"./api"}, append(warmupBuildFlags(), "-tags=startup_budget"))
						case "targeted":
							_, _ = p.runListing(ctx, root, []string{"./api"}, append(targetedWarmupBuildFlags(), "-tags=startup_budget"))
						}
						// Then the compiler limits are explicit and stricter runtime settings survive.
						require.NotEmpty(t, configs, "route must reach an expensive load")
						for _, cfg := range configs {
							require.Contains(t, cfg.BuildFlags, "-p=1")
							require.Equal(t, procs, startupEnvValue(cfg.Env, "GOMAXPROCS"))
							require.Equal(t, "keep-me", startupEnvValue(cfg.Env, "STARTUP_UNRELATED"))
							require.Equal(t, "-mod=readonly -tags=startup_budget", startupEnvValue(cfg.Env, "GOFLAGS"))
							require.Equal(t, ctx.Done(), cfg.Context.Done(), "the manager cancellation must propagate")
							if route == "committed" {
								require.Equal(t, "off", startupEnvValue(cfg.Env, "GOWORK"))
							}
							if route == "background" || route == "targeted" {
								require.Contains(t, cfg.BuildFlags, "-tags=startup_budget")
								require.Contains(t, cfg.BuildFlags, "-trimpath")
							}
						}
						for _, cfg := range metadata {
							require.Zero(t, cfg.Mode&(packages.NeedTypes|packages.NeedExportFile))
							require.Empty(t, cfg.BuildFlags, "metadata-only Config is not changed by compiler tuning")
							if route != "committed" {
								require.Nil(t, cfg.Env, "metadata Config retains its native environment policy")
							}
						}
					})
				}
			})
		}
	})
}

func startupCgoBoundary(t *testing.T) {
	// Given a private empty build cache and a cgo package whose export load
	// invokes the compiler and the configured C compiler, not only metadata.
	root := resolvedTempDir(t)
	writeGoMod(t, root, "example.com/startupcgo")
	writeFile(t, root, "cgo/cgo.go", "package cgo\n/*\nint answer(void) { return 42; }\n*/\nimport \"C\"\ntype Number int\nvar Value = Number(C.answer())\n")
	cc, err := exec.LookPath("cc")
	require.NoError(t, err, "the bounded cgo fixture needs the already installed compiler")
	log := startupDriverLog(t)
	t.Setenv("GOFLAGS", "-mod=readonly")
	t.Setenv("GOCACHE", resolvedTempDir(t))
	binary, err := os.Executable()
	require.NoError(t, err)
	tools := resolvedTempDir(t)
	prefix := "#!/bin/sh\nexec \"" + binary + "\" -test.run='^TestStartupGoDriverHelper$' -- "
	tool := filepath.Join(tools, "tool")
	require.NoError(t, os.WriteFile(tool, []byte(prefix+"startup-tool \"$@\"\n"), 0o700))
	ccShim := filepath.Join(tools, "cc")
	require.NoError(t, os.WriteFile(ccShim, []byte(prefix+"startup-cc \""+cc+"\" \"$@\"\n"), 0o700))
	t.Setenv("CC", ccShim)
	p := newTestProvider(t)
	t.Cleanup(func() { require.NoError(t, p.Close()) })
	p.packagesLoad = func(cfg *packages.Config, patterns ...string) ([]*packages.Package, error) {
		if cfg.Mode&(packages.NeedTypes|packages.NeedExportFile) != 0 {
			cfg.BuildFlags = append(slices.Clone(cfg.BuildFlags), "-toolexec="+tool)
		}
		return packages.Load(cfg, patterns...)
	}
	for _, cache := range []string{"cold", "warm"} {
		t.Run(cache, func(t *testing.T) {
			require.NoError(t, os.WriteFile(log, nil, 0o600))
			// When the production compiler boundary loads one bounded pattern chunk.
			pkgs, fset, err := p.loadPackagesContext(t.Context(), root, filepath.Join(root, "cgo"))
			require.NoError(t, err)
			require.Len(t, pkgs, 1)
			require.Empty(t, pkgs[0].Errors)
			rows := buildSemanticBindingTypes(pkgs, fset, root, "")
			require.Len(t, rows, 2)
			for _, row := range rows {
				require.Equal(t, "Number", row.TypeName)
			}
			records := startupDriverRecords(t, log)
			data, err := json.Marshal(records)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(os.Getenv("HOME"), "startup-cgo-"+cache+".json"), data, 0o600))
			active, maxActive, expensive, metadata, compiler, cCompiler := 0, 0, 0, 0, 0, 0
			started := map[int]bool{}
			for _, record := range records {
				expensiveGo := record.Kind == "go" && slices.Contains(record.Args, "-export=true")
				switch record.Event {
				case "start":
					require.False(t, started[record.PID], "helper PID must start once")
					started[record.PID] = true
					require.Equal(t, "2", record.GOMAXPROCS, "child runtimes receive their bound")
					if expensiveGo {
						require.Contains(t, record.Args, "-p=1")
						active++
						expensive++
						maxActive = max(maxActive, active)
					} else if record.Kind == "go" {
						metadata++
						require.NotContains(t, record.Args, "-export=true")
					}
					if record.Kind == "tool" && filepath.Base(record.Command) == "compile" && !slices.Contains(record.Args, "-V=full") {
						compiler++
					}
					if record.Kind == "cc" {
						cCompiler++
					}
				case "exit":
					require.True(t, started[record.PID], "every helper exit must join its start")
					delete(started, record.PID)
					// Native cgo feature/name probes intentionally compile invalid C.
					// They must join, but only their owning Go/cgo command must succeed.
					if record.Kind != "cc" {
						require.Zero(t, record.ExitCode)
					}
					if expensiveGo {
						active--
					}
				}
			}
			// Then every helper/child has exited before return. A cold export load
			// actually compiles; the warm hit is not substituted for cold evidence.
			require.Empty(t, started)
			require.Zero(t, active)
			require.Equal(t, 1, maxActive, "a sub-limit pattern list has one expensive driver go command")
			require.Positive(t, expensive)
			require.Positive(t, metadata)
			if cache == "cold" {
				require.Positive(t, compiler)
				require.Positive(t, cCompiler)
			}
		})
	}
}

func startupRunRoute(ctx context.Context, p *Provider, route, root string) error {
	switch route {
	case "partial":
		_, err := p.EnrichFilesContext(ctx, graph.New(), "", root, []string{"api/api.go"})
		return err
	case "full":
		_, err := p.EnrichRepoContext(ctx, scopeHandleGraph(), "", root, nil)
		return err
	case "checkout":
		_, err := p.EnrichRepoContext(semantic.WithCheckoutCompilerScope(ctx, cachedScope), scopeHandleGraph(), "", root, nil)
		return err
	case "committed":
		ctx = semantic.WithCheckoutCompilerScope(ctx, semantic.CheckoutCompilerScope{HandleRoots: true, Committed: true, Preempt: func() {}})
		_, err := p.EnrichRepoContext(ctx, scopeHandleGraph(), "", root, nil)
		return err
	case "background":
		_, err := p.runListing(ctx, root, []string{"./api"}, warmupBuildFlags())
		return err
	case "targeted":
		_, err := p.runListing(ctx, root, []string{"./api"}, targetedWarmupBuildFlags())
		return err
	}
	panic("unknown startup fixture route")
}

func startupFakeMetadata(cfg *packages.Config) []*packages.Package {
	return []*packages.Package{{ID: "example.com/scope", PkgPath: "example.com/scope", Name: "scope", GoFiles: []string{filepath.Join(cfg.Dir, "api/api.go")}, Module: &packages.Module{Main: true, Path: "example.com/scope"}}}
}

func startupWarmupAdmission(t *testing.T) {
	routes := []string{"partial", "full", "checkout", "committed", "background", "targeted"}
	var roots []string
	for range routes {
		roots = append(roots, typecheckCacheFixture(t))
	}
	synctest.Test(t, func(t *testing.T) {
		p := newTestProvider(t)
		p.heavyGate = make(chan struct{}, 2)
		p.compilerGC = func() {}
		t.Cleanup(func() { require.NoError(t, p.Close()) })
		end := p.BeginStartupWarmup()
		defer end()
		release := make(chan struct{})
		var active, peak, starts atomic.Int32
		p.packagesLoad = func(cfg *packages.Config, _ ...string) ([]*packages.Package, error) {
			if cfg.Mode&(packages.NeedTypes|packages.NeedExportFile) == 0 {
				return startupFakeMetadata(cfg), nil
			}
			n := active.Add(1)
			for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
			}
			starts.Add(1)
			select {
			case <-release:
			case <-cfg.Context.Done():
			}
			active.Add(-1)
			return nil, cfg.Context.Err()
		}
		done := make(chan error, len(routes))
		// Given independent heavy-gate callers and independent listing callers in
		// different modules, with the ordinary heavy gate deliberately set to two.
		for i, route := range routes {
			go func() { done <- startupRunRoute(t.Context(), p, route, roots[i]) }()
		}
		synctest.Wait()
		startedBeforeRelease := starts.Load()
		// When all attempts are deterministically blocked in their admission or driver.
		close(release)
		for range routes {
			<-done
		}
		// Then flags alone cannot satisfy this cross-route exclusion assertion.
		require.EqualValues(t, 1, startedBeforeRelease)
		require.EqualValues(t, 1, peak.Load())
		require.Zero(t, active.Load())
	})
}

func startupWarmupPreemption(t *testing.T) {
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
		go func() { front <- startupRunRoute(t.Context(), p, "partial", roots[1]) }()
		synctest.Wait()
		cancelled := bgCtx.Err() != nil
		ranBeforeJoin := foreground.Load()
		// Complete cleanup even on the flags-only/base behavior so the failed
		// assertion cannot strand the fixture goroutines.
		cancel()
		close(cleanup)
		<-background
		<-front
		require.True(t, cancelled, "foreground must preempt the different module's listing")
		require.False(t, ranBeforeJoin, "foreground must wait for owned child cleanup")
		require.False(t, early.Load())
		require.True(t, foreground.Load())
	})
}

func startupWarmupTerminal(t *testing.T) {
	root := typecheckCacheFixture(t)
	for _, outcome := range []string{"success", "failure", "cancel"} {
		t.Run(outcome, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				p := newTestProvider(t)
				p.heavyGate = make(chan struct{}, 2)
				t.Cleanup(func() { require.NoError(t, p.Close()) })
				end := p.BeginStartupWarmup()
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				release := make(chan struct{})
				p.packagesLoad = func(cfg *packages.Config, _ ...string) ([]*packages.Package, error) {
					<-release
					switch outcome {
					case "failure":
						return nil, errors.New("driver failed")
					case "cancel":
						return nil, cfg.Context.Err()
					}
					return nil, nil
				}
				loadDone := make(chan error, 1)
				go func() { _, err := p.runListing(ctx, root, []string{"./api"}, nil); loadDone <- err }()
				synctest.Wait()
				ended := make(chan struct{})
				var loadErr error
				go func() { loadErr = <-loadDone; end(); close(ended) }()
				synctest.Wait()
				returnedEarly := false
				select {
				case <-ended:
					returnedEarly = true
				default:
				}
				if outcome == "cancel" {
					cancel()
				}
				close(release)
				<-ended
				err := loadErr
				end() // release is idempotent
				require.False(t, returnedEarly, "the window owner joins its load before policy release")
				if outcome == "success" {
					require.NoError(t, err)
				} else {
					require.Error(t, err)
				}
				// Outside warmup, both ordinary heavy slots remain available and
				// module-local listings stay outside that gate.
				first, err := p.acquireHeavy(t.Context(), false)
				require.NoError(t, err)
				second, err := p.acquireHeavy(t.Context(), false)
				require.NoError(t, err)
				listed := make(chan error, 1)
				go func() { _, err := p.runListing(t.Context(), root, []string{"./api"}, nil); listed <- err }()
				synctest.Wait()
				outsideCompleted := false
				select {
				case <-listed:
					outsideCompleted = true
				default:
				}
				first()
				second()
				if !outsideCompleted {
					<-listed
				}
				require.True(t, outsideCompleted, "outside-warmup listing admission must stay unchanged")
			})
		})
	}
}

func startupWarmupApplyBarrier(t *testing.T) {
	t.Setenv("GORTEX_GOTYPES_NEEDDEPS", "1")
	root := typecheckCacheFixture(t)
	synctest.Test(t, func(t *testing.T) {
		p := newTestProvider(t)
		p.heavyGate = make(chan struct{}, 2)
		p.compilerGC = func() {}
		t.Cleanup(func() { require.NoError(t, p.Close()) })
		end := p.BeginStartupWarmup()
		defer end()
		gate := make(chan struct{})
		var loads atomic.Int32
		p.packagesLoad = func(*packages.Config, ...string) ([]*packages.Package, error) {
			loads.Add(1)
			return []*packages.Package{{ID: "example.com/scope/api", GoFiles: []string{filepath.Join(root, "api/api.go")}}}, nil
		}
		full := make(chan error, 1)
		go func() {
			_, err := p.enrichRepoContext(semantic.WithApplyGate(t.Context(), gate), graph.New(), "", root)
			full <- err
		}()
		synctest.Wait()
		partial := make(chan error, 1)
		go func() { partial <- startupRunRoute(t.Context(), p, "partial", root) }()
		synctest.Wait()
		before := loads.Load()
		close(gate)
		<-full
		<-partial
		require.EqualValues(t, 1, before, "a second load must not overlap the retained program at apply")
		require.EqualValues(t, 2, loads.Load())
	})
}

func startupInterruptedJoin(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p := newTestProvider(t)
		p.heavyGate = make(chan struct{}, 2)
		t.Cleanup(func() { require.NoError(t, p.Close()) })
		end := p.BeginStartupWarmup()
		// Given a publication whose independent context outlives the cancelled
		// startup join, its already admitted load keeps both original reservations.
		committed := semantic.WithCheckoutCompilerScope(t.Context(), semantic.CheckoutCompilerScope{Committed: true, Preempt: func() {}})
		publication, err := p.acquireHeavy(committed, false)
		require.NoError(t, err)
		ended := make(chan struct{})
		go func() { end(); close(ended) }()
		synctest.Wait()
		policyReleased := false
		select {
		case <-ended:
			policyReleased = true
		default:
		}
		heldPublication := len(p.largeGate) == 1 && len(p.heavyGate) == 1
		later := make(chan func(), 1)
		go func() {
			release, err := p.acquireHeavy(t.Context(), false)
			if err != nil {
				panic(err)
			}
			later <- release
		}()
		synctest.Wait()
		var laterRelease func()
		select {
		case laterRelease = <-later:
		default:
		}
		laterAdmitted := laterRelease != nil
		publication()
		<-ended
		if laterRelease == nil {
			laterRelease = <-later
		}
		laterRelease()
		end()
		// Then policy release does not perform a second publication wait or revoke
		// the publication's lease. Later live work sees ordinary admission.
		require.True(t, policyReleased)
		require.True(t, heldPublication)
		require.True(t, laterAdmitted)
	})
}
