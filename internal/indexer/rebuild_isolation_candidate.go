package indexer

import (
	"context"
	"fmt"

	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/pathkey"
)

// A query may mask several probes, but the first suppressed positive is
// native's first answer on this ordered candidate path. Replay only when
// such a positive was seen. A changed probe with the same final rewrite is
// not an affected relation answer.
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
	raw, err := resolve(ns.probeFile)
	if err != nil {
		return "", err
	}
	if raw == selected {
		return selected, nil
	}
	file := pathkey.Normalize(caller)
	language := census.byPath[caller]
	if language == "" {
		language = census.byPath[census.originByGraphPath[file]]
	}
	if !rebuildLinkedExtractor(language) {
		return "", fmt.Errorf("%w: isolation affected query has no supported receipt for %q", errRebuildCallerAttribution, caller)
	}
	if err := build.MarkRebuildIsolationAffected(ctx, store_pebble.RebuildIsolationAffectedScope{
		FilePath: file, SupportedLanguage: language, DeferredLanguage: deferred, Cause: cause,
	}, spec); err != nil {
		return "", err
	}
	return selected, nil
}
