package indexer

import (
	"context"
	"fmt"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/resolver"
)

// runRebuildFrozenMain mutates only private scratch. Callbacks authorize
// filesystem-shaped queries from the exact receipt origin and the sealed
// sidecar. Unproved input is an error, not an unresolved import result.
func runRebuildFrozenMain(ctx context.Context, lease string, digest rebuildDigest,
	build *store_pebble.BuildStore, census *rebuildExtractorCensus, opts store_pebble.RebuildLinkOptions,
) error {
	var goFacts rebuildGoManifests
	var aliases rebuildTSAliasConfigs
	loadConfigs := func() error {
		if goFacts != nil {
			return nil
		}
		var err error
		goFacts, aliases, err = openRebuildResolverConfigFacts(ctx, lease, digest)
		return err
	}
	var ns *rebuildResolverNamespace
	var manifests map[string]rebuildNpmManifestFact
	var followed rebuildDigest
	loadNamespace := func() error {
		if ns != nil {
			return nil
		}
		var err error
		ns, manifests, followed, err = openRebuildNpmManifestFacts(ctx, lease, digest)
		return err
	}
	rawPath := func(file string) (string, error) {
		raw := census.originByGraphPath[file]
		if census.byPath[raw] == "" {
			return "", fmt.Errorf("%w: no admitted receipt for %q", errRebuildCallerAttribution, file)
		}
		return raw, nil
	}
	var npm *rebuildNpmLookup
	view := build.RebuildView()
	opts.ScratchParent = lease
	return resolver.RunRebuildMain(ctx, build, opts, resolver.RebuildMainInputs{
		SelectCallers: census.selectPendingCallers,
		PathAlias: func(file, spec string) (string, error) {
			caller, err := rawPath(file)
			if err != nil {
				return "", err
			}
			if err := loadConfigs(); err != nil {
				return "", err
			}
			if err := loadNamespace(); err != nil {
				return "", err
			}
			stem, err := aliases.resolve(ns, caller, spec)
			// The private checkout has no repo prefix. Keep native's
			// returned stem as the graph probe; a first-target fallback
			// need not have an admitted file to translate.
			return string(stem), err
		},
		RewriteNpm: func(file, spec string) (string, error) {
			caller, err := rawPath(file)
			if err != nil {
				return "", err
			}
			if err := loadNamespace(); err != nil {
				return "", err
			}
			if npm == nil {
				glob, err := newRebuildWorkspaceGlob(ctx, ns)
				if err != nil {
					return "", err
				}
				npm = newRebuildNpmLookup(ctx, rebuildNpmInputs{
					namespace: ns, manifests: manifests, followedRoot: followed,
					isFile:    func(_ context.Context, key string) (bool, error) { return ns.probeFile(key) },
					matchDirs: glob.matchDirs,
				})
			}
			return npm.rewrite(caller, spec)
		},
		GoOwnership: func(candidate resolver.GoImportCandidate) (resolver.GoPackageOwnershipResult, error) {
			hasGoFile := func(file string) (bool, error) {
				n, err := view.Node(ctx, file)
				return err == nil && n != nil && n.ID == file && n.FilePath == file && n.RepoPrefix == "" &&
					n.Kind == graph.KindFile && n.Language == "go", err
			}
			manifest := func(key string) (rebuildGoManifest, error) {
				if err := loadConfigs(); err != nil {
					return rebuildGoManifest{}, err
				}
				return goFacts.lookup(key)
			}
			return rebuildGoOwnership(candidate, hasGoFile, rawPath, manifest)
		},
	})
}
