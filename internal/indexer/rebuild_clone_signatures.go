package indexer

import (
	"context"
	"fmt"
	"iter"
	"strings"

	"github.com/zzet/gortex/internal/clones"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// rebuildClonePredecessors holds frozen evidence for native writes missing
// from the private build. Later writer checks still gate the worker splice.
type rebuildClonePredecessors struct {
	namespace *rebuildResolverNamespace
	modules   bool
}

func (p rebuildClonePredecessors) mayReplaceWithRootManifest(id string) bool {
	if !p.modules {
		return false
	}
	// These six roots from native rootManifests are not replayed. The
	// worker uses unprefixed graph keys. An opaque name is not absence.
	switch id {
	case "pyproject.toml", "requirements.txt", "Cargo.toml", "pom.xml", "composer.json", "composer.lock":
		return p.mayReadRoot(id)
	default:
		return false
	}
}

// Omitted module specs are not frozen. A possible manifest read is enough
// to keep its ecosystem's ID space in scope, even without an exact match.
func (p rebuildClonePredecessors) mayReplaceWithRootModule(id string) bool {
	if !p.modules {
		return false
	}
	module, ok := strings.CutPrefix(id, "module::")
	if !ok {
		return false
	}
	ecosystem, name, ok := strings.Cut(module, ":")
	if !ok || name == "" {
		return false
	}
	switch ecosystem {
	case "pypi":
		return p.mayReadRoot("pyproject.toml") || p.mayReadRoot("requirements.txt")
	case "cargo":
		return p.mayReadRoot("Cargo.toml")
	case "maven":
		return p.mayReadRoot("pom.xml")
	case "composer":
		return p.mayReadRoot("composer.json") || p.mayReadRoot("composer.lock")
	default:
		return false
	}
}

func (p rebuildClonePredecessors) mayReadRoot(path string) bool {
	present, err := p.namespace.probeFile(path)
	return present || err != nil // opaque or unchecked is not absence
}

// Native commitContracts writes literal registry IDs, without a repo prefix.
// Its legacy route is not ruled out by the frozen parse receipts. Unlike its
// insert-only Go dependency seed, the late pass can replace an existing ID.
func (p rebuildClonePredecessors) mayReplaceWithContract(id string) bool {
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

type rebuildCloneSignatureCounts struct {
	Corpus, Kept, Signed int
}

// visitRebuildCloneSignatures finalizes each body from the same immutable
// receipts and quiet working view in both passes. Only raw bodies feed the
// sketch, including those that later drop out. A signature-only input is
// already finalized and an item even when there are no raw bodies. A
// callback with kept=false is a finalized drop. All callbacks are tentative
// until the visitor returns nil; the callback must not mutate the view.
func visitRebuildCloneSignatures(ctx context.Context, view store_pebble.RebuildView,
	receipts iter.Seq2[store_pebble.ParseFacts, error], predecessors rebuildClonePredecessors,
	yield func(row rebuildCloneInput, sig clones.Signature, kept bool) error,
) (rebuildCloneSignatureCounts, error) {
	cms := clones.NewCMS(65536, 4)
	corpus, signed := 0, 0
	var affected, rootManifests, rootModules, contracts uint64
	if err := visitRebuildCloneInputs(ctx, view, receipts, func(row rebuildCloneInput) error {
		// The checked cohort contains each ID once. Count possible
		// replacements before the second pass banks or emits any row.
		manifest := predecessors.mayReplaceWithRootManifest(row.ID)
		module := predecessors.mayReplaceWithRootModule(row.ID)
		contract := predecessors.mayReplaceWithContract(row.ID)
		if manifest {
			rootManifests++
		}
		if module {
			rootModules++
		}
		if contract {
			contracts++
		}
		if manifest || module || contract {
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
	}); err != nil {
		return rebuildCloneSignatureCounts{}, err
	}
	if affected != 0 {
		return rebuildCloneSignatureCounts{}, fmt.Errorf("clone predecessor root_manifest=%d root_module=%d contract=%d: %w",
			rootManifests, rootModules, contracts, &store_pebble.RebuildUnknownProvenanceError{Count: affected})
	}
	if corpus == 0 && signed == 0 {
		return rebuildCloneSignatureCounts{}, nil
	}
	useFilter := corpus >= cmsMinCorpus
	threshold := max(uint32(1), uint32(float64(corpus)*cmsBoilerplateRatio))
	var counts rebuildCloneSignatureCounts
	if err := visitRebuildCloneInputs(ctx, view, receipts, func(row rebuildCloneInput) error {
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
