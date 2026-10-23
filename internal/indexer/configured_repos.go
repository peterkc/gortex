package indexer

import (
	"context"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/pathkey"
)

// ResolveConfiguredRepos selects one root per prefix before parallel warmup.
// Existing corpus and catalog ownership win; otherwise config order decides.
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
	seen := make(map[string]bool)
	out := make([]config.RepoEntry, 0, len(entries))
	for _, entry := range entries {
		abs := pathkey.CanonicalPath(entry.Path)
		prefix := EffectiveRepoPrefix(mi.configMgr, entry)
		for registered, meta := range metadata {
			if meta != nil && pathkey.SamePathIdentity(pathkey.CanonicalPath(meta.RootPath), abs) {
				prefix = registered
				break
			}
		}
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
					checkout, found, err := catalog.GetCheckout(ctx, binding.OwnerCheckoutID)
					if err != nil {
						mi.logger.Warn("reading configured repository owner; using config order", zap.Error(err))
					} else if found {
						owner, held = checkout.RootPath, true
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
			mi.logCollision(&PrefixCollisionError{Prefix: prefix, ExistingPath: owner, Path: abs})
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
