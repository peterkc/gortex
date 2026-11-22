package indexer

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/indexer/source"
)

// rebuildFileStages carries the enumerator's frozen inputs into the worker.
// The process owns workDir and never opens a serving head.
type rebuildFileStages struct {
	workDir     string
	incarnation uint64
	inputs      []rebuildInput
	recipes     map[string]rebuildDigest
	bounds      rebuildTransformBounds
	witness     store_pebble.Witness

	classifications map[string]rebuildClassificationFact // from the verified sidecar only
}

type rebuildParsedCounts struct {
	parsed, reused uint64
	census         *rebuildExtractorCensus
}

var errRebuildCallerAttribution = errors.New("rebuild caller attribution is unproved")

// rebuildExtractorCensus is complete only after exact file assembly. It
// retains one literal path key per admitted input and counts its effective
// extractor ID, not the manifest language or any node language.
type rebuildExtractorCensus struct {
	admitted uint64
	byPath   map[string]string
	byID     map[string]uint64
}

// selectPendingCallers selects a main-link page before starting workers or
// applying changes. It borrows the page's edges in their original order.
// This does not filter the candidate source, check endpoints or publish rows.
func (c *rebuildExtractorCensus) selectPendingCallers(page []*graph.Edge) ([]*graph.Edge, error) {
	if c == nil || c.byPath == nil || uint64(len(c.byPath)) != c.admitted {
		return nil, fmt.Errorf("%w: incomplete extractor census", errRebuildCallerAttribution)
	}
	var selected []*graph.Edge
	for _, edge := range page {
		if edge == nil {
			return nil, fmt.Errorf("%w: nil pending edge", errRebuildCallerAttribution)
		}
		// JSX uses the javascript extractor; TSX uses typescript.
		switch c.byPath[edge.FilePath] {
		case "":
			return nil, fmt.Errorf("%w: no admitted receipt for %q", errRebuildCallerAttribution, edge.FilePath)
		case "go", "javascript", "typescript":
			selected = append(selected, edge)
		}
	}
	return selected, nil
}

// buildRebuildFileStages sends the manifest before opening a source file.
// Later passes use the same private store.
func (idx *Indexer) buildRebuildFileStages(
	ctx context.Context, src source.ContentSource, store rebuildParseStore, private *store_pebble.BuildStore,
	job rebuildFileStages, send func(store_pebble.StageTables) error,
) (rebuildParsedCounts, error) {
	var counts rebuildParsedCounts
	if !filepath.IsAbs(job.workDir) || job.incarnation == 0 {
		return counts, errors.New("invalid build worker lease")
	}
	root, err := rebuildInputRoot(job.inputs)
	if err != nil {
		return counts, err
	}
	if root != hex.EncodeToString(job.witness.Input[:]) {
		return counts, errors.New("build inputs do not match the frozen root")
	}
	// Order is part of shared-node behavior. The native first-file-wins
	// rule uses the exact path bytes, not the walk or worker completion order.
	slices.SortFunc(job.inputs, func(a, b rebuildInput) int { return strings.Compare(a.Path, b.Path) })
	for key, fact := range job.classifications {
		if err := ctx.Err(); err != nil {
			return counts, err
		}
		if err := idx.checkRebuildClassification(key, fact); err != nil {
			return counts, err
		}
	}
	files := make([]store_pebble.ManifestFile, 0, len(job.inputs))
	for _, input := range job.inputs {
		rel := input.Path
		// Cache hits do not pass through readRebuildInput's path check.
		if rel == "." || path.Clean(rel) != rel || !filepath.IsLocal(filepath.FromSlash(rel)) ||
			filepath.ToSlash(rel) != rel || strings.IndexByte(rel, 0) >= 0 {
			return counts, fmt.Errorf("%w: invalid admitted path %q", errRebuildCallerAttribution, rel)
		}
		if fact, ok := job.classifications[rel]; ok && !fact.matchesParseInput(input) {
			return counts, fmt.Errorf("%w: classification contradicts admitted parse input %q", errRebuildInputSidecar, rel)
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
		admitted: manifest.Witness.Families[0].Count,
		byPath:   make(map[string]string, len(job.inputs)),
		byID:     make(map[string]uint64),
	}
	for _, input := range job.inputs {
		facts, reused, err := idx.loadRebuildFile(ctx, src, store, input, job.recipes[input.Language], job.bounds)
		if err != nil {
			return counts, err
		}
		if facts.File.FilePath != input.Path || facts.EffectiveLanguage == "" {
			return counts, fmt.Errorf("%w: missing receipt attribution for %q", errParseEntryCorrupt, input.Path)
		}
		if err := private.AddParsedFile("", facts); err != nil {
			return counts, err
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
	// Native emits size skips after parsing. Write just their nodes; a
	// skip has no parse receipt, no raw edges and no manifest parse leaf.
	for key, fact := range job.classifications {
		if err := ctx.Err(); err != nil {
			return counts, err
		}
		if fact.sizeSkipped() {
			n := sizeSkipNode(skippedFile{relPath: key, lang: fact.Language, size: fact.Size}, fact.MaxFileSize)
			idx.applyRepoPrefix([]*graph.Node{n}, nil)
			private.AddNode(n)
		}
	}
	graphDir := filepath.Join(job.workDir, store_pebble.StageFileGraph.String())
	if err := os.Mkdir(graphDir, 0o700); err != nil {
		return counts, err
	}
	fileGraph, err := private.PrepareFileGraph(ctx, graphDir, manifest.Witness, job.incarnation)
	if err != nil {
		return counts, err
	}
	if err := send(fileGraph); err != nil {
		return counts, err
	}
	counts.census = &census
	return counts, nil
}
