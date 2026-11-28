package store_pebble

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/graph"
)

const rebuildSpringBeanRowLimit = 16 << 20

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
		return b.putRebuildScratch(key, value, rebuildSpringBeanRowLimit, "Spring bean")
	}
	view := b.RebuildView()
	var haveBeans bool
	if err := view.visitSpringBeanEdges(ctx, repo, beanType, func(token string, _ *graph.Edge) error {
		haveBeans = true
		return put(springBeanScratch('t', token), nil)
	}); err != nil || !haveBeans {
		return err
	}
	if err := view.visit(ctx, []byte{familyNode}, func(_ *pebble.Snapshot, r row) error {
		n, err := checkedRebuildLightNode(r, "Spring bean")
		if err != nil || n.RepoPrefix != repo || n.Language != "java" || n.Kind != graph.KindMethod {
			return err
		}
		if len(r.key)+len(r.value) > rebuildSpringBeanRowLimit {
			return fmt.Errorf("%w: Spring bean Java method", ErrRebuildLinkInputSize)
		}
		n, err = decodeNode(r.value)
		if err != nil {
			return err
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
	return view.visitSpringBeanEdges(ctx, repo, beanType, func(token string, e *graph.Edge) error {
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

// visitSpringBeanEdges skips other edge kinds and foreign sources before
// decoding metadata. Oversized Provides rows need only the two bean fields
// to decide whether the native consumer would read them.
func (v RebuildView) visitSpringBeanEdges(ctx context.Context, repo string, beanType func(*graph.Edge) string,
	yield func(string, *graph.Edge) error,
) error {
	return v.visit(ctx, []byte{familyEdge, edgeOut}, func(snap *pebble.Snapshot, r row) error {
		from, rest, err := readKeyString(r.key[2:])
		if err != nil {
			return err
		}
		kind, _, err := readKeyString(rest)
		if err != nil || graph.EdgeKind(kind) != graph.EdgeProvides {
			return err
		}
		n, err := pendingGet(snap, pendingNodeKey(from), decodeNodeLight)
		if err != nil || n == nil {
			return err
		}
		if n.ID != from {
			return invalid("working Spring bean edge source identity")
		}
		if n.RepoPrefix != repo {
			return nil
		}
		tooLarge := len(r.key)+len(r.value) > rebuildSpringBeanRowLimit
		decode := decodeEdge
		if tooLarge {
			decode = func(raw []byte) (*graph.Edge, error) {
				return readEdgeMetadata(raw, readSpringBeanRoleMetadata)
			}
		}
		e, err := decode(r.value)
		if err != nil {
			return err
		}
		if !bytes.Equal(r.key, pendingEdgeKey(e)) {
			return invalid("working Spring bean edge identity")
		}
		token := beanType(e)
		if token == "" {
			return nil
		}
		if tooLarge {
			return fmt.Errorf("%w: Spring bean Provides edge", ErrRebuildLinkInputSize)
		}
		return yield(token, e)
	})
}

func readSpringBeanRoleMetadata(c *recordCursor) map[string]any {
	return readRebuildRoleMetadata(c, func(key string) bool {
		return key == graph.MetaDIBinding || key == graph.MetaDIProvidesFor
	})
}

func springBeanScratch(col byte, fields ...string) []byte {
	return keyStrings([]byte{'g', 'q', 's', col}, fields...)
}
