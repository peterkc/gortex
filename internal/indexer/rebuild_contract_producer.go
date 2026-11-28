package indexer

import (
	"context"
	"errors"
	"fmt"
	"iter"

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
func (r rebuildContractReceiptReader) withRebuildContractProducer(ctx context.Context, b *store_pebble.BuildStore,
	source func(func(contracts.Contract) error) error, early rebuildEarlyContractIDs,
	use func(seal func(string, store_pebble.RelationsBase) (store_pebble.StageTables, error)) error,
) error {
	if source == nil || early == nil || use == nil {
		return fmt.Errorf("%w: missing contract producer input", errRebuildContractIDs)
	}
	deferred, err := r.deferredContractLanguages()
	if err != nil {
		return err
	}
	view := b.RebuildView()
	return b.WithRebuildContractRecords(ctx, func(records *store_pebble.RebuildContractRecords) error {
		if err := source(func(c contracts.Contract) error {
			return records.Add(store_pebble.RebuildContractRecord{Contract: c})
		}); err != nil {
			return err
		}
		return records.WithContractSelection(func(s *store_pebble.RebuildContractSelection) error {
			return records.WithContractOutput(func(add func(store_pebble.RebuildContractBatch) error) error {
				if err := visitRebuildSpringBeanCalls(ctx, b, r.idx.repoPrefix, func(e *graph.Edge) error {
					return add(store_pebble.RebuildContractBatch{Edges: []*graph.Edge{e}})
				}); err != nil {
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
				if err := r.visitSpringConfig(ctx, records, add); err != nil {
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
				// Native snapshots referenced shapes before any inline writes.
				if err := records.VisitRepresentatives(func(item store_pebble.RebuildContractRecord) error {
					owner, err := rebuildContractOwner(ctx, view, item.Contract)
					if err != nil {
						return err
					}
					keep, err := r.selectRebuildContractRecord(ctx, view, item, owner)
					if err != nil || !keep {
						return err
					}
					return rememberRebuildShapeRefs(s, item.Contract)
				}); err != nil {
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
				return r.captureRebuildContractRows(ctx, view, records, s, add)
			}, func(batches iter.Seq2[store_pebble.RebuildContractBatch, error], coverage store_pebble.ContractCoverage) error {
				active, attempted := true, false
				var sealErr error
				err := use(func(dir string, base store_pebble.RelationsBase) (tables store_pebble.StageTables, err error) {
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
			return add(batch)
		})
	})
	if err == nil {
		r.idx.warnMissingContractOwners(missingOwners)
	}
	return err
}
