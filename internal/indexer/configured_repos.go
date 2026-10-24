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
	registeredRoots := make(map[string]string, len(metadata))
	for prefix, meta := range metadata {
		if meta != nil {
			registeredRoots[pathkey.CanonicalPath(meta.RootPath)] = prefix
		}
	}
	prefixes, roots := make([]string, len(entries)), make([]string, len(entries))
	configured := make(map[string][]string)
	for i, entry := range entries {
		roots[i] = pathkey.CanonicalPath(entry.Path)
		prefixes[i] = EffectiveRepoPrefix(mi.configMgr, entry)
		for root, registered := range registeredRoots {
			if pathkey.SamePathIdentity(root, roots[i]) {
				prefixes[i] = registered
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
						root := pathkey.CanonicalPath(checkout.RootPath)
						for _, configuredRoot := range configured[prefix] {
							if pathkey.SamePathIdentity(root, configuredRoot) {
								owner, held = checkout.RootPath, true
								break
							}
						}
					}
				}
			}
			if implicitInstance && held && !pathkey.SamePathIdentity(pathkey.CanonicalPath(owner), abs) && !sameDirectory(owner, abs) {
				prefix = WorktreeInstanceCollisionName(prefix, abs)
				implicitInstance = false // Only one suffix, then enforce ownership normally.
				continue
			}
			break
		}
		if held && !pathkey.SamePathIdentity(pathkey.CanonicalPath(owner), abs) && !sameDirectory(owner, abs) {
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
