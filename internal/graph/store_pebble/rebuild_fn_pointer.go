package store_pebble

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/graph"
)

var fnPointerPrefix = []byte{'p', 'f'} // private pending rows only

const (
	fnPointerRefs byte = iota + 1
	fnPointerCalls
	fnPointerCopies
	fnPointerMembers
	fnPointerRegMoves
	fnPointerCallMoves
	fnPointerFanoutOrder
	fnPointerFanoutID
)

// RebuildFnPointerPass owns one scratch range in the build's pending DB. The
// coordinator serializes census, earlier passes, this pass, and Close. No
// graph mutations may interleave with this pass's input reads or application.
type RebuildFnPointerPass struct {
	build     *BuildStore
	threshold int
	unflushed int
	fanout    uint64
	spill     FnPointerSpill
	closed    bool
}

// FnPointerSpill counts threshold-triggered flushes and the serialized
// scratch input they cover. TableBytes counts actual L0 bytes written during
// those flush calls, not every table flushed by the build. The final partial
// interval is excluded.
type FnPointerSpill struct {
	Flushes    int
	InputBytes int64
	TableBytes uint64
}

// CaptureFnPointerCensus records identities, not payloads, before any framework
// pass applies. match is the fn-pointer census predicate. Apply the pass only
// if admitted. Close the returned handle even if the coordinator skips it.
func (b *BuildStore) CaptureFnPointerCensus(ctx context.Context, threshold int, match func(*graph.Edge) bool) (*RebuildFnPointerPass, error) {
	if threshold < 1 {
		return nil, invalid("function-pointer spill threshold must be positive")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := b.withWrites(func() error {
		active, err := pendingGet(b.pending, fnPointerPrefix, fnPointerPresent)
		if err != nil {
			return err
		}
		if active {
			return invalid("function-pointer census is already open")
		}
		return b.pending.Set(fnPointerPrefix, nil, pebble.NoSync)
	}); err != nil {
		return nil, err
	}
	p := &RebuildFnPointerPass{build: b, threshold: threshold}
	err := b.RebuildView().VisitEdges(ctx, func(e *graph.Edge) error {
		column := fnPointerInputColumn(e.Kind)
		if column == 0 || e.From == "" || !match(e) {
			return nil
		}
		return p.put(fnPointerEdgeKey(column, e), nil)
	})
	if err != nil {
		return nil, errors.Join(err, p.Close())
	}
	return p, nil
}

func (p *RebuildFnPointerPass) Close() error {
	if p.closed {
		return nil
	}
	if err := p.build.pending.DeleteRange(fnPointerPrefix, prefixEnd(fnPointerPrefix), pebble.NoSync); err != nil {
		return err
	}
	p.closed = true
	return nil
}

func (p *RebuildFnPointerPass) View() RebuildView { return p.build.RebuildView() }

// VisitInputs exact-refetches a frozen census group in complete pending-key
// order. An earlier pass may have removed the old identity or changed the
// payload at that identity. The coordinator stages decisions before applying
// any graph mutations, so both groups see the same pass-start graph state.
func (p *RebuildFnPointerPass) VisitInputs(ctx context.Context, kind graph.EdgeKind, yield func(*graph.Edge) error) error {
	column := fnPointerInputColumn(kind)
	if column == 0 {
		return invalid("unknown function-pointer input kind")
	}
	prefix := fnPointerColumn(column)
	return p.View().visit(ctx, prefix, func(snap *pebble.Snapshot, r row) error {
		key := r.key[len(prefix):]
		e, err := pendingGet(snap, key, decodeEdge)
		if err != nil || e == nil {
			return err
		}
		if !bytes.Equal(key, pendingEdgeKey(e)) {
			return invalid("function-pointer edge identity does not match key")
		}
		return yield(e)
	})
}

// Seed adds a target ID to one slot. The set retains no node payloads.
func (p *RebuildFnPointerPass) Seed(slot, id string) error {
	_, err := p.addMember(slot, id)
	return err
}

// SaveCopy records a transfer under its registration identity. Copies are
// revisited in the same complete-key order on each sweep.
func (p *RebuildFnPointerPass) SaveCopy(e *graph.Edge, to, from string) error {
	return p.put(fnPointerEdgeKey(fnPointerCopies, e), keyStrings(nil, to, from))
}

// Sweep reopens the member iterator for each source slot, so a later copy
// sees any memberships added earlier in the same sweep.
func (p *RebuildFnPointerPass) Sweep(ctx context.Context) (changed bool, err error) {
	err = pendingVisit(ctx, p.build.pending, fnPointerColumn(fnPointerCopies), func(r row) error {
		to, rest, err := readKeyString(r.value)
		if err != nil {
			return err
		}
		from, rest, err := readKeyString(rest)
		if err != nil {
			return err
		}
		if len(rest) != 0 {
			return invalid("trailing function-pointer copy data")
		}
		if to == from {
			return nil
		}
		return p.visitMembers(ctx, from, func(id string) error {
			added, err := p.addMember(to, id)
			changed = changed || added
			return err
		})
	})
	return changed, err
}

func (p *RebuildFnPointerPass) Targets(ctx context.Context, slot string, limit int) ([]string, error) {
	var ids []string
	err := p.visitMembers(ctx, slot, func(id string) error {
		if len(ids) < limit {
			ids = append(ids, id)
		}
		return nil
	})
	return ids, err
}

// StageMove uses the old complete key, so registration and dispatch moves
// keep their independent stream orders across staging and application.
func (p *RebuildFnPointerPass) StageMove(move graph.EdgeReindex) error {
	e := move.Edge
	column := fnPointerRegMoves
	if e.Kind == graph.EdgeCalls {
		column = fnPointerCallMoves
	}
	old := *e
	old.To = move.OldTo
	value, err := encodeEdge(e)
	if err != nil {
		return err
	}
	return p.put(fnPointerEdgeKey(column, &old), append(appendKeyString(nil, move.OldTo), value...))
}

// StageFanout retains the first position but the last payload for duplicates,
// matching the native framework batch store before its AddBatch flush.
func (p *RebuildFnPointerPass) StageFanout(e *graph.Edge) error {
	key := fnPointerEdgeKey(fnPointerFanoutID, e)
	ordinal, err := pendingGet(p.build.pending, key, copyValue)
	if err != nil {
		return err
	}
	if ordinal == nil {
		ordinal = binary.BigEndian.AppendUint64(nil, p.fanout)
		if err := p.put(key, ordinal); err != nil {
			return err
		}
		p.fanout++
	}
	value, err := encodeEdge(e)
	if err != nil {
		return err
	}
	return p.put(append(fnPointerColumn(fnPointerFanoutOrder), ordinal...), value)
}

// Apply installs the staged moves then fanout. No caller may seal relations
// after an error; mutation failures latch on the owning BuildStore.
func (p *RebuildFnPointerPass) Apply(ctx context.Context) (FnPointerSpill, error) {
	err := p.build.withWrites(func() error {
		for _, column := range [...]byte{fnPointerRegMoves, fnPointerCallMoves} {
			if err := p.applyMoves(ctx, column); err != nil {
				return err
			}
		}
		return pendingVisit(ctx, p.build.pending, fnPointerColumn(fnPointerFanoutOrder), func(r row) error {
			e, err := decodeEdge(r.value)
			if err != nil {
				return err
			}
			// SQLite AddBatch inserts or ignores an existing identity, including
			// a row installed by the dispatch reindexes above.
			existing, err := p.build.pendingEdge(pendingEdgeKey(e))
			if err != nil || existing != nil {
				return err
			}
			// Fanout is synthetic, not an exact receipt from the recorder site.
			return p.build.addEdgeWithCustody(e, rebuildEdgeCustody{proof: rebuildProofUntrackedMove})
		})
	})
	if err != nil {
		return FnPointerSpill{}, err
	}
	return p.spill, nil
}

func (p *RebuildFnPointerPass) applyMoves(ctx context.Context, column byte) error {
	// SQLite frees moving identities before choosing first winners in each
	// 5,000-move window. BuildStore's public method re-chunks at 256.
	const nativeChunk = 5000
	var batch []graph.EdgeReindex
	if err := pendingVisit(ctx, p.build.pending, fnPointerColumn(column), func(r row) error {
		oldTo, rest, err := readKeyString(r.value)
		if err != nil {
			return err
		}
		e, err := decodeEdge(rest)
		if err != nil {
			return err
		}
		batch = append(batch, graph.EdgeReindex{Edge: e, OldTo: oldTo})
		if len(batch) < nativeChunk {
			return nil
		}
		if err := p.build.reindexChunk(batch); err != nil {
			return err
		}
		clear(batch)
		batch = batch[:0]
		return nil
	}); err != nil {
		return err
	}
	return p.build.reindexChunk(batch)
}

func (p *RebuildFnPointerPass) visitMembers(ctx context.Context, slot string, yield func(string) error) error {
	return pendingVisit(ctx, p.build.pending, keyStrings(fnPointerColumn(fnPointerMembers), slot), func(r row) error {
		return yield(string(r.value))
	})
}

func (p *RebuildFnPointerPass) addMember(slot, id string) (bool, error) {
	key := keyStrings(fnPointerColumn(fnPointerMembers), slot, id)
	present, err := pendingGet(p.build.pending, key, fnPointerPresent)
	if err != nil || present {
		return false, err
	}
	return true, p.put(key, []byte(id))
}

func (p *RebuildFnPointerPass) put(key, value []byte) error {
	if err := p.build.pending.Set(key, value, pebble.NoSync); err != nil {
		return err
	}
	p.unflushed += len(key) + len(value)
	if p.unflushed < p.threshold {
		return nil
	}
	before := p.build.pending.Metrics().Levels[0].TableBytesFlushed
	if err := p.build.pending.Flush(); err != nil {
		return err
	}
	p.spill.TableBytes += p.build.pending.Metrics().Levels[0].TableBytesFlushed - before
	p.spill.Flushes++
	p.spill.InputBytes += int64(p.unflushed)
	p.unflushed = 0
	return nil
}

func fnPointerPresent([]byte) (bool, error) { return true, nil }

func fnPointerColumn(column byte) []byte {
	return []byte{fnPointerPrefix[0], fnPointerPrefix[1], column}
}

func fnPointerEdgeKey(column byte, e *graph.Edge) []byte {
	return append(fnPointerColumn(column), pendingEdgeKey(e)...)
}

func fnPointerInputColumn(kind graph.EdgeKind) byte {
	switch kind {
	case graph.EdgeReferences:
		return fnPointerRefs
	case graph.EdgeCalls:
		return fnPointerCalls
	default:
		return 0
	}
}
