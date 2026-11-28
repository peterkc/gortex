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
	build *store_pebble.BuildStore, census *rebuildExtractorCensus, classifications map[string]rebuildClassificationFact,
	opts store_pebble.RebuildLinkOptions, skipPlaceholderSources bool,
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
	// A legacy sidecar without a root observation cannot certify a bridge
	// miss. New checked checkout captures always include this row.
	dependencyContractsReady := false
	if digest != (rebuildDigest{}) {
		if err := loadConfigs(); err != nil {
			return err
		}
		if root, ok := goFacts["go.mod"]; ok {
			if err := seedRebuildDependencyContracts(ctx, build, root); err != nil {
				return err
			}
			dependencyContractsReady = true
		}
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
		if err != nil {
			return err
		}
		return nil
	}
	rawPath := func(file string) (string, error) {
		raw := census.originByGraphPath[file]
		if census.byPath[raw] == "" {
			return "", fmt.Errorf("%w: no admitted receipt for %q", errRebuildCallerAttribution, file)
		}
		return raw, nil
	}
	var glob *rebuildWorkspaceGlob
	loadGlob := func() error {
		if glob != nil {
			return nil
		}
		if err := loadNamespace(); err != nil {
			return err
		}
		var err error
		glob, err = newRebuildWorkspaceGlob(ctx, ns)
		return err
	}
	var npm *rebuildNpmLookup
	loadNpm := func() error {
		if npm != nil {
			return nil
		}
		if err := loadGlob(); err != nil {
			return err
		}
		npm = newRebuildNpmLookupFromNamespace(ctx, ns, manifests, followed, glob, ns.probeFile)
		return nil
	}
	var workspaceMembers map[string]struct{} // nil until one checked lookup builds the membership index
	view := build.RebuildView()
	var heldImportPoint func(string, bool) (string, error)
	if rebuildHasHeldInput(census, classifications) {
		heldImportPoint = func(id string, file bool) (string, error) {
			language, kind, err := view.RebuildHeldImportPoint(ctx, id)
			if file && kind != graph.KindFile {
				return "", err
			}
			return language, err
		}
	}
	opts.ScratchParent = lease
	return resolver.RunRebuildMain(ctx, build, opts, resolver.RebuildMainInputs{
		SelectCallers:            census.selectPendingCallers,
		Receipts:                 census.receipts,
		DependencyContractsReady: dependencyContractsReady,
		SkipPlaceholderSources:   skipPlaceholderSources,
		HeldImportPoint:          heldImportPoint,
		MarkImportIsolation: func(caller, spec, targetFile, deferred, cause string) error {
			return markRebuildImportIsolation(ctx, build, census, caller, spec, targetFile, deferred, cause)
		},
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
			return resolveRebuildAffectedCandidate(ctx, build, ns, census, classifications, caller, spec, "ts-alias", func(probe func(string) (bool, error)) (string, error) {
				stem, err := aliases.resolve(probe, caller, spec)
				return string(stem), err
			})
		},
		RewriteNpm: func(file, spec string) (string, error) {
			caller, err := rawPath(file)
			if err != nil {
				return "", err
			}
			if err := loadNpm(); err != nil {
				return "", err
			}
			return npm.rewriteAffected(build, census, classifications, caller, spec)
		},
		ExternalNpmDependency: func(file, spec string) (bool, error) {
			caller, err := rawPath(file)
			if err != nil {
				return false, err
			}
			if err := loadNpm(); err != nil {
				return false, err
			}
			return npm.declaresExternal(caller, spec)
		},
		WorkspaceMembership: func(file string) (string, error) {
			if workspaceMembers == nil {
				if err := loadNamespace(); err != nil {
					return "", err
				}
				manifest, err := openRebuildWorkspaceManifest(ctx, lease, digest, ns, manifests["package.json"], followed)
				if err != nil {
					return "", err
				}
				if manifest != nil {
					if err := loadGlob(); err != nil {
						return "", err
					}
				}
				workspaceMembers, err = newRebuildWorkspaceMembers(ctx, glob, manifest)
				if err != nil {
					return "", err
				}
			}
			return rebuildWorkspaceMember(workspaceMembers, file), nil
		},
		CheckGoExtern: func(importPath string) error {
			if err := loadConfigs(); err != nil {
				return err
			}
			row, err := goFacts.lookup("go.mod")
			if err != nil {
				return err
			}
			// The worker has already rejected tracked repositories and dependency
			// exports. A versioned replacement has no local directory constraint.
			_, err = row.rootReplaceBlocked(importPath)
			return err
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
