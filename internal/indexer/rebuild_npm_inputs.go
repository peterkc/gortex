package indexer

import (
	"context"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"strings"
)

var errRebuildNpmInput = errors.New("unproved frozen npm input")

const rebuildNpmFactBytes = 1 << 20

// Each callback returns a miss only for a proved negative. No callback may
// read an ignored, non-admitted or out-of-root path. The caller owns checked
// traversal and the shared raw/encoded sidecar limits, and must apply limit
// before reading or collecting results. The same callbacks can replay facts.
type rebuildNpmInputs struct {
	readFile  func(context.Context, string, int) ([]byte, bool, error)
	isFile    func(context.Context, string) (bool, error)
	matchDirs func(context.Context, string, int) ([]string, error)
}

// A legacy manifestTree reports misses without errors. Keep a sticky error
// and refuse the whole tentative answer rather than treating a failed read
// as a missing manifest or proceeding to the next native probe. Use one
// instance serially for a lease; discard it after any error.
type rebuildNpmLookup struct {
	ctx context.Context
	in  rebuildNpmInputs
	idx *npmAliasIndex
	err error
}

func newRebuildNpmLookup(ctx context.Context, in rebuildNpmInputs) *rebuildNpmLookup {
	x := &rebuildNpmLookup{ctx: ctx, in: in}
	if in.readFile == nil || in.isFile == nil || in.matchDirs == nil {
		x.err = fmt.Errorf("%w: missing checked callback", errRebuildNpmInput)
	}
	// A fixed cache namespace, not the checkout path. The provider always
	// returns x, so npmAliasIndex.tree cannot fall back to a disk tree.
	x.idx = newNpmAliasIndexWithTrees(map[string]string{"": x.root()}, func(string) manifestTree { return x })
	return x
}

func (x *rebuildNpmLookup) rewrite(caller, spec string) (string, error) {
	if !x.beginQuery(caller, spec) {
		return "", x.err
	}
	out := x.idx.Resolve(caller, spec)
	if err := errors.Join(x.err, x.ctx.Err()); err != nil {
		return "", err
	}
	return out, nil
}

func (x *rebuildNpmLookup) declaresExternal(caller, spec string) (bool, error) {
	if !x.beginQuery(caller, spec) {
		return false, x.err
	}
	out := x.idx.DeclaresExternalDependency(caller, spec)
	if err := errors.Join(x.err, x.ctx.Err()); err != nil {
		return false, err
	}
	return out, nil
}

func (x *rebuildNpmLookup) root() string  { return "." }
func (x *rebuildNpmLookup) sourced() bool { return true }

func (x *rebuildNpmLookup) fail(op, value string, cause error) {
	if x.err == nil {
		x.err = fmt.Errorf("%w: %s %q: %w", errRebuildNpmInput, op, value, cause)
	}
}

func (x *rebuildNpmLookup) beginQuery(caller, spec string) bool {
	if caller == "." || path.Clean(caller) != caller || len(spec) > rebuildNpmFactBytes || strings.ContainsRune(spec, 0) {
		x.fail("npm query", "", errors.New("invalid caller or specifier"))
	}
	return x.begin("import caller", caller)
}

func (x *rebuildNpmLookup) begin(op, rel string) bool {
	if x.err != nil {
		return false
	}
	if err := x.ctx.Err(); err != nil {
		x.err = err
		return false
	}
	// Pass native spelling unchanged. Do not collapse raw path tokens that
	// could cross a symlink before a '..'; checked traversal owns that proof.
	if len(rel) > rebuildNpmFactBytes || rel == "" || !filepath.IsLocal(filepath.FromSlash(rel)) || strings.ContainsRune(rel, 0) {
		x.fail(op, "", errors.New("nonlocal or oversized fact key"))
	}
	return x.err == nil
}

func (x *rebuildNpmLookup) readFile(rel string) ([]byte, bool) {
	if !x.begin("manifest", rel) {
		return nil, false
	}
	data, present, err := x.in.readFile(x.ctx, rel, rebuildNpmFactBytes-len(rel))
	if err == nil && (len(data) > rebuildNpmFactBytes-len(rel) || !present && len(data) != 0) {
		err = errors.New("invalid manifest fact")
	}
	if err != nil {
		x.fail("manifest", rel, err)
		return nil, false
	}
	return data, present
}

func (x *rebuildNpmLookup) isFile(rel string) bool {
	if !x.begin("file probe", rel) {
		return false
	}
	present, err := x.in.isFile(x.ctx, rel)
	if err != nil {
		x.fail("file probe", rel, err)
		return false
	}
	return present
}

func (x *rebuildNpmLookup) matchDirs(pattern string) []string {
	if pattern == "" { // diskManifestTree.match proves this miss without I/O.
		return nil
	}
	if !x.begin("workspace glob", pattern) {
		return nil
	}
	left := rebuildNpmFactBytes - len(pattern)
	dirs, err := x.in.matchDirs(x.ctx, pattern, left)
	if err == nil {
		for i, dir := range dirs {
			if len(dir) >= left || path.Clean(dir) != dir || !filepath.IsLocal(filepath.FromSlash(dir)) ||
				strings.ContainsRune(dir, 0) || i > 0 && dirs[i-1] >= dir {
				err = errors.New("invalid ordered workspace directories")
				break
			}
			left -= len(dir) + 1
		}
	}
	if err != nil {
		x.fail("workspace glob", pattern, err)
		return nil
	}
	return dirs
}

func (x *rebuildNpmLookup) unsupported(op string) {
	if x.begin(op, ".") {
		x.fail(op, ".", errors.New("unexpected npm tree operation"))
	}
}

func (x *rebuildNpmLookup) isDir(string) bool {
	x.unsupported("directory probe")
	return false
}

func (x *rebuildNpmLookup) matchFiles(string) []string {
	x.unsupported("file glob")
	return nil
}

func (x *rebuildNpmLookup) topLevelDirs(...string) map[string]bool {
	x.unsupported("root listing")
	return nil
}
