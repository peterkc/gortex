package indexer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/zeebo/blake3"
	"github.com/zzet/gortex/internal/gitcmd"
)

var errRebuildGitMembership = errors.New("unproved rebuild Git membership")

type rebuildGitMember uint8

const (
	rebuildGitTracked rebuildGitMember = iota + 1
	rebuildGitUntracked
	rebuildGitBoundary
)

type rebuildGitInventory struct {
	members map[string]rebuildGitMember
	stage   rebuildDigest
	version string
}

// readRebuildGitInventory captures only stage-0 paths and the Git decoder's
// identity. Untracked paths must use point ignore decisions during the walk;
// neither an index hit nor an index miss can close a directory.
func readRebuildGitInventory(ctx context.Context, root *os.Root) (rebuildGitInventory, error) {
	if root == nil || !filepath.IsAbs(root.Name()) {
		return rebuildGitInventory{}, errRebuildGitMembership
	}
	// Git can report the right worktree but select a different index. Do
	// not accept caller-inherited repository redirection on this route.
	for _, key := range [...]string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR", "GIT_PREFIX"} {
		if _, set := os.LookupEnv(key); set {
			return rebuildGitInventory{}, fmt.Errorf("%w: repository selector %s is set", errRebuildGitMembership, key)
		}
	}
	if err := checkRebuildGitWorktreeRoot(root); err != nil {
		return rebuildGitInventory{}, err
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
		return rebuildGitInventory{}, err
	}
	if string(location) != want {
		return rebuildGitInventory{}, fmt.Errorf("%w: held root is not the Git worktree root", errRebuildGitMembership)
	}
	version, err := run(128, "--version")
	if err != nil {
		return rebuildGitInventory{}, err
	}
	v, ended := strings.CutSuffix(string(version), "\n")
	if !ended || !strings.HasPrefix(v, "git version ") || len(v) == len("git version ") ||
		strings.ContainsFunc(v, func(c rune) bool { return c < ' ' || c > '~' }) {
		return rebuildGitInventory{}, fmt.Errorf("%w: malformed Git version", errRebuildGitMembership)
	}
	tracked, err := run(rebuildCppRawLimit, "ls-files", "--stage", "-z")
	if err != nil {
		return rebuildGitInventory{}, err
	}
	if err := checkRebuildGitWorktreeRoot(root); err != nil {
		return rebuildGitInventory{}, err
	}
	members, err := parseRebuildGitIndex(ctx, tracked)
	if err != nil {
		return rebuildGitInventory{}, err
	}
	return rebuildGitInventory{members: members, stage: blake3.Sum256(tracked), version: v}, nil
}

// Check the process path against the held directory. A replaced pathname is
// not authority for Git metadata, even if rev-parse returns the same spelling.
func checkRebuildGitWorktreeRoot(root *os.Root) error {
	held, err := root.Stat(".")
	if err != nil {
		return fmt.Errorf("%w: held worktree root: %w", errRebuildGitMembership, err)
	}
	live, err := os.Lstat(root.Name())
	if err != nil {
		return fmt.Errorf("%w: worktree pathname: %w", ErrBuildInputChanged, err)
	}
	if !live.IsDir() || !os.SameFile(held, live) {
		return fmt.Errorf("%w: worktree pathname changed", ErrBuildInputChanged)
	}
	return nil
}

func parseRebuildGitIndex(ctx context.Context, tracked []byte) (map[string]rebuildGitMember, error) {
	if len(tracked) > rebuildCppRawLimit {
		return nil, fmt.Errorf("%w: %w", errRebuildGitMembership, errRebuildCppCaptureLimit)
	}
	out := make(map[string]rebuildGitMember)
	remaining := rebuildNamespaceKeyBytesLimit
	for left := tracked; len(left) != 0; {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		row, tail, ended := bytes.Cut(left, []byte{0})
		if !ended {
			return nil, fmt.Errorf("%w: missing Git record terminator", errRebuildGitMembership)
		}
		left = tail
		header, key, ok := bytes.Cut(row, []byte{'\t'})
		if !ok || len(header) > 73 {
			return nil, fmt.Errorf("%w: malformed Git index entry", errRebuildGitMembership)
		}
		mode, oidStage, spaced := strings.Cut(string(header), " ")
		oid, stage, complete := strings.Cut(oidStage, " ")
		if !spaced || !complete || stage != "0" || !rebuildGitIndexOID(oid) {
			return nil, fmt.Errorf("%w: malformed or unmerged Git index entry", errRebuildGitMembership)
		}
		var member rebuildGitMember
		switch mode {
		case "100644", "100755", "120000":
			member = rebuildGitTracked
		case "160000":
			member = rebuildGitBoundary
		default:
			return nil, fmt.Errorf("%w: unknown Git index type", errRebuildGitMembership)
		}
		if !validRebuildNamespaceKey(string(key)) || out[string(key)] != 0 {
			return nil, fmt.Errorf("%w: invalid or duplicate Git path", errRebuildGitMembership)
		}
		if len(out) == rebuildNamespaceEntryLimit || len(key) > remaining {
			return nil, fmt.Errorf("%w: %w", errRebuildGitMembership, errRebuildCppCaptureLimit)
		}
		remaining -= len(key)
		out[string(key)] = member
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
