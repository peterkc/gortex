package storetest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

const knownGapChildEnv = "STORETEST_KNOWN_GAP_CHILD"

type knownGap struct {
	path   string
	kind   string
	marker string
}

// validateKnownGaps keeps the exception list SQLite-only and unambiguous.
func validateKnownGaps(t *testing.T, table map[string]expectation) {
	t.Helper()
	if err := knownGapTableProblem(table); err != nil {
		t.Fatal(err)
	}
}

func knownGapTableProblem(table map[string]expectation) error {
	markers := map[string]bool{}
	for backend, row := range table {
		if backend != "sqlite" && len(row.knownGaps) != 0 {
			return fmt.Errorf("known gaps are forbidden for backend %q", backend)
		}
		paths := map[string]bool{}
		for _, gap := range row.knownGaps {
			if gap.path == "" || gap.marker == "" || paths[gap.path] || markers[gap.marker] || (gap.kind != "feature" && gap.kind != "driver") {
				return fmt.Errorf("invalid or duplicate known-gap entry: %+v", gap)
			}
			paths[gap.path], markers[gap.marker] = true, true
		}
	}
	return nil
}

func runContractCase(t *testing.T, backend, path string, body func(*testing.T)) {
	t.Helper()
	for _, gap := range expectations(Backends())[backend].knownGaps {
		if gap.path != path {
			continue
		}
		if os.Getenv(knownGapChildEnv) == t.Name() {
			body(t)
			return
		}
		if err := checkKnownGapChild(caseContext(t), t.Name(), gap); err != nil {
			t.Fatal(err)
		}
		return
	}
	body(t)
}

// checkKnownGapChild follows K14's native child-binary pattern. Each path
// component is separately quoted and anchored because testing splits on '/'.
func checkKnownGapChild(ctx context.Context, selected string, gap knownGap) error {
	var pattern []string
	for _, part := range strings.Split(selected, "/") {
		pattern = append(pattern, "^"+regexp.QuoteMeta(part)+"$")
	}
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run="+strings.Join(pattern, "/"), "-test.v", "-test.count=1")
	cmd.Env = append(os.Environ(), knownGapChildEnv+"="+selected)
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if ctx.Err() != nil || !errors.As(err, &exit) || exit.ExitCode() != 1 {
		return fmt.Errorf("known gap %s: want child exit 1, got %v (context %v)\n%s", gap.path, err, ctx.Err(), out)
	}
	if err := knownGapOutputProblem(string(out), selected, gap.marker); err != nil {
		return fmt.Errorf("known gap %s: %w\n%s", gap.path, err, out)
	}
	return nil
}

var gapResultLine = regexp.MustCompile(`^\s*--- (FAIL|PASS|SKIP): (\S+) \(`)

// knownGapOutputProblem parses Go testing's -v records. The active test owns
// each indented diagnostic; unindented backend logs are not testing messages.
// Failed ancestors may only propagate failure. Each failed leaf must emit a
// marked diagnostic, and no failed test may emit an unmarked diagnostic.
func knownGapOutputProblem(output, selected, marker string) error {
	ran := map[string]bool{}
	failed := map[string]bool{}
	messages := map[string][]string{}
	active := ""
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "panic:") || strings.Contains(line, "test timed out") {
			return fmt.Errorf("child panicked or timed out")
		}
		if name, ok := strings.CutPrefix(line, "=== RUN   "); ok {
			active = strings.TrimSpace(name)
			ran[active] = true
			continue
		}
		if name, ok := strings.CutPrefix(line, "=== NAME  "); ok {
			active = strings.TrimSpace(name)
			continue
		}
		if result := gapResultLine.FindStringSubmatch(line); result != nil {
			status, name := result[1], result[2]
			if status == "SKIP" && (name == selected || strings.HasPrefix(name, selected+"/")) {
				return fmt.Errorf("selected test or descendant skipped: %s", name)
			}
			if status == "FAIL" {
				if name != selected && !strings.HasPrefix(name, selected+"/") && !strings.HasPrefix(selected, name+"/") {
					return fmt.Errorf("unrelated failure: %s", name)
				}
				failed[name] = true
			}
			continue
		}
		if strings.HasPrefix(line, "    ") || strings.HasPrefix(line, "\t") {
			messages[active] = append(messages[active], strings.TrimSpace(line))
		}
	}
	if !ran[selected] || !failed[selected] {
		return fmt.Errorf("selected test did not run and fail: %s", selected)
	}
	for name := range failed {
		for _, message := range messages[name] {
			if !strings.Contains(message, marker) {
				return fmt.Errorf("unmarked message in %s: %s", name, message)
			}
		}
		leaf := true
		for other := range failed {
			if strings.HasPrefix(other, name+"/") {
				leaf = false
				break
			}
		}
		if leaf && len(messages[name]) == 0 {
			return fmt.Errorf("failed leaf has no marked message: %s", name)
		}
	}
	return nil
}
