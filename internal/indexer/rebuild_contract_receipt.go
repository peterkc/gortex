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

// visitFilesAtParseCut answers endpoint questions before main can change the
// graph's candidates. Each call borrows one file's records and checked facts.
// The caller must not retain a whole receipt census for the late pass.
func (r rebuildContractReceiptReader) visitFilesAtParseCut(ctx context.Context, view store_pebble.RebuildView,
	visit func(path string, records []contracts.Contract, facts *contracts.FileFacts) error,
) error {
	constants := rebuildEndpointConstants{ctx: ctx, view: view}
	for _, input := range r.job.inputs {
		_, facts, err := r.load(ctx, input)
		if err != nil {
			return err
		}
		records, err := contracts.ResolveEndpointQuestionsWithLookup(facts.Records, facts.Questions, constants.lookup)
		if err != nil {
			return fmt.Errorf("%w: endpoint questions for %q: %w", errParseEntryCorrupt, input.Path, err)
		}
		if constants.err != nil {
			return fmt.Errorf("resolve endpoint constants for %q: %w", input.Path, constants.err)
		}
		if err := visit(input.Path, records, facts); err != nil {
			return err
		}
	}
	return ctx.Err()
}

// visitLateContractSourcesAtParseCut emits the original records needed by the
// late pass. Capture this stream before main; do not replay the questions over
// final Relations. The checked root fact contributes full dependency records,
// not the minimal identities seeded for import resolution.
func (r rebuildContractReceiptReader) visitLateContractSourcesAtParseCut(ctx context.Context, view store_pebble.RebuildView,
	root rebuildGoManifest, visit func(contracts.Contract) error,
) error {
	if err := root.checkContractDeps(); err != nil {
		return err
	}
	scoped := func(c contracts.Contract) error {
		c.RepoPrefix = r.idx.repoPrefix
		if c.WorkspaceID == "" {
			c.WorkspaceID = r.idx.workspaceID
		}
		if c.ProjectID == "" {
			c.ProjectID = r.idx.projectID
		}
		return visit(c)
	}
	if err := r.visitFilesAtParseCut(ctx, view, func(_ string, records []contracts.Contract, _ *contracts.FileFacts) error {
		for _, c := range records {
			if err := scoped(c); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	return root.visitContractDeps(scoped)
}

// forLinkedNode checks the source range before returning its captured body
// or shape inputs. Link metadata may legitimately change without moving the
// declaration; it is not an input identity. An absent shape for a proved
// declaration is represented in FileFacts, never by an absent receipt.
func (r rebuildContractReceiptReader) forLinkedNode(ctx context.Context, n *graph.Node) (*contracts.FileFacts, error) {
	_, facts, err := r.forLinkedDeclaration(ctx, n)
	return facts, err
}

func (r rebuildContractReceiptReader) forLinkedDeclaration(ctx context.Context, n *graph.Node) (store_pebble.ParseFacts, *contracts.FileFacts, error) {
	if n.FilePath == "" || n.RepoPrefix != "" {
		return store_pebble.ParseFacts{}, nil, fmt.Errorf("%w: no admitted contract declaration for %q", graph.ErrContractProjectionStale, n.ID)
	}
	path := r.census.originByGraphPath[n.FilePath]
	i, ok := slices.BinarySearchFunc(r.job.inputs, path, func(in rebuildInput, key string) int {
		return strings.Compare(in.Path, key)
	})
	if !ok {
		return store_pebble.ParseFacts{}, nil, fmt.Errorf("%w: no admitted contract receipt for %q", graph.ErrContractProjectionStale, n.FilePath)
	}
	base, facts, err := r.load(ctx, r.job.inputs[i])
	if err != nil {
		return store_pebble.ParseFacts{}, nil, err
	}
	for _, saved := range base.Nodes {
		if saved.ID == n.ID && saved.FilePath == n.FilePath && saved.Kind == n.Kind && saved.Language == n.Language &&
			saved.StartLine == n.StartLine && saved.EndLine == n.EndLine &&
			saved.StartColumn == n.StartColumn && saved.EndColumn == n.EndColumn {
			return base, facts, nil
		}
	}
	return store_pebble.ParseFacts{}, nil, fmt.Errorf("%w: contract declaration %q does not match its receipt", graph.ErrContractProjectionStale, n.ID)
}
