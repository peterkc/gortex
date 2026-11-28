package indexer

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// rebuildContractReceiptReader reopens checked detached facts, never source.
// The caller keeps only the facts it needs from each receipt. The manifest
// inputs are already sorted by literal path by buildRebuildFileStages.
type rebuildContractReceiptReader struct {
	idx    *Indexer
	job    rebuildFileStages
	store  rebuildParseStore
	census *rebuildExtractorCensus
}

func (r rebuildContractReceiptReader) load(ctx context.Context, input rebuildInput) (store_pebble.ParseFacts, *contracts.FileFacts, error) {
	if err := ctx.Err(); err != nil {
		return store_pebble.ParseFacts{}, nil, err
	}
	recipe := r.idx.rebuildExtractorRecipe(r.job.recipes[input.Language], input)
	raw, err := r.store.read(input.parseKey(recipe))
	if err != nil {
		return store_pebble.ParseFacts{}, nil, fmt.Errorf("%w: reload %q for contracts: %w", errParseEntryCorrupt, input.Path, err)
	}
	base, facts, err := r.idx.decodeRebuildFileReceipt(input, raw)
	if err != nil {
		return store_pebble.ParseFacts{}, nil, err
	}
	if base.EffectiveLanguage != r.census.byPath[input.Path] {
		return store_pebble.ParseFacts{}, nil, fmt.Errorf("%w: early contract receipt attribution for %q", errParseEntryCorrupt, input.Path)
	}
	return base, facts, ctx.Err()
}

// forLinkedNode checks the source range before returning its captured body
// or shape inputs. Link metadata may legitimately change without moving the
// declaration; it is not an input identity. An absent shape for a proved
// declaration is represented in FileFacts, never by an absent receipt.
func (r rebuildContractReceiptReader) forLinkedNode(ctx context.Context, n *graph.Node) (*contracts.FileFacts, error) {
	if n.FilePath == "" || n.RepoPrefix != "" {
		return nil, fmt.Errorf("%w: no admitted contract declaration for %q", graph.ErrContractProjectionStale, n.ID)
	}
	path := r.census.originByGraphPath[n.FilePath]
	i, ok := slices.BinarySearchFunc(r.job.inputs, path, func(in rebuildInput, key string) int {
		return strings.Compare(in.Path, key)
	})
	if !ok {
		return nil, fmt.Errorf("%w: no admitted contract receipt for %q", graph.ErrContractProjectionStale, n.FilePath)
	}
	base, facts, err := r.load(ctx, r.job.inputs[i])
	if err != nil {
		return nil, err
	}
	for _, saved := range base.Nodes {
		if saved.ID == n.ID && saved.FilePath == n.FilePath && saved.Kind == n.Kind && saved.Language == n.Language &&
			saved.StartLine == n.StartLine && saved.EndLine == n.EndLine &&
			saved.StartColumn == n.StartColumn && saved.EndColumn == n.EndColumn {
			return facts, nil
		}
	}
	return nil, fmt.Errorf("%w: contract declaration %q does not match its receipt", graph.ErrContractProjectionStale, n.ID)
}
