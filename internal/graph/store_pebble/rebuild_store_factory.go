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
			b: b, prefix: rebuildStoreFactoryPrefix, rowBytes: rebuildStoreFactoryRowBytes, label: "Store Factory", edgeRow: "edge row",
		},
		rules: rules,
	}
	haveActions, err := p.collect(ctx)
	if err != nil || !haveActions {
		return stats, err
	}
	if err := p.collectGetters(ctx); err != nil {
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

// b: binding has an action; c/g: action/getter proof with affected key by ID.
// a/w: global/workspace action singleton; f/s: their file singletons.
// t: getter file singleton by binding. u/v: affected action/getter proof.
// n: affected caller proof; r: raw-key claims;
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
		if err := p.put(rebuildStoreFactoryKey('b', binding), nil); err != nil {
			return err
		}
		if err := p.put(rebuildStoreFactoryKey('c', n.ID),
			append([]byte{rebuildCapabilityWanted}, rebuildStoreFactoryKey('u', binding, member)...)); err != nil {
			return err
		}
		for _, key := range [][]byte{
			rebuildStoreFactoryKey('a', binding, member),
			rebuildStoreFactoryKey('w', binding, member, n.WorkspaceID),
			rebuildStoreFactoryKey('f', binding, member, n.FilePath),
			rebuildStoreFactoryKey('s', binding, member, n.WorkspaceID, n.FilePath),
		} {
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
	target string
	caller *graph.Node
	seen   bool // an action cohort exists before workspace filtering
	getter bool // the filtered cohort needs the getter/fallback decision
}

func (p *rebuildStoreFactoryPass) elect(from, binding, member string) (pick rebuildStoreFactoryElection, err error) {
	pick.target, pick.seen, err = p.unique(rebuildStoreFactoryKey('a', binding, member))
	if err != nil || !pick.seen {
		return pick, err
	}
	pick.caller, err = p.node(from)
	if err != nil {
		return pick, err
	}
	seen := pick.seen
	if pick.caller != nil {
		pick.target, seen, err = p.unique(rebuildStoreFactoryKey('w', binding, member, pick.caller.WorkspaceID))
	}
	if err != nil || !seen || pick.target != "" {
		return pick, err
	}
	pick.getter = true
	file, _, err := p.unique(rebuildStoreFactoryKey('t', binding))
	if err != nil {
		return pick, err
	}
	if file != "" {
		pick.target, err = p.actionInFile(binding, member, file, pick.caller)
		if err != nil || pick.target != "" {
			return pick, err
		}
	}
	if pick.caller != nil && pick.caller.FilePath != "" {
		pick.target, err = p.actionInFile(binding, member, pick.caller.FilePath, pick.caller)
	}
	return pick, err
}

func (p *rebuildStoreFactoryPass) actionInFile(binding, member, file string, caller *graph.Node) (string, error) {
	key := rebuildStoreFactoryKey('f', binding, member, file)
	if caller != nil {
		key = rebuildStoreFactoryKey('s', binding, member, caller.WorkspaceID, file)
	}
	id, _, err := p.unique(key)
	return id, err
}

func (p *rebuildStoreFactoryPass) plan(ctx context.Context) error {
	return p.b.RebuildView().visit(ctx, []byte{familyEdge, edgeOut}, func(snap *pebble.Snapshot, r row) error {
		kind, err := rebuildAttributionEdgeKind(r.key)
		if err != nil || kind != graph.EdgeCalls {
			return err
		}
		e, err := p.decodeEdge(r.key, r.value)
		if err != nil {
			return err
		}
		binding, member := p.rules.Stub(e)
		if binding == "" || member == "" {
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
		pick, err := p.elect(e.From, binding, member)
		if err != nil {
			return err
		}
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
		} else {
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
		if err := p.stage(r.key, e); err != nil {
			return err
		}
		p.stats.Moves++
		return nil
	})
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
	anchor, err := p.foldProof(ctx, 'c')
	if err != nil {
		return err
	}
	if _, err := p.foldProof(ctx, 'g'); err != nil {
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
		source := lang == "javascript" || lang == "typescript"
		ok := source && raw.FilePath == file && binding == liveBinding && member == liveMember
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
		ok := rebuildStoreFactorySameNode(raw, live, file, lang) && raw.WorkspaceID == live.WorkspaceID
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

func rebuildStoreFactorySameNode(raw, live *graph.Node, file, lang string) bool {
	return live != nil && lang != "" && raw.FilePath == file && live.FilePath == file &&
		raw.Language == live.Language && raw.Kind == live.Kind
}

func (p *rebuildStoreFactoryPass) recordCandidate(col byte, raw *graph.Node, file, lang string) error {
	key := rebuildStoreFactoryKey(col, raw.ID)
	claim, err := p.marked(key)
	if err != nil || len(claim) == 0 {
		return err
	}
	if col != 'c' || !p.needsAnchor {
		state, err := p.state(claim[1:])
		if err != nil || state == 0 {
			return err
		}
	}
	live, err := p.node(raw.ID)
	if err != nil {
		return err
	}
	ok := rebuildStoreFactorySameNode(raw, live, file, lang)
	if ok && col == 'c' {
		binding, member := p.rules.Action(raw)
		ok = raw.WorkspaceID == live.WorkspaceID && bytes.Equal(claim[1:], rebuildStoreFactoryKey('u', binding, member))
	} else if ok {
		ok = p.rules.Getter(raw, raw.Name) && bytes.Equal(claim[1:], rebuildStoreFactoryKey('v', raw.Name))
	}
	return noteRebuildScratchPayload(p.put, key, claim, rebuildScratchProofBit(ok))
}

func (p *rebuildStoreFactoryPass) foldProof(ctx context.Context, col byte) (exact bool, err error) {
	err = pendingVisit(ctx, p.b.pending, rebuildStoreFactoryKey(col), func(r row) error {
		if len(r.value) < 2 {
			return invalid("Store Factory candidate proof")
		}
		if r.value[0] == rebuildScratchExact {
			exact = true
			return nil
		}
		key := r.value[1:]
		state, err := p.state(key)
		if err != nil || state == 0 {
			return err
		}
		return noteRebuildScratchState(p.put, key, state, rebuildCapabilityUnproved)
	})
	return exact, err
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
	cohort, err := p.state(rebuildStoreFactoryKey('u', binding, member))
	if err != nil || cohort == 0 {
		// An empty cohort can restub only if an exact action establishes
		// that native would not skip the Call scan entirely.
		return anchor && err == nil, err
	}
	if cohort != rebuildCapabilityWanted {
		return false, nil
	}
	pick, err := p.elect(e.From, binding, member)
	if err != nil {
		return false, err
	}
	if pick.getter {
		state, err := p.state(rebuildStoreFactoryKey('v', binding))
		if err != nil || state != rebuildCapabilityWanted {
			return false, err
		}
	}
	if pick.caller != nil {
		state, err := p.state(rebuildStoreFactoryKey('n', pick.caller.ID))
		return state == rebuildScratchExact, err
	}
	return true, nil
}
