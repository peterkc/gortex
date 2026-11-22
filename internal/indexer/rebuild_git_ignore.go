package indexer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/zzet/gortex/internal/gitcmd"
)

const (
	rebuildGitIgnoreBatchLimit   = 1024
	rebuildGitIgnoreBatchTimeout = 10 * time.Second
)

var errRebuildGitIgnore = errors.New("unproved rebuild Git ignore verdict")

type rebuildGitIgnoreVerdict uint8

const (
	rebuildGitIgnoreNoMatch rebuildGitIgnoreVerdict = iota + 1
	rebuildGitIgnoreMatchedNegation
	rebuildGitIgnoreIgnored
)

// checkRebuildGitIgnore returns one verdict per requested path in input order,
// with the raw stdin plus stdout byte charge. The owner binds dir to the checked
// checkout and supplies its admitted point queries, including a slash suffix
// for directories. --no-index asks only about rules; tracked membership and
// Git boundaries are the owner's separate authority. No answers survive an error.
func checkRebuildGitIgnore(ctx context.Context, dir string, paths []string, rawRemaining int64) ([]rebuildGitIgnoreVerdict, int64, error) {
	if !filepath.IsAbs(dir) {
		return nil, 0, fmt.Errorf("%w: missing absolute checkout directory", errRebuildGitIgnore)
	}
	if len(paths) > rebuildGitIgnoreBatchLimit || rawRemaining < 0 || rawRemaining > rebuildCppRawLimit {
		return nil, 0, fmt.Errorf("%w: %w", errRebuildGitIgnore, errRebuildCppCaptureLimit)
	}
	ctx, cancel := context.WithTimeout(ctx, rebuildGitIgnoreBatchTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, 0, fmt.Errorf("%w: %w", errRebuildGitIgnore, err)
	}
	if len(paths) == 0 {
		return nil, 0, nil
	}

	seen := make(map[string]struct{}, len(paths))
	var inputBytes int
	for i, rel := range paths {
		key := strings.TrimSuffix(rel, "/")
		if !validRebuildNamespaceKey(key) {
			return nil, 0, fmt.Errorf("%w: invalid query path at index %d", errRebuildGitIgnore, i)
		}
		if _, duplicate := seen[key]; duplicate {
			return nil, 0, fmt.Errorf("%w: duplicate query path at index %d", errRebuildGitIgnore, i)
		}
		seen[key] = struct{}{}
		inputBytes += len(rel) + 1
	}
	// The smallest possible answer echoes each path with four NULs.
	if int64(2*inputBytes+3*len(paths)) > rawRemaining {
		return nil, 0, fmt.Errorf("%w: %w", errRebuildGitIgnore, errRebuildCppCaptureLimit)
	}
	input := make([]byte, 0, inputBytes)
	for _, rel := range paths {
		input = append(input, rel...)
		input = append(input, 0)
	}
	out, wroteStderr, err := gitcmd.RunNoLazyBoundedStdin(ctx, dir, input, rawRemaining-int64(inputBytes),
		"check-ignore", "--no-index", "--stdin", "-z", "-v", "-n")
	if errors.Is(err, gitcmd.ErrOutputLimit) {
		return nil, 0, fmt.Errorf("%w: %w: %w", errRebuildGitIgnore, errRebuildCppCaptureLimit, err)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, 0, fmt.Errorf("%w: %w", errRebuildGitIgnore, ctxErr)
	}
	var exitErr *exec.ExitError
	exitOne := errors.As(err, &exitErr) && exitErr.ExitCode() == 1
	if err != nil && (wroteStderr || !exitOne) {
		return nil, 0, fmt.Errorf("%w: %w", errRebuildGitIgnore, err)
	}
	if wroteStderr {
		return nil, 0, fmt.Errorf("%w: Git wrote to stderr", errRebuildGitIgnore)
	}
	answers, err := parseRebuildGitIgnore(ctx, paths, out, exitOne)
	if err != nil {
		return nil, 0, err
	}
	return answers, int64(inputBytes + len(out)), nil
}

func parseRebuildGitIgnore(ctx context.Context, paths []string, out []byte, exitOne bool) ([]rebuildGitIgnoreVerdict, error) {
	answers := make([]rebuildGitIgnoreVerdict, len(paths))
	matched := false
	for i, rel := range paths {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("%w: %w", errRebuildGitIgnore, err)
		}
		before := len(out)
		var fields [4][]byte // source, line, pattern, exact pathname echo
		for j := range fields {
			var ok bool
			fields[j], out, ok = bytes.Cut(out, []byte{0})
			if !ok {
				return nil, fmt.Errorf("%w: incomplete answer at index %d", errRebuildGitIgnore, i)
			}
		}
		if before-len(out) > rebuildCppEntryLimit {
			return nil, fmt.Errorf("%w: %w", errRebuildGitIgnore, errRebuildCppCaptureLimit)
		}
		if string(fields[3]) != rel {
			return nil, fmt.Errorf("%w: unexpected path echo at index %d", errRebuildGitIgnore, i)
		}
		source, line, pattern := fields[0], fields[1], fields[2]
		if len(source) == 0 && len(line) == 0 && len(pattern) == 0 {
			answers[i] = rebuildGitIgnoreNoMatch
			continue
		}
		if len(source) == 0 || len(line) == 0 || len(line) > 10 || line[0] < '1' || line[0] > '9' || len(pattern) == 0 {
			return nil, fmt.Errorf("%w: malformed pattern metadata at index %d", errRebuildGitIgnore, i)
		}
		if _, err := strconv.ParseUint(string(line), 10, 31); err != nil {
			return nil, fmt.Errorf("%w: malformed line number at index %d", errRebuildGitIgnore, i)
		}
		matched = true
		if pattern[0] == '!' {
			answers[i] = rebuildGitIgnoreMatchedNegation
			continue
		}
		if exitOne {
			return nil, fmt.Errorf("%w: exit 1 contradicts ignored path at index %d", errRebuildGitIgnore, i)
		}
		answers[i] = rebuildGitIgnoreIgnored
	}
	if len(out) != 0 {
		return nil, fmt.Errorf("%w: unexpected trailing answer bytes", errRebuildGitIgnore)
	}
	// Git 2.56 also exits 0 for a negation-only match. Status is never the
	// verdict; accept exit 1 only with complete answers and no positive rule.
	if !exitOne && !matched {
		return nil, fmt.Errorf("%w: exit 0 without a pattern match", errRebuildGitIgnore)
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("%w: %w", errRebuildGitIgnore, err)
	}
	return answers, nil
}
