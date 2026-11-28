package indexer

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"iter"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/indexer/source"
	"github.com/zzet/gortex/internal/pathkey"
)

// rebuildFileStages carries the enumerator's frozen inputs into the worker.
// The process owns workDir and never opens a serving head.
type rebuildFileStages struct {
	workDir     string
	incarnation string
	inputs      []rebuildInput
	recipes     map[string]rebuildDigest
	bounds      rebuildTransformBounds
	witness     store_pebble.Witness

	classifications map[string]rebuildClassificationFact // from the verified sidecar only
	retainFileGraph bool
}

type rebuildParsedCounts struct {
	parsed, reused uint64
	census         *rebuildExtractorCensus
	claims         store_pebble.RebuildSharedClaimStats
	fileGraphBase  store_pebble.FileGraphBase
}

var (
	errRebuildCallerAttribution  = errors.New("rebuild caller attribution is unproved")
	errRebuildGraphPathCollision = errors.New("rebuild inputs share an NFC graph path")
)

// rebuildExtractorCensus is complete only after exact file assembly. It
// retains one literal path key per admitted input and counts its effective
// extractor ID, not the manifest language or any node language.
type rebuildExtractorCensus struct {
	admitted uint64
	byPath   map[string]string
	byID     map[string]uint64

	originByGraphPath map[string]string // exact graph key → literal path; stubs have no receipt in byPath
	receipts          iter.Seq2[store_pebble.ParseFacts, error]
}

// supportsRelationPublication is the temporary D72 full-link-only boundary.
// No deferred format has a checked link-free proof yet.
func (c *rebuildExtractorCensus) supportsRelationPublication() bool {
	for id := range c.byID {
		if !rebuildContractLinkedExtractor(id) {
			return false
		}
	}
	return true
}

// selectPendingCallers selects a main-link page before starting workers or
// applying changes. It borrows the page's edges in their original order.
// This does not filter the candidate source, check endpoints or publish rows.
func (c *rebuildExtractorCensus) selectPendingCallers(page []*graph.Edge) ([]*graph.Edge, error) {
	if c == nil || c.originByGraphPath == nil || c.byPath == nil || uint64(len(c.byPath)) != c.admitted {
		return nil, fmt.Errorf("%w: incomplete extractor census", errRebuildCallerAttribution)
	}
	var selected []*graph.Edge
	for _, edge := range page {
		if edge == nil {
			return nil, fmt.Errorf("%w: nil pending edge", errRebuildCallerAttribution)
		}
		// JSX uses the javascript extractor; TSX uses typescript.
		switch c.byPath[c.originByGraphPath[edge.FilePath]] {
		case "":
			return nil, fmt.Errorf("%w: no admitted receipt for %q", errRebuildCallerAttribution, edge.FilePath)
		case "go", "javascript", "typescript":
			selected = append(selected, edge)
		}
	}
	return selected, nil
}

func recordRebuildGraphPath(origins map[string]string, raw string) error {
	key := pathkey.Normalize(raw)
	if prior, ok := origins[key]; ok && prior != raw {
		return fmt.Errorf("%w: %q and %q", errRebuildGraphPathCollision, min(prior, raw), max(prior, raw))
	}
	origins[key] = raw
	return nil
}

// buildRebuildFileStages sends the manifest before opening a source file.
// Later passes use the same private store.
func (idx *Indexer) buildRebuildFileStages(
	ctx context.Context, src source.ContentSource, store rebuildParseStore, private *store_pebble.BuildStore,
	job rebuildFileStages, send func(store_pebble.StageTables) error,
) (rebuildParsedCounts, error) {
	var counts rebuildParsedCounts
	if !filepath.IsAbs(job.workDir) || job.incarnation == "" {
		return counts, errors.New("invalid build worker lease")
	}
	root, err := rebuildInputRoot(job.inputs)
	if err != nil {
		return counts, err
	}
	if root != hex.EncodeToString(job.witness.Input[:]) {
		return counts, errors.New("build inputs do not match the frozen root")
	}
	// Use literal path order to assign stable receipt ordinals. Shared-node
	// placement is decided separately from the stamped node paths.
	slices.SortFunc(job.inputs, func(a, b rebuildInput) int { return strings.Compare(a.Path, b.Path) })
	// Check all graph-producing names before sending any stage or reading a
	// parse source. Sorted raw names make refusals repeatable.
	origins := make(map[string]string, len(job.inputs))
	classified := slices.Sorted(maps.Keys(job.classifications))
	content := idx.newContentAdmissionGate()
	for _, key := range classified {
		if err := ctx.Err(); err != nil {
			return counts, err
		}
		fact := job.classifications[key]
		if err := idx.checkRebuildClassification(key, fact, content); err != nil {
			return counts, err
		}
		if fact.sizeSkipped() || fact.ContentSkipReason != "" {
			if err := recordRebuildGraphPath(origins, key); err != nil {
				return counts, err
			}
		}
	}
	files := make([]store_pebble.ManifestFile, 0, len(job.inputs))
	for _, input := range job.inputs {
		if err := ctx.Err(); err != nil {
			return counts, err
		}
		rel := input.Path
		// Cache hits do not pass through readRebuildInput's path check.
		if rel == "." || path.Clean(rel) != rel || !filepath.IsLocal(filepath.FromSlash(rel)) ||
			filepath.ToSlash(rel) != rel || strings.IndexByte(rel, 0) >= 0 {
			return counts, fmt.Errorf("%w: invalid admitted path %q", errRebuildCallerAttribution, rel)
		}
		if fact, ok := job.classifications[rel]; ok && !fact.matchesParseInput(input) {
			return counts, fmt.Errorf("%w: classification contradicts admitted parse input %q", errRebuildInputSidecar, rel)
		}
		if err := recordRebuildGraphPath(origins, rel); err != nil {
			return counts, err
		}
		base, ok := job.recipes[input.Language]
		if !ok || base == (rebuildDigest{}) {
			return counts, fmt.Errorf("extractor recipe missing for %s", input.Language)
		}
		files = append(files, store_pebble.ManifestFile{
			Path: input.Path, Language: input.Language, Kind: byte(input.Kind), Size: input.Size,
			Content: store_pebble.HeadDigest(input.Content), Extractor: store_pebble.HeadDigest(idx.rebuildExtractorRecipe(base, input)),
		})
	}
	manifestDir := filepath.Join(job.workDir, store_pebble.StageManifest.String())
	if err := os.Mkdir(manifestDir, 0o700); err != nil {
		return counts, err
	}
	manifest, err := store_pebble.PrepareManifest(ctx, manifestDir, files, job.witness, job.incarnation)
	if err != nil {
		return counts, err
	}
	if err := send(manifest); err != nil {
		return counts, err
	}
	// Do not retain receipt payloads or read the source again. Each key below
	// borrows the path from the admitted inputs bounded by rebuildRequestBytes.
	census := rebuildExtractorCensus{
		admitted:          manifest.Witness.Families[0].Count,
		byPath:            make(map[string]string, len(job.inputs)),
		byID:              make(map[string]uint64),
		originByGraphPath: origins,
	}
	for _, input := range job.inputs {
		facts, reused, err := idx.loadRebuildFile(ctx, src, store, input, job.recipes[input.Language], job.bounds)
		if err != nil {
			return counts, err
		}
		if facts.File.FilePath != input.Path || facts.EffectiveLanguage == "" {
			return counts, fmt.Errorf("%w: missing receipt attribution for %q", errParseEntryCorrupt, input.Path)
		}

		census.byPath[input.Path] = facts.EffectiveLanguage
		census.byID[facts.EffectiveLanguage]++
		if reused {
			counts.reused++
		} else {
			counts.parsed++
		}
	}
	var total uint64
	for _, n := range census.byID {
		total += n
	}
	if total != census.admitted || uint64(len(census.byPath)) != census.admitted || census.admitted != uint64(len(job.inputs)) {
		return counts, fmt.Errorf("%w: %d files and %d receipts for %d admitted inputs (manifest %d)",
			errRebuildCallerAttribution, len(census.byPath), total, len(job.inputs), census.admitted)
	}
	// Qualify every raw claim before any graph row is materialized. The
	// parse store refuses different bytes at an existing immutable key.
	// Later passes also use these same checked receipt keys, not the source.
	receipts := idx.rebuildDataflowReceipts(ctx, job, store, &census)
	census.receipts = receipts
	counts.claims, err = private.ReduceRebuildSharedClaims(ctx, rebuildCloneClaimBytes, receipts,
		func(keep func(string, uint64) (bool, error)) error {
			var ordinal uint64
			for facts, err := range receipts {
				if err != nil {
					return err
				}
				ordinal++
				// Only the fresh replay value uses NFC for these store keys.
				if key := pathkey.Normalize(facts.File.FilePath); key != facts.File.FilePath {
					facts.File.FilePath = key
					for i := range facts.Constants {
						facts.Constants[i].FilePath = key
					}
				}
				if err := private.AddParsedFileSelected("", facts, func(id string) (bool, error) {
					return keep(id, ordinal)
				}); err != nil {
					return err
				}
			}
			return ctx.Err()
		})
	if err != nil {
		return counts, err
	}
	// Native emits size and content skips after parsing. Write just their
	// nodes; a skip has no parse receipt, raw edges or manifest parse leaf.
	for _, key := range classified {
		if err := ctx.Err(); err != nil {
			return counts, err
		}
		n := rebuildCheckedSkipNode(key, job.classifications[key])
		if n == nil {
			continue
		}
		idx.applyRepoPrefix([]*graph.Node{n}, nil)
		private.AddNode(n)
	}
	graphDir := filepath.Join(job.workDir, store_pebble.StageFileGraph.String())
	if err := os.Mkdir(graphDir, 0o700); err != nil {
		return counts, err
	}
	fileGraph, err := private.PrepareFileGraph(ctx, graphDir, manifest.Witness, job.incarnation)
	if err != nil {
		return counts, err
	}
	if job.retainFileGraph && census.supportsRelationPublication() {
		// The owner consumes these names during send. Retain only when
		// there is a later relation seal to consume the private base.
		counts.fileGraphBase, err = private.RetainFileGraph(ctx, fileGraph)
		if err != nil {
			return counts, err
		}
	}
	if err := send(fileGraph); err != nil {
		return counts, err
	}
	counts.census = &census
	return counts, nil
}
