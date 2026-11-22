package indexer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
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
	tracked []string    // byte-sorted non-gitlink index entries
	marker  fs.FileInfo // checked .git file or directory; never opened as source
	stage   rebuildDigest
	version string
}

// readRebuildGitInventory captures only stage-0 paths and the Git decoder's
// identity. Untracked paths must use point ignore decisions during the walk;
// neither an index hit nor an index miss can close a directory.
func readRebuildGitInventory(ctx context.Context, root *os.Root, rawRemaining *int64) (rebuildGitInventory, error) {
	if root == nil || !filepath.IsAbs(root.Name()) || rawRemaining == nil || *rawRemaining < 0 || *rawRemaining > rebuildCppRawLimit {
		return rebuildGitInventory{}, errRebuildGitMembership
	}
	// Git can report the right worktree but select a different index. Do
	// not accept caller-inherited repository redirection on this route.
	for _, key := range [...]string{
		"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR", "GIT_PREFIX",
		"GIT_IMPLICIT_WORK_TREE", "GIT_CONFIG", "GIT_CONFIG_COUNT", "GIT_CONFIG_PARAMETERS",
	} {
		if _, set := os.LookupEnv(key); set {
			return rebuildGitInventory{}, fmt.Errorf("%w: repository selector %s is set", errRebuildGitMembership, key)
		}
	}
	if err := checkRebuildGitWorktreeRoot(root); err != nil {
		return rebuildGitInventory{}, err
	}
	marker, err := root.Lstat(".git")
	if err != nil {
		return rebuildGitInventory{}, fmt.Errorf("%w: checkout has no checked Git marker: %w", errRebuildGitMembership, err)
	}
	if !marker.IsDir() && !marker.Mode().IsRegular() {
		return rebuildGitInventory{}, fmt.Errorf("%w: unsupported Git marker type", errRebuildGitMembership)
	}
	ctx, cancel := context.WithTimeout(ctx, rebuildGitIgnoreBatchTimeout)
	defer cancel()
	dir := root.Name()
	run := func(limit int64, args ...string) ([]byte, error) {
		out, stderr, err := gitcmd.RunNoLazyBoundedStdin(ctx, dir, nil, min(limit, *rawRemaining), args...)
		if err != nil {
			if errors.Is(err, gitcmd.ErrOutputLimit) {
				err = errors.Join(err, errRebuildCppCaptureLimit)
			}
			return nil, fmt.Errorf("%w: %w", errRebuildGitMembership, err)
		}
		if stderr {
			return nil, fmt.Errorf("%w: Git wrote to stderr", errRebuildGitMembership)
		}
		*rawRemaining -= int64(len(out))
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
	if !ended || !validRebuildGitVersion(v) {
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
	current, err := root.Lstat(".git")
	if err != nil || !sameRebuildPathMetadata(marker, current) {
		return rebuildGitInventory{}, fmt.Errorf("%w: Git marker changed: %v", ErrBuildInputChanged, err)
	}
	var files []string
	for key, member := range members {
		if member == rebuildGitTracked {
			files = append(files, key)
		}
	}
	slices.Sort(files)
	return rebuildGitInventory{members: members, tracked: files, marker: marker, stage: blake3.Sum256(tracked), version: v}, nil
}

func (g rebuildGitInventory) hasTrackedDescendant(dir string) bool {
	prefix := dir + "/"
	i, _ := slices.BinarySearch(g.tracked, prefix)
	return i < len(g.tracked) && strings.HasPrefix(g.tracked[i], prefix)
}

func (g rebuildGitInventory) checkRoot(root *os.Root) error {
	if err := checkRebuildGitWorktreeRoot(root); err != nil {
		return err
	}
	current, err := root.Lstat(".git")
	if err != nil || !sameRebuildPathMetadata(g.marker, current) {
		return fmt.Errorf("%w: selected Git marker changed: %v", ErrBuildInputChanged, err)
	}
	return nil
}

func sameRebuildPathMetadata(a, b fs.FileInfo) bool {
	return a != nil && b != nil && os.SameFile(a, b) && a.Mode() == b.Mode() && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
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
