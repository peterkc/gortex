package resolver

import (
	"context"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// withRebuildTemporalConstants replaces only buildConstDerefMap for a private
// worker. The caller streams requested names and reads exact values in consume.
func withRebuildTemporalConstants(
	ctx context.Context, view store_pebble.RebuildView,
	seed func(yield func(string) error) error,
	consume func(*store_pebble.TemporalConstDeref) error,
) (store_pebble.TemporalConstStats, error) {
	return view.WithTemporalConstDeref(ctx, seed, projectTemporalConstCandidate, consume)
}

func projectTemporalConstCandidate(n *graph.Node) store_pebble.TemporalConstCandidate {
	var c store_pebble.TemporalConstCandidate
	switch n.Kind {
	case graph.KindConstant, graph.KindFunction, graph.KindMethod:
		c.ReadSidecar = true
		c.Alias, _ = n.Meta["const_ref"].(string)
	case graph.KindField:
		// Java fields contribute metadata but not aliases or sidecars.
	default:
		return c
	}
	if n.Language == "java" {
		c.JavaLiteral, _ = n.Meta["value"].(string)
	}
	return c
}
