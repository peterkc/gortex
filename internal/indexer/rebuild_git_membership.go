package indexer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/zzet/gortex/internal/gitcmd"
)

var errRebuildGitMembership = errors.New("unproved rebuild Git membership")

type rebuildGitMember uint8

const (
	rebuildGitTracked rebuildGitMember = iota + 1
	rebuildGitUntracked
	rebuildGitBoundary
)

// readRebuildGitMembership captures the §8 tracked and nonignored sets. It
// does not assert directory closure or preauthorize any working-tree bytes.
// Gitlinks and the directory records for embedded repositories are boundaries.
func readRebuildGitMembership(ctx context.Context, root *os.Root) (map[string]rebuildGitMember, error) {
	if root == nil || !filepath.IsAbs(root.Name()) {
		return nil, errRebuildGitMembership
	}
	dir := root.Name()
	run := func(limit int64, args ...string) ([]byte, error) {
		out, err := gitcmd.RunNoLazyBounded(ctx, dir, limit, args...)
		if err != nil {
			if errors.Is(err, gitcmd.ErrOutputLimit) {
				err = errors.Join(err, errRebuildCppCaptureLimit)
			}
			return nil, fmt.Errorf("%w: %w", errRebuildGitMembership, err)
		}
		return out, nil
	}
	// --show-prefix is empty only at the Git worktree root. Do not borrow
	// the enclosing repository's membership for a configured subdirectory.
	want := dir + "\n\ntrue\n"
	location, err := run(int64(len(want)), "rev-parse", "--show-toplevel", "--show-prefix", "--is-inside-work-tree")
	if err != nil {
		return nil, err
	}
	if string(location) != want {
		return nil, fmt.Errorf("%w: held root is not the Git worktree root", errRebuildGitMembership)
	}
	tracked, err := run(rebuildCppRawLimit, "ls-files", "--stage", "-z")
	if err != nil {
		return nil, err
	}
	others, err := run(int64(rebuildCppRawLimit-len(tracked)), "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, err
	}
	return parseRebuildGitMembership(ctx, tracked, others)
}

func parseRebuildGitMembership(ctx context.Context, tracked, others []byte) (map[string]rebuildGitMember, error) {
	if len(tracked) > rebuildCppRawLimit || len(others) > rebuildCppRawLimit-len(tracked) {
		return nil, fmt.Errorf("%w: %w", errRebuildGitMembership, errRebuildCppCaptureLimit)
	}
	out := make(map[string]rebuildGitMember)
	remaining := rebuildNamespaceKeyBytesLimit
	add := func(key []byte, member rebuildGitMember) error {
		if !validRebuildNamespaceKey(string(key)) || out[string(key)] != 0 {
			return fmt.Errorf("%w: invalid or duplicate Git path", errRebuildGitMembership)
		}
		if len(out) == rebuildNamespaceEntryLimit || len(key) > remaining {
			return fmt.Errorf("%w: %w", errRebuildGitMembership, errRebuildCppCaptureLimit)
		}
		remaining -= len(key)
		out[string(key)] = member
		return nil
	}
	for _, batch := range [...]struct {
		data  []byte
		stage bool
	}{{tracked, true}, {others, false}} {
		for left := batch.data; len(left) != 0; {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			row, tail, ended := bytes.Cut(left, []byte{0})
			if !ended {
				return nil, fmt.Errorf("%w: missing Git record terminator", errRebuildGitMembership)
			}
			left = tail
			member := rebuildGitUntracked
			if batch.stage {
				header, key, ok := bytes.Cut(row, []byte{'\t'})
				mode, oidStage, spaced := strings.Cut(string(header), " ")
				oid, stage, complete := strings.Cut(oidStage, " ")
				if !ok || !spaced || !complete || stage != "0" || !rebuildGitIndexOID(oid) {
					return nil, fmt.Errorf("%w: malformed or unmerged Git index entry", errRebuildGitMembership)
				}
				row = key
				switch mode {
				case "100644", "100755", "120000":
					member = rebuildGitTracked
				case "160000":
					member = rebuildGitBoundary
				default:
					return nil, fmt.Errorf("%w: unknown Git index type", errRebuildGitMembership)
				}
			} else if key, nested := bytes.CutSuffix(row, []byte{'/'}); nested {
				row, member = key, rebuildGitBoundary
			}
			if err := add(row, member); err != nil {
				return nil, err
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func rebuildGitIndexOID(oid string) bool {
	if len(oid) != 40 && len(oid) != 64 {
		return false
	}
	for _, c := range oid {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
