package indexer

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

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
}

type rebuildParsedCounts struct{ parsed, reused uint64 }

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
	files := make([]store_pebble.ManifestFile, 0, len(job.inputs))
	for _, input := range job.inputs {
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
	for _, input := range job.inputs {
		facts, reused, err := idx.loadRebuildFile(ctx, src, store, input, job.recipes[input.Language], job.bounds)
		if err != nil {
			return counts, err
		}
		if err := private.AddParsedFile("", facts); err != nil {
			return counts, err
		}
		if reused {
			counts.reused++
		} else {
			counts.parsed++
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
	return counts, nil
}
