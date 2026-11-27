package store_pebble

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"iter"
	"slices"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/pathkey"
)

// ErrRebuildTemporal marks a failure before the private framework turn can be offered.
var ErrRebuildTemporal = errors.New("pebble rebuild Temporal stage failed")

var rebuildTemporalPrefix = []byte{'g', 'Y'}

const rebuildTemporalRowLimit = 16 << 20

// RebuildTemporalStats describes the complete private turn, including spill.
type RebuildTemporalStats struct {
	WrapperRounds                         int
	WrapperCapped                         bool
	Wrappers, Resolved, Moves, SpillBytes uint64
	ConstSpillBytes                       uint64
}

// RebuildTemporalPass owns one scratch range for the second framework turn.
// The worker must serialize this callback with all other private graph writes.
// Scratch, including the node overlay, is not a serving family.
type RebuildTemporalPass struct {
	build     *BuildStore
	threshold int
	buffered  int
	spill     uint64
	moves     uint64
	adds      uint64
	wants     bool
}

// RebuildTemporal runs a reducer over a quiet private graph, proves the
// requested receipt ownership, and applies the result. On error the owner
// must abandon the build. It must not offer any relations from this turn.
func (b *BuildStore) RebuildTemporal(ctx context.Context, receipts iter.Seq2[ParseFacts, error], spillAt int,
	reduce func(*RebuildTemporalPass) (RebuildTemporalStats, error),
) (stats RebuildTemporalStats, err error) {
	defer func() {
		if err != nil {
			stats = RebuildTemporalStats{}
			err = fmt.Errorf("%w: %w", ErrRebuildTemporal, err)
		}
	}()
	if spillAt < 1 || spillAt > goldBufferBytes || receipts == nil || reduce == nil {
		return stats, fmt.Errorf("%w: invalid Temporal stage inputs", ErrRebuildLinkInputSize)
	}
	occupied, err := rebuildGuardHasPrefix(ctx, b.pending, rebuildTemporalPrefix)
	if err != nil {
		return stats, err
	}
	if occupied {
		return stats, invalid("Temporal scratch range occupied")
	}
	p := &RebuildTemporalPass{build: b, threshold: spillAt}
	defer func() {
		p.build = nil
		err = errors.Join(err, b.pending.DeleteRange(rebuildTemporalPrefix, prefixEnd(rebuildTemporalPrefix), pebble.NoSync))
	}()
	stats, err = reduce(p)
	if err != nil {
		return stats, err
	}
	if p.wants {
		if err := p.prove(ctx, receipts); err != nil {
			return stats, err
		}
	}
	err = b.withWrites(func() error { return p.apply(ctx) })
	stats.SpillBytes = p.spill + stats.ConstSpillBytes
	return stats, err
}

func rebuildTemporalKey(col byte, fields ...string) []byte {
	return keyStrings([]byte{rebuildTemporalPrefix[0], rebuildTemporalPrefix[1], col}, fields...)
}

func rebuildTemporalInput(key, value []byte) error {
	if len(key)+len(value) > rebuildTemporalRowLimit {
		return fmt.Errorf("%w: Temporal row", ErrRebuildLinkInputSize)
	}
	return nil
}

func (p *RebuildTemporalPass) put(ctx context.Context, key, value []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := rebuildTemporalInput(key, value); err != nil {
		return err
	}
	if err := p.build.pending.Set(key, value, pebble.NoSync); err != nil {
		return err
	}
	p.buffered += len(key) + len(value)
	if p.buffered >= p.threshold {
		before := p.build.pending.Metrics().Levels[0].TableBytesFlushed
		if err := p.build.pending.Flush(); err != nil {
			return err
		}
		p.spill += p.build.pending.Metrics().Levels[0].TableBytesFlushed - before
		p.buffered = 0
	}
	return nil
}

// Put saves one resolver-owned row in an uppercase scratch column.
func (p *RebuildTemporalPass) Put(ctx context.Context, col byte, value string, fields ...string) error {
	if col < 'A' || col > 'Z' {
		return invalid("Temporal resolver scratch column")
	}
	return p.put(ctx, rebuildTemporalKey(col, fields...), []byte(value))
}

// Get reads a live scratch row. An empty value and a missing row are distinct.
func (p *RebuildTemporalPass) Get(ctx context.Context, col byte, fields ...string) (string, bool, error) {
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	return p.get(rebuildTemporalKey(col, fields...))
}

func (p *RebuildTemporalPass) get(key []byte) (string, bool, error) {
	var found bool
	value, err := pendingGet(p.build.pending, key, func(raw []byte) (string, error) {
		found = true
		return string(raw), rebuildTemporalInput(key, raw)
	})
	return value, found, err
}

// Visit streams the selected column in tuple order from a starting snapshot.
// A nested Get observes writes made by an earlier callback in this sweep.
func (p *RebuildTemporalPass) Visit(ctx context.Context, col byte, fields []string, yield func([]string, string) error) error {
	return pendingVisit(ctx, p.build.pending, rebuildTemporalKey(col, fields...), func(r row) error {
		if err := rebuildTemporalInput(r.key, r.value); err != nil {
			return err
		}
		var parts []string
		for tail := r.key[3:]; len(tail) > 0; {
			field, rest, err := readKeyString(tail)
			if err != nil {
				return err
			}
			parts = append(parts, field)
			tail = rest
		}
		return yield(parts, string(r.value))
	})
}

func (p *RebuildTemporalPass) View() RebuildView { return p.build.RebuildView() }

// Node reads the node role overlay before durable state. The overlay only
// changes metadata, never identity or candidate-index membership.
func (p *RebuildTemporalPass) Node(ctx context.Context, id string) (*graph.Node, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	n, err := p.stagedNode(id)
	if err != nil || n != nil {
		return n, err
	}
	return p.View().Node(ctx, id)
}

func (p *RebuildTemporalPass) stagedNode(id string) (*graph.Node, error) {
	key := rebuildTemporalKey('n', id)
	return pendingGet(p.build.pending, key, func(raw []byte) (*graph.Node, error) {
		if err := rebuildTemporalInput(key, raw); err != nil {
			return nil, err
		}
		n, err := decodeNode(raw)
		if err == nil && n.ID != id {
			return nil, invalid("Temporal staged node identity")
		}
		return n, err
	})
}

func (p *RebuildTemporalPass) StampNode(ctx context.Context, n *graph.Node) error {
	raw, err := encodeNode(n)
	if err != nil {
		return err
	}
	return p.put(ctx, rebuildTemporalKey('n', n.ID), raw)
}

// Edge reads exactly one durable identity; wrapper slots live separately.
func (p *RebuildTemporalPass) Edge(ctx context.Context, id graph.EdgeIdentity) (*graph.Edge, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return p.edge(p.build.pending, pendingEdgeKey(&graph.Edge{
		From: id.From, To: id.To, Kind: id.Kind, FilePath: id.FilePath, Line: id.Line,
	}))
}

func (p *RebuildTemporalPass) edge(reader pebble.Reader, key []byte) (*graph.Edge, error) {
	return pendingGet(reader, key, func(raw []byte) (*graph.Edge, error) {
		if err := rebuildTemporalInput(key, raw); err != nil {
			return nil, err
		}
		e, err := decodeEdge(raw)
		if err == nil && !bytes.Equal(key, pendingEdgeKey(e)) {
			return nil, invalid("Temporal edge identity")
		}
		return e, err
	})
}

// IndexInputs freezes the tagged census in complete edge-key order. First
// probe for a row that can drive a write. A marker-only checkout needs no
// census scratch. Only an active turn pays a second scan and exact refetches.
func (p *RebuildTemporalPass) IndexInputs(ctx context.Context, classify func(*graph.Edge) (candidate, active bool)) (bool, error) {
	prefix := []byte{familyEdge, edgeOut}
	active := false
	err := p.viewEdges(ctx, prefix, func(e *graph.Edge) error {
		_, drive := classify(e)
		active = active || drive
		return nil
	}, graph.EdgeCalls, graph.EdgeAnnotated)
	if err != nil || !active {
		return false, err
	}
	err = p.viewEdges(ctx, prefix, func(e *graph.Edge) error {
		if candidate, _ := classify(e); !candidate {
			return nil
		}
		key := append(rebuildTemporalKey('i', string(e.Kind)), pendingEdgeKey(e)...)
		return p.put(ctx, key, nil)
	}, graph.EdgeCalls, graph.EdgeAnnotated)
	return err == nil, err
}

func (p *RebuildTemporalPass) VisitInputs(ctx context.Context, kind graph.EdgeKind, yield func(*graph.Edge) error) error {
	prefix := rebuildTemporalKey('i', string(kind))
	return p.View().visit(ctx, prefix, func(snap *pebble.Snapshot, r row) error {
		e, err := p.edge(snap, r.key[len(prefix):])
		if err != nil {
			return err
		}
		if e == nil || e.Kind != kind {
			return invalid("Temporal census lost an edge")
		}
		return yield(e)
	})
}

// VisitEdges reads durable edges of the selected kind in complete key order.
func (p *RebuildTemporalPass) VisitEdges(ctx context.Context, kind graph.EdgeKind, yield func(*graph.Edge) error) error {
	return p.viewEdges(ctx, []byte{familyEdge, edgeOut}, yield, kind)
}

func (p *RebuildTemporalPass) viewEdges(ctx context.Context, prefix []byte, yield func(*graph.Edge) error, kinds ...graph.EdgeKind) error {
	return p.View().visit(ctx, prefix, func(_ *pebble.Snapshot, r row) error {
		if len(kinds) != 0 {
			got, err := rebuildAttributionEdgeKind(r.key)
			if err != nil || !slices.Contains(kinds, got) {
				return err
			}
		}
		if err := rebuildTemporalInput(r.key, r.value); err != nil {
			return err
		}
		e, err := decodeEdge(r.value)
		if err != nil {
			return err
		}
		if !bytes.Equal(r.key, pendingEdgeKey(e)) {
			return invalid("Temporal edge identity")
		}
		return yield(e)
	})
}

func (p *RebuildTemporalPass) Outgoing(ctx context.Context, from string, yield func(*graph.Edge) error) error {
	return p.viewEdges(ctx, keyStrings([]byte{familyEdge, edgeOut}, from), yield)
}

func (p *RebuildTemporalPass) Incoming(ctx context.Context, target string, kind graph.EdgeKind, yield func(*graph.Edge) error) error {
	prefix := keyStrings([]byte{familyEdge, edgeIn}, target)
	return p.View().visit(ctx, prefix, func(snap *pebble.Snapshot, r row) error {
		if len(r.value) < 2 || !bytes.Equal(r.key[len(prefix):], r.value) || !bytes.Equal(r.value[:2], []byte{familyEdge, edgeOut}) {
			return invalid("Temporal reverse index identity")
		}
		got, err := rebuildAttributionEdgeKind(r.value)
		if err != nil || got != kind {
			return err
		}
		e, err := p.edge(snap, r.value)
		if err != nil {
			return err
		}
		if e == nil || e.To != target {
			return invalid("Temporal reverse index has no matching row")
		}
		return yield(e)
	})
}

func (p *RebuildTemporalPass) QueueMove(ctx context.Context, oldTo string, e *graph.Edge, staged bool) error {
	old := *e
	old.To = oldTo
	raw, err := encodeEdge(e)
	if err != nil {
		return err
	}
	ordinal := p.moves
	p.moves++
	key := binary.BigEndian.AppendUint64(rebuildTemporalKey('m'), ordinal)
	oldKey := pendingEdgeKey(&old)
	// A staged wrapper is a new payload even if its old key shadows a
	// durable call. Only a durable move can inherit that call's recorder.
	if !staged {
		recorder, err := readLinkRecorder(p.build.pending, oldKey)
		if err != nil {
			return err
		}
		if len(recorder) != 0 {
			if err := p.put(ctx, rebuildTemporalMoveRecorderKey(ordinal), recorder); err != nil {
				return err
			}
		}
	}
	return p.put(ctx, key, append(appendKeyString(nil, string(oldKey)), raw...))
}

func (p *RebuildTemporalPass) QueueAdd(ctx context.Context, e *graph.Edge) error {
	raw, err := encodeEdge(e)
	if err != nil {
		return err
	}
	key := binary.BigEndian.AppendUint64(rebuildTemporalKey('a'), p.adds)
	p.adds++
	return p.put(ctx, key, raw)
}

func (p *RebuildTemporalPass) apply(ctx context.Context) error {
	if err := pendingVisit(ctx, p.build.pending, rebuildTemporalKey('n'), func(r row) error {
		n, err := decodeNode(r.value)
		if err != nil {
			return err
		}
		return p.build.writeNode(n)
	}); err != nil {
		return err
	}
	// Native slices mutations (not inputs) into windows of 5,000. Record
	// which old keys were present at the window start. Temporal can move a
	// staged-only wrapper; an absent old key can cancel an earlier insert.
	const window = 5000
	for start := uint64(0); start < p.moves; start += window {
		stop := min(start+window, p.moves)
		var present [window]bool
		for ordinal := start; ordinal < stop; ordinal++ {
			old, _, err := p.move(ctx, ordinal)
			if err != nil {
				return err
			}
			e, err := p.edge(p.build.pending, old)
			if err != nil {
				return err
			}
			if e != nil {
				present[ordinal-start] = true
			}
		}
		for ordinal := start; ordinal < stop; ordinal++ {
			old, _, err := p.move(ctx, ordinal)
			if err != nil {
				return err
			}
			if err := p.deleteIfPresent(old); err != nil {
				return err
			}
		}
		for ordinal := start; ordinal < stop; ordinal++ {
			old, e, err := p.move(ctx, ordinal)
			if err != nil {
				return err
			}
			if !present[ordinal-start] {
				if err := p.deleteIfPresent(old); err != nil {
					return err
				}
			}
			key := rebuildTemporalMoveRecorderKey(ordinal)
			recorder, err := pendingGet(p.build.pending, key, func(raw []byte) ([]byte, error) {
				return bytes.Clone(raw), rebuildTemporalInput(key, raw)
			})
			if err != nil {
				return err
			}
			if err := p.insertIfAbsent(e, recorder); err != nil {
				return err
			}
		}
	}
	// SQLite flushes staged slots after the durable reindex. The actual
	// final identity at each slot is inserted only if still unoccupied.
	return pendingVisit(ctx, p.build.pending, rebuildTemporalKey('a'), func(r row) error {
		e, err := decodeEdge(r.value)
		if err != nil {
			return err
		}
		return p.insertIfAbsent(e, nil)
	})
}

func rebuildTemporalMoveRecorderKey(ordinal uint64) []byte {
	return binary.BigEndian.AppendUint64(rebuildTemporalKey('l'), ordinal)
}

func (p *RebuildTemporalPass) move(ctx context.Context, ordinal uint64) ([]byte, *graph.Edge, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	var old []byte
	e, err := pendingGet(p.build.pending, binary.BigEndian.AppendUint64(rebuildTemporalKey('m'), ordinal), func(raw []byte) (*graph.Edge, error) {
		key, tail, err := readKeyString(raw)
		if err != nil {
			return nil, err
		}
		old = []byte(key)
		return decodeEdge(tail)
	})
	if err == nil && e == nil {
		err = invalid("Temporal move ordinal missing")
	}
	return old, e, err
}

func (p *RebuildTemporalPass) deleteIfPresent(key []byte) error {
	e, err := p.edge(p.build.pending, key)
	if err != nil || e == nil {
		return err
	}
	return p.build.deleteEdge(key, e)
}

func (p *RebuildTemporalPass) insertIfAbsent(e *graph.Edge, recorder []byte) error {
	prior, err := p.edge(p.build.pending, pendingEdgeKey(e))
	if err != nil || prior != nil {
		return err
	}
	if len(recorder) != 0 {
		return p.build.putEdgeWithLinkRecorder(e, recorder)
	}
	return p.build.addEdge(e)
}

// Prove roles and changed calls using the original exact row, not the
// synthetic placeholder, a bare file suffix, or a same-line contestant.
func (p *RebuildTemporalPass) WantRecorder(ctx context.Context, e *graph.Edge) error {
	id := graph.EdgeIdentityFor(e)
	raw, found, err := p.View().OriginalLinkRecorder(ctx, id)
	if err != nil {
		return err
	}
	if found {
		id = raw
	}
	// No inherited identity doesn't establish provenance. In that case
	// the very same full live key must be present in a checked receipt.
	return p.want(ctx, rebuildTemporalRecorder(id, e))
}

func rebuildTemporalRecorder(id graph.EdgeIdentity, e *graph.Edge) []byte {
	key := pendingEdgeKey(&graph.Edge{From: id.From, To: id.To, Kind: id.Kind, FilePath: id.FilePath, Line: id.Line})
	via, _ := e.Meta["via"].(string)
	kind, _ := e.Meta["temporal_kind"].(string)
	name, _ := e.Meta["temporal_name"].(string)
	args, _ := e.Meta["args"].(string)
	callee, _ := e.Meta["callee"].(string)
	key = rebuildTemporalKey('e', string(key), via, kind, name, args, callee)
	switch names := e.Meta["arg_names"].(type) {
	case []string:
		key = keyStrings(key, names...)
	case []any:
		for _, v := range names {
			s, _ := v.(string)
			key = appendKeyString(key, s)
		}
	}
	return key
}

func (p *RebuildTemporalPass) WantNode(ctx context.Context, id string) error {
	return p.want(ctx, rebuildTemporalKey('d', id))
}

// WantRoleNode proves the declaration and, for a preexisting role, the
// role/name on its receipt. New role writes have their own recorder proof.
func (p *RebuildTemporalPass) WantRoleNode(ctx context.Context, n *graph.Node) error {
	if err := p.WantNode(ctx, n.ID); err != nil {
		return err
	}
	staged, err := p.stagedNode(n.ID)
	if err != nil || staged != nil {
		return err
	}
	return p.want(ctx, rebuildTemporalRole(n))
}

func rebuildTemporalRole(n *graph.Node) []byte {
	role, _ := n.Meta["temporal_role"].(string)
	name, _ := n.Meta["temporal_name"].(string)
	return rebuildTemporalKey('h', n.ID, role, name)
}

func (p *RebuildTemporalPass) want(ctx context.Context, key []byte) error {
	p.wants = true
	return p.put(ctx, key, []byte{rebuildCapabilityWanted})
}

func (p *RebuildTemporalPass) prove(ctx context.Context, receipts iter.Seq2[ParseFacts, error]) error {
	for facts, err := range receipts {
		if err != nil {
			return err
		}
		file, lang := pathkey.Normalize(facts.File.FilePath), facts.EffectiveLanguage
		for _, n := range facts.Nodes {
			key := rebuildTemporalKey('d', n.ID)
			_, wanted, err := p.get(key)
			if err != nil {
				return err
			}
			if !wanted {
				continue
			}
			live, err := p.View().Node(ctx, n.ID)
			if err != nil {
				return err
			}
			ok := lang != "" && n.FilePath == file && live != nil && live.FilePath == file &&
				live.Kind == n.Kind && live.Language == n.Language && live.Name == n.Name && live.RepoPrefix == n.RepoPrefix
			if err := p.note(ctx, key, ok); err != nil {
				return err
			}
			if err := p.note(ctx, rebuildTemporalRole(n), ok); err != nil {
				return err
			}
		}
		for _, e := range facts.Edges {
			if err := ctx.Err(); err != nil {
				return err
			}
			if e.Kind != graph.EdgeCalls && e.Kind != graph.EdgeAnnotated {
				continue
			}
			if err := p.note(ctx, rebuildTemporalRecorder(graph.EdgeIdentityFor(e), e), lang != "" && e.FilePath == file); err != nil {
				return err
			}
		}
	}
	var count uint64
	for _, col := range []byte{'d', 'e', 'h'} {
		if err := pendingVisit(ctx, p.build.pending, rebuildTemporalKey(col), func(r row) error {
			if !slices.Equal(r.value, []byte{rebuildCapabilityWanted | rebuildCapabilityProved}) {
				count++
			}
			return nil
		}); err != nil {
			return err
		}
	}
	if count != 0 {
		return &RebuildUnknownProvenanceError{Count: count}
	}
	return nil
}

func (p *RebuildTemporalPass) note(ctx context.Context, key []byte, proved bool) error {
	state, found, err := p.get(key)
	if err != nil || !found {
		return err
	}
	if len(state) != 1 {
		return invalid("Temporal receipt proof state")
	}
	bit := byte(rebuildCapabilityUnproved)
	if proved {
		bit = rebuildCapabilityProved
	}
	if state[0]&bit == 0 {
		return p.put(ctx, key, []byte{state[0] | bit})
	}
	return nil
}
