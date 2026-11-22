package indexer

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
)

// prepareRebuildGitDirectory decides which immediate, Gortex-allowed names
// can reach the leaf callback. A gitlink or nested checkout stays opaque.
func prepareRebuildGitDirectory(ctx context.Context, root *os.Root, git *rebuildGitInventory,
	entries []rebuildResolverListedEntry, raw *int64, emit func(rebuildSidecarRow) error,
) (map[string]rebuildGitMember, error) {
	admitted := make(map[string]rebuildGitMember, len(entries))
	queries := make([]string, 0, min(rebuildGitIgnoreBatchLimit, len(entries)))
	flush := func() error {
		if len(queries) == 0 {
			return nil
		}
		if err := git.checkRoot(root); err != nil {
			return err
		}
		verdicts, used, err := checkRebuildGitIgnore(ctx, root.Name(), queries, *raw)
		if err != nil {
			return err
		}
		if err := git.checkRoot(root); err != nil {
			return err
		}
		*raw -= used
		for i, query := range queries {
			verdict := verdicts[i]
			if err := emit(rebuildSidecarRow{Kind: rebuildGitIgnoreVerdictRow, Path: []byte(query), GitVerdict: verdict}); err != nil {
				return err
			}
			if query[len(query)-1] == '/' {
				key := query[:len(query)-1]
				if verdict == rebuildGitIgnoreIgnored && !git.hasTrackedDescendant(key) {
					continue
				}
				nested, err := hasRebuildNestedGitMarker(ctx, root, key)
				if err != nil {
					return err
				}
				if !nested {
					admitted[key] = rebuildGitUntracked
				}
				continue
			}
			if verdict != rebuildGitIgnoreIgnored {
				admitted[query] = rebuildGitUntracked
			}
		}
		queries = queries[:0]
		return nil
	}
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if git == nil {
			if e.mode.IsDir() {
				nested, err := hasRebuildNestedGitMarker(ctx, root, e.key)
				if err != nil {
					return nil, err
				}
				if nested {
					continue
				}
			}
			admitted[e.key] = rebuildGitUntracked
			continue
		}
		member := git.members[e.key]
		if member == rebuildGitBoundary {
			continue
		}
		if member == rebuildGitTracked && !e.mode.IsDir() {
			admitted[e.key] = member
			continue
		}
		query := e.key
		if e.mode.IsDir() {
			query += "/"
		}
		queries = append(queries, query)
		if len(queries) == rebuildGitIgnoreBatchLimit {
			if err := flush(); err != nil {
				return nil, err
			}
		}
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return admitted, nil
}

// A nested marker is metadata, never source or a gitdir to follow. Pin the
// child identity before checking its immediate marker and recheck the name
// afterwards. An ignored subtree that has no tracked member never gets here.
func hasRebuildNestedGitMarker(ctx context.Context, root *os.Root, dir string) (_ bool, err error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	before, err := root.Lstat(dir)
	if err != nil || !before.IsDir() {
		return false, fmt.Errorf("%w: nested boundary directory %q: %v", ErrBuildInputChanged, dir, err)
	}
	child, err := root.OpenRoot(dir)
	if err != nil {
		return false, fmt.Errorf("%w: hold nested boundary directory %q: %w", ErrBuildInputChanged, dir, err)
	}
	defer func() { err = errors.Join(err, child.Close()) }()
	held, err := child.Stat(".")
	if err != nil || !sameRebuildPathMetadata(before, held) {
		return false, fmt.Errorf("%w: nested boundary identity %q: %v", ErrBuildInputChanged, dir, err)
	}
	_, markerErr := child.Lstat(".git")
	if markerErr != nil && !errors.Is(markerErr, fs.ErrNotExist) {
		return false, fmt.Errorf("%w: detect nested boundary %q: %w", errRebuildGitMembership, path.Join(dir, ".git"), markerErr)
	}
	after, err := root.Lstat(dir)
	if err != nil || !sameRebuildPathMetadata(before, after) {
		return false, fmt.Errorf("%w: nested boundary changed %q: %v", ErrBuildInputChanged, dir, err)
	}
	return markerErr == nil, nil
}
