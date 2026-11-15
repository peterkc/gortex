package goanalysis

import (
	"context"
	"encoding/json"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"

	"github.com/zzet/gortex/internal/graph"
)

// TestStartupGoDriverHelper forwards to the real Go command. Its append-only
// record observes go/packages' driver, rather than only counting Load calls.
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
	record, err := json.Marshal(args)
	if err != nil {
		os.Exit(91)
	}
	f, err := os.OpenFile(os.Getenv("STARTUP_GO_DRIVER_LOG"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		os.Exit(92)
	}
	_, err = f.Write(append(record, '\n'))
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		os.Exit(93)
	}
	cmd := exec.Command(os.Getenv("STARTUP_REAL_GO"), args...)
	cmd.Env = os.Environ()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	err = cmd.Run()
	if err == nil {
		os.Exit(0)
	}
	if exit, ok := err.(*exec.ExitError); ok {
		os.Exit(exit.ExitCode())
	}
	os.Exit(94)
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

func startupDriverQueries(t *testing.T, log string) [][]string {
	t.Helper()
	data, err := os.ReadFile(log)
	require.NoError(t, err)
	var queries [][]string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var args []string
		require.NoError(t, json.Unmarshal([]byte(line), &args))
		if len(args) == 0 || args[0] != "list" {
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
