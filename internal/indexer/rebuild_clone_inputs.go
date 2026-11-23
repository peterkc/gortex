package indexer

import (
	"context"
	"fmt"
	"iter"

	"github.com/zzet/gortex/internal/clones"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/pathkey"
)

type rebuildCloneInput struct {
	ID, FilePath, Language string
	Line, Tokens           int
	Shingles               []uint64
}

// visitRebuildCloneInputs checks the complete clone cohort on a quiet build.
// The checked receipt iterator is in literal path order. Use all languages:
// even a deferred body can change the CMS filter or a whole LSH bucket.
// The caller may stage results, but must discard them on any error.
func visitRebuildCloneInputs(ctx context.Context, view store_pebble.RebuildView,
	receipts iter.Seq2[store_pebble.ParseFacts, error], yield func(rebuildCloneInput) error,
) error {
	var matched uint64
	var prior string
	for facts, err := range receipts {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		path := facts.File.FilePath
		if path <= prior || facts.EffectiveLanguage == "" {
			return fmt.Errorf("%w: out-of-order or unattributed clone receipt %q", errParseEntryCorrupt, path)
		}
		prior = path
		file := pathkey.Normalize(path)
		seen := make(map[string]struct{}, len(facts.Shingles))
		for _, n := range facts.Nodes {
			if err := ctx.Err(); err != nil {
				return err
			}
			shingles, present := facts.Shingles[n.ID]
			tokens, hasTokens := n.Meta[cloneTokensMetaKey]
			if !present && !hasTokens {
				continue
			}
			count, ok := tokens.(int)
			_, duplicate := seen[n.ID]
			if !present || !ok || count < clones.MinTokens || len(shingles) == 0 || len(shingles) > count-clones.ShingleK+1 ||
				duplicate || n.FilePath != file || n.StartLine < 1 || n.EndLine < n.StartLine ||
				(n.Kind != graph.KindFunction && n.Kind != graph.KindMethod) {
				return fmt.Errorf("%w: invalid clone input %q from %q", errParseEntryCorrupt, n.ID, path)
			}
			for i := 1; i < len(shingles); i++ {
				if shingles[i-1] >= shingles[i] {
					return fmt.Errorf("%w: unordered or duplicate clone shingle for %q", errParseEntryCorrupt, n.ID)
				}
			}
			if err := view.CheckRebuildCloneInput(ctx, n, shingles); err != nil {
				return fmt.Errorf("clone input %q: %w", n.ID, err)
			}
			seen[n.ID] = struct{}{}
			matched++
			if err := yield(rebuildCloneInput{ID: n.ID, FilePath: file, Language: facts.EffectiveLanguage,
				Line: n.StartLine, Tokens: count, Shingles: shingles}); err != nil {
				return err
			}
		}
		if len(seen) != len(facts.Shingles) {
			return fmt.Errorf("%w: clone receipt %q contains shingles without a node", errParseEntryCorrupt, path)
		}
	}
	// Each matched key is unique: duplicate node IDs in one receipt refuse,
	// and an ID cannot match two distinct manifest-normalized file paths.
	// Thus the exact working count also proves absence of orphan rows.
	var saved uint64
	if err := view.VisitCloneShingles(ctx, "", func(string, []uint64) error {
		saved++
		return nil
	}); err != nil {
		return err
	}
	if matched != saved {
		return fmt.Errorf("%w: clone receipt cohort %d differs from working cohort %d", errParseEntryCorrupt, matched, saved)
	}
	return nil
}
