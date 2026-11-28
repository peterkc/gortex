package indexer

import (
	"context"
	"fmt"
	"iter"
	"maps"
	"slices"
	"strings"

	"github.com/zzet/gortex/internal/clones"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// rebuildClonePredecessors holds evidence from the preceding stages and
// the possible native writers left out of the private build.
type rebuildClonePredecessors struct {
	namespace *rebuildResolverNamespace
	contracts rebuildEarlyContractIDs
	// FileGraph proves raw claims over all receipts before D72 F selects
	// the clone cohort. Standalone callers still need a raw claim census.
	fileClaims *store_pebble.RebuildSharedClaimStats
	// FileGraph counts raw and already-signed callable winners before the
	// hold-out cut, by effective deferred extractor. No held body enters CMS
	// or LSH, but its absence can change the selected clone corpus answers.
	heldCloneByLanguage map[string]uint64
}

func (p rebuildClonePredecessors) mayReadRoot(path string) bool {
	present, err := p.namespace.probeFile(path)
	return present || err != nil // opaque or unchecked is not absence
}

// Native commitContracts writes literal registry IDs, without a repo prefix.
// Its legacy route is not ruled out by the frozen parse receipts. Unlike its
// insert-only Go dependency seed, the late pass can replace an existing ID.
func (p rebuildClonePredecessors) mayReplaceWithContract(id string) bool {
	if p.contracts != nil {
		return p.contracts.contains(id)
	}
	// Legacy standalone callers do not carry full checked contract facts.
	kind, name, ok := strings.Cut(id, "::")
	if !ok || name == "" {
		return false
	}
	switch kind {
	case "http", "grpc", "graphql", "thrift", "trpc", "topic", "ws", "env", "di":
		return true
	case "dep":
		return p.mayReadRoot("go.mod")
	default:
		return false
	}
}

// Native Go extern attribution can replace a module after an edge binds to
// an external. The function IDs it materializes are insert-only.
func isRebuildCloneGoModuleID(id string) bool {
	const prefix = "module::go:"
	if name, ok := strings.CutPrefix(id, prefix); ok {
		return name != ""
	}
	repo, rest, ok := strings.Cut(id, "::")
	return ok && repo != "" && strings.HasPrefix(rest, prefix) && len(rest) > len(prefix)
}

func isRebuildCloneProcessID(id string) bool {
	mechanism, ok := strings.CutPrefix(id, "string::process::")
	if !ok || mechanism == "" {
		return false
	}
	for _, name := range processExecAPIs {
		if name == mechanism {
			return true
		}
	}
	for _, name := range processExecBareNames {
		if name == mechanism {
			return true
		}
	}
	return false
}

// Bound cumulative encoded claim writes separately from clone pair scratch.
const rebuildCloneClaimBytes = 512 << 20

type rebuildCloneSignatureCounts struct {
	Corpus, Kept, Signed int
	Claims               store_pebble.RebuildShadowedIDStats
}

// visitRebuildCloneSignatures finalizes each body from the same immutable
// receipts and quiet working view in both passes. Only raw bodies feed the
// sketch, including those that later drop out. A signature-only input is
// already finalized and an item even when there are no raw bodies. A
// callback with kept=false is a finalized drop. All callbacks are tentative
// until the visitor returns nil; the callback must not mutate the view.
func visitRebuildCloneSignatures(ctx context.Context, build *store_pebble.BuildStore,
	receipts iter.Seq2[store_pebble.ParseFacts, error], predecessors rebuildClonePredecessors,
	yield func(row rebuildCloneInput, sig clones.Signature, kept bool) error,
) (rebuildCloneSignatureCounts, error) {
	cms := clones.NewCMS(65536, 4)
	corpus, signed := 0, 0
	var affected, contracts, goModules, processes uint64
	collect := func(row rebuildCloneInput) error {
		// The checked cohort contains each ID once. Count possible
		// replacements before the second pass banks or emits any row.
		contract := predecessors.mayReplaceWithContract(row.ID)
		if contract {
			contracts++
		}
		goModule := isRebuildCloneGoModuleID(row.ID)
		process := isRebuildCloneProcessID(row.ID)
		if goModule {
			goModules++
		}
		if process {
			processes++
		}
		if contract || goModule || process {
			affected++
			return nil
		}
		if row.Signed != nil {
			signed++
			return nil
		}
		corpus++
		for _, shingle := range row.Shingles {
			cms.Add(shingle)
		}
		return ctx.Err()
	}
	var claimStats store_pebble.RebuildShadowedIDStats
	var err error
	if prior := predecessors.fileClaims; prior != nil {
		// None of the accepted shared families is callable, and the early
		// writer rejects contested shingle sidecars. Still verify the live
		// clone cohort and every retained shingle against the selected receipts.
		claimStats = prior.RebuildShadowedIDStats
		err = visitRebuildCloneInputs(ctx, build.RebuildView(), receipts, collect)
	} else {
		claimStats, err = censusRebuildCloneInputs(ctx, build, receipts, rebuildCloneClaimBytes, collect)
	}
	if err != nil {
		return rebuildCloneSignatureCounts{}, err
	}
	if affected != 0 {
		// Go/JS/TS callable IDs are file-qualified, unlike bare manifest IDs.
		// Keep the two root fields as zero, as on the old worker path.
		return rebuildCloneSignatureCounts{}, fmt.Errorf("clone predecessor root_manifest=0 root_module=0 contract=%d go_module=%d process=%d: %w",
			contracts, goModules, processes,
			&store_pebble.RebuildUnknownProvenanceError{Count: affected})
	}
	counts := rebuildCloneSignatureCounts{Claims: claimStats}
	if corpus == 0 && signed == 0 {
		return counts, nil
	}
	useFilter := corpus >= cmsMinCorpus
	threshold := max(uint32(1), uint32(float64(corpus)*cmsBoilerplateRatio))
	held := slices.Sorted(maps.Keys(predecessors.heldCloneByLanguage))
	if err := visitRebuildCloneInputs(ctx, build.RebuildView(), receipts, func(row rebuildCloneInput) error {
		// Mark the checked callable cohort, not just survivors. Raw signatures
		// may change through the CMS and both raw and signed pair answers may
		// change through LSH bucket admission. The receipt ID is the unique
		// event within this file and deferred extractor scope.
		for _, language := range held {
			if predecessors.heldCloneByLanguage[language] == 0 {
				continue
			}
			if err := build.MarkRebuildIsolationAffected(ctx, store_pebble.RebuildIsolationAffectedScope{
				FilePath: row.FilePath, SupportedLanguage: row.Language, DeferredLanguage: language, Cause: "clone-corpus",
			}, row.ID); err != nil {
				return err
			}
		}
		if row.Signed != nil {
			counts.Signed++
			return yield(row, *row.Signed, true)
		}
		sig, kept := computeCloneSigFromShingles(cms, threshold, useFilter, row.Shingles)
		counts.Corpus++
		if kept {
			counts.Kept++
		}
		return yield(row, sig, kept)
	}); err != nil {
		return rebuildCloneSignatureCounts{}, err
	}
	if counts.Corpus != corpus || counts.Signed != signed {
		return rebuildCloneSignatureCounts{}, fmt.Errorf("%w: clone cohort changed between passes: raw %d then %d, signed %d then %d", errParseEntryCorrupt, corpus, counts.Corpus, signed, counts.Signed)
	}
	return counts, nil
}
