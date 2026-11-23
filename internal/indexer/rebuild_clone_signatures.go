package indexer

import (
	"context"
	"fmt"
	"iter"
	"strings"

	"github.com/zzet/gortex/internal/clones"
	"github.com/zzet/gortex/internal/graph"
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

// These IDs cover explicit Go builtin writes and the implicit Graph
// AddBatch sentinel writes. The latter use settled edge destinations,
// which may differ from the raw receipts. Match the native constructor's
// first-infix split and first/last nonempty segments without allocating a node.
func isRebuildCloneBuiltinID(id string) bool {
	if !graph.IsBuiltinStub(id) {
		return false
	}
	const prefix = graph.StubKindBuiltin + "::"
	const infix = "::" + prefix
	var rest string
	if i := strings.Index(id, infix); i > 0 {
		rest = id[i+len(infix):]
	} else {
		var ok bool
		if rest, ok = strings.CutPrefix(id, prefix); !ok {
			return false
		}
	}
	lang, tail, ok := strings.Cut(rest, "::")
	if !ok || lang == "" {
		return false
	}
	for {
		_, next, ok := strings.Cut(tail, "::")
		if !ok {
			return tail != ""
		}
		tail = next
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
	var affected, rootManifests, rootModules, contracts, builtins, goModules, processes uint64
	claimStats, err := censusRebuildCloneInputs(ctx, build, receipts, rebuildCloneClaimBytes, func(row rebuildCloneInput) error {
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
		builtin := isRebuildCloneBuiltinID(row.ID)
		goModule := isRebuildCloneGoModuleID(row.ID)
		process := isRebuildCloneProcessID(row.ID)
		if builtin {
			builtins++
		}
		if goModule {
			goModules++
		}
		if process {
			processes++
		}
		if manifest || module || contract || builtin || goModule || process {
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
	})
	if err != nil {
		return rebuildCloneSignatureCounts{}, err
	}
	if affected != 0 {
		return rebuildCloneSignatureCounts{}, fmt.Errorf("clone predecessor root_manifest=%d root_module=%d contract=%d builtin=%d go_module=%d process=%d: %w",
			rootManifests, rootModules, contracts, builtins, goModules, processes,
			&store_pebble.RebuildUnknownProvenanceError{Count: affected})
	}
	counts := rebuildCloneSignatureCounts{Claims: claimStats}
	if corpus == 0 && signed == 0 {
		return counts, nil
	}
	useFilter := corpus >= cmsMinCorpus
	threshold := max(uint32(1), uint32(float64(corpus)*cmsBoilerplateRatio))
	if err := visitRebuildCloneInputs(ctx, build.RebuildView(), receipts, func(row rebuildCloneInput) error {
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
