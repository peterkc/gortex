package store_pebble

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/graph"
)

// RebuildGoMethodReceivers repairs cross-file Go member_of targets in the
// private graph. Run after main and its guard, before attribution or derived
// reads. The coordinator owns the store serially and discards it on error.
func (b *BuildStore) RebuildGoMethodReceivers(ctx context.Context) (moved int, err error) {
	const maxPageBytes = 16 << 20
	view := b.RebuildView()
	if err := view.mutationError(); err != nil {
		return 0, err
	}
	prefix := []byte{'g', 'r'}
	occupied, err := rebuildGuardHasPrefix(ctx, b.pending, prefix)
	if err != nil {
		return 0, err
	}
	if occupied {
		return 0, invalid("Go receiver scratch range occupied")
	}
	defer func() {
		err = errors.Join(err, b.pending.DeleteRange(prefix, prefixEnd(prefix), pebble.NoSync))
		if err != nil {
			moved = 0
			err = fmt.Errorf("%w: Go method receivers: %w", ErrRebuildView, err)
		}
	}()
	key := func(repo, dir, name string) []byte {
		return keyStrings(bytes.Clone(prefix), repo, dir, name)
	}
	haveTypes := false
	if err := view.visit(ctx, []byte{familyNode}, func(_ *pebble.Snapshot, r row) error {
		if len(r.key)+len(r.value) > maxPageBytes {
			return fmt.Errorf("%w: Go receiver node", ErrRebuildLinkInputSize)
		}
		n, err := decodeNodeLight(r.value)
		if err != nil {
			return err
		}
		if !bytes.Equal(r.key, pendingNodeKey(n.ID)) {
			return invalid("Go receiver node identity does not match key")
		}
		if n.Language != "go" || !goReceiverType(n) || n.FilePath == "" || n.Name == "" {
			return nil
		}
		k := key(n.RepoPrefix, rebuildReceiverDir(n.FilePath), n.Name)
		// Read the live scratch key, not the primary scan's snapshot. Once
		// ambiguous, this tuple stays ambiguous regardless of input order.
		seen, err := pendingGet(b.pending, k, func([]byte) (bool, error) { return true, nil })
		if err != nil {
			return err
		}
		id := n.ID
		if seen {
			id = ""
		}
		haveTypes = true
		return b.pending.Set(k, []byte(id), pebble.NoSync)
	}); err != nil || !haveTypes {
		return 0, err
	}

	var page []graph.EdgeReindex
	pageBytes := 0
	apply := func() error {
		if len(page) == 0 {
			return nil
		}
		if err := b.ApplyLinkReindexes(ctx, page); err != nil {
			return err
		}
		moved += len(page)
		clear(page)
		page = page[:0]
		pageBytes = 0
		return nil
	}
	edges := []byte{familyEdge, edgeOut}
	if err := view.visit(ctx, edges, func(snap *pebble.Snapshot, r row) error {
		_, rest, err := readKeyString(r.key[len(edges):]) // from
		if err != nil {
			return err
		}
		kind, rest, err := readKeyString(rest)
		if err != nil || kind != string(graph.EdgeMemberOf) {
			return err
		}
		if len(r.key)+len(r.value) > maxPageBytes {
			return fmt.Errorf("%w: Go receiver edge", ErrRebuildLinkInputSize)
		}
		oldTo, _, err := readKeyString(rest)
		if err != nil || strings.Index(oldTo, "::") <= 0 {
			return err
		}
		last := strings.LastIndexByte(oldTo, ':')
		if last+1 == len(oldTo) {
			return nil
		}
		e, err := guardLightEdge(r)
		if err != nil {
			return err
		}
		method, err := pendingGet(snap, pendingNodeKey(e.From), decodeNodeLight)
		if err != nil || method == nil || method.Language != "go" || method.Kind != graph.KindMethod {
			return err
		}
		target, err := pendingGet(snap, pendingNodeKey(oldTo), decodeNodeLight)
		if err != nil || goReceiverType(target) {
			return err
		}
		// Match SQLite's virtual columns: they skip the last colon and
		// its preceding rune, even for a noncanonical plugin target.
		_, width := utf8.DecodeLastRuneInString(oldTo[:last])
		dir := rebuildReceiverDir(oldTo[:last-width])
		to, err := pendingGet(snap, key(method.RepoPrefix, dir, oldTo[last+1:]), func(raw []byte) (string, error) {
			return string(raw), nil
		})
		if err != nil || to == "" || to == oldTo {
			return err
		}
		if graph.StructuralEdgeTargetInvalid(graph.EdgeMemberOf, to) {
			return invalid("Go receiver destination is not a structural endpoint")
		}
		cost := len(r.key) + len(r.value) + len(to)
		if cost > maxPageBytes {
			return fmt.Errorf("%w: Go receiver move", ErrRebuildLinkInputSize)
		}
		if len(page) == rebuildLinkPendingRows || cost > maxPageBytes-pageBytes {
			if err := apply(); err != nil {
				return err
			}
		}
		e, err = decodeEdge(r.value)
		if err != nil {
			return err
		}
		e.To = to
		page = append(page, graph.EdgeReindex{Edge: e, OldTo: oldTo})
		pageBytes += cost
		return nil
	}); err != nil {
		return 0, err
	}
	// Primary keys already impose the complete old-key order. Identical
	// old keys have just one saved payload. Real type targets never move,
	// so an earlier claimant is stationary for each subsequent page.
	err = apply()
	return moved, err
}

func goReceiverType(n *graph.Node) bool {
	return n != nil && (n.Kind == graph.KindType || n.Kind == graph.KindInterface)
}

// Match SQLite file_dir and member_receiver_dir: fold separators and trim
// the suffix without cleaning dot components of the recorded graph path.
func rebuildReceiverDir(file string) string {
	file = strings.ReplaceAll(file, `\`, "/")
	i := strings.LastIndexByte(file, '/')
	if i < 0 {
		return "."
	}
	dir := strings.TrimRight(file[:i], "/")
	if dir == "" {
		return "/"
	}
	return dir
}
