package source

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// ObserveFrozenLiteralRootPath checks one canonical checkout-relative path
// without following any symlink in it. It does not prove admission or absence
// and the observation does not survive a change to the checkout.
func ObserveFrozenLiteralRootPath(ctx context.Context, root *os.Root, rel string) (FrozenRootPath, error) {
	if path.Clean(rel) != rel || !filepath.IsLocal(filepath.FromSlash(rel)) || strings.ContainsAny(rel, "\\\x00") {
		return FrozenRootPath{}, fmt.Errorf("invalid literal path: %w", ErrFrozenRootPath)
	}
	return resolveFrozenRootPath(ctx, root, rel, nil, true)
}

// VisitFrozenDirectory supplies the immediate child names and types for one
// directory the caller's admission policy has allowed. It does not descend,
// follow links, read contents, or distinguish Git-admitted and denied names.
// The visitor receives tentative entries in filesystem order. Only a nil
// result marks the listing complete, including an empty directory. The caller
// must discard observations on failure and may not infer case/Unicode lookup
// equality from a successful listing. The checks do not make an atomic snapshot.
func VisitFrozenDirectory(ctx context.Context, root *os.Root, rel string, visit func(name string, mode fs.FileMode) error) (err error) {
	if visit == nil {
		return fmt.Errorf("missing directory visitor: %w", ErrFrozenRootPath)
	}
	obs, err := ObserveFrozenLiteralRootPath(ctx, root, rel)
	if err != nil {
		return err
	}
	dir, err := obs.OpenDir(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, dir.Close()) }()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		// ReadDir can silently skip a name that disappears during its type
		// lookup. First read just names, then check the type through our root.
		names, readErr := dir.Readdirnames(128)
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return readErr
		}
		for _, name := range names {
			if err := ctx.Err(); err != nil {
				return err
			}
			key := path.Join(rel, name)
			info, err := root.Lstat(filepath.FromSlash(key))
			if err != nil {
				return fmt.Errorf("listed path %q: %w: %w", key, ErrFrozenRootChanged, err)
			}
			if err := visit(name, info.Mode().Type()); err != nil {
				return err
			}
		}
		if errors.Is(readErr, io.EOF) {
			return errors.Join(obs.checkDescriptor(dir), obs.checkCurrent(ctx))
		}
	}
}
