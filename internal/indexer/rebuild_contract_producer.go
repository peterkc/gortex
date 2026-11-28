package indexer

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"maps"
	"slices"

	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"go.uber.org/zap"
)

// withRebuildContractSources runs at the post-parse cut, before main. U6
// owns main and the native tail inside use; there are no stage offers here.
func (r rebuildContractReceiptReader) withRebuildContractSources(ctx context.Context, b *store_pebble.BuildStore,
	lease string, root rebuildGoManifest, spillAt int,
	use func(read func(func(contracts.Contract) error) error) error,
) (store_pebble.RebuildContractSourceStats, error) {
	return store_pebble.WithRebuildContractSources(ctx, lease, spillAt, func(add func(contracts.Contract) error) error {
		return r.visitLateContractSourcesAtParseCut(ctx, b.RebuildView(), root, add)
	}, use)
}

// withRebuildContractProducer runs at the native post-main/pre-overlay cut.
// All payload and selection reads finish before use. The caller runs the
// remaining native tail and retains Relations, then calls seal once. A
// failed capture or seal invalidates the private build; no offer is made here.
// Preflight replays detached frozen batches only before the seal begins.
func (r rebuildContractReceiptReader) withRebuildContractProducer(ctx context.Context, b *store_pebble.BuildStore,
	source func(func(contracts.Contract) error) error, early rebuildEarlyContractIDs,
	use func(preflight iter.Seq2[store_pebble.RebuildContractBatch, error], seal func(string, store_pebble.RelationsBase) (store_pebble.StageTables, error)) error,
) error {
	if source == nil || early == nil || use == nil {
		return fmt.Errorf("%w: missing contract producer input", errRebuildContractIDs)
	}
	deferred, err := r.deferredContractLanguages()
	if err != nil {
		return err
	}
	view := b.RebuildView()
	r.linkView = view
	r.proofCache = &rebuildContractReceiptCache{}
	return b.WithRebuildContractRecords(ctx, func(records *store_pebble.RebuildContractRecords) error {
		if err := source(func(c contracts.Contract) error {
			return records.Add(store_pebble.RebuildContractRecord{Contract: c})
		}); err != nil {
			return err
		}
		return records.WithContractSelection(func(s *store_pebble.RebuildContractSelection) error {
			return records.WithContractOutput(func(add func(store_pebble.RebuildContractBatch) error) error {
				if err := r.visitHeldOutContractOmissions(ctx, add); err != nil {
					return err
				}
				if err := r.idx.appendRebuildDIContracts(records); err != nil {
					return err
				}
				if err := r.bindRebuildProviderHandlers(ctx, view, records); err != nil {
					return err
				}
				if err := records.UpgradeBareTypeRefs(); err != nil {
					return err
				}
				if err := r.joinLateRouterPrefixes(ctx, records, func(file string) error {
					r.idx.logger.Debug("rebuild late router native map-order difference", zap.String("path", file))
					return nil
				}); err != nil {
					return err
				}
				if err := r.resolveLateCallReturnTypes(ctx, view, records); err != nil {
					return err
				}
				if err := s.FreezeCompleteIDs(); err != nil {
					return err
				}
				if err := compareRebuildContractIDs(ctx, orderedEarlyContractIDs(early), s.CompleteIDs()); err != nil {
					return fmt.Errorf("compare complete private contract IDs: %w", err)
				}
				if err := r.selectRebuildShapeRefs(ctx, view, records, s, add); err != nil {
					return err
				}
				for batch, err := range r.shapeBatches(ctx, view, s.ShapeRefs()) {
					if err != nil {
						return err
					}
					if err := add(batch); err != nil {
						return err
					}
				}
				if err := inlineRebuildContractShapes(records, r.shapeLookup(ctx, view)); err != nil {
					return err
				}
				if err := r.captureRebuildContractRows(ctx, view, records, s, add); err != nil {
					return err
				}
				// Neither direct Spring delta supplies inputs to the late registry
				// reducers. Classify at the same quiet cut, after D83 election.
				if err := r.captureRebuildSpringBeanCalls(ctx, b, records, s, add); err != nil {
					return err
				}
				err := r.visitSpringConfig(ctx, view, records, add)
				*r.proofCache = rebuildContractReceiptCache{}
				return err
			}, func(batches iter.Seq2[store_pebble.RebuildContractBatch, error], coverage store_pebble.ContractCoverage) error {
				active, attempted := true, false
				var sealErr error
				preflight := func(yield func(store_pebble.RebuildContractBatch, error) bool) {
					if !active || attempted {
						yield(store_pebble.RebuildContractBatch{}, fmt.Errorf("%w: inactive or sealed contract preflight", errRebuildContractIDs))
						return
					}
					batches(yield)
				}
				err := use(preflight, func(dir string, base store_pebble.RelationsBase) (tables store_pebble.StageTables, err error) {
					if !active || attempted {
						return tables, fmt.Errorf("%w: inactive or repeated contract seal", errRebuildContractIDs)
					}
					attempted = true
					defer func() { sealErr = err }()
					if err := base.CheckDeferredLanguages(deferred); err != nil {
						return tables, err
					}
					return prepareRebuildContracts(ctx, b, dir, base, early, s.CompleteIDs(), s.SelectedIDs(), batches, coverage, r.extractorOf)
				})
				active = false
				if err == nil && !attempted {
					return fmt.Errorf("%w: contract output was not sealed", errRebuildContractIDs)
				}
				return errors.Join(err, sealErr)
			})
		})
	})
}

func (r rebuildContractReceiptReader) captureRebuildContractRows(ctx context.Context, view store_pebble.RebuildView,
	records *store_pebble.RebuildContractRecords, selection *store_pebble.RebuildContractSelection,
	add func(store_pebble.RebuildContractBatch) error,
) error {
	var missingOwners int
	err := records.WithOrderedRepresentatives(func(read func(func(store_pebble.RebuildContractRecord) error) error) error {
		var previous string
		return read(func(item store_pebble.RebuildContractRecord) error {
			c := item.Contract
			owner, err := rebuildContractOwner(ctx, view, c)
			if err != nil {
				return err
			}
			if owner == "" {
				missingOwners++ // native counts all elected logical records before D83
			}
			winner := previous != c.ID
			previous = c.ID
			var batch store_pebble.RebuildContractBatch
			for _, e := range appendContractGraphEdges(nil, c, owner) {
				first, err := records.FirstOutputEdge(e)
				if err != nil {
					return err
				}
				if first {
					batch.Edges = append(batch.Edges, e)
				}
			}
			keep, err := r.selectRebuildContractRecord(ctx, view, item, owner)
			if err != nil {
				return err
			}
			if winner && keep {
				if err := selection.SelectID(c.ID); err != nil {
					return err
				}
				batch.Nodes = []*graph.Node{contractGraphNode(c, owner != "")}
			}
			selected, err := selection.Selected(c.ID)
			if err != nil {
				return err
			}
			if !keep || !selected {
				// A later sibling must never replace an omitted canonical
				// winner or publish ownership of an absent target.
				return add(store_pebble.RebuildContractBatch{Omitted: []store_pebble.RebuildContractOmission{rebuildContractRecordOmission(c)}})
			}
			if c.Type == contracts.ContractDI && c.Meta[graph.MetaDIBinding] == graph.DIBindingBean {
				if err := selection.MarkSelectedDI(c); err != nil {
					return err
				}
			}
			return add(batch)
		})
	})
	if err == nil {
		r.idx.warnMissingContractOwners(missingOwners)
	}
	return err
}

// Native snapshots referenced shapes before any inline writes. Elect the
// canonical registry rows first, then keep independently raw-proved refs from
// omitted records. A ref no selected or raw record proves gets type-file O.
func (r rebuildContractReceiptReader) selectRebuildShapeRefs(ctx context.Context, view store_pebble.RebuildView,
	records *store_pebble.RebuildContractRecords, selection *store_pebble.RebuildContractSelection,
	add func(store_pebble.RebuildContractBatch) error,
) error {
	if err := records.WithOrderedRepresentatives(func(read func(func(store_pebble.RebuildContractRecord) error) error) error {
		var previous string
		var winnerKept bool
		return read(func(item store_pebble.RebuildContractRecord) error {
			c := item.Contract
			owner, err := rebuildContractOwner(ctx, view, c)
			if err != nil {
				return err
			}
			keep, err := r.selectRebuildContractRecord(ctx, view, item, owner)
			if err != nil {
				return err
			}
			if c.ID != previous {
				previous, winnerKept = c.ID, keep
			}
			refs := rebuildShapeRefs(c)
			if len(refs) == 0 {
				return nil
			}
			if !keep || !winnerKept {
				raw, err := r.rawRebuildShapeRefs(ctx, c)
				if err != nil {
					return err
				}
				for id := range refs {
					if _, ok := raw[id]; !ok {
						delete(refs, id)
					}
				}
			}
			for _, id := range slices.Sorted(maps.Keys(refs)) {
				if err := selection.RememberShapeRef(id); err != nil {
					return err
				}
			}
			return nil
		})
	}); err != nil {
		return err
	}
	return records.VisitRepresentatives(func(item store_pebble.RebuildContractRecord) error {
		for _, id := range slices.Sorted(maps.Keys(rebuildShapeRefs(item.Contract))) {
			selected, err := selection.HasShapeRef(id)
			if err != nil {
				return err
			}
			if selected {
				continue
			}
			n, err := view.Node(ctx, id)
			if err != nil {
				return err
			}
			if n == nil || (n.Kind != graph.KindType && n.Kind != graph.KindInterface) || n.StartLine <= 0 || contracts.ShapeExtractorForPath(n.FilePath) == nil {
				continue // native cannot emit a shape for this reference
			}
			if err := add(store_pebble.RebuildContractBatch{Omitted: []store_pebble.RebuildContractOmission{rebuildShapeOmission(item.Contract, n)}}); err != nil {
				return err
			}
		}
		return nil
	})
}
