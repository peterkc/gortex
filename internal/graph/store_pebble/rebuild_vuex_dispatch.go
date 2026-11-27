package store_pebble

import (
	"context"
	"errors"
	"iter"

	"github.com/zzet/gortex/internal/graph"
)

var ErrRebuildVuexDispatch = errors.New("pebble rebuild Vuex Dispatch stage failed")

var rebuildVuexDispatchPrefix = []byte{'g', 'q', 'V'}

type RebuildVuexDispatchStats = RebuildStoreFactoryStats

// RebuildVuexDispatchRules supplies the native marker and payload decisions.
type RebuildVuexDispatchRules struct {
	Action func(*graph.Node) (string, string)
	Stub   func(*graph.Edge) (string, string, string)
	Getter func(*graph.Node, string) bool
	Rebind func(*graph.Edge, string, string, bool) bool
}

// RebuildVuexDispatch matches direct SQLite: the native sibling mutation
// targets detached edges, so only the Call reindexes change saved rows.
func (b *BuildStore) RebuildVuexDispatch(ctx context.Context, receipts iter.Seq2[ParseFacts, error],
	rules RebuildVuexDispatchRules,
) (RebuildVuexDispatchStats, error) {
	var read func(*graph.Edge) rebuildStoreDispatchCall
	if rules.Stub != nil {
		read = func(e *graph.Edge) rebuildStoreDispatchCall {
			kind, key, name := rules.Stub(e)
			binding, _ := e.Meta["store_binding"].(string)
			return rebuildStoreDispatchCall{group: kind, member: key, getter: binding, reset: name}
		}
	}
	p := rebuildStoreFactoryPass{
		rebuildCallMoves: rebuildCallMoves{
			b: b, prefix: rebuildVuexDispatchPrefix, rowBytes: 16 << 20, label: "Vuex Dispatch", inputRow: "edge row",
		},
		rules: rebuildStoreDispatchRules{
			kind: func(kind graph.NodeKind) bool { return kind == graph.KindFunction || kind == graph.KindMethod },
			action: rules.Action, call: read, getter: rules.Getter, rebind: rules.Rebind,
		},
	}
	return p.run(ctx, receipts, ErrRebuildVuexDispatch)
}
