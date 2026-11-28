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
	idx                   *Indexer
	job                   rebuildFileStages
	store                 rebuildParseStore
	census                *rebuildExtractorCensus
	proofCache            *rebuildContractReceiptCache
	isolate               bool
	linkView              store_pebble.RebuildView
	markIsolationAffected func(file string, kind contracts.ContractType, language string) error
}

type rebuildContractReceiptCache struct {
	path  string
	base  store_pebble.ParseFacts
	facts *contracts.FileFacts
}

func (r rebuildContractReceiptReader) holdsOut(path string) (bool, error) {
	if !r.isolate {
		return false, nil
	}
	return r.census.holdsOutLinkInput(path)
}

func (r rebuildContractReceiptReader) requireSelected(path string) error {
	held, err := r.holdsOut(path)
	if err != nil {
		return err
	}
	if held {
		return fmt.Errorf("%w: held-out contract receipt %q", store_pebble.ErrRebuildLinkIsolation, path)
	}
	return nil
}

func (r rebuildContractReceiptReader) load(ctx context.Context, input rebuildInput) (store_pebble.ParseFacts, *contracts.FileFacts, error) {
	if err := r.requireSelected(input.Path); err != nil {
		return store_pebble.ParseFacts{}, nil, err
	}
	base, facts, err := r.loadRaw(ctx, input)
	if err == nil && r.isolate {
		base, err = r.linkView.SelectRebuildLinkFacts(ctx, base)
	}
	return base, facts, err
}

// loadRaw is the checked immutable receipt decode. Held-out inputs may use it
// only in the separate omission inventory, never in a link-facing reducer.
func (r rebuildContractReceiptReader) loadRaw(ctx context.Context, input rebuildInput) (store_pebble.ParseFacts, *contracts.FileFacts, error) {
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
		return store_pebble.ParseFacts{}, nil, fmt.Errorf("%w: contract receipt attribution for %q", errParseEntryCorrupt, input.Path)
	}
	return base, facts, ctx.Err()
}

// visitFilesAtParseCut answers endpoint questions before main can change the
// graph's candidates. Each call borrows one file's records and checked facts.
// The caller must not retain a whole receipt census for the late pass.
func (r rebuildContractReceiptReader) visitFilesAtParseCut(ctx context.Context, view store_pebble.RebuildView,
	visit func(path string, records []contracts.Contract, facts *contracts.FileFacts) error,
) error {
	r.linkView = view
	constants := rebuildEndpointConstants{ctx: ctx, view: view}
	for _, input := range r.job.inputs {
		held, err := r.holdsOut(input.Path)
		if err != nil {
			return err
		}
		if held {
			continue
		}
		_, facts, err := r.load(ctx, input)
		if err != nil {
			return err
		}
		lookup := constants.lookup
		if r.markIsolationAffected != nil {
			// Scope an actual lookup to the contract families that ask this
			// name in the current receipt. A held value never enters the vote.
			kinds := make(map[string][]contracts.ContractType)
			for _, q := range facts.Questions {
				for _, term := range q.Expression.Terms {
					name := strings.TrimSpace(term.Name)
					if name != "" && !slices.Contains(kinds[name], q.Record.Type) {
						kinds[name] = append(kinds[name], q.Record.Type)
					}
				}
			}
			lookup = func(name, file, repo string) string {
				value := constants.lookup(name, file, repo)
				if constants.err != nil {
					return ""
				}
				name = strings.TrimSpace(name)
				for _, kind := range kinds[name] {
					if err := r.markHeldName(ctx, view, file, kind, name, graph.KindConstant); err != nil {
						constants.err = err
						return ""
					}
				}
				return value
			}
		}
		records, err := contracts.ResolveEndpointQuestionsWithLookup(facts.Records, facts.Questions, lookup)
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

// markHeldName records only affected scope, not excluded candidate data.
func (r rebuildContractReceiptReader) markHeldName(ctx context.Context, view store_pebble.RebuildView,
	file string, kind contracts.ContractType, name string, kinds ...graph.NodeKind,
) error {
	if r.markIsolationAffected == nil || name == "" {
		return nil
	}
	languages, err := view.RebuildHeldNameLanguages(ctx, name, kinds...)
	if err != nil {
		return err
	}
	for _, language := range languages {
		if err := r.markIsolationAffected(file, kind, language); err != nil {
			return err
		}
	}
	return nil
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

// forLinkedDeclaration checks the last upsert for this ID in its checked
// receipt. Link metadata may change without moving the declaration.
func (r rebuildContractReceiptReader) forLinkedDeclaration(ctx context.Context, n *graph.Node) (store_pebble.ParseFacts, *contracts.FileFacts, error) {
	base, facts, err := r.loadLinkedFile(ctx, n)
	if err != nil {
		return store_pebble.ParseFacts{}, nil, err
	}
	var saved *graph.Node
	for i := len(base.Nodes) - 1; i >= 0; i-- {
		if base.Nodes[i].ID == n.ID {
			saved = base.Nodes[i]
			break
		}
	}
	if err := checkLinkedContractDeclaration(n, saved); err != nil {
		return store_pebble.ParseFacts{}, nil, err
	}
	return base, facts, nil
}

func checkLinkedContractDeclaration(n, saved *graph.Node) error {
	if saved == nil || saved.FilePath != n.FilePath || saved.Kind != n.Kind || saved.Language != n.Language ||
		saved.StartLine != n.StartLine || saved.EndLine != n.EndLine ||
		saved.StartColumn != n.StartColumn || saved.EndColumn != n.EndColumn {
		return fmt.Errorf("%w: contract declaration %q does not match its receipt", graph.ErrContractProjectionStale, n.ID)
	}
	return nil
}

func (r rebuildContractReceiptReader) loadLinkedFile(ctx context.Context, n *graph.Node) (store_pebble.ParseFacts, *contracts.FileFacts, error) {
	if n.FilePath == "" || n.RepoPrefix != "" {
		return store_pebble.ParseFacts{}, nil, fmt.Errorf("%w: no admitted contract declaration for %q", graph.ErrContractProjectionStale, n.ID)
	}
	return r.forGraphPath(ctx, n.FilePath)
}

func (r rebuildContractReceiptReader) forGraphPath(ctx context.Context, file string) (store_pebble.ParseFacts, *contracts.FileFacts, error) {
	path := r.census.originByGraphPath[file]
	i, ok := slices.BinarySearchFunc(r.job.inputs, path, func(in rebuildInput, key string) int {
		return strings.Compare(in.Path, key)
	})
	if !ok {
		return store_pebble.ParseFacts{}, nil, fmt.Errorf("%w: no admitted contract receipt for %q", graph.ErrContractProjectionStale, file)
	}
	if err := ctx.Err(); err != nil {
		return store_pebble.ParseFacts{}, nil, err
	}
	if err := r.requireSelected(path); err != nil {
		return store_pebble.ParseFacts{}, nil, err
	}
	if cache := r.proofCache; cache != nil && cache.path == file && cache.facts != nil {
		return cache.base, cache.facts, nil
	}
	base, facts, err := r.load(ctx, r.job.inputs[i])
	if err == nil && r.proofCache != nil {
		*r.proofCache = rebuildContractReceiptCache{path: file, base: base, facts: facts}
	}
	return base, facts, err
}

// springRawDeclaration requires an actual parse declaration, not a synthetic
// or moved linked node. Receipt failures still refuse the build.
func (r rebuildContractReceiptReader) springRawDeclaration(ctx context.Context, n *graph.Node) (store_pebble.ParseFacts, *graph.Node, error) {
	if n == nil || n.FilePath == "" || n.RepoPrefix != r.idx.repoPrefix || r.extractorOf(n.FilePath) == "" {
		return store_pebble.ParseFacts{}, nil, nil
	}
	base, _, err := r.forGraphPath(ctx, n.FilePath)
	if err != nil {
		return store_pebble.ParseFacts{}, nil, err
	}
	return base, springRawNode(base, n), nil
}

func springRawNode(base store_pebble.ParseFacts, n *graph.Node) *graph.Node {
	for i := len(base.Nodes) - 1; i >= 0; i-- {
		saved := base.Nodes[i]
		if saved.ID == n.ID {
			if checkLinkedContractDeclaration(n, saved) == nil && n.Name == saved.Name {
				return saved
			}
			break
		}
	}
	return nil
}

func (r rebuildContractReceiptReader) visitSpringConfig(ctx context.Context, view store_pebble.RebuildView, records *store_pebble.RebuildContractRecords,
	yield func(store_pebble.RebuildContractBatch) error,
) error {
	scope := contracts.SpringConfigScope{RepoRoot: r.idx.rootPath, RepoPrefix: r.idx.repoPrefix, WorkspaceID: r.idx.workspaceID}
	var lastReader string
	var readerProved bool
	return records.VisitSpringConfig(scope, func(path string) ([]*graph.Node, bool, error) {
		if r.sizeSkippedContractSource(path) {
			return nil, true, nil
		}
		_, facts, err := r.forGraphPath(ctx, path)
		if err != nil {
			return nil, false, err
		}
		return facts.ConfigKeys, false, nil
	}, func(batch store_pebble.RebuildContractBatch) error {
		// Key nodes are elected directly from checked, value-redacted file
		// facts above. That pass has no link inputs. Existing targets, unlike
		// newly elected keys, must be proved retained declarations.
		for _, e := range batch.Edges {
			if e.From != lastReader {
				lastReader, readerProved = e.From, false
				reader, err := view.Node(ctx, e.From)
				if err != nil {
					return err
				}
				_, raw, err := r.springRawDeclaration(ctx, reader)
				if err != nil {
					return err
				}
				readerProved = raw != nil && r.extractorOf(reader.FilePath) == "java" &&
					slices.Equal(contracts.SpringConfigKeysOf(raw), contracts.SpringConfigKeysOf(reader))
			}
			keep := readerProved
			if keep {
				target, err := view.Node(ctx, e.To)
				if err != nil {
					return err
				}
				if target != nil {
					_, raw, err := r.springRawDeclaration(ctx, target)
					if err != nil {
						return err
					}
					keep = raw != nil
				}
			}
			if !keep {
				return yield(store_pebble.RebuildContractBatch{Omitted: []store_pebble.RebuildContractOmission{
					store_pebble.RebuildContractEdgeOmission(scope.RepoPrefix, contracts.ContractType(graph.EdgeReadsConfig), e.To, e),
				}})
			}
		}
		return yield(batch)
	})
}
