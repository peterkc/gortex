package store_pebble

import (
	"context"
	"slices"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/graph"
)

// MarkRebuildSvelteKitIsolation compares the isolated and held route winners.
// Only a checked server-file skip has the exact native skip projection; other
// held parsed roles use the coarse cause when they can alter a route answer.
func (b *BuildStore) MarkRebuildSvelteKitIsolation(ctx context.Context, admitted bool, gateCauses []string,
	language func(string) string, rules RebuildSvelteKitLoadRules,
) error {
	p := rebuildSvelteKitIsolation{
		rebuildFrameworkIsolation: rebuildFrameworkIsolation{b: b, language: language, cause: "sveltekit-load"},
		rebuildSvelteKitLoad: rebuildSvelteKitLoad{
			rebuildCallMoves: rebuildCallMoves{b: b, prefix: rebuildSvelteKitLoadPrefix, rowBytes: 16 << 20, label: "SvelteKit isolation", inputRow: "node row"},
			rules: rules,
		},
		admitted: admitted, gateCauses: gateCauses,
	}
	return p.scratch(ctx, rebuildSvelteKitLoadPrefix, func() error { return p.run(ctx) })
}

type rebuildSvelteKitIsolation struct {
	rebuildFrameworkIsolation
	rebuildSvelteKitLoad
	admitted   bool
	gateCauses []string
}

func (p rebuildSvelteKitIsolation) run(ctx context.Context) error {
	b := p.rebuildFrameworkIsolation.b
	if err := b.visitRebuildHeldNodes(ctx, func(n *graph.Node, receipt string, _ []byte) error {
		dir, ok := p.role(n)
		if !ok {
			return nil
		}
		if err := p.offer('h', dir, n); err != nil {
			return err
		}
		if receipt == "" && n.Kind == graph.KindFile && n.ID == n.FilePath {
			return p.put(p.key('s', n.ID), nil)
		}
		return nil
	}); err != nil {
		return err
	}
	held, err := rebuildGuardHasPrefix(ctx, b.pending, p.column('h'))
	if err != nil || !held && p.admitted {
		return err
	}
	if err := b.RebuildView().visit(ctx, []byte{familyNode}, func(_ *pebble.Snapshot, r row) error {
		n, err := checkedRebuildLightNode(r, p.label)
		if err != nil {
			return err
		}
		if n.Kind == graph.KindType && p.rules.PageFile(n.FilePath) {
			n, err = decodeNode(r.value)
			if err != nil {
				return err
			}
		}
		dir, ok := p.role(n)
		if !ok {
			return nil
		}
		return p.offer('l', dir, n)
	}); err != nil {
		return err
	}
	prefix := p.column('d')
	return pendingVisit(ctx, b.pending, prefix, func(r row) error {
		dir, rest, err := readKeyString(r.key[len(prefix):])
		if err != nil || len(rest) != 0 {
			return invalid("SvelteKit isolation route directory")
		}
		return p.route(ctx, dir)
	})
}

func (p rebuildSvelteKitIsolation) route(ctx context.Context, dir string) error {
	b := p.rebuildFrameworkIsolation.b
	var old *graph.Edge
	if p.admitted {
		var err error
		old, err = p.reference('l', dir)
		if err != nil {
			return err
		}
	}
	var changed []*graph.Node
	for _, kind := range []graph.NodeKind{graph.KindType, graph.KindFunction, graph.KindFile} {
		live, err := p.winner('l', dir, kind)
		if err != nil {
			return err
		}
		held, err := p.winner('h', dir, kind)
		if err != nil {
			return err
		}
		winner := live
		if held != nil && (live == nil || live.ID < held.ID) {
			winner = held
			changed = append(changed, held)
		}
		if winner != nil {
			if err := p.offer('f', dir, winner); err != nil {
				return err
			}
		}
	}
	full, err := p.reference('f', dir)
	if err != nil {
		return err
	}
	// An elected load takes precedence over any server-file fallback.
	load, err := p.winner('f', dir, graph.KindFunction)
	if err != nil {
		return err
	}
	if load != nil {
		changed = slices.DeleteFunc(changed, func(n *graph.Node) bool { return n.Kind == graph.KindFile })
	}
	oldFresh, err := p.rebuildFrameworkIsolation.fresh(old)
	if err != nil {
		return err
	}
	fullFresh, err := p.rebuildFrameworkIsolation.fresh(full)
	if err != nil || !oldFresh && !fullFresh || oldFresh && fullFresh && graph.EdgeIdentityFor(old) == graph.EdgeIdentityFor(full) {
		return err
	}
	causes := make(map[string]string)
	for _, n := range changed {
		deferred, err := rebuildHeldLanguage(b.pending, n.ID)
		if err != nil {
			return err
		}
		checkedSkip, err := pendingGet(b.pending, p.key('s', n.ID), fnPointerPresent)
		if err != nil {
			return err
		}
		cause := p.cause
		if checkedSkip && len(changed) == 1 {
			cause = "held-endpoint"
		}
		causes[deferred] = cause
	}
	if !p.admitted && len(changed) == 0 {
		for _, deferred := range p.gateCauses {
			causes[deferred] = p.cause
		}
	}
	for deferred, cause := range causes {
		if oldFresh {
			if err := p.markEdge(ctx, old, deferred, cause); err != nil {
				return err
			}
		}
		if fullFresh {
			if err := p.markEdge(ctx, full, deferred, cause); err != nil {
				return err
			}
		}
	}
	return nil
}
