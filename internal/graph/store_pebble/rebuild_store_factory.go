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

var ErrRebuildStoreFactory = errors.New("pebble rebuild Store Factory stage failed")

var rebuildStoreFactoryPrefix = []byte{'g', 'V'}

const rebuildStoreFactoryRowBytes = 16 << 20

// RebuildStoreFactoryStats describes the live census and planned moves.
type RebuildStoreFactoryStats struct {
	Stubs, Resolved, Moves uint64
}

// RebuildStoreFactoryRules supplies the native metadata and payload decisions.
type RebuildStoreFactoryRules struct {
	Action func(*graph.Node) (string, string)
	Stub   func(*graph.Edge) (string, string)
	Getter func(*graph.Node, string) bool
	Rebind func(*graph.Edge, string, string, bool) bool
}

type rebuildStoreFactoryPass struct {
	rebuildCallMoves
	rules       RebuildStoreFactoryRules
	needsAnchor bool
	stats       RebuildStoreFactoryStats
}

// RebuildStoreFactory runs on the quiet private graph after SQL callsites.
// The worker must discard the build if applying the moves fails.
func (b *BuildStore) RebuildStoreFactory(ctx context.Context, receipts iter.Seq2[ParseFacts, error],
	rules RebuildStoreFactoryRules,
) (stats RebuildStoreFactoryStats, err error) {
	defer func() {
		if err != nil {
			stats = RebuildStoreFactoryStats{}
			err = fmt.Errorf("%w: %w", ErrRebuildStoreFactory, err)
		}
	}()
	if receipts == nil || rules.Action == nil || rules.Stub == nil || rules.Getter == nil || rules.Rebind == nil {
		return stats, fmt.Errorf("%w: invalid Store Factory inputs", ErrRebuildLinkInputSize)
	}
	prefix := rebuildStoreFactoryPrefix
	occupied, err := rebuildGuardHasPrefix(ctx, b.pending, prefix)
	if err != nil {
		return stats, err
	}
	if occupied {
		return stats, invalid("Store Factory scratch range occupied")
	}
	defer func() {
		err = errors.Join(err, b.pending.DeleteRange(prefix, prefixEnd(prefix), pebble.NoSync))
	}()
	p := rebuildStoreFactoryPass{
		rebuildCallMoves: rebuildCallMoves{
			b: b, prefix: rebuildStoreFactoryPrefix, rowBytes: rebuildStoreFactoryRowBytes, label: "Store Factory", inputRow: "edge row",
		},
		rules: rules,
	}
	haveActions, err := p.collect(ctx)
	if err != nil || !haveActions {
		return stats, err
	}
	if err := p.plan(ctx); err != nil || p.stats.Moves == 0 {
		return p.stats, err
	}
	if err := p.prove(ctx, receipts); err != nil {
		return stats, err
	}
	err = b.withWrites(func() error { return p.apply(ctx, p.stats.Moves) })
	return p.stats, err
}

// b: needed getter bindings; c/g: action/getter proof with affected key by ID.
// a/w: global/workspace action singleton; f/s: their file singletons.
// t: live getter files, x: exact getter files. u/v: affected action/getter proof.
// l: deferred Calls; n: affected caller proof; p: election witnesses; r: raw-key claims;
// m: new payload by old key; o: raw receipt key in move order.
func rebuildStoreFactoryKey(col byte, fields ...string) []byte {
	return keyStrings(append(bytes.Clone(rebuildStoreFactoryPrefix), col), fields...)
}

func (p *rebuildStoreFactoryPass) state(key []byte) (byte, error) {
	return rebuildScratchState(p.b.pending, key, "Store Factory receipt proof")
}

func (p *rebuildStoreFactoryPass) marked(key []byte) ([]byte, error) {
	return pendingGet(p.b.pending, key, func(val []byte) ([]byte, error) {
		if len(val) == 0 || val[0]&rebuildCapabilityWanted == 0 {
			return nil, invalid("Store Factory receipt claim")
		}
		return bytes.Clone(val), nil
	})
}

// A singleton row contains byte one and its value. Byte zero marks any
// non-singleton. Getter declarations in the same file share a value.
func (p *rebuildStoreFactoryPass) unique(key []byte) (value string, seen bool, err error) {
	state, err := pendingGet(p.b.pending, key, func(val []byte) (string, error) {
		if len(val) == 1 && val[0] == 0 || len(val) > 1 && val[0] == 1 {
			return string(val), nil
		}
		return "", invalid("Store Factory election row")
	})
	if len(state) > 0 && state[0] == 1 {
		return state[1:], true, err
	}
	return "", state != "", err
}

func (p *rebuildStoreFactoryPass) addUnique(key []byte, value string) error {
	old, seen, err := p.unique(key)
	if err != nil || seen && (old == "" || old == value) {
		return err
	}
	if seen {
		return p.put(key, []byte{0})
	}
	return p.put(key, append([]byte{1}, value...))
}

func (p *rebuildStoreFactoryPass) collect(ctx context.Context) (bool, error) {
	haveActions := false
	err := p.b.RebuildView().visit(ctx, []byte{familyNode}, func(_ *pebble.Snapshot, r row) error {
		light, err := checkedRebuildLightNode(r, "Store Factory")
		if err != nil || light.Kind != graph.KindFunction {
			return err
		}
		n, err := decodeNode(r.value)
		if err != nil {
			return err
		}
		binding, member := p.rules.Action(n)
		if binding == "" {
			return nil
		}
		if n.ID == "" {
			return invalid("Store Factory action has no ID")
		}
		if err := rebuildStoreFactoryNodeInput(r.key, r.value); err != nil {
			return err
		}
		haveActions = true
		cohort := rebuildStoreFactoryKey('u', binding, member)
		if err := p.put(rebuildStoreFactoryKey('c', n.ID), append([]byte{rebuildCapabilityWanted}, cohort...)); err != nil {
			return err
		}
		for _, key := range rebuildStoreScopeKeys(cohort, len(p.prefix), 'a', n) {
			if err := p.addUnique(key, n.ID); err != nil {
				return err
			}
		}
		return nil
	})
	return haveActions, err
}

func rebuildStoreFactoryGetterKind(kind graph.NodeKind) bool {
	return kind == graph.KindVariable || kind == graph.KindFunction || kind == graph.KindConstant
}

func (p *rebuildStoreFactoryPass) collectGetters(ctx context.Context) error {
	return p.b.RebuildView().visit(ctx, []byte{familyNode}, func(_ *pebble.Snapshot, r row) error {
		light, err := checkedRebuildLightNode(r, "Store Factory")
		if err != nil || !rebuildStoreFactoryGetterKind(light.Kind) || light.Name == "" || light.FilePath == "" {
			return err
		}
		wanted, err := p.present(rebuildStoreFactoryKey('b', light.Name))
		if err != nil || !wanted {
			return err
		}
		n, err := decodeNode(r.value)
		if err != nil || !p.rules.Getter(n, light.Name) {
			return err
		}
		if err := rebuildStoreFactoryNodeInput(r.key, r.value); err != nil {
			return err
		}
		if err := p.put(rebuildStoreFactoryKey('g', n.ID),
			append([]byte{rebuildCapabilityWanted}, rebuildStoreFactoryKey('v', n.Name)...)); err != nil {
			return err
		}
		return p.addUnique(rebuildStoreFactoryKey('t', n.Name), n.FilePath)
	})
}

func rebuildStoreFactoryNodeInput(key, val []byte) error {
	if len(key)+len(val) > rebuildStoreFactoryRowBytes {
		return fmt.Errorf("%w: Store Factory node row", ErrRebuildLinkInputSize)
	}
	return nil
}

func (p *rebuildStoreFactoryPass) node(id string) (*graph.Node, error) {
	key := pendingNodeKey(id)
	return pendingGet(p.b.pending, key, func(val []byte) (*graph.Node, error) {
		if err := rebuildStoreFactoryNodeInput(key, val); err != nil {
			return nil, err
		}
		return checkedRebuildLightNode(row{key, val}, "Store Factory")
	})
}

type rebuildStoreFactoryElection struct {
	rebuildStoreCandidatePick
	caller      *graph.Node
	seen        bool // an action cohort exists before workspace filtering
	getter      bool // the filtered cohort needs the getter/fallback decision
	getterProof bool // the getter file can change the decision
}

func (p *rebuildStoreFactoryPass) elect(from, binding, member string, resolveGetter bool) (pick rebuildStoreFactoryElection, err error) {
	scoped := rebuildStoreFactoryKey('a', binding, member)
	id, seen, err := p.unique(scoped)
	pick.seen = seen
	if err != nil {
		return pick, err
	}
	if !seen {
		pick.buckets = []rebuildStoreCandidateWitness{{scoped, 0}}
		return pick, nil
	}
	pick.caller, err = p.node(from)
	if err != nil {
		return pick, err
	}
	var callerFile string
	if pick.caller != nil {
		scoped = rebuildStoreFactoryKey('w', binding, member, pick.caller.WorkspaceID)
		id, seen, err = p.unique(scoped)
		if err != nil {
			return pick, err
		}
		callerFile = pick.caller.FilePath
	}
	if !resolveGetter && seen && id == "" {
		pick.getter = true
		return pick, nil
	}
	var getterFile string
	pick.rebuildStoreCandidatePick, err = pickRebuildStoreCandidate(p.unique, scoped, id, seen, func() (string, error) {
		pick.getter = true
		file, _, err := p.unique(rebuildStoreFactoryKey('t', binding))
		getterFile = file
		return file, err
	}, callerFile, func(file string) []byte {
		if pick.caller != nil {
			return rebuildStoreFactoryKey('s', binding, member, pick.caller.WorkspaceID, file)
		}
		return rebuildStoreFactoryKey('f', binding, member, file)
	})
	pick.getterProof = pick.getter && (getterFile == "" || pick.getterWinner)
	return pick, err
}

func (p *rebuildStoreFactoryPass) plan(ctx context.Context) error {
	deferred := false
	if err := p.b.RebuildView().visit(ctx, []byte{familyEdge, edgeOut}, func(snap *pebble.Snapshot, r row) error {
		kind, err := rebuildAttributionEdgeKind(r.key)
		if err != nil || kind != graph.EdgeCalls {
			return err
		}
		e, err := p.decodeKeyedEdge(r.key, r.value)
		if err != nil {
			return err
		}
		binding, member := p.rules.Stub(e)
		if binding == "" || member == "" {
			return nil
		}
		if err := p.input(r.key, r.value); err != nil {
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
		pick, err := p.elect(e.From, binding, member, false)
		if err != nil {
			return err
		}
		if !pick.getter {
			return p.planCall(r.key, e, binding, member, pick)
		}
		// Only a many-candidate scoped election consults the getter.
		deferred = true
		if err := p.put(rebuildStoreFactoryKey('b', binding), nil); err != nil {
			return err
		}
		return p.put(append(rebuildStoreFactoryKey('l'), r.key...), nil)
	}); err != nil || !deferred {
		return err
	}
	if err := p.collectGetters(ctx); err != nil {
		return err
	}
	prefix := rebuildStoreFactoryKey('l')
	return pendingVisit(ctx, p.b.pending, prefix, func(r row) error {
		old := r.key[len(prefix):]
		e, err := p.edge(old)
		if err != nil {
			return err
		}
		if e == nil {
			return invalid("Store Factory deferred Call missing")
		}
		binding, member := p.rules.Stub(e)
		pick, err := p.elect(e.From, binding, member, true)
		if err != nil {
			return err
		}
		return p.planCall(old, e, binding, member, pick)
	})
}

func (p *rebuildStoreFactoryPass) planCall(old []byte, e *graph.Edge, binding, member string, pick rebuildStoreFactoryElection) error {
	if pick.target != "" {
		p.stats.Resolved++
	}
	if !p.rules.Rebind(e, member, pick.target, pick.target != "") {
		return nil
	}
	if pick.seen {
		if err := p.want(rebuildStoreFactoryKey('u', binding, member)); err != nil {
			return err
		}
	}
	if !pick.nonempty {
		p.needsAnchor = true
	}
	if pick.getter {
		if err := p.want(rebuildStoreFactoryKey('v', binding)); err != nil {
			return err
		}
	}
	if pick.caller != nil {
		if err := p.want(rebuildStoreFactoryKey('n', pick.caller.ID)); err != nil {
			return err
		}
	}
	if err := p.wantCandidateBuckets(pick.buckets); err != nil {
		return err
	}
	if err := p.stage(old, e); err != nil {
		return err
	}
	p.stats.Moves++
	return nil
}

func (p *rebuildStoreFactoryPass) want(key []byte) error {
	return p.put(key, []byte{rebuildCapabilityWanted})
}

func (p *rebuildStoreFactoryPass) claim(raw, old []byte) error {
	if raw == nil {
		raw = old
	}
	key := append(rebuildStoreFactoryKey('r'), raw...)
	claim, err := p.marked(key)
	if err != nil {
		return err
	}
	if claim != nil {
		return p.put(key, []byte{rebuildCapabilityWanted | rebuildCapabilityUnproved})
	}
	return p.put(key, append([]byte{rebuildCapabilityWanted}, old...))
}

func (p *rebuildStoreFactoryPass) prove(ctx context.Context, receipts iter.Seq2[ParseFacts, error]) error {
	for facts, err := range receipts {
		if err != nil {
			return err
		}
		if err := p.record(ctx, facts); err != nil {
			return err
		}
	}
	anchor, err := p.proveActions(ctx)
	if err != nil {
		return err
	}
	if err := p.foldGetterFiles(ctx, rebuildStoreFactoryKey('g'), p.node, p.addUnique); err != nil {
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

func (p *rebuildStoreFactoryPass) record(ctx context.Context, facts ParseFacts) error {
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
		key := append(rebuildStoreFactoryKey('r'), pendingEdgeKey(raw)...)
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
			return invalid("Store Factory recorder has no old call")
		}
		binding, member := p.rules.Stub(raw)
		liveBinding, liveMember := p.rules.Stub(live)
		ok := lang != "" && raw.FilePath == file && binding == liveBinding && member == liveMember
		if err := p.noteReceipt(key, claim, ok); err != nil {
			return err
		}
	}
	return nil
}

func (p *rebuildStoreFactoryPass) recordNode(raw *graph.Node, file, lang string) error {
	key := rebuildStoreFactoryKey('n', raw.ID)
	state, err := p.state(key)
	if err != nil {
		return err
	}
	if state != 0 {
		live, err := p.node(raw.ID)
		if err != nil {
			return err
		}
		ok := rebuildStoreCallerReceipt(raw, live, file, lang)
		if err := noteRebuildScratchState(p.put, key, state, rebuildScratchProofBit(ok)); err != nil {
			return err
		}
	}
	for _, col := range [...]byte{'c', 'g'} {
		if err := p.recordCandidate(col, raw, file, lang); err != nil {
			return err
		}
	}
	return nil
}

func (p *rebuildStoreFactoryPass) recordCandidate(col byte, raw *graph.Node, file, lang string) error {
	key := rebuildStoreFactoryKey(col, raw.ID)
	claim, err := p.marked(key)
	if err != nil || len(claim) == 0 {
		return err
	}
	var state byte
	if col != 'c' || !p.needsAnchor {
		state, err = p.state(claim[1:])
		if err != nil || state == 0 {
			return err
		}
	}
	live, err := p.node(raw.ID)
	if err != nil {
		return err
	}
	receipt := rebuildStoreNodeReceipt(raw, live, file, lang)
	var proof byte
	if col == 'c' {
		binding, member := p.rules.Action(raw)
		eligible := raw.Kind == graph.KindFunction && binding != ""
		cohort := rebuildStoreFactoryKey('u', binding, member)
		sameRole := eligible && bytes.Equal(claim[1:], cohort)
		if receipt && eligible && (!sameRole || raw.WorkspaceID != live.WorkspaceID || raw.FilePath != live.FilePath) {
			if err := p.noteCandidateCohortChange(claim[1:], cohort, len(p.prefix), 'a', raw, live); err != nil {
				return err
			}
		}
		proof = rebuildStoreCandidateProof(receipt && sameRole, raw, live)
		if p.needsAnchor {
			proof |= rebuildStoreAnchorProof(receipt && eligible)
		}
	} else {
		sameGetter := rebuildStoreFactoryGetterKind(raw.Kind) && p.rules.Getter(raw, raw.Name) && bytes.Equal(claim[1:], rebuildStoreFactoryKey('v', raw.Name))
		if sameGetter {
			if err := p.noteGetterFileChange(claim[1:], state, raw, live, file, lang); err != nil {
				return err
			}
		}
		proof = rebuildScratchProofBit(receipt && sameGetter && raw.FilePath == live.FilePath)
	}
	return p.noteStoreNodeProof(key, claim, proof)
}

func (p *rebuildStoreFactoryPass) proveActions(ctx context.Context) (anchor bool, err error) {
	prefix := rebuildStoreFactoryKey('c')
	err = pendingVisit(ctx, p.b.pending, prefix, func(r row) error {
		if len(r.value) <= len(p.prefix)+1 {
			return invalid("Store Factory candidate proof")
		}
		anchor = anchor || rebuildStoreAnchorExact(r.value[0])
		if !rebuildStoreCandidateExact(r.value[0]) {
			return nil
		}
		state, err := p.state(r.value[1:])
		if err != nil || state == 0 {
			return err // no deciding buckets in this cohort
		}
		return p.foldExactCandidate(r, prefix, len(p.prefix), 'a', p.node)
	})
	return anchor, err
}

func (p *rebuildStoreFactoryPass) proved(old []byte, e *graph.Edge, anchor bool) (bool, error) {
	raw, err := rebuildReceiptKey(p.b.pending, old)
	if errors.Is(err, ErrRebuildUnknownProvenance) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	claim, err := p.marked(append(rebuildStoreFactoryKey('r'), raw...))
	if err != nil || len(claim) <= 1 || claim[0] != rebuildScratchExact || !bytes.Equal(claim[1:], old) {
		return false, err
	}
	binding, member := p.rules.Stub(e)
	pick, err := p.elect(e.From, binding, member, true)
	if err != nil {
		return false, err
	}
	if !pick.nonempty && !anchor {
		// An empty scoped cohort has no candidate witness of its own.
		return false, nil
	}
	if ok, err := p.candidateBucketsProved(pick.buckets); err != nil || !ok {
		return false, err
	}
	if pick.getter {
		wanted := rebuildStoreFactoryKey('v', binding)
		var ok bool
		if pick.getterProof {
			ok, err = p.getterFilesProved(rebuildStoreFactoryKey('t', binding), wanted, p.unique)
		} else {
			ok, err = p.getterFileUnchanged(wanted)
		}
		if err != nil || !ok {
			return false, err
		}
	}
	if pick.caller != nil {
		state, err := p.state(rebuildStoreFactoryKey('n', pick.caller.ID))
		return state == rebuildScratchExact, err
	}
	return true, nil
}
