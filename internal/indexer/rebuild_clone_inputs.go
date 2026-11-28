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
	Signed                 *clones.Signature
}

// visitRebuildCloneInputs checks the clone cohort on a quiet build.
// The checked receipt iterator is in literal path order. On exact checkouts
// D72 F excludes deferred bodies before they can affect the CMS or LSH buckets.
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
		if err := checkRebuildCloneReceipt(facts, prior); err != nil {
			return err
		}
		prior = path
		file := pathkey.Normalize(path)
		seen := make(map[string]struct{}, len(facts.Shingles))
		rawCount := 0
		for _, n := range facts.Nodes {
			if err := ctx.Err(); err != nil {
				return err
			}
			shingles, present := facts.Shingles[n.ID]
			_, typedRaw := n.Meta[cloneShinglesMetaKey].([]uint64)
			tokens, hasTokens := n.Meta[cloneTokensMetaKey]
			callable := n.Kind == graph.KindFunction || n.Kind == graph.KindMethod
			if !present && !typedRaw && callable {
				stamp, _ := n.Meta[cloneSigMetaKey].(string)
				if sig, ok := clones.DecodeSignature(stamp); ok {
					if n.FilePath != file {
						return fmt.Errorf("%w: signed clone %q recorded by %q", errParseEntryCorrupt, n.ID, path)
					}
					if _, duplicate := seen[n.ID]; duplicate {
						return fmt.Errorf("%w: duplicate signed clone %q", errParseEntryCorrupt, n.ID)
					}
					row, err := checkRebuildSignedClone(ctx, view, n, stamp)
					if err != nil {
						return err
					}
					row.Language, row.Signed = facts.EffectiveLanguage, &sig
					seen[n.ID] = struct{}{}
					if err := yield(row); err != nil {
						return err
					}
					continue
				}
			}
			if !present && !hasTokens && !typedRaw {
				continue
			}
			count, ok := tokens.(int)
			_, duplicate := seen[n.ID]
			if !present || !ok || count < clones.MinTokens || len(shingles) == 0 || len(shingles) > count-clones.ShingleK+1 ||
				duplicate || n.FilePath != file || n.StartLine < 1 || n.EndLine < n.StartLine ||
				!callable {
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
			rawCount++
			if err := yield(rebuildCloneInput{ID: n.ID, FilePath: file, Language: facts.EffectiveLanguage,
				Line: n.StartLine, Tokens: count, Shingles: shingles}); err != nil {
				return err
			}
		}
		if rawCount != len(facts.Shingles) {
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

// censusRebuildCloneInputs feeds every claim before clone eligibility checks.
// If a settled row no longer matches, finish the same receipt replay without
// checking more live rows: the complete census can explain that mismatch.
func censusRebuildCloneInputs(ctx context.Context, build *store_pebble.BuildStore,
	receipts iter.Seq2[store_pebble.ParseFacts, error], maxScratchBytes uint64, yield func(rebuildCloneInput) error,
) (store_pebble.RebuildShadowedIDStats, error) {
	var inputErr error
	stats, err := build.CountRebuildShadowedIDs(ctx, maxScratchBytes, func(claimNode func(string, uint64) error, claimEdge func(graph.EdgeIdentity, uint64) error) error {
		var claimErr error
		checked := func(next func(store_pebble.ParseFacts, error) bool) {
			var prior string
			var ordinal uint64
			visiting := true
			for facts, err := range receipts {
				if err == nil {
					err = ctx.Err()
				}
				if err == nil {
					err = checkRebuildCloneReceipt(facts, prior)
				}
				if err == nil {
					prior = facts.File.FilePath
					ordinal++
					for _, n := range facts.Nodes {
						if err = claimNode(n.ID, ordinal); err != nil {
							break
						}
					}
				}
				if err == nil {
					for _, e := range facts.Edges {
						if err = claimEdge(graph.EdgeIdentityFor(e), ordinal); err != nil {
							break
						}
					}
				}
				if err != nil {
					claimErr = err
				}
				if visiting {
					visiting = next(facts, err)
				}
				if err != nil {
					return
				}
			}
		}
		inputErr = visitRebuildCloneInputs(ctx, build.RebuildView(), checked, yield)
		return claimErr
	})
	if err != nil {
		return store_pebble.RebuildShadowedIDStats{}, err
	}
	if count := stats.Shadowed + stats.ShadowedEdges; count != 0 {
		// These disjoint claim domains are separate from possible predecessors.
		return store_pebble.RebuildShadowedIDStats{}, fmt.Errorf("clone receipt shadowed_id=%d shadowed_edge=%d: %w",
			stats.Shadowed, stats.ShadowedEdges, &store_pebble.RebuildUnknownProvenanceError{Count: count})
	}
	if inputErr != nil {
		return store_pebble.RebuildShadowedIDStats{}, inputErr
	}
	return stats, nil
}

func checkRebuildCloneReceipt(facts store_pebble.ParseFacts, prior string) error {
	path := facts.File.FilePath
	if path <= prior || facts.EffectiveLanguage == "" {
		return fmt.Errorf("%w: out-of-order or unattributed clone receipt %q", errParseEntryCorrupt, path)
	}
	return nil
}

func checkRebuildSignedClone(ctx context.Context, view store_pebble.RebuildView, receipt *graph.Node, stamp string) (rebuildCloneInput, error) {
	if err := view.CheckRebuildCloneInput(ctx, receipt, nil); err != nil {
		return rebuildCloneInput{}, fmt.Errorf("signed clone input %q: %w", receipt.ID, err)
	}
	// The check bounds the encoded row and verifies its location. The view
	// stays quiet while we read the full metadata for native item selection.
	live, err := view.Node(ctx, receipt.ID)
	if err != nil {
		return rebuildCloneInput{}, err
	}
	tokens := tokensFromMeta(receipt)
	if live != nil {
		got, _ := live.Meta[cloneSigMetaKey].(string)
		_, raw := live.Meta[cloneShinglesMetaKey].([]uint64)
		if !raw && got == stamp && tokensFromMeta(live) == tokens {
			return rebuildCloneInput{ID: receipt.ID, FilePath: receipt.FilePath, Line: receipt.StartLine, Tokens: tokens}, nil
		}
	}
	return rebuildCloneInput{}, fmt.Errorf("%w: signed clone metadata differs from receipt for %q", store_pebble.ErrRebuildView, receipt.ID)
}
