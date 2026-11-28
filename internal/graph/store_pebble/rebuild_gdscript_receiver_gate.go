package store_pebble

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"iter"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/graph"
)

var ErrRebuildGDScriptReceiverGate = errors.New("pebble rebuild GDScript receiver gate failed")

var rebuildGDScriptGatePrefix = []byte{'g', 'q', 52}

// RebuildGDScriptReceiverDemoter returns true after editing a candidate's
// payload in place. Its node lookup is scoped to this decision.
type RebuildGDScriptReceiverDemoter func(*graph.Edge, func(string) (*graph.Node, error)) (bool, error)

// RebuildGDScriptReceiverGate plans same-key refreshes from settled rows, not
// from parse receipts. prepare may return a nil demoter when no classes exist.
func (b *BuildStore) RebuildGDScriptReceiverGate(ctx context.Context,
	classify func(*graph.Node, bool) (full, candidate bool),
	prepare func(classes, scripts iter.Seq2[*graph.Node, error]) (RebuildGDScriptReceiverDemoter, error),
) (count uint64, err error) {
	defer func() {
		if err != nil {
			count = 0
			err = fmt.Errorf("%w: %w", ErrRebuildGDScriptReceiverGate, err)
		}
	}()
	err = b.withWrites(func() (err error) {
		prefix := rebuildGDScriptGatePrefix
		busy, err := rebuildGuardHasPrefix(ctx, b.pending, prefix)
		if err != nil {
			return err
		}
		if busy {
			return invalid("GDScript receiver scratch occupied")
		}
		defer func() { err = errors.Join(err, b.pending.DeleteRange(prefix, prefixEnd(prefix), pebble.NoSync)) }()
		demote, err := prepare(b.rebuildGDScriptGateNodes(ctx, classify, true), b.rebuildGDScriptGateNodes(ctx, classify, false))
		if err != nil || demote == nil {
			return err
		}
		if err := b.planRebuildGDScriptGate(ctx, demote); err != nil {
			return err
		}
		count, err = b.applyRebuildGDScriptGate(ctx)
		return err
	})
	return count, err
}

func checkRebuildGDScriptGateRow(key, value []byte) error {
	if len(key)+len(value) > rebuildFrameworkGateRowBytes {
		return fmt.Errorf("%w: GDScript receiver row", ErrRebuildLinkInputSize)
	}
	return nil
}

func (b *BuildStore) rebuildGDScriptGateNodes(ctx context.Context, classify func(*graph.Node, bool) (full, candidate bool), classes bool) iter.Seq2[*graph.Node, error] {
	return func(yield func(*graph.Node, error) bool) {
		stop := errors.New("stop GDScript receiver node scan")
		err := pendingVisit(ctx, b.pending, []byte{familyNode}, func(r row) error {
			n, err := checkedRebuildLightNode(r, "GDScript receiver")
			if err != nil {
				return err
			}
			full, candidate := classify(n, classes)
			if !candidate {
				return nil
			}
			if full {
				if err := checkRebuildGDScriptGateRow(r.key, r.value); err != nil {
					return err
				}
				n, err = decodeNode(r.value)
				if err != nil {
					return err
				}
			}
			if !yield(n, nil) {
				return stop
			}
			return nil
		})
		if err != nil && !errors.Is(err, stop) {
			yield(nil, err)
		}
	}
}

func (b *BuildStore) rebuildGDScriptGateNode(id string, full bool) (*graph.Node, error) {
	key := pendingNodeKey(id)
	return pendingGet(b.pending, key, func(raw []byte) (*graph.Node, error) {
		n, err := decodeNodeLight(raw)
		if err != nil {
			return nil, err
		}
		if n.ID != id {
			return nil, invalid("GDScript receiver node identity")
		}
		if n.Language != "gdscript" {
			return n, nil
		}
		if err := checkRebuildGDScriptGateRow(key, raw); err != nil {
			return nil, err
		}
		if !full {
			return n, nil
		}
		return decodeNode(raw)
	})
}

func (b *BuildStore) planRebuildGDScriptGate(ctx context.Context, demote RebuildGDScriptReceiverDemoter) error {
	return pendingVisit(ctx, b.pending, []byte{familyEdge, edgeOut}, func(r row) error {
		// Filter identity and target language before loading the full edge.
		if len(r.key) > rebuildFrameworkGateRowBytes {
			return checkRebuildGDScriptGateRow(r.key, nil)
		}
		id, err := decodeLinkRecorder(r.key)
		if err != nil || (id.Kind != graph.EdgeCalls && id.Kind != graph.EdgeReferences) || id.To == "" || graph.IsUnresolvedTarget(id.To) {
			return err
		}
		target, err := b.rebuildGDScriptGateNode(id.To, false)
		if err != nil || target == nil || target.Language != "gdscript" {
			return err
		}
		if err := checkRebuildGDScriptGateRow(r.key, r.value); err != nil {
			return err
		}
		e, err := decodeEdge(r.value)
		if err != nil {
			return err
		}
		if !bytes.Equal(r.key, pendingEdgeKey(e)) {
			return invalid("GDScript receiver edge identity")
		}
		changed, err := demote(e, func(id string) (*graph.Node, error) {
			return b.rebuildGDScriptGateNode(id, true)
		})
		if err != nil || !changed {
			return err
		}
		if !bytes.Equal(r.key, pendingEdgeKey(e)) {
			return invalid("GDScript receiver demotion changed identity")
		}
		after, err := encodeEdge(e)
		if err != nil {
			return err
		}
		if err := checkRebuildGDScriptGateRow(r.key, after); err != nil {
			return err
		}
		key := append(bytes.Clone(rebuildGDScriptGatePrefix), r.key...)
		return b.putRebuildScratch(key, append(appendBytes(nil, r.value), after...), 2*rebuildFrameworkGateRowBytes, "GDScript receiver proposal")
	})
}

// Verify every saved before payload and its custody before the first refresh.
// The central refresh rebinds custody. Check the complete saved after payload.
func (b *BuildStore) applyRebuildGDScriptGate(ctx context.Context) (count uint64, err error) {
	prefix := rebuildGDScriptGatePrefix
	visit := func(apply bool) error {
		return pendingVisit(ctx, b.pending, prefix, func(r row) error {
			key := r.key[len(prefix):]
			payload := recordCursor{buf: r.value}
			before := payload.bytes()
			if payload.err != nil {
				return payload.err
			}
			after, err := decodeEdge(payload.buf)
			if err != nil {
				return err
			}
			if !bytes.Equal(key, pendingEdgeKey(after)) {
				return invalid("GDScript receiver refresh identity")
			}
			if !apply {
				if err := b.proveRebuildEdgePayload(key, before); err != nil {
					return err
				}
				_, err := readRebuildEdgeCustody(b.pending, key)
				return err
			}
			if err := b.refreshPendingEdge(after); err != nil {
				return err
			}
			if err := b.proveRebuildEdgePayload(key, payload.buf); err != nil {
				return err
			}
			count++
			return nil
		})
	}
	if err := visit(false); err != nil {
		return 0, err
	}
	err = visit(true)
	return count, err
}
