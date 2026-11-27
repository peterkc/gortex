package store_pebble

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"

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

// RebuildFnPointerPass owns a scratch range and deferred move runs. The
// coordinator serializes census, earlier passes, this pass, and Close. No
// graph mutations may interleave with this pass's input reads or application.
type RebuildFnPointerPass struct {
	build     *BuildStore
	threshold int
	runDir    string
	moves     [2]fnPointerMoveStream
	fanout    uint64
	spill     FnPointerSpill
	closed    bool
}

// FnPointerSpill counts closed threshold SST runs consumed by Apply, their
// logical move input bytes, and their physical file bytes. It excludes the
// pending overlay and final in-memory tails.
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
	err := p.build.pending.DeleteRange(fnPointerPrefix, prefixEnd(fnPointerPrefix), pebble.NoSync)
	if p.runDir != "" {
		err = errors.Join(err, os.RemoveAll(p.runDir))
	}
	if err == nil {
		p.closed = true
		p.moves = [2]fnPointerMoveStream{}
	}
	return err
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

// StageMove accepts strictly ascending, unique old complete keys in each
// stream. VisitInputs provides this order. Registration and dispatch use
// separate streams. A duplicate or descending key is refused before staging.
func (p *RebuildFnPointerPass) StageMove(ctx context.Context, move graph.EdgeReindex) error {
	e := move.Edge
	column := fnPointerRegMoves
	if e.Kind == graph.EdgeCalls {
		column = fnPointerCallMoves
	}
	old := *e
	old.To = move.OldTo
	key := fnPointerEdgeKey(column, &old)
	stream := &p.moves[column-fnPointerRegMoves]
	if stream.lastKey != nil && bytes.Compare(key, stream.lastKey) <= 0 {
		return invalid("function-pointer move keys must be strictly ascending")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	value, err := encodeEdge(e)
	if err != nil {
		return err
	}
	value = append(appendKeyString(nil, move.OldTo), value...)
	size := len(key) + len(value)
	if len(stream.rows) != 0 && size > p.threshold-stream.bytes {
		if err := p.spillMoves(ctx, column, stream); err != nil {
			return err
		}
	}
	stream.rows = append(stream.rows, row{key, value})
	stream.lastKey = key
	stream.bytes += size
	if stream.bytes >= p.threshold {
		return p.spillMoves(ctx, column, stream)
	}
	return nil
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
	if err := p.visitMoves(ctx, column, func(r row) error {
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

type fnPointerMoveStream struct {
	rows    []row
	lastKey []byte
	bytes   int
	runs    int
}

func (p *RebuildFnPointerPass) spillMoves(ctx context.Context, column byte, stream *fnPointerMoveStream) error {
	if p.runDir == "" {
		dir, err := os.MkdirTemp(filepath.Join(p.build.scratchParent, "pending"), "fn-pointer-moves-")
		if err != nil {
			return err
		}
		p.runDir = dir
	}
	path := p.runPath(column, stream.runs)
	if err := writeLinkRun(ctx, path, stream.rows); err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	stream.runs++
	p.spill.Flushes++
	p.spill.InputBytes += int64(stream.bytes)
	p.spill.TableBytes += uint64(info.Size())
	clear(stream.rows)
	stream.rows = stream.rows[:0]
	stream.bytes = 0
	return nil
}

func (p *RebuildFnPointerPass) runPath(column byte, run int) string {
	return filepath.Join(p.runDir, fmt.Sprintf("%d-%d.sst", column, run))
}

func (p *RebuildFnPointerPass) visitMoves(ctx context.Context, column byte, yield func(row) error) error {
	stream := &p.moves[column-fnPointerRegMoves]
	// Each run and the tail are consecutive ranges of one ordered input scan.
	for i := range stream.runs {
		if err := visitLinkRun(ctx, p.runPath(column, i), nil, yield); err != nil {
			return err
		}
	}
	for _, r := range stream.rows {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := yield(r); err != nil {
			return err
		}
	}
	return ctx.Err()
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
	return p.build.pending.Set(key, value, pebble.NoSync)
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
