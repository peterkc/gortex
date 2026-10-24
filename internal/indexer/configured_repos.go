package indexer

import (
	"context"
	"strings"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/pathkey"
)

// ResolveConfiguredRepos selects one configured root per prefix for startup
// and lifecycle discovery. Live ownership wins. A published catalog owner
// wins only while configured under that prefix; otherwise config order decides.
// Returned entries pin the resolved name without changing the config list.
func (mi *MultiIndexer) ResolveConfiguredRepos(ctx context.Context, entries []config.RepoEntry) []config.RepoEntry {
	canonicalize := mi.canonicalRoot
	if canonicalize == nil {
		canonicalize = pathkey.CanonicalPath
	}
	if mi.configMgr != nil {
		snapshot := mi.configMgr.RepoPathSnapshot()
		fallback := canonicalize
		canonicalize = func(root string) string { return snapshot.CanonicalRoot(root, fallback) }
	}
	owners := make(map[string]string)
	metadata := mi.AllMetadata()
	for prefix, meta := range metadata {
		if meta != nil {
			owners[prefix] = meta.RootPath
		}
	}
	var catalog *store_sqlite.Catalog
	if provider, ok := mi.graph.(interface{ Catalog() *store_sqlite.Catalog }); ok {
		catalog = provider.Catalog()
	}
	type registeredRoot struct{ root, prefix string }
	registeredRoots := make(map[string][]registeredRoot, len(metadata))
	for prefix, meta := range metadata {
		if meta != nil {
			root := canonicalize(meta.RootPath)
			key := configuredRootKey(root)
			registeredRoots[key] = append(registeredRoots[key], registeredRoot{root, prefix})
		}
	}
	prefixes, roots := make([]string, len(entries)), make([]string, len(entries))
	configured := make(map[string][]string)
	for i, entry := range entries {
		roots[i] = canonicalize(entry.Path)
		prefixes[i] = EffectiveRepoPrefix(mi.configMgr, entry)
		for _, registered := range registeredRoots[configuredRootKey(roots[i])] {
			if pathkey.SamePathIdentity(registered.root, roots[i]) {
				prefixes[i] = registered.prefix
				break
			}
		}
		configured[prefixes[i]] = append(configured[prefixes[i]], roots[i])
	}
	seen := make(map[string]bool)
	out := make([]config.RepoEntry, 0, len(entries))
	for i, entry := range entries {
		abs, prefix := roots[i], prefixes[i]
		if prefix == "" || prefix == "." {
			continue
		}
		var owner string
		var held bool
		implicitInstance := entry.Name == "" && prefix != config.ResolvePrefix(entry)
		for {
			owner, held = owners[prefix]
			if !held && catalog != nil {
				binding, found, err := catalog.GetDedicatedGraph(ctx, GraphIDFor(prefix))
				if err != nil {
					mi.logger.Warn("reading configured repository owner; using config order", zap.Error(err))
				} else if found {
					_, published, err := catalog.DedicatedBasePublication(ctx, binding.GraphID)
					if err != nil {
						mi.logger.Warn("reading configured repository publication; using config order", zap.Error(err))
					}
					if err != nil || !published {
						break
					}
					checkout, found, err := catalog.GetCheckout(ctx, binding.OwnerCheckoutID)
					if err != nil {
						mi.logger.Warn("reading configured repository owner; using config order", zap.Error(err))
					} else if found {
						root := canonicalize(checkout.RootPath)
						for _, configuredRoot := range configured[prefix] {
							if pathkey.SamePathIdentity(root, configuredRoot) {
								owner, held = checkout.RootPath, true
								break
							}
						}
					}
				}
			}
			if implicitInstance && held && !pathkey.SamePathIdentity(canonicalize(owner), abs) && !sameDirectory(owner, abs) {
				prefix = WorktreeInstanceCollisionName(prefix, abs)
				implicitInstance = false // Only one suffix, then enforce ownership normally.
				continue
			}
			break
		}
		if held && !pathkey.SamePathIdentity(canonicalize(owner), abs) && !sameDirectory(owner, abs) {
			mi.logCollision(NewPrefixCollisionError(prefix, owner, abs))
			continue
		}
		if seen[prefix] {
			continue
		}
		owners[prefix], seen[prefix] = abs, true
		entry.Name = prefix
		out = append(out, entry)
	}
	return out
}

// configuredPrefixCollision protects configured owners that warmup has not
// loaded yet. Filter by base name before resolving workspaces: the ordinary
// one-root case needs no workspace reads or catalog ownership pass.
func (mi *MultiIndexer) configuredPrefixCollision(prefix, root string) error {
	if mi.configMgr == nil {
		return nil
	}
	if mi.configMgr.RepoPathSnapshot().IsOnlyRepoForPrefix(prefix, root) {
		return nil
	}
	var candidates []config.RepoEntry
	for _, entry := range mi.configMgr.Global().ReposSnapshot() {
		base := config.ResolvePrefix(entry)
		if prefix == base || entry.Name == "" && strings.HasPrefix(prefix, base+"@") {
			candidates = append(candidates, entry)
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	if len(candidates) == 1 && (pathkey.SamePathIdentity(candidates[0].Path, root) || sameDirectory(candidates[0].Path, root)) {
		return nil
	}
	for _, entry := range mi.ResolveConfiguredRepos(context.Background(), candidates) {
		if entry.Name == prefix && !pathkey.SamePathIdentity(entry.Path, root) && !sameDirectory(entry.Path, root) {
			return NewPrefixCollisionError(prefix, entry.Path, root)
		}
	}
	return nil
}

// Bucket only; SamePathIdentity still distinguishes case-sensitive directories.
func configuredRootKey(root string) string {
	root = pathkey.Normalize(root)
	if pathkey.CaseInsensitivePaths {
		return strings.ToLower(root)
	}
	return root
}

// Each registration resolves configured ownership once. A changed repository
// snapshot invalidates the result before the coordinated recheck. Live ownership
// is checked separately under the registry lock at every admission boundary.
func (mi *MultiIndexer) configuredPrefixCheck() func(string, string) error {
	var snapshot *config.RepoPathSnapshot
	var checkedPrefix, checkedRoot string
	var collision error
	return func(prefix, root string) error {
		if mi.configMgr == nil {
			return nil
		}
		current := mi.configMgr.RepoPathSnapshot()
		if current != snapshot || prefix != checkedPrefix || root != checkedRoot {
			collision = mi.configuredPrefixCollision(prefix, root)
			snapshot, checkedPrefix, checkedRoot = current, prefix, root
		}
		return collision
	}
}
