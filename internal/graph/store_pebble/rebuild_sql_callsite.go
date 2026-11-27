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

var ErrRebuildSQLCallsite = errors.New("pebble rebuild SQL callsite stage failed")

var rebuildSQLCallsitePrefix = []byte{'g', 'P'}

const rebuildSQLCallsiteRowBytes = 16 << 20

// RebuildSQLCallsiteStats describes the live census and planned moves.
type RebuildSQLCallsiteStats struct {
	Stubs, Resolved, Moves uint64
}

type rebuildSQLCallsitePass struct {
	rebuildCallMoves
	fn          func(*graph.Edge) string
	rebind      func(*graph.Edge, string, string, bool) bool
	needsAnchor bool
	stats       RebuildSQLCallsiteStats
}

func newRebuildSQLCallsitePass(b *BuildStore, fn func(*graph.Edge) string, rebind func(*graph.Edge, string, string, bool) bool) rebuildSQLCallsitePass {
	return rebuildSQLCallsitePass{
		rebuildCallMoves: rebuildCallMoves{
			b: b, prefix: rebuildSQLCallsitePrefix, rowBytes: rebuildSQLCallsiteRowBytes, label: "SQL callsite", edgeRow: "edge row",
		},
		fn: fn, rebind: rebind,
	}
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
	p := newRebuildSQLCallsitePass(b, fn, rebind)
	haveFunctions, err := p.collect(ctx)
	if err != nil || !haveFunctions {
		return stats, err
	}
	if err := p.plan(ctx); err != nil || p.stats.Moves == 0 {
		return p.stats, err
	}
	if err := p.prove(ctx, receipts); err != nil {
		return stats, err
	}
	err = b.withWrites(func() error { return p.apply(ctx, p.stats.Moves, p.present) })
	return p.stats, err
}

// c: SQL Function proof and name by ID; a: unique ID or ambiguity by name;
// u: affected name, storing 1 plus the exact member count (capped at two);
// r: raw-key claims.
// m: new payload by old key; o: raw receipt key in move order.
func rebuildSQLCallsiteKey(col byte, fields ...string) []byte {
	return keyStrings(append(bytes.Clone(rebuildSQLCallsitePrefix), col), fields...)
}

func (p *rebuildSQLCallsitePass) cohort(name string) (byte, error) {
	return pendingGet(p.b.pending, rebuildSQLCallsiteKey('u', name), func(val []byte) (byte, error) {
		if len(val) != 1 || val[0] < 1 || val[0] > 3 {
			return 0, invalid("SQL callsite receipt proof")
		}
		return val[0], nil
	})
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
		e, err := p.decodeEdge(r.key, r.value)
		if err != nil {
			return err
		}
		name := p.fn(e)
		if name == "" {
			return nil
		}
		if err := p.edgeInput(r.key, r.value); err != nil {
			return err
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
			if err := p.put(rebuildSQLCallsiteKey('u', name), []byte{1}); err != nil {
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
		ok := lang != "" && raw.FilePath == file && p.fn(raw) == p.fn(live)
		if err := p.noteReceipt(key, claim, ok); err != nil {
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
		state, err := p.cohort(string(claim[1:]))
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
		if r.value[0] != rebuildScratchExact {
			return nil
		}
		anchor = true
		name := string(r.value[1:])
		count, err := p.cohort(name)
		if err != nil || count == 0 || count == 3 {
			return err
		}
		return p.put(rebuildSQLCallsiteKey('u', name), []byte{count + 1})
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
	name := p.fn(e)
	count, err := p.cohort(name)
	if err != nil {
		return false, err
	}
	if count == 0 {
		// A different exact SQL function proves native reaches the Call pass.
		return anchor, nil
	}
	target, _, err := p.elect(name)
	if target != "" {
		return count == 2, err // the unique live member must be exact
	}
	// Two exact members rule out a unique target even if others are missing.
	return count == 3, err
}
