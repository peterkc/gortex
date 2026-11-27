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

// All inputs come from one verified frozen sidecar. A manifest row is a
// positive fact even when it has no useful fields. File and glob callbacks
// return misses only for proved negatives and must not consult live files.
type rebuildNpmInputs struct {
	namespace    *rebuildResolverNamespace
	manifests    map[string]rebuildNpmManifestFact
	followedRoot rebuildDigest // verified target digest for a root package.json symlink
	isFile       func(context.Context, string) (bool, error)
	matchDirs    func(context.Context, string, int) ([]string, error)
}

// A legacy manifestTree reports misses without errors. Keep a sticky error
// and refuse the whole tentative answer rather than treating a failed read
// as a missing manifest or proceeding to the next native probe. Use one
// instance serially for a lease; discard it after any error.
type rebuildNpmLookup struct {
	ctx           context.Context
	in            rebuildNpmInputs
	idx           *npmAliasIndex
	err           error
	workspaceLeft int // bound the whole workspaceNames pre-expansion, including repeats
}

func newRebuildNpmLookup(ctx context.Context, in rebuildNpmInputs) *rebuildNpmLookup {
	x := &rebuildNpmLookup{ctx: ctx, in: in, workspaceLeft: rebuildNpmFactBytes}
	if in.namespace == nil || !in.namespace.frozen || in.isFile == nil || in.matchDirs == nil {
		x.err = fmt.Errorf("%w: missing checked npm inputs", errRebuildNpmInput)
	}
	// A fixed cache namespace, not the checkout path. The provider always
	// returns x, so npmAliasIndex.tree cannot fall back to a disk tree.
	x.idx = newNpmAliasIndexWithTrees(map[string]string{"": x.root()}, func(string) manifestTree { return x })
	x.idx.frozenManifest = x.manifest
	return x
}

func newRebuildNpmLookupFromNamespace(ctx context.Context, ns *rebuildResolverNamespace,
	manifests map[string]rebuildNpmManifestFact, followed rebuildDigest, glob *rebuildWorkspaceGlob,
) *rebuildNpmLookup {
	return newRebuildNpmLookup(ctx, rebuildNpmInputs{
		namespace: ns, manifests: manifests, followedRoot: followed,
		isFile:    func(_ context.Context, key string) (bool, error) { return ns.probeFile(key) },
		matchDirs: glob.matchDirs,
	})
}

// rebuildExternalNpmRewrite uses the edge path as native npm resolution does.
// Only the queried manifest and package paths need filesystem evidence. A
// configured extractor can supply a recording path with no file at that path.
func rebuildExternalNpmRewrite(ctx context.Context, lease string, digest rebuildDigest) func(string, string) (string, error) {
	var npm *rebuildNpmLookup
	return func(file, spec string) (string, error) {
		if npm == nil {
			ns, manifests, followed, err := openRebuildNpmManifestFacts(ctx, lease, digest)
			if err != nil {
				return "", err
			}
			glob, err := newRebuildWorkspaceGlob(ctx, ns)
			if err != nil {
				return "", err
			}
			npm = newRebuildNpmLookupFromNamespace(ctx, ns, manifests, followed, glob)
		}
		return npm.rewrite(file, spec)
	}
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

func (x *rebuildNpmLookup) manifest(rel string) npmResolverManifest {
	if !x.begin("manifest", rel) {
		return npmResolverManifest{}
	}
	ns := x.in.namespace
	fact, present := x.in.manifests[rel]
	if rel == "package.json" && x.in.followedRoot != (rebuildDigest{}) &&
		ns.entries[rel].kind == rebuildNamespaceUnsupported && present && fact.Path == rel && fact.Digest == x.in.followedRoot {
		return fact.Manifest
	}
	kind, err := ns.lookup(rel)
	if err != nil {
		x.fail("manifest", rel, err)
		return npmResolverManifest{}
	}
	switch {
	case rel == "package.json" && x.in.followedRoot != (rebuildDigest{}):
		// A followed target proof contradicts this namespace answer.
	case kind == rebuildNamespaceAbsent && !present:
		return npmResolverManifest{}
	case kind == rebuildNamespaceRegular && present && fact.Path == rel && fact.Digest != (rebuildDigest{}):
		return fact.Manifest
	}
	x.fail("manifest", rel, fmt.Errorf("%w: namespace and manifest fact disagree", errRebuildResolverNamespace))
	return npmResolverManifest{}
}

func (x *rebuildNpmLookup) readFile(string) ([]byte, bool) {
	x.unsupported("raw manifest read")
	return nil, false
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
	left := x.workspaceLeft - len(pattern)
	if left < 0 {
		x.fail("workspace glob", pattern, errRebuildCppCaptureLimit)
		return nil
	}
	dirs, err := x.in.matchDirs(x.ctx, pattern, left)
	if err == nil {
		for i, dir := range dirs {
			if len(dir) >= left {
				err = errRebuildCppCaptureLimit
				break
			}
			if path.Clean(dir) != dir || !filepath.IsLocal(filepath.FromSlash(dir)) ||
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
	x.workspaceLeft = left
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
