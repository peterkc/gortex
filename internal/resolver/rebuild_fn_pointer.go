package resolver

import (
	"context"
	"errors"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// CaptureRebuildFnPointer records the pass's two candidate identity groups at
// framework census, before any of the registry passes act. The caller closes
// the returned pass if it isn't run. This does not replace framework admission.
func CaptureRebuildFnPointer(ctx context.Context, b *store_pebble.BuildStore, spillBytes int) (*store_pebble.RebuildFnPointerPass, error) {
	return b.CaptureFnPointerCensus(ctx, spillBytes, func(e *graph.Edge) bool {
		via, _ := e.Meta["via"].(string)
		c := graph.FrameworkCensusEdge{Via: via}
		return (e.Kind == graph.EdgeReferences && fnPtrRegCandidateEdge(c)) ||
			(e.Kind == graph.EdgeCalls && fnPtrDispatchCandidateEdge(c))
	})
}

// RebuildFnPointerStats reports native resolved count and sweep cap, plus
// coordinator scratch accounting. A legitimate cap does not fail the pass.
type RebuildFnPointerStats struct {
	Resolved int
	Sweeps   int
	Capped   bool
	Spill    store_pebble.FnPointerSpill
}

// RunRebuildFnPointer runs at the native registry turn and closes p. The
// coordinator must discard the private build on error and not seal relations.
func RunRebuildFnPointer(ctx context.Context, p *store_pebble.RebuildFnPointerPass) (stats RebuildFnPointerStats, err error) {
	defer func() {
		err = errors.Join(err, p.Close())
		if err != nil {
			stats = RebuildFnPointerStats{}
		}
	}()
	haveSeed := false
	err = p.VisitInputs(ctx, graph.EdgeReferences, func(e *graph.Edge) error {
		st, field, ok := rebuildFnPointerSlot(e, fnPtrRegVia)
		if !ok {
			return nil
		}
		slot := st + "\x00" + field
		if cs, _ := e.Meta["fnptr_copy_struct"].(string); cs != "" {
			cf, _ := e.Meta["fnptr_copy_field"].(string)
			return p.SaveCopy(e, slot, cs+"\x00"+cf)
		}
		name, _ := e.Meta["fnptr_fn"].(string)
		if name == "" {
			return nil
		}
		target, err := rebuildFnPointerTarget(ctx, p.View(), e, name)
		if err != nil || target == nil {
			return err
		}
		haveSeed = true
		if err := p.Seed(slot, target.ID); err != nil {
			return err
		}
		if e.To == target.ID {
			return nil
		}
		oldTo := e.To
		e.To, e.Origin = target.ID, graph.OriginASTInferred
		return p.StageMove(ctx, graph.EdgeReindex{Edge: e, OldTo: oldTo})
	})
	if err != nil {
		return stats, err
	}
	if haveSeed {
		const maxSweeps = 8
		for stats.Sweeps < maxSweeps {
			changed, err := p.Sweep(ctx)
			if err != nil {
				return stats, err
			}
			stats.Sweeps++
			if !changed {
				break
			}
			stats.Capped = stats.Sweeps == maxSweeps
		}
		err = p.VisitInputs(ctx, graph.EdgeCalls, func(e *graph.Edge) error {
			st, field, ok := rebuildFnPointerSlot(e, fnPtrDispatchVia)
			if !ok {
				return nil
			}
			ids, err := p.Targets(ctx, st+"\x00"+field, fnPointerFanoutCap)
			if err != nil {
				return err
			}
			var first *graph.Node
			if len(ids) > 0 {
				first = &graph.Node{ID: ids[0]}
			}
			var moves []graph.EdgeReindex
			stats.Resolved += fnPtrRebind(e, first, &moves)
			for _, move := range moves {
				if err := p.StageMove(ctx, move); err != nil {
					return err
				}
			}
			for i := 1; i < len(ids); i++ {
				if err := p.StageFanout(fnPtrFanoutEdge(e, &graph.Node{ID: ids[i]}, st, field)); err != nil {
					return err
				}
				stats.Resolved++
			}
			return nil
		})
		if err != nil {
			return stats, err
		}
	}
	// Reindexing refs does not change slot membership, name candidates, or
	// the captured call group. Defer all graph writes until reads succeed.
	stats.Spill, err = p.Apply(ctx)
	return stats, err
}

func rebuildFnPointerSlot(e *graph.Edge, wantVia string) (st, field string, ok bool) {
	via, _ := e.Meta["via"].(string)
	st, _ = e.Meta["fnptr_struct"].(string)
	field, _ = e.Meta["fnptr_field"].(string)
	return st, field, via == wantVia && st != "" && field != ""
}

// rebuildFnPointerTarget reduces the live AllRepos exact-name stream used by
// SelectCandidatePage. No selection here depends on candidate order: a winner
// must be unique globally, in the getter file, or in the caller's file. Use
// the stream directly so one large name doesn't need to fit in a result page.
func rebuildFnPointerTarget(ctx context.Context, v store_pebble.RebuildView, e *graph.Edge, name string) (*graph.Node, error) {
	caller, err := v.Node(ctx, e.From)
	if err != nil {
		return nil, err
	}
	type unique struct {
		node  *graph.Node
		count int
	}
	add := func(u *unique, n *graph.Node) {
		u.count++
		if u.count == 1 {
			u.node = n
		} else {
			u.node = nil
		}
	}
	var all, local unique
	visitTargets := func(visit func(*graph.Node) error) error {
		return v.VisitNameCandidates(ctx, name, nil, func(n *graph.Node) error {
			if (n.Kind != graph.KindFunction && n.Kind != graph.KindMethod) ||
				graph.IsStub(n.ID) || graph.IsUnresolvedTarget(n.ID) ||
				(caller != nil && !sameDispatchBoundary(caller, n)) {
				return nil
			}
			return visit(n)
		})
	}
	if err := visitTargets(func(n *graph.Node) error {
		add(&all, n)
		if caller != nil && caller.FilePath != "" && caller.FilePath == n.FilePath {
			add(&local, n)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if all.count < 2 {
		return all.node, nil
	}
	if binding, _ := e.Meta["store_binding"].(string); binding != "" {
		file, err := rebuildFnPointerGetterFile(ctx, v, binding)
		if err != nil {
			return nil, err
		}
		if file != "" {
			var getter unique
			if err := visitTargets(func(n *graph.Node) error {
				if n.FilePath == file {
					add(&getter, n)
				}
				return nil
			}); err != nil {
				return nil, err
			}
			if getter.node != nil {
				return getter.node, nil
			}
		}
	}
	return local.node, nil
}

func rebuildFnPointerGetterFile(ctx context.Context, v store_pebble.RebuildView, binding string) (string, error) {
	var file string
	var ambiguous bool
	err := v.VisitNameCandidates(ctx, binding, nil, func(n *graph.Node) error {
		switch n.Kind {
		case graph.KindVariable, graph.KindFunction, graph.KindConstant:
		default:
			return nil
		}
		if _, action := n.Meta["store_factory"]; action || n.FilePath == "" {
			return nil
		}
		ambiguous = ambiguous || (file != "" && file != n.FilePath)
		file = n.FilePath
		return nil
	})
	if err != nil || ambiguous {
		return "", err
	}
	return file, nil
}
