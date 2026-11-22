package indexer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/indexer/source"
	"github.com/zzet/gortex/internal/parser"
	"go.uber.org/zap"
)

// loadRebuildFile loads one immutable entry before touching the checkout. A
// matching entry remains usable even if an edit replaced the frozen bytes.
// Only a missing entry permits extraction; corruption refuses the build.
func (idx *Indexer) loadRebuildFile(ctx context.Context, src source.ContentSource, store rebuildParseStore, input rebuildInput, recipe rebuildDigest, bounds rebuildTransformBounds) (facts store_pebble.ParseFacts, reused bool, err error) {
	return idx.loadRebuildFileWith(ctx, src, store, input, recipe, bounds,
		func(ctx context.Context, input rebuildInput, ext parser.Extractor, prepared []byte) (store_pebble.ParseFacts, []byte, error) {
			facts, err := idx.extractPreparedRebuildFile(ctx, input, ext, prepared)
			return facts, nil, err
		})
}

// The two extraction paths share the cache and the work around extraction.
// Only a cache miss calls extract, with the exact prepared bytes.
func (idx *Indexer) loadRebuildFileWith(ctx context.Context, src source.ContentSource, store rebuildParseStore, input rebuildInput, recipe rebuildDigest, bounds rebuildTransformBounds,
	extract func(context.Context, rebuildInput, parser.Extractor, []byte) (store_pebble.ParseFacts, []byte, error),
) (facts store_pebble.ParseFacts, reused bool, err error) {
	if err := ctx.Err(); err != nil {
		return facts, false, err
	}
	key := input.parseKey(idx.rebuildExtractorRecipe(recipe, input))
	payload, err := store.read(key)
	if err == nil {
		facts, err = store_pebble.DecodeParseFacts(payload)
		if err != nil {
			return facts, false, fmt.Errorf("%w: %s: %w", errParseEntryCorrupt, input.Path, err)
		}
		if facts.File.FilePath != input.Path {
			return facts, false, fmt.Errorf("%w: %s: file identity differs", errParseEntryCorrupt, input.Path)
		}
		return facts, true, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return facts, false, err
	}
	if bounds.FileBytes > 0 && input.Size > int64(bounds.FileBytes) {
		return facts, false, fmt.Errorf("%w: %s has %d bytes, limit %d", ErrBuildTransformBounds, input.Path, input.Size, bounds.FileBytes)
	}
	raw, err := readFrozenRebuildInput(ctx, src, input)
	if err != nil {
		return facts, false, err
	}
	rel, lang := input.Path, input.Language
	ext, ok := idx.registry.GetByLanguage(lang)
	if !ok || ext == nil {
		return facts, false, fmt.Errorf("no extractor for %s (%s)", rel, lang)
	}
	prepared, spilled, err := idx.transforms.runRebuild(ctx, rel, raw, bounds)
	if err != nil {
		return facts, false, err
	}
	if bounds.FileBytes > 0 && len(prepared) > bounds.FileBytes {
		return facts, false, fmt.Errorf("%w: %s produced %d bytes, limit %d", ErrBuildTransformBounds, rel, len(prepared), bounds.FileBytes)
	}
	if spilled != 0 {
		idx.logger.Debug("build command transform spilled", zap.String("file", rel), zap.Int64("spill_bytes", spilled))
	}
	facts, payload, err = extract(ctx, input, ext, prepared)
	if err != nil {
		return store_pebble.ParseFacts{}, false, err
	}
	if payload == nil {
		payload, err = store_pebble.EncodeParseFacts(facts)
		if err != nil {
			return facts, false, fmt.Errorf("encode parse facts for %s: %w", input.Path, err)
		}
	}
	if err := ctx.Err(); err != nil {
		return facts, false, err
	}
	return facts, false, store.write(key, payload)
}

// extractPreparedRebuildFile runs only inside the build worker or the strict
// parse worker. It classifies, extracts, and shapes the same prepared bytes.
func (idx *Indexer) extractPreparedRebuildFile(ctx context.Context, input rebuildInput, ext parser.Extractor, prepared []byte) (store_pebble.ParseFacts, error) {
	rel, lang := input.Path, input.Language
	admission := newNativeParseExtractionAdmission(0, nil, idx.nativeParseAdmission.Load())
	result, skipped, err := idx.extractFileCtxWithRawLease(ctx, admission, nil, nil, nil,
		filepath.Join(idx.rootPath, filepath.FromSlash(rel)), rel, lang, ext, prepared)
	if result != nil {
		defer result.ReleaseTree()
	}
	if err != nil {
		return store_pebble.ParseFacts{}, fmt.Errorf("extract %s: %w", rel, err)
	}
	if result == nil {
		return store_pebble.ParseFacts{}, fmt.Errorf("extract %s: no parse facts", rel)
	}
	if !skipped {
		if !extractionDispositionFor(result).omitSecondarySourceScans() {
			idx.applyCoverageDomains(rel, lang, prepared, result)
		}
		stampExtractionGraphFingerprint(result)
		idx.stampContractDependencyInputs(rel, lang, prepared, result)
	}
	meta, _ := idx.prepareFileMeta(rel, prepared, result)
	constants, _ := idx.prepareConstValues(result)
	facts := store_pebble.ParseFacts{Nodes: result.Nodes, Edges: result.Edges, File: meta, Constants: constants}
	for _, n := range facts.Nodes {
		if n == nil {
			continue
		}
		if shingles, ok := n.Meta[cloneShinglesMetaKey].([]uint64); ok {
			if facts.Shingles == nil {
				facts.Shingles = make(map[string][]uint64)
			}
			facts.Shingles[n.ID] = shingles
		}
		if idx.shouldIndexForSearch(n) {
			facts.Symbols = append(facts.Symbols, graph.SymbolFTSItem{NodeID: n.ID, Tokens: ftsTokensFor(n, "")})
		}
	}
	return facts, nil
}
