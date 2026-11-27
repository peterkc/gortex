package store_pebble

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"slices"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/pathkey"
)

// ErrRebuildTemporal marks a failure before the private framework turn can be offered.
var ErrRebuildTemporal = errors.New("pebble rebuild Temporal stage failed")

var rebuildTemporalPrefix = []byte{'g', 'Y'}

const (
	rebuildTemporalRowLimit        = 16 << 20
	rebuildTemporalProofStateBytes = 1 + 8 // flags and a big-endian ordinal
)

// RebuildTemporalStats describes the private turn. SpillBytes counts physical
// bytes of closed threshold SST runs consumed by the insertion stream. The
// pending overlay and final in-memory tail add none.
type RebuildTemporalStats struct {
	WrapperRounds                         int
	WrapperCapped                         bool
	Wrappers, Resolved, Moves, SpillBytes uint64
}

// RebuildTemporalPass owns one scratch range for the second framework turn.
// The worker must serialize this callback with all other private graph writes.
// Scratch, including the node overlay, is not a serving family.
type RebuildTemporalPass struct {
	build     *BuildStore
	threshold int
	moves     uint64
	adds      uint64
	wants     uint64
	addRows   []row
	addBytes  int
	addRuns   int
	addDir    string
	spill     uint64
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
		p.addRows = nil
		err = errors.Join(err, b.pending.DeleteRange(rebuildTemporalPrefix, prefixEnd(rebuildTemporalPrefix), pebble.NoSync))
		if p.addDir != "" {
			err = errors.Join(err, os.RemoveAll(p.addDir))
		}
	}()
	stats, err = reduce(p)
	if err != nil {
		return stats, err
	}
	if p.wants != 0 {
		if err := p.prove(ctx, receipts); err != nil {
			return stats, err
		}
	}
	err = b.withWrites(func() error { return p.apply(ctx) })
	stats.SpillBytes = p.spill
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
	return p.build.pending.Set(key, value, pebble.NoSync)
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
	// A staged wrapper can shadow a durable call, but it cannot inherit
	// that call's custody. The stage proves the wrapper's trigger and
	// declarations before the queued effect can reach the writer.
	custody := rebuildEdgeCustody{proof: rebuildProofTemporalSynthetic}
	if !staged {
		custody, err = readRebuildEdgeCustody(p.build.pending, oldKey)
		if err != nil {
			return err
		}
	}
	if saved := rebuildMoveCustodyBytes(custody); len(saved) != 0 {
		if err := p.put(ctx, rebuildTemporalMoveCustodyKey(ordinal), saved); err != nil {
			return err
		}
	}
	return p.put(ctx, key, append(appendKeyString(nil, string(oldKey)), raw...))
}

// QueueAdd appends a final slot or cross-language insertion. Only Apply
// reads these rows, in ordinal order after the reindex windows.
func (p *RebuildTemporalPass) QueueAdd(ctx context.Context, e *graph.Edge) error {
	raw, err := encodeEdge(e)
	if err != nil {
		return err
	}
	key := binary.BigEndian.AppendUint64(rebuildTemporalKey('a'), p.adds)
	p.adds++
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := rebuildTemporalInput(key, raw); err != nil {
		return err
	}
	size := len(key) + len(raw)
	if len(p.addRows) != 0 && size > p.threshold-p.addBytes {
		if err := p.spillAdds(ctx); err != nil {
			return err
		}
	}
	p.addRows = append(p.addRows, row{key, raw})
	p.addBytes += size
	if p.addBytes >= p.threshold {
		return p.spillAdds(ctx)
	}
	return nil
}

func (p *RebuildTemporalPass) spillAdds(ctx context.Context) error {
	if p.addDir == "" {
		dir, err := os.MkdirTemp(filepath.Join(p.build.scratchParent, "pending"), "temporal-adds-")
		if err != nil {
			return err
		}
		p.addDir = dir
	}
	path := p.addRunPath(p.addRuns)
	if err := writeLinkRun(ctx, path, p.addRows); err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	p.addRuns++
	p.spill += uint64(info.Size())
	clear(p.addRows)
	p.addRows = p.addRows[:0]
	p.addBytes = 0
	return nil
}

func (p *RebuildTemporalPass) addRunPath(run int) string {
	return filepath.Join(p.addDir, fmt.Sprintf("%d.sst", run))
}

func (p *RebuildTemporalPass) visitAdds(ctx context.Context, yield func(row) error) error {
	// Ordinals increase as additions enter the buffer. Each run precedes
	// both the next run and the tail, even for repeated edge identities.
	for i := range p.addRuns {
		if err := visitLinkRun(ctx, p.addRunPath(i), yield); err != nil {
			return err
		}
	}
	for _, r := range p.addRows {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := yield(r); err != nil {
			return err
		}
	}
	return ctx.Err()
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
			key := rebuildTemporalMoveCustodyKey(ordinal)
			custody, err := pendingGet(p.build.pending, key, func(raw []byte) (rebuildEdgeCustody, error) {
				if err := rebuildTemporalInput(key, raw); err != nil {
					return rebuildEdgeCustody{}, err
				}
				return decodeRebuildMoveCustody(raw)
			})
			if err != nil {
				return err
			}
			// Only the inherited raw recorder used the direct writer before
			// custody. Do not bypass the add gate for the exact-key fallback.
			inheritedRaw := len(custody.raw) != 0
			if !inheritedRaw && custody.proof == 0 {
				custody.raw = old
			}
			if err := p.insertIfAbsent(e, custody, inheritedRaw); err != nil {
				return err
			}
		}
	}
	// SQLite flushes staged slots after the durable reindex. The actual
	// final identity at each slot is inserted only if still unoccupied.
	return p.visitAdds(ctx, func(r row) error {
		e, err := decodeEdge(r.value)
		if err != nil {
			return err
		}
		// Both wrapper and cross-language additions enter from the proved
		// reducer paths, not from tags on the final edge.
		return p.insertIfAbsent(e, rebuildEdgeCustody{proof: rebuildProofTemporalSynthetic}, false)
	})
}

func rebuildTemporalMoveCustodyKey(ordinal uint64) []byte {
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

func (p *RebuildTemporalPass) insertIfAbsent(e *graph.Edge, custody rebuildEdgeCustody, inheritedRaw bool) error {
	prior, err := p.edge(p.build.pending, pendingEdgeKey(e))
	if err != nil || prior != nil {
		return err
	}
	if inheritedRaw {
		return p.build.putEdgeWithCustody(e, custody)
	}
	return p.build.addEdgeWithCustody(e, custody)
}

// RebuildTemporalProof attaches receipt demands to one proposed output row.
// Wrapper slots have a separate identity until the rebind sweep settles them.
type RebuildTemporalProof struct {
	pass *RebuildTemporalPass
	key  []byte
}

func (p *RebuildTemporalPass) EdgeProof(e *graph.Edge) RebuildTemporalProof {
	return RebuildTemporalProof{pass: p, key: pendingEdgeKey(e)}
}

func (p *RebuildTemporalPass) NodeProof(n *graph.Node) RebuildTemporalProof {
	return RebuildTemporalProof{pass: p, key: pendingNodeKey(n.ID)}
}

func (p *RebuildTemporalPass) WrapperProof(e *graph.Edge) RebuildTemporalProof {
	return RebuildTemporalProof{pass: p, key: append([]byte{'w'}, pendingEdgeKey(e)...)}
}

// FinishWrapper associates a staged slot's original proof demands with its
// final output key, even when rebind moves the slot away from its placeholder.
func (p *RebuildTemporalPass) FinishWrapper(ctx context.Context, old graph.EdgeIdentity, e *graph.Edge) error {
	if old == graph.EdgeIdentityFor(e) {
		return nil
	}
	key := pendingEdgeKey(&graph.Edge{From: old.From, To: old.To, Kind: old.Kind, FilePath: old.FilePath, Line: old.Line})
	return p.put(ctx, append(rebuildTemporalKey('f'), key...), pendingEdgeKey(e))
}

// WantRecorder proves the original exact row, not the synthetic placeholder,
// a bare file suffix, or a same-line contestant.
func (s RebuildTemporalProof) WantRecorder(ctx context.Context, e *graph.Edge) error {
	var raw []byte
	err := s.pass.View().read(ctx, func(snap *pebble.Snapshot) error {
		var err error
		raw, err = rebuildReceiptKey(snap, pendingEdgeKey(e))
		return err
	})
	if err != nil {
		return err
	}
	id, err := decodeLinkRecorder(raw)
	if err != nil {
		return err
	}
	return s.want(ctx, rebuildTemporalRecorder(id, e))
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

func (s RebuildTemporalProof) WantNode(ctx context.Context, id string) error {
	return s.want(ctx, rebuildTemporalKey('d', id))
}

// WantRoleNode proves the declaration and, for a preexisting role, the
// role/name on its receipt. New role writes have their own recorder proof.
func (s RebuildTemporalProof) WantRoleNode(ctx context.Context, n *graph.Node) error {
	if err := s.WantNode(ctx, n.ID); err != nil {
		return err
	}
	staged, err := s.pass.stagedNode(n.ID)
	if err != nil || staged != nil {
		return err
	}
	return s.want(ctx, rebuildTemporalRole(n))
}

func rebuildTemporalRole(n *graph.Node) []byte {
	role, _ := n.Meta["temporal_role"].(string)
	name, _ := n.Meta["temporal_name"].(string)
	return rebuildTemporalKey('h', n.ID, role, name)
}

func (s RebuildTemporalProof) want(ctx context.Context, key []byte) error {
	p := s.pass
	state, found, err := p.get(key)
	if err != nil {
		return err
	}
	if !found {
		p.wants++
		state = string(binary.BigEndian.AppendUint64([]byte{rebuildCapabilityWanted}, p.wants))
		if err := p.put(ctx, key, []byte(state)); err != nil {
			return err
		}
	}
	if len(state) != rebuildTemporalProofStateBytes {
		return invalid("Temporal receipt proof state")
	}
	// Use the proof ordinal instead of combining two potentially large keys.
	link := append(rebuildTemporalKey('p'), state[1:]...)
	return p.put(ctx, append(link, s.key...), nil)
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
	return p.refusedOutputs(ctx)
}

func (p *RebuildTemporalPass) refusedOutputs(ctx context.Context) error {
	// Only unproved demands enter the output set. Deduplicate on the durable
	// key so that several failed proofs or moves cannot count a row twice.
	for _, col := range []byte{'d', 'e', 'h'} {
		if err := pendingVisit(ctx, p.build.pending, rebuildTemporalKey(col), func(r row) error {
			if len(r.value) != rebuildTemporalProofStateBytes {
				return invalid("Temporal receipt proof state")
			}
			if r.value[0] == rebuildCapabilityWanted|rebuildCapabilityProved {
				return nil
			}
			link := append(rebuildTemporalKey('p'), r.value[1:]...)
			return pendingVisit(ctx, p.build.pending, link, func(r row) error {
				out := r.key[len(link):]
				if len(out) == 0 {
					return invalid("Temporal output proof key")
				}
				if out[0] == 'w' {
					out = out[1:]
					final, found, err := p.get(append(rebuildTemporalKey('f'), out...))
					if err != nil {
						return err
					}
					if found {
						out = []byte(final)
					}
				}
				return p.put(ctx, append(rebuildTemporalKey('u'), out...), nil)
			})
		}); err != nil {
			return err
		}
	}
	var refused RebuildUnknownProvenanceError
	if err := pendingVisit(ctx, p.build.pending, rebuildTemporalKey('u'), func(r row) error {
		if refused.Count == 0 {
			key := r.key[3:]
			if key[0] == familyNode {
				id, tail, err := readKeyString(key[1:])
				if err != nil {
					return err
				}
				if len(tail) != 0 {
					return invalid("Temporal refused node key")
				}
				n, err := p.stagedNode(id)
				if err != nil {
					return err
				}
				if n == nil {
					return invalid("Temporal refused node missing")
				}
				refused.First = graph.EdgeIdentity{From: n.ID, FilePath: n.FilePath}
			} else {
				id, err := decodeLinkRecorder(key)
				if err != nil {
					return err
				}
				refused.First = id
			}
		}
		refused.Count++
		return nil
	}); err != nil {
		return err
	}
	if refused.Count != 0 {
		return &refused
	}
	return nil
}

func (p *RebuildTemporalPass) note(ctx context.Context, key []byte, proved bool) error {
	state, found, err := p.get(key)
	if err != nil || !found {
		return err
	}
	if len(state) != rebuildTemporalProofStateBytes {
		return invalid("Temporal receipt proof state")
	}
	bit := byte(rebuildCapabilityUnproved)
	if proved {
		bit = rebuildCapabilityProved
	}
	if state[0]&bit == 0 {
		return p.put(ctx, key, append([]byte{state[0] | bit}, state[1:]...))
	}
	return nil
}
