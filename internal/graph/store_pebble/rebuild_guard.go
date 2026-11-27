package store_pebble

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/graph"
)

// Edge reads the complete saved identity, returning nil only for a true miss.
func (v RebuildView) Edge(ctx context.Context, id graph.EdgeIdentity) (e *graph.Edge, err error) {
	key := pendingEdgeKey(&graph.Edge{From: id.From, Kind: id.Kind, To: id.To, FilePath: id.FilePath, Line: id.Line})
	err = v.read(ctx, func(snap *pebble.Snapshot) error {
		var err error
		e, err = pendingGet(snap, key, decodeGuardEdge)
		if err == nil && e != nil && graph.EdgeIdentityFor(e) != id {
			return invalid("working guard edge identity does not match key")
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return e, nil
}

// VisitGuardOutgoing streams one source/kind prefix without collecting adjacency.
func (v RebuildView) VisitGuardOutgoing(ctx context.Context, from string, kind graph.EdgeKind, yield func(*graph.Edge) error) error {
	return v.visit(ctx, guardOutgoing(from, kind), func(_ *pebble.Snapshot, r row) error {
		e, err := guardLightEdge(r)
		if err != nil {
			return err
		}
		return yield(e)
	})
}

func guardOutgoing(from string, kind graph.EdgeKind) []byte {
	return keyStrings([]byte{familyEdge, edgeOut}, from, string(kind))
}

func decodeGuardEdge(raw []byte) (*graph.Edge, error) {
	if len(raw) > rebuildGuardSQLBytes {
		return nil, fmt.Errorf("%w: guard edge payload", ErrRebuildLinkInputSize)
	}
	return decodeEdge(raw)
}

func guardLightEdge(r row) (*graph.Edge, error) {
	e, err := decodeEdgeLight(r.value)
	if err == nil && !bytes.Equal(r.key, pendingEdgeKey(e)) {
		return nil, invalid("working guard edge identity does not match key")
	}
	return e, err
}

// AnyCaller tests the global empty-closure branch. HasCaller tests whether
// there is any reachability evidence for one caller (which need not own a file node).
func (c *RebuildImportClosure) AnyCaller(ctx context.Context) (bool, error) {
	return c.hasDirectoryPrefix(ctx, reexportKey(reexportDirectory))
}

func (c *RebuildImportClosure) HasCaller(ctx context.Context, file string) (bool, error) {
	return c.hasDirectoryPrefix(ctx, reexportKey(reexportDirectory, file))
}

func (c *RebuildImportClosure) hasDirectoryPrefix(ctx context.Context, prefix []byte) (found bool, err error) {
	if c.db == nil {
		return false, fmt.Errorf("%w: result outside its callback", ErrRebuildReexports)
	}
	found, err = rebuildGuardHasPrefix(ctx, c.db, prefix)
	if err != nil {
		return false, fmt.Errorf("%w: read caller presence: %w", ErrRebuildReexports, err)
	}
	return found, nil
}

func rebuildGuardHasPrefix(ctx context.Context, db *pebble.DB, prefix []byte) (_ bool, err error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	it, err := db.NewIterWithContext(ctx, &pebble.IterOptions{LowerBound: prefix, UpperBound: prefixEnd(prefix)})
	if err != nil {
		return false, err
	}
	defer func() { err = errors.Join(err, it.Close()) }()
	found := it.First()
	return found, errors.Join(ctx.Err(), it.Error())
}

const (
	// Preserve the native SQLite transaction cut, not the legacy Pebble
	// adapter's 256-row cut. Cutting early can change the collision winner.
	rebuildGuardSQLRows  = 5000
	rebuildGuardSQLBytes = 32 << 20
)

// ApplyRebuildGuardPage applies one guard page in its recorded verdict order.
// It mutates the borrowed revert rows' provenance for LSP retry refresh.
// The source census sees the graph after the target writes, but is fixed across
// source reindex windows. Discard the private build on every error.
func (b *BuildStore) ApplyRebuildGuardPage(ctx context.Context, reverts []graph.EdgeReindex, sources []graph.PlaceholderRepoint) (int, error) {
	if len(reverts) > rebuildLinkPendingRows || len(sources) > len(reverts) {
		return 0, invalid("guard page exceeds its row bound")
	}
	if len(reverts) == 0 {
		return 0, ctx.Err()
	}
	err := b.withWrites(func() error {
		// Native SetEdgeProvenanceBatch sees the final destination identity,
		// because the revert has already mutated To on the shared edge.
		// A stationary unresolved destination can survive with this clear.
		for _, m := range reverts {
			if err := ctx.Err(); err != nil {
				return err
			}
			key := pendingEdgeKey(m.Edge)
			dst, err := pendingGet(b.pending, key, decodeGuardEdge)
			if err != nil {
				return err
			}
			if dst == nil || dst.Origin == "" {
				continue
			}
			dst.Origin = ""
			dst.Tier = m.Edge.Tier
			if dst.Tier != "" {
				dst.Tier = graph.ResolvedBy("")
			}
			if err := b.putEdge(dst); err != nil {
				return err
			}
			// Match the native write's change to the borrowed pointer too.
			m.Edge.Origin, m.Edge.Tier = "", dst.Tier
		}
		for _, m := range reverts {
			if m.Edge.Origin != "" {
				m.Edge.Origin = ""
				if m.Edge.Tier != "" {
					m.Edge.Tier = graph.ResolvedBy("")
				}
			}
		}
		return b.rebuildGuardReindex(ctx, reverts)
	})
	if err != nil || len(sources) == 0 {
		return 0, err
	}
	return b.restoreGuardSources(ctx, sources)
}

func (b *BuildStore) restoreGuardSources(ctx context.Context, sources []graph.PlaceholderRepoint) (count int, err error) {
	// Aliased native candidate pointers give the first repoint at a site
	// the claim. This set is bounded by one guard page, not by dataflow fanout.
	type site struct {
		from, file string
		line       int
	}
	seen := make(map[site]struct{}, len(sources))
	batch := make([]graph.EdgeReindex, 0, rebuildGuardSQLRows)
	var size int
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := b.withWrites(func() error { return b.rebuildGuardReindex(ctx, batch) }); err != nil {
			return err
		}
		count += len(batch)
		clear(batch)
		batch = batch[:0]
		size = 0
		return nil
	}
	err = b.RebuildView().read(ctx, func(snap *pebble.Snapshot) error {
		for _, rp := range sources {
			if rp.OldFrom == "" || rp.NewFrom == "" || rp.OldFrom == rp.NewFrom {
				continue
			}
			key := site{rp.OldFrom, rp.FilePath, rp.Line}
			if _, claimed := seen[key]; claimed {
				continue
			}
			seen[key] = struct{}{}
			for _, kind := range [...]graph.EdgeKind{graph.EdgeArgOf, graph.EdgeValueFlow} {
				err := pendingVisit(ctx, snap, guardOutgoing(rp.OldFrom, kind), func(r row) error {
					e, err := guardLightEdge(r)
					if err != nil || e.FilePath != rp.FilePath || e.Line != rp.Line {
						return err
					}
					size += len(r.key) + len(r.value) + len(rp.NewFrom)
					if size > rebuildGuardSQLBytes {
						return fmt.Errorf("%w: guard source restoration window", ErrRebuildLinkInputSize)
					}
					e, err = decodeEdge(r.value)
					if err != nil {
						return err
					}
					e.From = rp.NewFrom
					batch = append(batch, graph.EdgeReindex{
						Edge: e, OldFrom: rp.OldFrom, OldTo: e.To, RefreshIdentity: true,
					})
					if len(batch) == rebuildGuardSQLRows {
						return flush()
					}
					return nil
				})
				if err != nil {
					return err
				}
			}
		}
		return flush()
	})
	return count, err
}

// rebuildGuardReindex mirrors the pinned SQLite bounded set replay. In
// particular, a second record for a shared target pointer can name an old
// identity that did not exist, or a no-op identity already marked moving.
// The main reindex adapter's stationary and missing-old rules differ here.
func (b *BuildStore) rebuildGuardReindex(ctx context.Context, moves []graph.EdgeReindex) (err error) {
	type moveKeys struct{ old, next string }
	keys := make([]moveKeys, 0, len(moves))
	initial := make(map[string]*graph.Edge, 2*len(moves))
	state := make(map[string]*graph.Edge, 2*len(moves))
	origins := make(map[*graph.Edge]string, len(moves))
	moving := make(map[string]bool, len(moves))
	var size int
	load := func(key string) error {
		if _, loaded := initial[key]; loaded {
			return nil
		}
		e, err := pendingGet(b.pending, []byte(key), func(raw []byte) (*graph.Edge, error) {
			size += len(raw)
			if size > rebuildGuardSQLBytes {
				return nil, fmt.Errorf("%w: guard reindex affected payloads", ErrRebuildLinkInputSize)
			}
			return decodeEdge(raw)
		})
		if err != nil {
			return err
		}
		if e != nil {
			if key != string(pendingEdgeKey(e)) {
				return invalid("guard reindex affected identity does not match key")
			}
			state[key] = e
		}
		initial[key] = e
		return nil
	}
	for _, m := range moves {
		if err := ctx.Err(); err != nil {
			return err
		}
		old := *m.Edge
		old.To = m.OldTo
		if m.RefreshIdentity {
			old.From = m.OldFrom
		}
		k := moveKeys{string(pendingEdgeKey(&old)), string(pendingEdgeKey(m.Edge))}
		keys = append(keys, k)
		// A duplicate borrows the same payload after its target changed. Its
		// later old key does not identify the row that supplied that payload.
		if _, seen := origins[m.Edge]; !seen {
			origins[m.Edge] = k.old
		}
		for _, key := range [...]string{k.old, k.next} {
			if err := load(key); err != nil {
				return err
			}
		}
		if k.old != k.next {
			moving[k.old] = true
		}
	}
	for key := range moving {
		delete(state, key)
	}
	for i, k := range keys {
		if k.old == k.next {
			if !moving[k.old] {
				state[k.next] = moves[i].Edge
			}
			continue
		}
		if initial[k.old] == nil {
			delete(state, k.old)
		}
		if state[k.next] == nil {
			state[k.next] = moves[i].Edge
		}
	}
	// All winner choices are fixed. Iterate in first-observed order; the
	// pending writer never exposes the intermediate state as a serving head.
	// Read lineage from before the writes: an earlier delete or replacement
	// can remove a later winner's source. Only one recorder is held at a time.
	recorders := b.pending.NewSnapshot()
	defer func() { err = errors.Join(err, recorders.Close()) }()
	for _, k := range keys {
		for _, key := range [...]string{k.old, k.next} {
			if err := ctx.Err(); err != nil {
				return err
			}
			before, visit := initial[key]
			if !visit {
				continue
			}
			delete(initial, key)
			after := state[key]
			if before == after {
				continue
			}
			if after == nil {
				if err := b.deleteEdge([]byte(key), before); err != nil {
					return err
				}
				continue
			}
			origin := origins[after]
			custody, err := readRebuildEdgeCustody(recorders, []byte(origin))
			if err != nil {
				return err
			}
			// An untracked revert is not a stationary receipt at its new key.
			if origin != key && custody.raw == nil && custody.proof == 0 && rebuildObserverInputKind(after.Kind) {
				custody.proof = rebuildProofUntrackedMove
			}
			if err := b.writePendingEdge(after, custody, before != nil); err != nil {
				return err
			}
		}
	}
	return nil
}
