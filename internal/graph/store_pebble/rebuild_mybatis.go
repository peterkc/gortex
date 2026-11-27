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
	ErrRebuildMyBatis      = errors.New("pebble rebuild MyBatis stage failed")
	ErrRebuildMyBatisOrder = errors.New("rebuild MyBatis conservatively refuses interacting moves")
)

// RebuildMyBatisOrderError counts distinct destinations of interacting moves.
// First names the lowest such complete edge key.
type RebuildMyBatisOrderError struct {
	Count uint64
	First graph.EdgeIdentity
}

func (e *RebuildMyBatisOrderError) Unwrap() error { return ErrRebuildMyBatisOrder }

func (e *RebuildMyBatisOrderError) Error() string {
	return fmt.Sprintf("%s: count=%d first=%s", ErrRebuildMyBatisOrder, e.Count, attributionIdentityText(e.First))
}

var rebuildMyBatisPrefix = []byte{'g', 'I'}

const rebuildMyBatisRowBytes = 16 << 20

// RebuildMyBatisStats describes the live census and planned moves.
type RebuildMyBatisStats struct {
	Stubs, Resolved, Moves uint64
}

type rebuildMyBatisPass struct {
	rebuildCallMoves
	stats RebuildMyBatisStats
}

func newRebuildMyBatisPass(b *BuildStore) rebuildMyBatisPass {
	return rebuildMyBatisPass{
		rebuildCallMoves: rebuildCallMoves{
			b: b, prefix: rebuildMyBatisPrefix, rowBytes: rebuildMyBatisRowBytes, label: "MyBatis", edgeRow: "input row",
		},
	}
}

// RebuildMyBatisRules supplies the mapper and Java matching rules for RebuildMyBatis.
// All callbacks are required.
type RebuildMyBatisRules struct {
	MethodKey func(*graph.Node) string
	Stub      func(*graph.Edge) (namespace, statement string)
	Join      func(namespace, statement string) string
	Rebind    func(e *graph.Edge, namespace, statement, method string) bool
}

// RebuildMyBatis runs at the live mapper turn on a quiet private build.
// The reducer elects from all live Java candidates before checking receipts.
func (b *BuildStore) RebuildMyBatis(ctx context.Context, receipts iter.Seq2[ParseFacts, error],
	rules RebuildMyBatisRules,
) (stats RebuildMyBatisStats, err error) {
	defer func() {
		if err != nil {
			stats = RebuildMyBatisStats{}
			err = fmt.Errorf("%w: %w", ErrRebuildMyBatis, err)
		}
	}()
	if receipts == nil || rules.MethodKey == nil || rules.Stub == nil || rules.Join == nil || rules.Rebind == nil {
		return stats, fmt.Errorf("%w: invalid MyBatis inputs", ErrRebuildLinkInputSize)
	}
	occupied, err := rebuildGuardHasPrefix(ctx, b.pending, rebuildMyBatisPrefix)
	if err != nil {
		return stats, err
	}
	if occupied {
		return stats, invalid("MyBatis scratch range occupied")
	}
	defer func() {
		err = errors.Join(err, b.pending.DeleteRange(rebuildMyBatisPrefix, prefixEnd(rebuildMyBatisPrefix), pebble.NoSync))
	}()
	p := newRebuildMyBatisPass(b)
	if err := p.capture(ctx, rules); err != nil || p.stats.Stubs == 0 {
		return p.stats, err
	}
	if err := p.collect(ctx, rules.MethodKey); err != nil {
		return stats, err
	}
	if err := p.markRepoSensitive(ctx); err != nil {
		return stats, err
	}
	if err := p.plan(ctx, rules); err != nil || p.stats.Moves == 0 {
		return p.stats, err
	}
	if err := p.checkOrder(ctx, func(count uint64, first graph.EdgeIdentity) error {
		return &RebuildMyBatisOrderError{Count: count, First: first}
	}); err != nil {
		return stats, err
	}
	if err := p.prove(ctx, receipts, rules); err != nil {
		return stats, err
	}
	err = b.withWrites(func() error { return p.apply(ctx, p.validatedPresent) })
	return p.stats, err
}

// s: live mapper stub by old out-key; j: join named by a live stub.
// c: candidate by cohort and ID; e: election aggregate by cohort and repo
// (empty repo is workspace); q: join with a unique nonempty repo cohort;
// n: node proof by ID; u: affected cohort proof.
// m: new payload by old out-key; d: destinations seen; x: conflicting outputs;
// r: raw recorder proof and claiming old key.
func rebuildMyBatisKey(col byte, fields ...string) []byte {
	return keyStrings(append(bytes.Clone(rebuildMyBatisPrefix), col), fields...)
}

func rebuildMyBatisInput(key, val []byte) error {
	if len(key)+len(val) > rebuildMyBatisRowBytes {
		return fmt.Errorf("%w: MyBatis input row", ErrRebuildLinkInputSize)
	}
	return nil
}

func (p *rebuildMyBatisPass) collect(ctx context.Context, methodKey func(*graph.Node) string) error {
	return p.b.RebuildView().visit(ctx, []byte{familyNode}, func(_ *pebble.Snapshot, r row) error {
		light, err := checkedRebuildLightNode(r, "MyBatis")
		if err != nil || light.Kind != graph.KindMethod || light.Language != "java" {
			return err
		}
		if err := rebuildMyBatisInput(r.key, r.value); err != nil {
			return err
		}
		n, err := decodeNode(r.value)
		if err != nil {
			return err
		}
		join := methodKey(n)
		if join == "" {
			return nil
		}
		wanted, err := p.present(rebuildMyBatisKey('j', join))
		if err != nil || !wanted {
			return err
		}
		if err := p.put(rebuildMyBatisKey('c', join, n.ID), []byte(n.ID)); err != nil {
			return err
		}
		if err := p.put(rebuildMyBatisKey('n', n.ID), []byte{rebuildCapabilityWanted}); err != nil {
			return err
		}
		if err := p.noteCandidate(join, "", n.ID); err != nil || n.RepoPrefix == "" {
			return err
		}
		return p.noteCandidate(join, n.RepoPrefix, n.ID)
	})
}

// Missing is none, a single zero byte is many, and a leading one followed
// by an ID is unique. Node IDs can contain arbitrary bytes.
func (p *rebuildMyBatisPass) noteCandidate(join, repo, id string) error {
	key := rebuildMyBatisKey('e', join, repo)
	state, err := p.candidate(key)
	if err != nil || state == "\x00" {
		return err
	}
	if state != "" {
		return p.put(key, []byte{0})
	}
	return p.put(key, append([]byte{1}, id...))
}

func (p *rebuildMyBatisPass) candidate(key []byte) (string, error) {
	return pendingGet(p.b.pending, key, func(val []byte) (string, error) {
		if len(val) == 1 && val[0] == 0 || len(val) > 1 && val[0] == 1 {
			return string(val), nil
		}
		return "", invalid("MyBatis election state")
	})
}

// Record each repo-sensitive join once, after all repo cohorts settle.
func (p *rebuildMyBatisPass) markRepoSensitive(ctx context.Context) error {
	prefix := rebuildMyBatisKey('e')
	var marked string
	return pendingVisit(ctx, p.b.pending, prefix, func(r row) error {
		if len(r.value) <= 1 {
			return nil
		}
		join, rest, err := readKeyString(r.key[len(prefix):])
		if err != nil || join == marked {
			return err
		}
		repo, _, err := readKeyString(rest)
		if err != nil || repo == "" {
			return err
		}
		marked = join
		return p.put(rebuildMyBatisKey('q', join), nil)
	})
}

func (p *rebuildMyBatisPass) elect(join, from string) (method string, caller *graph.Node, err error) {
	if join == "" {
		return "", nil, nil
	}
	state, err := p.candidate(rebuildMyBatisKey('e', join, ""))
	if err != nil {
		return "", nil, err
	}
	// Repo can change a workspace tie only if a repo has a unique method.
	if state == "\x00" && from != "" {
		sensitive, flagErr := p.present(rebuildMyBatisKey('q', join))
		if flagErr != nil || !sensitive {
			return "", nil, flagErr
		}
		caller, err = p.node(from)
		if err != nil {
			return "", nil, err
		}
		if caller != nil && caller.RepoPrefix != "" {
			state, err = p.candidate(rebuildMyBatisKey('e', join, caller.RepoPrefix))
		}
	}
	if len(state) > 1 {
		method = state[1:]
	}
	return method, caller, err
}

func (p *rebuildMyBatisPass) capture(ctx context.Context, rules RebuildMyBatisRules) error {
	return p.b.RebuildView().visit(ctx, []byte{familyEdge, edgeOut}, func(snap *pebble.Snapshot, r row) error {
		kind, err := rebuildAttributionEdgeKind(r.key)
		if err != nil || kind != graph.EdgeCalls {
			return err
		}
		e, err := decodeEdge(r.value)
		if err != nil {
			return err
		}
		if !bytes.Equal(r.key, pendingEdgeKey(e)) {
			return invalid("MyBatis Calls row does not match key")
		}
		ns, stmt := rules.Stub(e)
		if ns == "" || stmt == "" {
			return nil
		}
		if err := rebuildMyBatisInput(r.key, r.value); err != nil {
			return err
		}
		p.stats.Stubs++
		custody, err := readRebuildEdgeCustody(snap, r.key)
		if err != nil && !errors.Is(err, ErrRebuildUnknownProvenance) {
			return err
		}
		// A stale stamp cannot claim a raw receipt. If the stub moves,
		// prove counts the refused destination; a stationary stub stays put.
		// Valid raw keys still allow only one claimant, even if stationary.
		if err == nil && custody.proof == 0 {
			if err := p.claimRecorder(custody.raw, r.key); err != nil {
				return err
			}
		}
		if join := rules.Join(ns, stmt); join != "" {
			if err := p.put(rebuildMyBatisKey('j', join), nil); err != nil {
				return err
			}
		}
		return p.put(append(rebuildMyBatisKey('s'), r.key...), nil)
	})
}

func (p *rebuildMyBatisPass) plan(ctx context.Context, rules RebuildMyBatisRules) error {
	prefix := rebuildMyBatisKey('s')
	return pendingVisit(ctx, p.b.pending, prefix, func(r row) error {
		old := r.key[len(prefix):]
		e, err := p.edge(old)
		if err != nil {
			return err
		}
		if e == nil {
			return invalid("MyBatis census lost a live stub")
		}
		ns, stmt := rules.Stub(e)
		join := rules.Join(ns, stmt)
		method, caller, err := p.elect(join, e.From)
		if err != nil {
			return err
		}
		if method != "" {
			p.stats.Resolved++
		}
		if !rules.Rebind(e, ns, stmt, method) {
			return nil
		}
		if caller != nil {
			if err := p.put(rebuildMyBatisKey('n', e.From), []byte{rebuildCapabilityWanted}); err != nil {
				return err
			}
		}
		if join != "" {
			if err := p.put(rebuildMyBatisKey('u', join), []byte{rebuildCapabilityWanted}); err != nil {
				return err
			}
		}
		if err := p.stage(old, e); err != nil {
			return err
		}
		p.stats.Moves++
		return nil
	})
}

func (p *rebuildMyBatisPass) claimRecorder(raw, old []byte) error {
	if raw == nil {
		raw = old
	}
	key := append(rebuildMyBatisKey('r'), raw...)
	seen, err := p.present(key)
	if err != nil {
		return err
	}
	if seen {
		return p.put(key, []byte{rebuildCapabilityWanted | rebuildCapabilityUnproved})
	}
	return p.put(key, append([]byte{rebuildCapabilityWanted}, old...))
}

func (p *rebuildMyBatisPass) node(id string) (*graph.Node, error) {
	key := pendingNodeKey(id)
	return pendingGet(p.b.pending, key, func(val []byte) (*graph.Node, error) {
		if err := rebuildMyBatisInput(key, val); err != nil {
			return nil, err
		}
		n, err := decodeNode(val)
		if err == nil && n.ID != id {
			return nil, invalid("MyBatis node does not match key")
		}
		return n, err
	})
}

func (p *rebuildMyBatisPass) state(key []byte) (byte, error) {
	return rebuildScratchState(p.b.pending, key, "MyBatis receipt proof")
}

func (p *rebuildMyBatisPass) prove(ctx context.Context, receipts iter.Seq2[ParseFacts, error], rules RebuildMyBatisRules) error {
	for facts, err := range receipts {
		if err != nil {
			return err
		}
		if err := p.record(ctx, facts, rules); err != nil {
			return err
		}
	}
	if err := p.checkCohorts(ctx); err != nil {
		return err
	}
	var refused rebuildScratchRefusals
	if err := p.moves(ctx, func(old []byte, e *graph.Edge) error {
		ok, err := p.proved(old, e, rules)
		if err == nil && !ok {
			refused.add(graph.EdgeIdentityFor(e), pendingEdgeKey(e))
		}
		return err
	}); err != nil {
		return err
	}
	return refused.err()
}

func (p *rebuildMyBatisPass) record(ctx context.Context, facts ParseFacts, rules RebuildMyBatisRules) error {
	file := pathkey.Normalize(facts.File.FilePath)
	for _, raw := range facts.Nodes {
		if err := ctx.Err(); err != nil {
			return err
		}
		key := rebuildMyBatisKey('n', raw.ID)
		state, err := p.state(key)
		if err != nil {
			return err
		}
		if state == 0 {
			continue
		}
		live, err := p.node(raw.ID)
		if err != nil {
			return err
		}
		ok := live != nil && facts.EffectiveLanguage != "" && raw.FilePath == file && live.FilePath == file &&
			raw.Language == live.Language && raw.Kind == live.Kind && raw.RepoPrefix == live.RepoPrefix
		if ok && live.Kind == graph.KindMethod && live.Language == "java" {
			ok = facts.EffectiveLanguage == "java" && raw.Name == live.Name && rules.MethodKey(raw) == rules.MethodKey(live)
		}
		if err := noteRebuildScratchState(p.put, key, state, rebuildScratchProofBit(ok)); err != nil {
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
		key := append(rebuildMyBatisKey('r'), pendingEdgeKey(raw)...)
		claim, err := p.recorder(key)
		if err != nil {
			return err
		}
		if len(claim) <= 1 {
			continue // no claimant or a duplicate claim
		}
		live, err := p.edge(claim[1:])
		if err != nil {
			return err
		}
		if live == nil {
			return invalid("MyBatis recorder has no old call")
		}
		ns, stmt := rules.Stub(live)
		rawNS, rawStmt := rules.Stub(raw)
		ok := facts.EffectiveLanguage == "mybatis" && raw.FilePath == file && ns == rawNS && stmt == rawStmt
		if err := noteRebuildScratchPayload(p.put, key, claim, rebuildScratchProofBit(ok)); err != nil {
			return err
		}
	}
	return nil
}

func (p *rebuildMyBatisPass) recorder(key []byte) ([]byte, error) {
	return pendingGet(p.b.pending, key, func(val []byte) ([]byte, error) {
		if len(val) == 0 || val[0]&rebuildCapabilityWanted == 0 {
			return nil, invalid("MyBatis recorder proof")
		}
		return bytes.Clone(val), nil
	})
}

func (p *rebuildMyBatisPass) checkCohorts(ctx context.Context) error {
	prefix := rebuildMyBatisKey('c')
	return pendingVisit(ctx, p.b.pending, prefix, func(r row) error {
		join, _, err := readKeyString(r.key[len(prefix):])
		if err != nil {
			return err
		}
		key := rebuildMyBatisKey('u', join)
		state, err := p.state(key)
		if err != nil || state == 0 || state&rebuildCapabilityUnproved != 0 {
			return err
		}
		proof, err := p.state(rebuildMyBatisKey('n', string(r.value)))
		if err != nil || proof == rebuildScratchExact {
			return err
		}
		return noteRebuildScratchState(p.put, key, state, rebuildCapabilityUnproved)
	})
}

func (p *rebuildMyBatisPass) proved(old []byte, e *graph.Edge, rules RebuildMyBatisRules) (bool, error) {
	raw, err := rebuildReceiptKey(p.b.pending, old)
	if errors.Is(err, ErrRebuildUnknownProvenance) {
		return false, nil // count the affected output, not the old call
	}
	if err != nil {
		return false, err
	}
	claim, err := p.recorder(append(rebuildMyBatisKey('r'), raw...))
	if err != nil || len(claim) <= 1 || claim[0] != rebuildScratchExact || !bytes.Equal(claim[1:], old) {
		return false, err
	}
	ns, stmt := rules.Stub(e)
	join := rules.Join(ns, stmt)
	if join == "" {
		return true, nil
	}
	cohort, err := p.state(rebuildMyBatisKey('u', join))
	if err != nil || cohort != rebuildCapabilityWanted {
		return false, err
	}
	_, caller, err := p.elect(join, e.From)
	if err != nil || caller == nil {
		return err == nil, err // the exact caller point is absent
	}
	state, err := p.state(rebuildMyBatisKey('n', e.From))
	return state == rebuildScratchExact, err
}
