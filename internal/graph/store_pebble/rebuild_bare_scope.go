package store_pebble

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/graph"
)

var rebuildBareScopePrefix = []byte{'g', 'b'}

// RebuildBareNameScope binds unresolved bare names after receiver rebind.
// The coordinator owns the private build serially and discards it on error.
func (b *BuildStore) RebuildBareNameScope(ctx context.Context) (_ int, err error) {
	prefix := rebuildBareScopePrefix
	occupied, err := rebuildGuardHasPrefix(ctx, b.pending, prefix)
	if err != nil {
		return 0, err
	}
	if occupied {
		return 0, invalid("bare-name scratch range occupied")
	}
	defer func() {
		if cleanupErr := b.pending.DeleteRange(prefix, prefixEnd(prefix), pebble.NoSync); cleanupErr != nil {
			err = errors.Join(err, cleanupErr)
		}
	}()

	// The primary sweep orders node IDs the same way as SQLite's binary ID
	// order. Local line keys keep an empty value once tied. The exact tuple
	// key toggles presence for each parameter, retaining the last odd ID.
	haveBindings := false
	if err := b.RebuildView().visit(ctx, []byte{familyNode}, func(_ *pebble.Snapshot, r row) error {
		if len(r.key)+len(r.value) > 16<<20 {
			return fmt.Errorf("%w: bare-name scope node", ErrRebuildLinkInputSize)
		}
		n, err := decodeNodeLight(r.value)
		if err != nil {
			return err
		}
		if !bytes.Equal(r.key, pendingNodeKey(n.ID)) {
			return invalid("bare-name node identity does not match key")
		}
		if n.Kind != graph.KindParam && (n.Kind != graph.KindLocal || n.StartLine <= 0) {
			return nil
		}
		key := rebuildBareScopeKey(n.ID, n.Name)
		if key == nil {
			return nil
		}
		haveBindings = true
		if n.Kind == graph.KindLocal {
			key = appendKeyLine(key, n.StartLine)
		}
		present, err := pendingGet(b.pending, key, func([]byte) (bool, error) { return true, nil })
		if err != nil {
			return err
		}
		if !present {
			return b.pending.Set(key, []byte(n.ID), pebble.NoSync)
		}
		if n.Kind == graph.KindParam {
			return b.pending.Delete(key, pebble.NoSync)
		}
		return b.pending.Set(key, nil, pebble.NoSync)
	}); err != nil || !haveBindings {
		return 0, err
	}

	it, err := b.pending.NewIterWithContext(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() {
		if closeErr := it.Close(); closeErr != nil {
			err = errors.Join(err, closeErr)
		}
	}()
	return b.rebuildAttributionMoves(ctx, func(e *graph.Edge) (string, error) {
		switch e.Kind {
		case graph.EdgeReads, graph.EdgeReferences, graph.EdgeArgOf, graph.EdgeValueFlow:
		default:
			return "", nil
		}
		name := graph.UnresolvedName(e.To)
		if name == "" || strings.ContainsAny(name, ".*:#") {
			return "", nil
		}
		key := rebuildBareScopeKey(e.From, name)
		if key == nil {
			return "", nil
		}
		last := key
		if e.Line > 0 {
			last = appendKeyLine(last, e.Line)
		}
		it.SetBounds(key, prefixEnd(last))
		if !it.Last() {
			return "", it.Error()
		}
		raw, err := it.ValueAndErr()
		if err != nil {
			return "", err
		}
		if len(raw) == 0 {
			// A tie at the latest eligible line falls back to the exact
			// parameter key, not to an earlier local.
			if !it.First() || !bytes.Equal(it.Key(), key) {
				return "", it.Error()
			}
			raw, err = it.ValueAndErr()
		}
		return string(raw), err
	})
}

func rebuildBareScopeKey(id, name string) []byte {
	if i := strings.IndexByte(id, '#'); i > 0 {
		id = id[:i]
	}
	if id == "" {
		return nil
	}
	return keyStrings(bytes.Clone(rebuildBareScopePrefix), id, name)
}
