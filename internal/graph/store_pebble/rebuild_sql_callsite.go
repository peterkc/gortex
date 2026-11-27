package store_pebble

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"iter"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/pathkey"
)

var (
	ErrRebuildSQLCallsite      = errors.New("pebble rebuild SQL callsite stage failed")
	ErrRebuildSQLCallsiteOrder = errors.New("rebuild SQL callsite conservatively refuses interacting moves")
)

// RebuildSQLCallsiteOrderError counts distinct interacting destinations.
// First names the lowest complete output key.
type RebuildSQLCallsiteOrderError struct {
	Count uint64
	First graph.EdgeIdentity
}

func (e *RebuildSQLCallsiteOrderError) Unwrap() error { return ErrRebuildSQLCallsiteOrder }

func (e *RebuildSQLCallsiteOrderError) Error() string {
	return fmt.Sprintf("%s: count=%d first=%s", ErrRebuildSQLCallsiteOrder, e.Count, attributionIdentityText(e.First))
}

var rebuildSQLCallsitePrefix = []byte{'g', 'P'}

const rebuildSQLCallsiteRowBytes = 16 << 20

// RebuildSQLCallsiteStats describes the live census and planned moves.
type RebuildSQLCallsiteStats struct {
	Stubs, Resolved, Moves uint64
}

type rebuildSQLCallsitePass struct {
	b           *BuildStore
	fn          func(*graph.Edge) string
	rebind      func(*graph.Edge, string, string, bool) bool
	needsAnchor bool
	stats       RebuildSQLCallsiteStats
}

// RebuildSQLCallsites runs on the quiet private graph after MyBatis.
func (b *BuildStore) RebuildSQLCallsites(ctx context.Context, receipts iter.Seq2[ParseFacts, error],
	fn func(*graph.Edge) string, rebind func(*graph.Edge, string, string, bool) bool,
) (stats RebuildSQLCallsiteStats, err error) {
	defer func() {
		if err != nil {
			stats = RebuildSQLCallsiteStats{}
			err = fmt.Errorf("%w: %w", ErrRebuildSQLCallsite, err)
		}
	}()
	if receipts == nil || fn == nil || rebind == nil {
		return stats, fmt.Errorf("%w: invalid SQL callsite inputs", ErrRebuildLinkInputSize)
	}
	prefix := rebuildSQLCallsitePrefix
	occupied, err := rebuildGuardHasPrefix(ctx, b.pending, prefix)
	if err != nil {
		return stats, err
	}
	if occupied {
		return stats, invalid("SQL callsite scratch range occupied")
	}
	defer func() {
		err = errors.Join(err, b.pending.DeleteRange(prefix, prefixEnd(prefix), pebble.NoSync))
	}()
	p := rebuildSQLCallsitePass{b: b, fn: fn, rebind: rebind}
	haveFunctions, err := p.collect(ctx)
	if err != nil || !haveFunctions {
		return stats, err
	}
	if err := p.plan(ctx); err != nil || p.stats.Moves == 0 {
		return p.stats, err
	}
	if err := p.checkOrder(ctx); err != nil {
		return stats, err
	}
	if err := p.prove(ctx, receipts); err != nil {
		return stats, err
	}
	err = b.withWrites(func() error { return p.apply(ctx) })
	return p.stats, err
}

// c: SQL Function proof and name by ID; a: unique ID or ambiguity by name;
// u: affected name proof; r: raw-key claims.
// m: new payload by old key; d: seen destinations; x: interacting destinations.
func rebuildSQLCallsiteKey(col byte, fields ...string) []byte {
	return keyStrings(append(bytes.Clone(rebuildSQLCallsitePrefix), col), fields...)
}

func (p *rebuildSQLCallsitePass) put(key, val []byte) error {
	return p.b.putRebuildScratch(key, val, rebuildSQLCallsiteRowBytes, "SQL callsite")
}

func (p *rebuildSQLCallsitePass) state(key []byte) (byte, error) {
	return rebuildScratchState(p.b.pending, key, "SQL callsite receipt proof")
}

func (p *rebuildSQLCallsitePass) marked(key []byte) ([]byte, error) {
	return pendingGet(p.b.pending, key, func(val []byte) ([]byte, error) {
		if len(val) == 0 || val[0]&rebuildCapabilityWanted == 0 {
			return nil, invalid("SQL callsite receipt claim")
		}
		return bytes.Clone(val), nil
	})
}

func rebuildSQLCallsiteCandidate(n *graph.Node) bool {
	return n.Kind == graph.KindFunction && n.Language == "sql" && n.Name != ""
}

func (p *rebuildSQLCallsitePass) collect(ctx context.Context) (bool, error) {
	haveFunctions := false
	err := p.b.RebuildView().visit(ctx, []byte{familyNode}, func(_ *pebble.Snapshot, r row) error {
		n, err := checkedRebuildLightNode(r, "SQL callsite")
		if err != nil || !rebuildSQLCallsiteCandidate(n) {
			return err
		}
		if n.ID == "" {
			return invalid("SQL function has no ID")
		}
		haveFunctions = true
		if err := p.put(rebuildSQLCallsiteKey('c', n.ID), append([]byte{rebuildCapabilityWanted}, n.Name...)); err != nil {
			return err
		}
		key := rebuildSQLCallsiteKey('a', n.Name)
		target, seen, err := p.elect(n.Name)
		if err != nil || (seen && target == "") {
			return err
		}
		if !seen {
			return p.put(key, append([]byte{1}, n.ID...))
		}
		return p.put(key, []byte{0})
	})
	return haveFunctions, err
}

// Seen separates empty cohorts from ambiguous ones. A unique row carries
// its ID after byte one; byte zero marks any non-singleton cohort.
func (p *rebuildSQLCallsitePass) elect(name string) (target string, seen bool, err error) {
	row, err := pendingGet(p.b.pending, rebuildSQLCallsiteKey('a', name), func(val []byte) (string, error) {
		if len(val) == 1 && val[0] == 0 || len(val) > 1 && val[0] == 1 {
			return string(val), nil
		}
		return "", invalid("SQL callsite election row")
	})
	if len(row) > 0 && row[0] == 1 {
		return row[1:], true, err
	}
	return "", row != "", err
}

func (p *rebuildSQLCallsitePass) plan(ctx context.Context) error {
	return p.b.RebuildView().visit(ctx, []byte{familyEdge, edgeOut}, func(snap *pebble.Snapshot, r row) error {
		kind, err := rebuildAttributionEdgeKind(r.key)
		if err != nil || kind != graph.EdgeCalls {
			return err
		}
		e, err := decodeRebuildSQLCallsiteEdge(r.key, r.value)
		if err != nil {
			return err
		}
		name := p.fn(e)
		if name == "" {
			return nil
		}
		p.stats.Stubs++
		custody, err := readRebuildEdgeCustody(snap, r.key)
		if err != nil && !errors.Is(err, ErrRebuildUnknownProvenance) {
			return err
		}
		// A stale stamp cannot claim an input. A stationary row stays put;
		// an unproved mover refuses by destination. Valid stationary claims
		// still compete for the same raw key as a mover.
		if err == nil && custody.proof == 0 {
			if err := p.claim(custody.raw, r.key); err != nil {
				return err
			}
		}
		target, seen, err := p.elect(name)
		if err != nil {
			return err
		}
		matched := seen && target != ""
		if matched {
			p.stats.Resolved++
		}
		if !p.rebind(e, name, target, matched) {
			return nil
		}
		if seen {
			if err := p.put(rebuildSQLCallsiteKey('u', name), []byte{rebuildCapabilityWanted}); err != nil {
				return err
			}
		} else {
			p.needsAnchor = true
		}
		if err := p.stage(r.key, e); err != nil {
			return err
		}
		p.stats.Moves++
		return nil
	})
}

func (p *rebuildSQLCallsitePass) claim(raw, old []byte) error {
	if raw == nil {
		raw = old
	}
	key := append(rebuildSQLCallsiteKey('r'), raw...)
	claim, err := p.marked(key)
	if err != nil {
		return err
	}
	if claim != nil {
		return p.put(key, []byte{rebuildCapabilityWanted | rebuildCapabilityUnproved})
	}
	return p.put(key, append([]byte{rebuildCapabilityWanted}, old...))
}

func decodeRebuildSQLCallsiteEdge(key, val []byte) (*graph.Edge, error) {
	if len(key)+len(val) > rebuildSQLCallsiteRowBytes {
		return nil, fmt.Errorf("%w: SQL callsite edge row", ErrRebuildLinkInputSize)
	}
	e, err := decodeEdge(val)
	if err == nil && !bytes.Equal(key, pendingEdgeKey(e)) {
		return nil, invalid("SQL callsite edge does not match key")
	}
	return e, err
}

func (p *rebuildSQLCallsitePass) edge(key []byte) (*graph.Edge, error) {
	return pendingGet(p.b.pending, key, func(val []byte) (*graph.Edge, error) {
		return decodeRebuildSQLCallsiteEdge(key, val)
	})
}

func (p *rebuildSQLCallsitePass) prove(ctx context.Context, receipts iter.Seq2[ParseFacts, error]) error {
	for facts, err := range receipts {
		if err != nil {
			return err
		}
		if err := p.record(ctx, facts); err != nil {
			return err
		}
	}
	anchor, err := p.proveCohorts(ctx)
	if err != nil {
		return err
	}
	var refused rebuildScratchRefusals
	if err := p.moves(ctx, func(old []byte, e *graph.Edge) error {
		ok, err := p.proved(old, e, anchor)
		if err == nil && !ok {
			refused.add(graph.EdgeIdentityFor(e), pendingEdgeKey(e))
		}
		return err
	}); err != nil {
		return err
	}
	return refused.err()
}

func (p *rebuildSQLCallsitePass) record(ctx context.Context, facts ParseFacts) error {
	file := pathkey.Normalize(facts.File.FilePath)
	lang := facts.EffectiveLanguage
	source := lang == "javascript" || lang == "typescript" || lang == "python"
	for _, raw := range facts.Nodes {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := p.recordNode(raw, file, lang); err != nil {
			return err
		}
	}
	for _, raw := range facts.Edges {
		if err := ctx.Err(); err != nil {
			return err
		}
		if raw.Kind != graph.EdgeCalls {
			continue
		}
		key := append(rebuildSQLCallsiteKey('r'), pendingEdgeKey(raw)...)
		claim, err := p.marked(key)
		if err != nil {
			return err
		}
		if len(claim) <= 1 {
			continue // not claimed, or claimed twice
		}
		live, err := p.edge(claim[1:])
		if err != nil {
			return err
		}
		if live == nil {
			return invalid("SQL callsite recorder has no old call")
		}
		// Native uses the Call marker and name, not the caller node or repo.
		ok := source && raw.FilePath == file && p.fn(raw) == p.fn(live)
		if err := noteRebuildScratchPayload(p.put, key, claim, rebuildScratchProofBit(ok)); err != nil {
			return err
		}
	}
	return nil
}

func (p *rebuildSQLCallsitePass) recordNode(raw *graph.Node, file, lang string) error {
	candidate := rebuildSQLCallsiteKey('c', raw.ID)
	claim, err := p.marked(candidate)
	if err != nil || claim == nil {
		return err
	}
	if !p.needsAnchor {
		state, err := p.state(rebuildSQLCallsiteKey('u', string(claim[1:])))
		if err != nil || state == 0 {
			return err
		}
	}
	key := pendingNodeKey(raw.ID)
	live, err := pendingGet(p.b.pending, key, func(val []byte) (*graph.Node, error) {
		if len(key)+len(val) > rebuildSQLCallsiteRowBytes {
			return nil, fmt.Errorf("%w: SQL callsite node row", ErrRebuildLinkInputSize)
		}
		return checkedRebuildLightNode(row{key, val}, "SQL callsite")
	})
	if err != nil {
		return err
	}
	proved := lang == "sql" && live != nil && rebuildSQLCallsiteCandidate(raw) &&
		raw.FilePath == file && live.FilePath == file && raw.Language == live.Language && raw.Kind == live.Kind && raw.Name == live.Name
	return noteRebuildScratchPayload(p.put, candidate, claim, rebuildScratchProofBit(proved))
}

func (p *rebuildSQLCallsitePass) proveCohorts(ctx context.Context) (anchor bool, err error) {
	err = pendingVisit(ctx, p.b.pending, rebuildSQLCallsiteKey('c'), func(r row) error {
		if len(r.value) < 2 {
			return invalid("SQL callsite candidate proof")
		}
		if r.value[0] == rebuildScratchExact {
			anchor = true
			return nil
		}
		key := rebuildSQLCallsiteKey('u', string(r.value[1:]))
		state, err := p.state(key)
		if err != nil || state == 0 {
			return err
		}
		return noteRebuildScratchState(p.put, key, state, rebuildCapabilityUnproved)
	})
	return anchor, err
}

func (p *rebuildSQLCallsitePass) proved(old []byte, e *graph.Edge, anchor bool) (bool, error) {
	raw, err := rebuildReceiptKey(p.b.pending, old)
	if errors.Is(err, ErrRebuildUnknownProvenance) {
		return false, nil // count the affected output rather than the old call
	}
	if err != nil {
		return false, err
	}
	claim, err := p.marked(append(rebuildSQLCallsiteKey('r'), raw...))
	if err != nil || len(claim) <= 1 || claim[0] != rebuildScratchExact || !bytes.Equal(claim[1:], old) {
		return false, err
	}
	cohort, err := p.state(rebuildSQLCallsiteKey('u', p.fn(e)))
	// An empty cohort may reset a stale Call only if some other exact SQL
	// Function proves that native does not take its early return.
	return cohort == rebuildCapabilityWanted || (cohort == 0 && anchor), err
}

func (p *rebuildSQLCallsitePass) present(key []byte) (bool, error) {
	return pendingGet(p.b.pending, key, func([]byte) (bool, error) { return true, nil })
}

func (p *rebuildSQLCallsitePass) stage(old []byte, e *graph.Edge) error {
	val, err := encodeEdge(e)
	if err != nil {
		return err
	}
	dest := pendingEdgeKey(e)
	key := append(rebuildSQLCallsiteKey('d'), dest...)
	seen, err := p.present(key)
	if err != nil {
		return err
	}
	if seen {
		key = append(rebuildSQLCallsiteKey('x'), dest...)
	}
	if err := p.put(key, nil); err != nil {
		return err
	}
	return p.put(append(rebuildSQLCallsiteKey('m'), old...), val)
}

func (p *rebuildSQLCallsitePass) moves(ctx context.Context, yield func([]byte, *graph.Edge) error) error {
	prefix := rebuildSQLCallsiteKey('m')
	return pendingVisit(ctx, p.b.pending, prefix, func(r row) error {
		e, err := decodeEdge(r.value)
		if err != nil {
			return err
		}
		return yield(r.key[len(prefix):], e)
	})
}

func (p *rebuildSQLCallsitePass) checkOrder(ctx context.Context) error {
	// With unique destinations and no destination among the moving old keys,
	// every incumbent stays put. No mover can affect another's SQLite result,
	// even across a 5,000-move window boundary.
	if err := p.moves(ctx, func(_ []byte, e *graph.Edge) error {
		dest := pendingEdgeKey(e)
		occupiedByMover, err := p.present(append(rebuildSQLCallsiteKey('m'), dest...))
		if err != nil || !occupiedByMover {
			return err
		}
		return p.put(append(rebuildSQLCallsiteKey('x'), dest...), nil)
	}); err != nil {
		return err
	}
	prefix := rebuildSQLCallsiteKey('x')
	var order RebuildSQLCallsiteOrderError
	if err := pendingVisit(ctx, p.b.pending, prefix, func(r row) error {
		if order.Count == 0 {
			var err error
			order.First, err = decodeLinkRecorder(r.key[len(prefix):])
			if err != nil {
				return err
			}
		}
		order.Count++
		return nil
	}); err != nil {
		return err
	}
	if order.Count != 0 {
		return &order
	}
	return nil
}

func (p *rebuildSQLCallsitePass) apply(ctx context.Context) error {
	return p.moves(ctx, func(old []byte, e *graph.Edge) error {
		prior, err := p.edge(old)
		if err != nil {
			return err
		}
		if prior == nil {
			return invalid("SQL callsite move has no old edge")
		}
		incumbent, err := p.edge(pendingEdgeKey(e))
		if err != nil {
			return err
		}
		raw, err := rebuildReceiptKey(p.b.pending, old)
		if err != nil {
			return err
		}
		if err := p.b.deleteEdge(old, prior); err != nil || incumbent != nil {
			return err
		}
		return p.b.addEdgeWithCustody(e, rebuildEdgeCustody{raw: raw})
	})
}
