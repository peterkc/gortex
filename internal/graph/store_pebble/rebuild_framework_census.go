package store_pebble

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/graph"
)

const rebuildFrameworkGateRowBytes = 16 << 20

// VisitFrameworkGateNodes streams the pre-framework light node projection.
func (v RebuildView) VisitFrameworkGateNodes(ctx context.Context, yield func(*graph.Node) error) error {
	return v.visit(ctx, []byte{familyNode}, func(_ *pebble.Snapshot, r row) error {
		n, err := rebuildFrameworkGateNode(r.value)
		if err == nil && !bytes.Equal(r.key, pendingNodeKey(n.ID)) {
			return invalid("framework census node identity")
		}
		if err != nil {
			return err
		}
		return yield(n)
	})
}

func rebuildFrameworkGateNode(raw []byte) (*graph.Node, error) {
	if len(raw) > rebuildFrameworkGateRowBytes {
		return nil, fmt.Errorf("%w: framework census node", ErrRebuildLinkInputSize)
	}
	return decodeNodeLight(raw)
}

// VisitFrameworkGateEdges streams only the kinds used by the wired call and
// annotation preflights. It never accumulates a kind or a metadata vocabulary.
func (v RebuildView) VisitFrameworkGateEdges(ctx context.Context, annotations bool,
	yield func(graph.FrameworkCensusEdge) error,
) error {
	return v.visit(ctx, []byte{familyEdge, edgeOut}, func(_ *pebble.Snapshot, r row) error {
		kind, err := rebuildAttributionEdgeKind(r.key)
		if err != nil {
			return err
		}
		if kind != graph.EdgeCalls && (!annotations || kind != graph.EdgeAnnotated) {
			return nil
		}
		decode := decodeEdgeLight
		if kind == graph.EdgeCalls {
			decode = decodeEdge
		}
		e, err := decode(r.value)
		if err != nil {
			return err
		}
		if !bytes.Equal(r.key, pendingEdgeKey(e)) {
			return invalid("framework census edge identity")
		}
		row := graph.FrameworkCensusEdge{EdgeIdentity: graph.EdgeIdentityFor(e)}
		row.Via, _ = e.Meta["via"].(string)
		row.RecvConst, _ = e.Meta["recv_const"].(string)
		_, row.HasExpressHandlerRef = e.Meta["express_handler_ref"]
		row.GRPCService, _ = e.Meta["grpc_service"].(string)
		row.GRPCMethod, _ = e.Meta["grpc_method"].(string)
		row.RegistryValue, _ = e.Meta["registry_value"].(string)
		return yield(row)
	})
}

// HasFrameworkAccessRoles checks if one field has every requested role. The
// reverse index groups accesses by field so only one field's mask stays live.
func (v RebuildView) HasFrameworkAccessRoles(ctx context.Context, want uint8, role func(*graph.Node) uint8) (bool, error) {
	var field string
	var seen uint8
	err := v.read(ctx, func(snap *pebble.Snapshot) (err error) {
		if want == 0 {
			return nil
		}
		prefix := []byte{familyEdge, edgeIn}
		it, err := snap.NewIterWithContext(ctx, &pebble.IterOptions{LowerBound: prefix, UpperBound: prefixEnd(prefix)})
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, it.Close()) }()
		for ok := it.First(); ok; ok = it.Next() {
			if err := ctx.Err(); err != nil {
				return err
			}
			value, err := it.ValueAndErr()
			if err != nil {
				return err
			}
			if err := v.mutationError(); err != nil {
				return err
			}
			kind, err := rebuildAttributionEdgeKind(value)
			if err != nil {
				return err
			}
			if kind != graph.EdgeAccessesField {
				continue
			}
			if len(it.Key())+len(value) > rebuildFrameworkGateRowBytes {
				return fmt.Errorf("%w: framework census access", ErrRebuildLinkInputSize)
			}
			id, err := decodeLinkRecorder(value)
			if err != nil {
				return err
			}
			if id.From == "" || id.To == "" {
				continue
			}
			to, primary, err := readKeyString(it.Key()[2:])
			if err != nil {
				return err
			}
			if to != id.To || !bytes.Equal(primary, value) {
				return invalid("framework census access identity")
			}
			if to != field {
				field, seen = to, 0
			}
			n, err := pendingGet(snap, pendingNodeKey(id.From), rebuildFrameworkGateNode)
			if err != nil {
				return err
			}
			if n == nil {
				continue
			}
			if n.ID != id.From {
				return invalid("framework census access source identity")
			}
			seen |= role(n)
			if seen == want {
				break
			}
		}
		return it.Error()
	})
	return seen == want, err
}
