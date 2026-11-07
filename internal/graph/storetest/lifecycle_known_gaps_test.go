package storetest

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Given a listed child, when it passes, fails elsewhere, fails silently,
// panics, or mixes marked and unmarked descendants, then its parent refuses
// the result. An exactly marked failure must pass, so refusal is not vacuous.
func TestStorageContractKnownGapSelfCheck(t *testing.T) {
	planted := []struct {
		name     string
		body     func(*testing.T)
		accepted bool
	}{
		{"Passing", func(t *testing.T) {}, false},
		{"UnmarkedBeforeMarked", func(t *testing.T) {
			t.Error("unmarked setup failure")
			t.Fatal("planted-gap: expected")
		}, false},
		// A silent Fail followed by a marked Fatal is indistinguishable in
		// -v output. The static ban below rejects Fail in cases and helpers.
		{"SilentBeforeMarked", func(t *testing.T) {
			if os.Getenv(knownGapChildEnv) != "" {
				t.FailNow()
			}
			t.Fatal("planted-gap: expected")
		}, false},
		{"Panic", func(t *testing.T) { panic("planted panic") }, false},
		{"MixedDescendants", func(t *testing.T) {
			t.Run("Marked", func(t *testing.T) { t.Fatal("planted-gap: expected") })
			t.Run("Unmarked", func(t *testing.T) { t.Fatal("unmarked other failure") })
		}, false},
		{"SkipDescendant", func(t *testing.T) {
			t.Run("Marked", func(t *testing.T) { t.Fatal("planted-gap: expected") })
			t.Run("Skipped", func(t *testing.T) { t.Skip("planted skip") })
		}, false},
		{"ExactlyMarked", func(t *testing.T) { t.Fatal("planted-gap: expected") }, true},
	}
	for _, p := range planted {
		t.Run(p.name, func(t *testing.T) {
			if os.Getenv(knownGapChildEnv) == t.Name() {
				p.body(t)
				return
			}
			err := checkKnownGapChild(caseContext(t), t.Name(), knownGap{path: p.name, kind: "feature", marker: "planted-gap"})
			if (err == nil) != p.accepted {
				t.Fatalf("accepted = %v, want %v: %v", err == nil, p.accepted, err)
			}
		})
	}
	t.Run("UnrelatedFailure", func(t *testing.T) {
		output := "=== RUN   Selected\n    planted.go:1: planted-gap: expected\n--- FAIL: Selected (0.00s)\n=== RUN   Unrelated\n    planted.go:2: planted-gap: expected\n--- FAIL: Unrelated (0.00s)\n"
		if err := knownGapOutputProblem(output, "Selected", "planted-gap"); err == nil {
			t.Fatal("accepted the planted unrelated FAIL record")
		}
	})
	t.Run("NoOtherBackendMayHaveEntries", func(t *testing.T) {
		for _, name := range []string{"pebble", "other"} {
			if err := knownGapTableProblem(map[string]expectation{name: {knownGaps: []knownGap{{path: "K1/Example", kind: "feature", marker: "example"}}}}); err == nil {
				t.Fatalf("accepted a known-gap entry for %s", name)
			}
		}
	})
	t.Run("ScannerReportsPlantedCalls", func(t *testing.T) {
		source := `package planted
func f(t *testing.T) { t.Fail(); t.FailNow(); t.SkipNow(); t.Fatal("message"); t.Skip("reason") }`
		if got := silentCalls(t, "planted.go", source); !slices.Equal(got, []string{"Fail", "FailNow", "SkipNow"}) {
			t.Fatalf("silent calls = %v, want Fail, FailNow, SkipNow", got)
		}
	})
	t.Run("CasesAndHelpersHaveNoSilentCalls", func(t *testing.T) {
		err := filepath.WalkDir(".", func(path string, e os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || e.Name() == "lifecycle_known_gaps_test.go" {
				return nil
			}
			source, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if got := silentCalls(t, path, string(source)); len(got) != 0 {
				t.Errorf("%s calls silent failure or skip methods: %v", path, got)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})
}

func silentCalls(t *testing.T, name, source string) []string {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), name, source, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	var found []string
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if ok && slices.Contains([]string{"Fail", "FailNow", "SkipNow"}, sel.Sel.Name) {
			found = append(found, sel.Sel.Name)
		}
		return true
	})
	return found
}

// The real K6/K9 paths remain behind SQLite's D18 driver markers. These
// planted outcomes keep their later verdicts falsifiable before B arrives.
func TestStorageContractLatentVerdicts(t *testing.T) {
	refused := errors.New("refused")
	t.Run("OpenFirstCannotLoseItsPin", func(t *testing.T) {
		if err := k6RaceProblem(true, "e0", "", refused, nil, false); err == nil {
			t.Fatal("accepted O refused, T reclaimed in O-first order")
		}
		if err := k6RaceProblem(true, "e0", "e0", nil, refused, true); err != nil {
			t.Fatal(err)
		}
		if err := k6RaceProblem(false, "e0", "", refused, nil, false); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("UntrackMustRecordCleanup", func(t *testing.T) {
		before := Publication{Version: "e0", Guard: "guard"}
		if err := k9UntrackProblem(before, Publication{}, nil, StateSealed); err == nil {
			t.Fatal("accepted gone R1 with a still-sealed version")
		}
		if err := k9UntrackProblem(before, Publication{}, refused, StateRetiring); err == nil {
			t.Fatal("accepted a generic Current error as gone R1")
		}
		notTracked := fmt.Errorf("withdrawn checkout: %w", ErrNotTracked)
		if err := k9UntrackProblem(before, Publication{}, notTracked, StateSealed); err == nil {
			t.Fatal("accepted not-tracked R1 with a still-sealed version")
		}
		if err := k9UntrackProblem(before, Publication{}, nil, StateRetiring); err == nil {
			t.Fatal("accepted a tracked R1 that serves nothing as gone")
		}
		for _, state := range []State{"", StateRetiring} {
			if err := k9UntrackProblem(before, Publication{}, notTracked, state); err != nil {
				t.Fatal(err)
			}
		}
		if err := k9UntrackProblem(before, before, nil, StateSealed); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("CounterAcceptsExactlyTheWriterBytes", func(t *testing.T) {
		if !k14CountsWriterBytes(2 * k14WriterBytes) {
			t.Fatal("rejected exactly the writer bytes from an otherwise read-only restart")
		}
		if k14CountsWriterBytes(2*k14WriterBytes - 1) {
			t.Fatal("accepted an undercount of the writer bytes")
		}
	})
}
