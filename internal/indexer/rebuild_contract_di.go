package indexer

import (
	"context"
	"reflect"

	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// visitRebuildSpringBeanCalls captures the native contract-era Call effects.
// Keep these rows as a private delta until Contracts validates its Calls.
func visitRebuildSpringBeanCalls(ctx context.Context, b *store_pebble.BuildStore, repo string, yield func(method string, bean, call *graph.Edge) error) error {
	return b.VisitSpringBeanConsumers(ctx, repo,
		func(e *graph.Edge) string {
			bean, _ := springBeanFromEdge(e)
			return bean.typeName
		},
		func(n *graph.Node) (string, map[string]struct{}) {
			params, _ := n.Meta["params_src"].(string)
			return enclosingClassID(n), javaIdentifierSet(params)
		},
		func(class, method string, e *graph.Edge) error {
			bean, _ := springBeanFromEdge(e)
			return yield(method, e, springBeanCall(class, bean))
		})
}

// appendRebuildDIContracts reads the native DI input. The Spring Call delta
// does not change Provides or Consumes.
func (idx *Indexer) appendRebuildDIContracts(records *store_pebble.RebuildContractRecords) error {
	return records.AppendDIContracts(idx.repoPrefix, func(e *graph.Edge) (contracts.Contract, bool) {
		c, ok := diContractFromEdge(e)
		if ok {
			c.RepoPrefix, c.WorkspaceID, c.ProjectID = idx.repoPrefix, idx.workspaceID, idx.projectID
		}
		return c, ok
	})
}

// captureRebuildSpringBeanCalls preserves the native first class/factory pair.
// A negative proof never promotes a later method or bean declaration.
func (r rebuildContractReceiptReader) captureRebuildSpringBeanCalls(ctx context.Context, b *store_pebble.BuildStore,
	records *store_pebble.RebuildContractRecords, selection *store_pebble.RebuildContractSelection, yield func(store_pebble.RebuildContractBatch) error,
) error {
	view := b.RebuildView()
	var previousBean graph.EdgeIdentity
	var beanProved bool
	return visitRebuildSpringBeanCalls(ctx, b, r.idx.repoPrefix, func(method string, bean, call *graph.Edge) error {
		first, err := records.FirstOutputEdge(call)
		if err != nil || !first {
			// Native ignores an incumbent. Relations owns it, even if its
			// later D72 prune removes it before Contracts.
			return err
		}
		c, _ := diContractFromEdge(bean)
		c = r.scopedRawContract(c)
		if identity := graph.EdgeIdentityFor(bean); identity != previousBean {
			previousBean = identity
			beanProved, err = selection.SelectedDI(c)
			if err != nil {
				return err
			}
			if beanProved {
				beanProved, err = r.proveSpringBeanFactory(ctx, view, bean)
				if err != nil {
					return err
				}
			}
		}
		keep := beanProved
		if keep {
			keep, err = r.proveSpringBeanConsumer(ctx, view, method, call)
			if err != nil {
				return err
			}
		}
		if !keep {
			return yield(store_pebble.RebuildContractBatch{Omitted: []store_pebble.RebuildContractOmission{
				store_pebble.RebuildContractEdgeOmission(c.RepoPrefix, c.Type, c.ID, call),
			}})
		}
		return yield(store_pebble.RebuildContractBatch{Edges: []*graph.Edge{call}})
	})
}

func (r rebuildContractReceiptReader) proveSpringBeanFactory(ctx context.Context, view store_pebble.RebuildView, bean *graph.Edge) (bool, error) {
	factory, err := view.Node(ctx, bean.To)
	if err != nil || factory == nil || factory.FilePath != bean.FilePath || factory.Kind != graph.KindMethod || r.extractorOf(factory.FilePath) != "java" {
		return false, err
	}
	base, raw, err := r.springRawDeclaration(ctx, factory)
	if err != nil || raw == nil {
		return false, err
	}
	for _, e := range base.Edges {
		if reflect.DeepEqual(e, bean) {
			return true, nil
		}
	}
	return false, nil
}

func (r rebuildContractReceiptReader) proveSpringBeanConsumer(ctx context.Context, view store_pebble.RebuildView, methodID string, call *graph.Edge) (bool, error) {
	method, err := view.Node(ctx, methodID)
	if err != nil || method == nil || r.extractorOf(method.FilePath) != "java" {
		return false, err
	}
	base, raw, err := r.springRawDeclaration(ctx, method)
	if err != nil || raw == nil {
		return false, err
	}
	rawParams, _ := raw.Meta["params_src"].(string)
	liveParams, _ := method.Meta["params_src"].(string)
	if rawParams != liveParams || enclosingClassID(raw) != call.From {
		return false, nil
	}
	class, err := view.Node(ctx, call.From)
	if err != nil || class == nil || class.FilePath != method.FilePath || (class.Kind != graph.KindType && class.Kind != graph.KindInterface) {
		return false, err
	}
	return class.RepoPrefix == method.RepoPrefix && springRawNode(base, class) != nil, nil
}
