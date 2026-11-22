package indexer

import (
	"context"
	"fmt"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// rebuildWorkspaceGlob expands npm workspace directories from a namespace
// returned by openRebuildResolverNamespace. The index borrows frozen keys;
// it adds at most one string reference per entry, and copies no key bytes.
type rebuildWorkspaceGlob struct {
	ns       *rebuildResolverNamespace
	children map[string][]string
}

func newRebuildWorkspaceGlob(ctx context.Context, ns *rebuildResolverNamespace) (*rebuildWorkspaceGlob, error) {
	if ns == nil || !ns.frozen || ns.entries["."].close == rebuildNamespaceUnknown {
		return nil, fmt.Errorf("%w: workspace glob needs a closed frozen root", errRebuildResolverNamespace)
	}
	g := &rebuildWorkspaceGlob{ns: ns, children: make(map[string][]string)}
	for key := range ns.entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if key != "." {
			parent := path.Dir(key)
			g.children[parent] = append(g.children[parent], key)
		}
	}
	for _, children := range g.children {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		slices.Sort(children)
	}
	return g, nil
}

// matchDirs mirrors filepath.Glob's literal prefix and listed suffix, then
// keeps directories in the order of diskManifestTree.match. Clean the pattern
// as native Join does, but never include the checkout root in the pattern.
func (g *rebuildWorkspaceGlob) matchDirs(ctx context.Context, pattern string, limit int) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if pattern == "" {
		return nil, nil
	}
	if len(pattern) > rebuildNpmFactBytes || limit < 0 || strings.ContainsRune(pattern, 0) {
		return nil, fmt.Errorf("%w: invalid workspace glob bounds or key", errRebuildResolverNamespace)
	}
	p := filepath.FromSlash(pattern)
	if !filepath.IsLocal(p) {
		return nil, fmt.Errorf("%w: nonlocal workspace glob", errRebuildResolverNamespace)
	}
	p = filepath.Clean(p)
	if _, err := filepath.Match(p, ""); err != nil {
		return nil, nil // native diskManifestTree.match suppresses malformed patterns
	}
	p = filepath.ToSlash(p)
	// ToSlash already converted the Windows separator; a remaining backslash
	// has the same escape meaning as in filepath.Glob on non-Windows hosts.
	prefix, suffix := p, ""
	if first := strings.IndexAny(p, `*?[\`); first >= 0 {
		cut := strings.LastIndexByte(p[:first], '/') + 1
		prefix, suffix = strings.TrimSuffix(p[:cut], "/"), p[cut:]
		// The right-to-left recursion in Go 1.27 Glob refuses at depth 10000.
		if strings.Count(suffix, "/") >= 10_000 {
			return nil, fmt.Errorf("%w: workspace glob depth: %w", errRebuildResolverNamespace, filepath.ErrBadPattern)
		}
	}

	var dirs []string
	left := min(limit, rebuildNpmFactBytes-len(pattern))
	add := func(key string) error {
		if len(key) >= left {
			return fmt.Errorf("%w: workspace glob result cap", errRebuildResolverNamespace)
		}
		left -= len(key) + 1
		dirs = append(dirs, key)
		return nil
	}

	dir := "."
	for start := 0; prefix != "." && start < len(prefix); {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		end := len(prefix)
		if i := strings.IndexByte(prefix[start:], '/'); i >= 0 {
			end = start + i
		}
		dir = prefix[:end]
		kind, err := g.ns.lookup(dir)
		if err != nil {
			return nil, err
		}
		if kind != rebuildNamespaceDirectory {
			return nil, nil // a proved absent name or regular file cannot be a directory
		}
		start = end + 1
	}
	if suffix == "" {
		if err := add(dir); err != nil {
			return nil, err
		}
		return dirs, nil
	}

	// Bound the work of the pinned Match (which may retry a chunk at each
	// byte of a name). Charge before each scan, independently of result size.
	work := rebuildNpmFactBytes
	var visit func(string, string) error
	visit = func(dir, suffix string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		segment, rest, more := strings.Cut(suffix, "/")
		if len(segment) >= work {
			return fmt.Errorf("%w: workspace glob work cap", errRebuildResolverNamespace)
		}
		work -= len(segment) + 1
		if _, err := filepath.Match(segment, ""); err != nil {
			return err
		}
		// Unlike the literal prefix, *every* suffix component is matched
		// against a listing, even if it has no metacharacters itself.
		if g.ns.entries[dir].close == rebuildNamespaceUnknown {
			return fmt.Errorf("%w: incomplete workspace listing at %q", errRebuildResolverNamespace, dir)
		}
		for _, key := range g.children[dir] {
			if err := ctx.Err(); err != nil {
				return err
			}
			if len(segment)+1 > work/(len(key)+1) {
				return fmt.Errorf("%w: workspace glob work cap", errRebuildResolverNamespace)
			}
			work -= (len(segment) + 1) * (len(key) + 1)
			matched, err := filepath.Match(segment, path.Base(key))
			if err != nil {
				return err
			}
			if !matched {
				continue
			}
			kind, err := g.ns.lookup(key)
			if err != nil {
				return err
			}
			if kind != rebuildNamespaceDirectory {
				continue
			}
			if more {
				err = visit(key, rest)
			} else {
				err = add(key)
			}
			if err != nil {
				return err
			}
		}
		return nil
	}
	if err := visit(dir, suffix); err != nil {
		if err == filepath.ErrBadPattern {
			return nil, nil
		}
		return nil, err
	}
	slices.Sort(dirs)
	return dirs, nil
}
