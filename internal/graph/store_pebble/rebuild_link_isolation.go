package store_pebble

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/graph"
)

// ErrRebuildLinkIsolation marks a refused private holdout or restoration.
var ErrRebuildLinkIsolation = errors.New("pebble rebuild link isolation failed")

// This private prefix is never read by a link producer or written to a stage.
// n: elected node bytes with receipt path (empty only for a checked skip).
// e: exact parsed edge key to owning receipt path. r: saved restore edge bytes.
var rebuildHoldoutPrefix = []byte{'g', '!'}

func rebuildHoldoutKey(col byte, key []byte) []byte {
	return append([]byte{'g', '!', col}, key...)
}

func (b *BuildStore) captureRebuildHeldNode(id, receipt string) error {
	key := pendingNodeKey(id)
	raw, err := pendingGet(b.pending, key, copyValue)
	if err != nil {
		return err
	}
	if raw == nil {
		return invalid("held node missing from file graph")
	}
	return b.pending.Set(rebuildHoldoutKey('n', key), append(appendMetaString(nil, receipt), raw...), pebble.NoSync)
}

// AddRebuildHeldSkip keeps a checked deferred skip in FileGraph only. Skip
// rows have no parse receipt or declaration to restore at Relations.
func (b *BuildStore) AddRebuildHeldSkip(n *graph.Node) error {
	return b.withWrites(func() error {
		if b.rebuildLinksIsolated || n == nil || n.Kind != graph.KindFile || n.ID == "" {
			return fmt.Errorf("%w: invalid held skip", ErrRebuildLinkIsolation)
		}
		prev, err := b.pendingNode(n.ID)
		if err != nil {
			return err
		}
		if prev != nil {
			return fmt.Errorf("%w: held skip collides with node %q", ErrRebuildLinkIsolation, n.ID)
		}
		if err := b.writeNode(n); err != nil {
			return err
		}
		return b.captureRebuildHeldNode(n.ID, "")
	})
}

func (b *BuildStore) visitRebuildHeldNodes(ctx context.Context, visit func(*graph.Node, string, []byte) error) error {
	prefix := rebuildHoldoutKey('n', nil)
	return pendingVisit(ctx, b.pending, prefix, func(r row) error {
		d := recordCursor{buf: r.value}
		receipt := d.str()
		if d.err != nil {
			return d.err
		}
		n, err := decodeNode(d.buf)
		if err != nil {
			return err
		}
		if !bytes.Equal(r.key[len(prefix):], pendingNodeKey(n.ID)) {
			return invalid("held node key differs from capture")
		}
		return visit(n, receipt, d.buf)
	})
}

// IsolateRebuildLinks opens the private cut after the all-language FileGraph
// offer. Its callers must use selected receipts and skip facts as well.
func (b *BuildStore) IsolateRebuildLinks(ctx context.Context) error {
	return b.withWrites(func() error {
		if b.rebuildLinksIsolated || b.contractOverlay {
			return fmt.Errorf("%w: private graph already in use", ErrRebuildLinkIsolation)
		}
		if err := b.isolateRebuildLinks(ctx); err != nil {
			return fmt.Errorf("%w: %w", ErrRebuildLinkIsolation, err)
		}
		b.rebuildLinksIsolated = true
		return nil
	})
}

func (b *BuildStore) isolateRebuildLinks(ctx context.Context) error {
	held, err := rebuildGuardHasPrefix(ctx, b.pending, rebuildHoldoutPrefix)
	if err != nil || !held {
		return err
	}
	if err := b.visitRebuildHeldNodes(ctx, func(n *graph.Node, _ string, raw []byte) error {
		match, err := pendingGet(b.pending, pendingNodeKey(n.ID), func(value []byte) (bool, error) {
			return bytes.Equal(value, raw), nil
		})
		if err != nil {
			return err
		}
		if !match {
			return fmt.Errorf("held raw node %q differs from receipt capture", n.ID)
		}
		return nil
	}); err != nil {
		return err
	}
	if err := pendingVisit(ctx, b.pending, []byte{familyEdge, edgeOut}, b.holdRebuildEdge); err != nil {
		return err
	}
	return b.visitRebuildHeldNodes(ctx, func(n *graph.Node, _ string, _ []byte) error {
		if err := b.pending.Delete(parsedConstantKey(n.ID), pebble.NoSync); err != nil {
			return err
		}
		if err := b.updateShingles(n.ID, nil, nil); err != nil {
			return err
		}
		return b.deleteNodeRecord(n)
	})
}

func (b *BuildStore) holdRebuildEdge(r row) error {
	e, err := decodeEdge(r.value)
	if err != nil {
		return err
	}
	receipt, err := pendingGet(b.pending, rebuildHoldoutKey('e', r.key), copyValue)
	if err != nil {
		return err
	}
	owner := func(id string) (*string, error) {
		return pendingGet(b.pending, rebuildHoldoutKey('n', pendingNodeKey(id)), func(raw []byte) (*string, error) {
			d := recordCursor{buf: raw}
			path := d.str()
			return &path, d.err
		})
	}
	from, err := owner(e.From)
	if err != nil {
		return err
	}
	to, err := owner(e.To)
	if err != nil {
		return err
	}
	if receipt == nil && from == nil && to == nil {
		return nil
	}
	if len(receipt) != 0 && from != nil && to != nil && *from == string(receipt) && *to == *from &&
		(e.Kind == graph.EdgeDefines || e.Kind == graph.EdgeContains) {
		// No parse writer attaches a move recorder or Go source proof to
		// either kind. A recorder would no longer be a raw receipt row.
		custody, err := readRebuildEdgeCustody(b.pending, r.key)
		if err != nil {
			return err
		}
		if custody.raw != nil || custody.proof != 0 {
			return invalid("held containment edge has non-raw custody")
		}
		if err := b.pending.Set(rebuildHoldoutKey('r', r.key), r.value, pebble.NoSync); err != nil {
			return err
		}
	}
	if err := b.deleteEdge(r.key, e); err != nil {
		return err
	}
	// A supported receipt may point to a held-out winner. Remove its
	// parse evidence too, so later reuse of that key cannot inherit it.
	for _, key := range [][]byte{factoryChainReceiptKey(r.key), rebuildExternalEvidenceKey('p', r.key), rebuildExternalEvidenceKey('r', r.key)} {
		if err := b.pending.Delete(key, pebble.NoSync); err != nil {
			return err
		}
	}
	return nil
}

// SelectRebuildLinkFacts removes claims on held-out winners from a fresh
// supported receipt. The caller owns the decoded slices and uses this only
// while the private cut is open. Holdout keys are immutable during the cut.
func (b *BuildStore) SelectRebuildLinkFacts(ctx context.Context, f ParseFacts) (ParseFacts, error) {
	anyHeld, err := rebuildGuardHasPrefix(ctx, b.pending, rebuildHoldoutKey('n', nil))
	if err != nil || !anyHeld {
		return f, err
	}
	seen := make(map[string]bool)
	held := func(id string) bool {
		if dropped, ok := seen[id]; ok {
			return dropped
		}
		if err != nil {
			return false
		}
		if err = ctx.Err(); err != nil {
			return false
		}
		var dropped bool
		dropped, err = pendingGet(b.pending, rebuildHoldoutKey('n', pendingNodeKey(id)), fnPointerPresent)
		seen[id] = dropped
		return dropped
	}
	f.Nodes = slices.DeleteFunc(f.Nodes, func(n *graph.Node) bool { return held(n.ID) })
	f.Edges = slices.DeleteFunc(f.Edges, func(e *graph.Edge) bool { return held(e.From) || held(e.To) })
	f.Constants = slices.DeleteFunc(f.Constants, func(c graph.ConstantValueRow) bool { return held(c.NodeID) })
	f.Symbols = slices.DeleteFunc(f.Symbols, func(s graph.SymbolFTSItem) bool { return held(s.NodeID) })
	for id := range f.Shingles {
		if held(id) {
			delete(f.Shingles, id)
		}
	}
	if err != nil {
		return ParseFacts{}, fmt.Errorf("%w: select supported receipt %q: %w", ErrRebuildLinkIsolation, f.File.FilePath, err)
	}
	f.File.NodeCount = len(f.Nodes)
	return f, nil
}

// RestoreRebuildDeclarations closes the private cut after all resolver and C3
// input reads, before any Relations or later stage writer. A collision is a
// refusal, never an upsert over the result of isolated linking.
func (b *BuildStore) RestoreRebuildDeclarations(ctx context.Context) error {
	return b.withWrites(func() error {
		if !b.rebuildLinksIsolated || b.contractOverlay {
			return fmt.Errorf("%w: private cut is not ready to restore", ErrRebuildLinkIsolation)
		}
		if err := b.restoreRebuildDeclarations(ctx); err != nil {
			return fmt.Errorf("%w: %w", ErrRebuildLinkIsolation, err)
		}
		b.rebuildLinksIsolated = false
		return nil
	})
}

func (b *BuildStore) restoreRebuildDeclarations(ctx context.Context) error {
	held, err := rebuildGuardHasPrefix(ctx, b.pending, rebuildHoldoutPrefix)
	if err != nil || !held {
		return err
	}
	if err := b.visitRebuildHeldNodes(ctx, func(n *graph.Node, _ string, _ []byte) error {
		live, err := b.pendingNode(n.ID)
		if err != nil {
			return err
		}
		if live != nil {
			return fmt.Errorf("isolated link node collides with held node %q", n.ID)
		}
		for _, dir := range []byte{edgeIn, edgeOut} {
			found, err := rebuildGuardHasPrefix(ctx, b.pending, keyStrings([]byte{familyEdge, dir}, n.ID))
			if err != nil {
				return err
			}
			if found {
				return fmt.Errorf("isolated link edge uses held node %q", n.ID)
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if err := b.visitRebuildHeldNodes(ctx, func(n *graph.Node, receipt string, raw []byte) error {
		if receipt == "" {
			return nil
		}
		return b.writeNodeRecord(n, nil, raw)
	}); err != nil {
		return err
	}
	prefix := rebuildHoldoutKey('r', nil)
	if err := pendingVisit(ctx, b.pending, prefix, func(r row) error {
		e, err := decodeEdge(r.value)
		if err != nil {
			return err
		}
		return b.writeEdgeRecord(e, r.key[len(prefix):], r.value, rebuildEdgeCustody{}, false)
	}); err != nil {
		return err
	}
	return b.pending.DeleteRange(rebuildHoldoutPrefix, prefixEnd(rebuildHoldoutPrefix), pebble.NoSync)
}
