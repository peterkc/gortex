package store_pebble

import (
	"context"
	"errors"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/graph"
)

var rebuildSpringBeanPrefix = []byte{'g', 'q', 's'}

// VisitSpringBeanConsumers joins the native bean declarations and Java method
// parameter tokens on the quiet post-link graph. It visits the winning class
// for each native (class, bean) pair, without adding any working graph edges.
// Only one node, edge, or matched candidate is decoded at a time. The index
// and seen pairs use owned ranges in the existing pending Pebble database.
func (b *BuildStore) VisitSpringBeanConsumers(ctx context.Context, repo string,
	beanType func(*graph.Edge) string,
	methodParams func(*graph.Node) (class string, tokens map[string]struct{}),
	yield func(class string, bean *graph.Edge) error,
) (err error) {
	if beanType == nil || methodParams == nil || yield == nil {
		return invalid("Spring bean candidates need native selectors and a visitor")
	}
	occupied, err := rebuildGuardHasPrefix(ctx, b.pending, rebuildSpringBeanPrefix)
	if err != nil {
		return err
	}
	if occupied {
		return invalid("Spring bean scratch range occupied")
	}
	defer func() {
		err = errors.Join(err, b.pending.DeleteRange(rebuildSpringBeanPrefix, prefixEnd(rebuildSpringBeanPrefix), pebble.NoSync))
	}()
	state := func(key []byte) (bool, error) {
		return pendingGet(b.pending, key, func([]byte) (bool, error) { return true, nil })
	}
	put := func(key, value []byte) error {
		return b.putRebuildScratch(key, value, 16<<20, "Spring bean")
	}
	view := b.RebuildView()
	var haveBeans bool
	if err := view.VisitDIContractEdges(ctx, repo, func(e *graph.Edge) error {
		if token := beanType(e); token != "" {
			haveBeans = true
			return put(springBeanScratch('t', token), nil)
		}
		return nil
	}); err != nil || !haveBeans {
		return err
	}
	if err := view.VisitNodes(ctx, func(n *graph.Node) error {
		if n.RepoPrefix != repo || n.Language != "java" || n.Kind != graph.KindMethod {
			return nil
		}
		class, tokens := methodParams(n)
		if class == "" {
			return nil
		}
		for token := range tokens {
			if err := ctx.Err(); err != nil {
				return err
			}
			wanted, err := state(springBeanScratch('t', token))
			if err != nil {
				return err
			}
			if wanted {
				if err := put(springBeanScratch('m', token, n.ID), []byte(class)); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		return err
	}
	// For Provides alone, working keys are in native From, To, FilePath,
	// Line order. The method bucket is likewise in native method ID order.
	return view.VisitDIContractEdges(ctx, repo, func(e *graph.Edge) error {
		token := beanType(e)
		if token == "" {
			return nil
		}
		prefix := springBeanScratch('m', token)
		return pendingVisit(ctx, b.pending, prefix, func(r row) error {
			method, rest, err := readKeyString(r.key[len(prefix):])
			if err != nil {
				return err
			}
			class := string(r.value)
			if len(rest) != 0 || method == "" || class == "" {
				return invalid("Spring bean consumer scratch row")
			}
			if method == e.To || class == e.To {
				return nil
			}
			// Match the native dedupe key exactly, including its separator.
			key := springBeanScratch('s', class+"->"+e.To)
			seen, err := state(key)
			if err != nil || seen {
				return err
			}
			if err := put(key, nil); err != nil {
				return err
			}
			return yield(class, e)
		})
	})
}

func springBeanScratch(col byte, fields ...string) []byte {
	return keyStrings([]byte{'g', 'q', 's', col}, fields...)
}
