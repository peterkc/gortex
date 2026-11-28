package store_pebble

import (
	"context"
	"strconv"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/graph"
)

// MarkRebuildFabricIsolation uses the Fabric role predicate and both output
// identities without admitting held candidates to the live Fabric reducer.
func (b *BuildStore) MarkRebuildFabricIsolation(ctx context.Context, admitted bool, gateCauses []string,
	language func(string) string, candidate rebuildFabricCandidate, makeEdge func(*graph.Node, *graph.Node, string) *graph.Edge,
) error {
	p := rebuildFrameworkIsolation{b: b, language: language, cause: "fabric"}
	return p.scratch(ctx, rebuildFabricPrefix, func() error {
		offer := func(n *graph.Node, deferred string) error {
			component, normalized, native := candidate(n)
			if component == "" {
				return nil
			}
			return p.candidate(rebuildFabricKey('c', normalized, strconv.FormatBool(native), n.ID), n, deferred)
		}
		if err := b.visitRebuildHeldNodes(ctx, func(n *graph.Node, _ string, _ []byte) error {
			if component, _, _ := candidate(n); component == "" {
				return nil
			}
			deferred, err := rebuildHeldLanguage(b.pending, n.ID)
			if err != nil {
				return err
			}
			return offer(n, deferred)
		}); err != nil {
			return err
		}
		held, err := rebuildGuardHasPrefix(ctx, b.pending, rebuildFabricKey('c'))
		if err != nil || !held && admitted {
			return err
		}
		if err := b.RebuildView().visit(ctx, []byte{familyNode}, func(_ *pebble.Snapshot, r row) error {
			n, err := checkedRebuildLightNode(r, p.cause)
			if err != nil || n.Kind != graph.KindType {
				return err
			}
			n, err = decodeNode(r.value)
			if err != nil {
				return err
			}
			return offer(n, "")
		}); err != nil {
			return err
		}
		prefix := rebuildFabricKey('c')
		return pendingVisit(ctx, b.pending, prefix, func(r row) error {
			normalized, tail, err := readKeyString(r.key[len(prefix):])
			if err != nil {
				return err
			}
			role, _, err := readKeyString(tail)
			if err != nil || role != "false" {
				return err
			}
			spec, specHeld, err := readFrameworkIsolationCandidate(r)
			if err != nil {
				return err
			}
			return pendingVisit(ctx, b.pending, rebuildFabricKey('c', normalized, "true"), func(r row) error {
				manager, managerHeld, err := readFrameworkIsolationCandidate(r)
				if err != nil || manager.ID == spec.ID || specHeld != "" && managerHeld != "" ||
					specHeld == "" && managerHeld == "" && admitted {
					return err
				}
				deferred := specHeld
				if deferred == "" {
					deferred = managerHeld
				}
				// The component is payload-only; this projection uses the
				// builder for its two directional identities, not graph writes.
				for _, e := range []*graph.Edge{makeEdge(spec, manager, normalized), makeEdge(manager, spec, normalized)} {
					if graph.IsProxyID(e.From) || graph.IsProxyID(e.To) || graph.StructuralEdgeTargetInvalid(e.Kind, e.To) {
						continue
					}
					fresh, err := p.fresh(e)
					if err != nil {
						return err
					}
					if !fresh {
						continue
					}
					if deferred != "" {
						if err := p.markEdge(ctx, e, deferred, "held-endpoint"); err != nil {
							return err
						}
						continue
					}
					for _, lang := range gateCauses {
						if err := p.markEdge(ctx, e, lang, p.cause); err != nil {
							return err
						}
					}
				}
				return nil
			})
		})
	})
}
