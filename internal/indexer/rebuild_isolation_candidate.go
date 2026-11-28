package indexer

import (
	"context"
	"fmt"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/pathkey"
	"github.com/zzet/gortex/internal/resolver"
)

// The first suppressed positive is native's first answer on the ordered
// candidate path. The rewrite itself may stay unchanged even though its
// held graph point is absent. Mark that caller and a selected live target.
func resolveRebuildAffectedCandidate(ctx context.Context, build *store_pebble.BuildStore,
	ns *rebuildResolverNamespace, census *rebuildExtractorCensus, classifications map[string]rebuildClassificationFact,
	caller, spec, cause string, resolve func(func(string) (bool, error)) (string, error),
) (string, error) {
	var deferred string
	probe := rebuildObservedCandidateProbe(ns, census, classifications, func(language string) {
		if deferred == "" {
			deferred = language
		}
	})
	selected, err := resolve(probe)
	if err != nil || deferred == "" || build == nil {
		return selected, err
	}
	if err := markRebuildImportIsolation(ctx, build, census, caller, spec, "", deferred, cause); err != nil {
		return "", err
	}
	if selected != "" {
		file, err := resolver.RebuildJSTSSelectedFile(pathkey.Normalize(caller), selected, cause, func(id string) (*graph.Node, error) {
			return build.RebuildView().Node(ctx, id)
		})
		if err != nil {
			return "", err
		}
		if file != "" {
			if err := markRebuildImportIsolation(ctx, build, census, caller, spec, file, deferred, cause); err != nil {
				return "", err
			}
		}
	}
	return selected, nil
}

func markRebuildImportIsolation(ctx context.Context, build *store_pebble.BuildStore, census *rebuildExtractorCensus,
	caller, spec, targetFile, deferred, cause string,
) error {
	file := pathkey.Normalize(caller)
	language := census.byPath[caller]
	if language == "" {
		language = census.byPath[census.originByGraphPath[file]]
	}
	if !rebuildLinkedExtractor(language) {
		return fmt.Errorf("%w: isolation affected query has no supported receipt for %q", errRebuildCallerAttribution, caller)
	}
	if err := build.MarkRebuildIsolationAffected(ctx, store_pebble.RebuildIsolationAffectedScope{
		FilePath: file, SupportedLanguage: language, DeferredLanguage: deferred, Cause: cause,
	}, spec); err != nil {
		return err
	}
	if targetFile != "" && targetFile != file {
		targetLanguage := census.byPath[targetFile]
		if targetLanguage == "" {
			targetLanguage = census.byPath[census.originByGraphPath[targetFile]]
		}
		if rebuildLinkedExtractor(targetLanguage) {
			return build.MarkRebuildIsolationAffected(ctx, store_pebble.RebuildIsolationAffectedScope{
				FilePath: targetFile, SupportedLanguage: targetLanguage, DeferredLanguage: deferred, Cause: cause,
			}, spec)
		}
	}
	return nil
}

func rebuildHasHeldInput(census *rebuildExtractorCensus, classifications map[string]rebuildClassificationFact) bool {
	for id := range census.byID {
		if !rebuildLinkedExtractor(id) {
			return true
		}
	}
	for _, fact := range classifications {
		if rebuildHoldsOutCheckedSkip(fact) {
			return true
		}
	}
	return false
}
