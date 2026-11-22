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

// RebuildGenericParams binds exactly the bare Go type-parameter references
// after dataflow attribution. The owner is an ID prefix, not a node join.
func (b *BuildStore) RebuildGenericParams(ctx context.Context) (_ int, err error) {
	prefix := []byte{'g', 't'}
	occupied, err := rebuildGuardHasPrefix(ctx, b.pending, prefix)
	if err != nil {
		return 0, err
	}
	if occupied {
		return 0, invalid("generic parameter scratch range occupied")
	}
	defer func() {
		err = errors.Join(err, b.pending.DeleteRange(prefix, prefixEnd(prefix), pebble.NoSync))
	}()

	key := func(owner, name string) []byte {
		return keyStrings(bytes.Clone(prefix), owner, name)
	}
	haveParams := false
	if err := b.RebuildView().visit(ctx, []byte{familyNode}, func(_ *pebble.Snapshot, r row) error {
		if len(r.key)+len(r.value) > 16<<20 {
			return fmt.Errorf("%w: generic parameter node", ErrRebuildLinkInputSize)
		}
		n, err := decodeNodeLight(r.value)
		if err != nil {
			return err
		}
		if !bytes.Equal(r.key, pendingNodeKey(n.ID)) {
			return invalid("generic parameter node identity does not match key")
		}
		if n.Kind != graph.KindGenericParam || n.Language != "go" || n.Name == "" {
			return nil
		}
		i := strings.IndexByte(n.ID, '#')
		if i <= 0 {
			return nil
		}
		k := key(n.ID[:i], n.Name)
		seen, err := pendingGet(b.pending, k, func([]byte) (bool, error) { return true, nil })
		if err != nil {
			return err
		}
		id := n.ID
		if seen {
			id = "" // ambiguous, even if a third parameter appears
		}
		haveParams = true
		return b.pending.Set(k, []byte(id), pebble.NoSync)
	}); err != nil || !haveParams {
		return 0, err
	}

	// Dataflow did not rewrite these kinds. The literal bare old target is
	// unique for each name and a selected ID contains '#', so destinations
	// cannot be other moving old keys. Keep ordinary stationary winners.
	return b.rebuildAttributionMoves(ctx, func(e *graph.Edge) (string, error) {
		switch e.Kind {
		case graph.EdgeReferences, graph.EdgeTypedAs, graph.EdgeReturns, graph.EdgeInstantiates:
		default:
			return "", nil
		}
		name, bare := strings.CutPrefix(e.To, graph.UnresolvedMarker)
		if !bare || name == "" || strings.ContainsAny(name, ".*:#") {
			return "", nil
		}
		owner := e.From
		if i := strings.IndexByte(owner, '#'); i > 0 {
			owner = owner[:i]
		}
		return pendingGet(b.pending, key(owner, name), func(raw []byte) (string, error) { return string(raw), nil })
	})
}
