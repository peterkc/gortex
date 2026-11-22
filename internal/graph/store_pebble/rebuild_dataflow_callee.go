package store_pebble

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"strings"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/graph"
)

const (
	dataflowCalleeFile byte = 'f'
	dataflowCalleeDir  byte = 'd'
	dataflowCalleeSite byte = 's'
)

// RebuildDataflowCallees attributes arg_of and value_flow after bare-name
// scope binding. It reduces the post-bare evidence to exact point lookups and
// plans both target shapes together so they share the same collision order.
func (b *BuildStore) RebuildDataflowCallees(ctx context.Context) (_ int, err error) {
	const maxRowBytes = 16 << 20
	prefix := []byte{'g', 'd'}
	occupied, err := rebuildGuardHasPrefix(ctx, b.pending, prefix)
	if err != nil {
		return 0, err
	}
	if occupied {
		return 0, invalid("dataflow callee scratch range occupied")
	}
	defer func() {
		err = errors.Join(err, b.pending.DeleteRange(prefix, prefixEnd(prefix), pebble.NoSync))
	}()

	key := func(col byte, scope, name string) []byte {
		return keyStrings([]byte{'g', 'd', col}, scope, name)
	}
	type match struct {
		id    string
		found bool
	}
	lookup := func(k []byte) (match, error) {
		return pendingGet(b.pending, k, func(raw []byte) (match, error) {
			return match{id: string(raw), found: true}, nil
		})
	}
	haveEvidence := false
	record := func(k []byte, id string) error {
		haveEvidence = true
		prior, err := lookup(k)
		if err != nil {
			return err
		}
		if prior.found {
			if prior.id == "" || prior.id == id {
				return nil // already ambiguous or the same site witness
			}
			id = "" // a present empty value blocks fallback
		}
		return b.pending.Set(k, []byte(id), pebble.NoSync)
	}
	dir := func(file string) string { return path.Dir(filepath.ToSlash(file)) }
	view := b.RebuildView()
	if err := view.visit(ctx, []byte{familyNode}, func(_ *pebble.Snapshot, r row) error {
		if len(r.key)+len(r.value) > maxRowBytes {
			return fmt.Errorf("%w: dataflow callee node", ErrRebuildLinkInputSize)
		}
		n, err := decodeNodeLight(r.value)
		if err != nil {
			return err
		}
		if !bytes.Equal(r.key, pendingNodeKey(n.ID)) {
			return invalid("dataflow callee node identity does not match key")
		}
		if n.Name == "" || n.FilePath == "" || (n.Kind != graph.KindFunction && n.Kind != graph.KindMethod) {
			return nil
		}
		if err := record(key(dataflowCalleeFile, n.FilePath, n.Name), n.ID); err != nil {
			return err
		}
		if n.Kind == graph.KindFunction {
			return record(key(dataflowCalleeDir, dir(n.FilePath), n.Name), n.ID)
		}
		return nil
	}); err != nil {
		return 0, err
	}

	// Include references just resolved by lexical binding, and all sources
	// at a recorded site. Match IDs, not target nodes or source owners.
	if err := view.visit(ctx, []byte{familyEdge, edgeOut}, func(_ *pebble.Snapshot, r row) error {
		_, rest, err := readKeyString(r.key[2:]) // from
		if err != nil {
			return err
		}
		kind, _, err := readKeyString(rest)
		if err != nil || (kind != string(graph.EdgeCalls) && kind != string(graph.EdgeReferences)) {
			return err
		}
		if len(r.key)+len(r.value) > maxRowBytes {
			return fmt.Errorf("%w: dataflow callee site edge", ErrRebuildLinkInputSize)
		}
		e, err := guardLightEdge(r)
		if err != nil || e.FilePath == "" || e.Line <= 0 || graph.IsUnresolvedTarget(e.To) {
			return err
		}
		if method := rebuildDataflowMethodSuffix(e.To); method != "" {
			return record(appendKeyLine(key(dataflowCalleeSite, e.FilePath, method), e.Line), e.To)
		}
		return nil
	}); err != nil || !haveEvidence {
		return 0, err
	}

	return b.rebuildAttributionMoves(ctx, func(e *graph.Edge) (string, error) {
		if e.Kind != graph.EdgeArgOf && e.Kind != graph.EdgeValueFlow {
			return "", nil
		}
		name := graph.UnresolvedName(e.To)
		method, wildcard := strings.CutPrefix(name, "*.")
		if method == "" || strings.ContainsAny(method, ".*:#") {
			return "", nil
		}
		if wildcard {
			callee, err := lookup(appendKeyLine(key(dataflowCalleeSite, e.FilePath, method), e.Line))
			return callee.id, err
		}
		callee, err := lookup(key(dataflowCalleeFile, e.FilePath, name))
		if err == nil && !callee.found {
			callee, err = lookup(key(dataflowCalleeDir, dir(e.FilePath), name))
		}
		return callee.id, err
	})
}

// A native method query cannot contain any of these delimiters. Thus only the
// suffix after the final delimiter can match, and it must follow . or ::.
func rebuildDataflowMethodSuffix(id string) string {
	i := strings.LastIndexAny(id, ".*:#")
	if i < 0 || (id[i] != '.' && !(id[i] == ':' && i > 0 && id[i-1] == ':')) {
		return ""
	}
	return id[i+1:]
}
