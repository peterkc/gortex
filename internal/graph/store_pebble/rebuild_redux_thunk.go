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

var ErrRebuildReduxThunk = errors.New("pebble rebuild Redux Thunk stage failed")

var rebuildReduxThunkPrefix = []byte{'g', 'N'}

const rebuildReduxThunkRowBytes = 16 << 20

// RebuildReduxThunkStats describes the live census and planned moves.
type RebuildReduxThunkStats struct {
	Stubs, Resolved, Moves uint64
}

// RebuildReduxThunkRules supplies the native marker and payload decisions.
type RebuildReduxThunkRules struct {
	Thunk  func(*graph.Node) string
	Action func(*graph.Node) (string, string)
	Stub   func(*graph.Edge) string
	Getter func(*graph.Node, string) bool
	Rebind func(*graph.Edge, string, string, bool) bool
}

type rebuildReduxThunkPass struct {
	rebuildCallMoves
	rules RebuildReduxThunkRules
	stats RebuildReduxThunkStats
}

// RebuildReduxThunk runs on the quiet private graph after Store Factory.
// The worker must discard the build if applying the moves fails.
func (b *BuildStore) RebuildReduxThunk(ctx context.Context, receipts iter.Seq2[ParseFacts, error],
	rules RebuildReduxThunkRules,
) (stats RebuildReduxThunkStats, err error) {
	defer func() {
		if err != nil {
			stats = RebuildReduxThunkStats{}
			err = fmt.Errorf("%w: %w", ErrRebuildReduxThunk, err)
		}
	}()
	if receipts == nil || rules.Thunk == nil || rules.Action == nil || rules.Stub == nil || rules.Getter == nil || rules.Rebind == nil {
		return stats, fmt.Errorf("%w: invalid Redux Thunk inputs", ErrRebuildLinkInputSize)
	}
	prefix := rebuildReduxThunkPrefix
	occupied, err := rebuildGuardHasPrefix(ctx, b.pending, prefix)
	if err != nil {
		return stats, err
	}
	if occupied {
		return stats, invalid("Redux Thunk scratch range occupied")
	}
	defer func() {
		err = errors.Join(err, b.pending.DeleteRange(prefix, prefixEnd(prefix), pebble.NoSync))
	}()
	p := rebuildReduxThunkPass{
		rebuildCallMoves: rebuildCallMoves{
			b: b, prefix: prefix, rowBytes: rebuildReduxThunkRowBytes, label: "Redux Thunk", edgeRow: "edge row",
		},
		rules: rules,
	}
	haveThunks, err := p.collect(ctx)
	if err != nil || !haveThunks {
		return stats, err
	}
	if err := p.plan(ctx); err != nil || p.stats.Moves == 0 {
		return p.stats, err
	}
	if err := p.prove(ctx, receipts); err != nil {
		return stats, err
	}
	// A destination that stays occupied wins by key, regardless of its payload.
	err = b.withWrites(func() error { return p.apply(ctx, p.stats.Moves) })
	return p.stats, err
}

// Pool keys start with t (thunks), a (actions), or g (getters), then
// a column: c candidate by ID, u cohort proof, i/w global/workspace
// singleton, f/s their file singletons. Other columns: b wanted getter
// names, l deferred Calls, n caller proof. The move helper uses r/m/o.
func (p *rebuildReduxThunkPass) poolKey(pool, col byte, fields ...string) []byte {
	return keyStrings(append(p.column(pool), col), fields...)
}

func (p *rebuildReduxThunkPass) nodeInput(key, val []byte) error {
	if len(key)+len(val) > p.rowBytes {
		return fmt.Errorf("%w: Redux Thunk node row", ErrRebuildLinkInputSize)
	}
	return nil
}

func (p *rebuildReduxThunkPass) node(id string) (*graph.Node, error) {
	key := pendingNodeKey(id)
	return pendingGet(p.b.pending, key, func(val []byte) (*graph.Node, error) {
		if err := p.nodeInput(key, val); err != nil {
			return nil, err
		}
		return checkedRebuildLightNode(row{key, val}, p.label)
	})
}

func (p *rebuildReduxThunkPass) collect(ctx context.Context) (bool, error) {
	haveThunks := false
	err := p.b.RebuildView().visit(ctx, []byte{familyNode}, func(_ *pebble.Snapshot, r row) error {
		light, err := checkedRebuildLightNode(r, p.label)
		if err != nil || !(light.Kind == graph.KindMethod || rebuildStoreFactoryGetterKind(light.Kind)) {
			return err
		}
		n, err := decodeNode(r.value)
		if err != nil {
			return err
		}
		thunk := p.rules.Thunk(n)
		binding, member := p.rules.Action(n)
		if thunk == "" && (binding == "" || member == "") {
			return nil
		}
		if err := p.nodeInput(r.key, r.value); err != nil {
			return err
		}
		if n.ID == "" {
			return invalid("Redux Thunk candidate has no ID")
		}
		if thunk != "" {
			haveThunks = true
			if err := p.candidate('t', thunk, n); err != nil {
				return err
			}
		}
		if binding != "" && member != "" {
			return p.candidate('a', member, n)
		}
		return nil
	})
	return haveThunks, err
}

func (p *rebuildReduxThunkPass) candidate(pool byte, name string, n *graph.Node) error {
	if err := p.put(p.poolKey(pool, 'c', n.ID),
		append([]byte{rebuildCapabilityWanted}, p.poolKey(pool, 'u', name)...)); err != nil {
		return err
	}
	for _, key := range [][]byte{
		p.poolKey(pool, 'i', name),
		p.poolKey(pool, 'w', name, n.WorkspaceID),
		p.poolKey(pool, 'f', name, n.FilePath),
		p.poolKey(pool, 's', name, n.WorkspaceID, n.FilePath),
	} {
		if err := p.addUnique(key, n.ID); err != nil {
			return err
		}
	}
	return nil
}

func (p *rebuildReduxThunkPass) collectGetters(ctx context.Context) error {
	return p.b.RebuildView().visit(ctx, []byte{familyNode}, func(_ *pebble.Snapshot, r row) error {
		light, err := checkedRebuildLightNode(r, p.label)
		if err != nil || !rebuildStoreFactoryGetterKind(light.Kind) || light.Name == "" || light.FilePath == "" {
			return err
		}
		wanted, err := p.present(keyStrings(p.column('b'), light.Name))
		if err != nil || !wanted {
			return err
		}
		n, err := decodeNode(r.value)
		if err != nil || !p.rules.Getter(n, light.Name) {
			return err
		}
		if err := p.nodeInput(r.key, r.value); err != nil {
			return err
		}
		if err := p.put(p.poolKey('g', 'c', n.ID),
			append([]byte{rebuildCapabilityWanted}, p.poolKey('g', 'u', n.Name)...)); err != nil {
			return err
		}
		return p.addUnique(p.poolKey('g', 'i', n.Name), n.FilePath)
	})
}

func (p *rebuildReduxThunkPass) unique(key []byte) (value string, seen bool, err error) {
	state, err := pendingGet(p.b.pending, key, func(val []byte) (string, error) {
		if len(val) == 1 && val[0] == 0 || len(val) > 1 && val[0] == 1 {
			return string(val), nil
		}
		return "", invalid("Redux Thunk election row")
	})
	if len(state) > 0 && state[0] == 1 {
		return state[1:], true, err
	}
	return "", state != "", err
}

// A singleton contains byte one and its value; zero means ambiguous.
// Getter declarations in the same file share a value.
func (p *rebuildReduxThunkPass) addUnique(key []byte, value string) error {
	old, seen, err := p.unique(key)
	if err != nil || seen && (old == "" || old == value) {
		return err
	}
	if seen {
		return p.put(key, []byte{0})
	}
	return p.put(key, append([]byte{1}, value...))
}

type rebuildReduxThunkPick struct {
	target string
	inputs [][]byte // only the live cohorts, caller and getter used by election
}

func (p *rebuildReduxThunkPass) elect(e *graph.Edge, callee, binding string) (pick rebuildReduxThunkPick, err error) {
	var caller *graph.Node
	var haveCaller, triedGetter bool
	var getterFile string
	for _, pool := range [...]byte{'t', 'a'} {
		id, seen, err := p.unique(p.poolKey(pool, 'i', callee))
		if err != nil {
			return pick, err
		}
		if !seen {
			continue
		}
		pick.inputs = append(pick.inputs, p.poolKey(pool, 'u', callee))
		if !haveCaller {
			caller, err = p.node(e.From)
			if err != nil {
				return pick, err
			}
			if caller != nil {
				pick.inputs = append(pick.inputs, keyStrings(p.column('n'), caller.ID))
			}
			haveCaller = true
		}
		if caller != nil {
			id, seen, err = p.unique(p.poolKey(pool, 'w', callee, caller.WorkspaceID))
			if err != nil {
				return pick, err
			}
		}
		if !seen {
			continue
		}
		if id != "" {
			pick.target = id
			return pick, nil
		}
		if binding != "" && !triedGetter {
			getterFile, _, err = p.unique(p.poolKey('g', 'i', binding))
			if err != nil {
				return pick, err
			}
			pick.inputs = append(pick.inputs, p.poolKey('g', 'u', binding))
			triedGetter = true
		}
		pick.target, err = p.inFile(pool, callee, getterFile, caller)
		if err != nil || pick.target != "" {
			return pick, err
		}
		if caller != nil {
			pick.target, err = p.inFile(pool, callee, caller.FilePath, caller)
			if err != nil || pick.target != "" {
				return pick, err
			}
		}
	}
	return pick, nil
}

func (p *rebuildReduxThunkPass) inFile(pool byte, callee, file string, caller *graph.Node) (string, error) {
	if file == "" {
		return "", nil
	}
	key := p.poolKey(pool, 'f', callee, file)
	if caller != nil {
		key = p.poolKey(pool, 's', callee, caller.WorkspaceID, file)
	}
	id, _, err := p.unique(key)
	return id, err
}

func (p *rebuildReduxThunkPass) plan(ctx context.Context) error {
	deferred := false
	if err := p.b.RebuildView().visit(ctx, []byte{familyEdge, edgeOut}, func(snap *pebble.Snapshot, r row) error {
		kind, err := rebuildAttributionEdgeKind(r.key)
		if err != nil || kind != graph.EdgeCalls {
			return err
		}
		e, err := p.decodeEdge(r.key, r.value)
		if err != nil {
			return err
		}
		callee := p.rules.Stub(e)
		if callee == "" {
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
		// Valid stationary rows compete for the raw claim too. A stale
		// stamp cannot claim a receipt and cannot prove a move.
		if err == nil && custody.proof == 0 {
			if err := p.claim(custody.raw, r.key); err != nil {
				return err
			}
		}
		binding, _ := e.Meta["store_binding"].(string)
		if binding == "" {
			return p.planCall(r.key, e, callee, binding)
		}
		// Extracted stubs have no binding. Only an explicit optional
		// binding needs a deferred Call key and the getter census.
		deferred = true
		if err := p.put(keyStrings(p.column('b'), binding), nil); err != nil {
			return err
		}
		return p.put(append(p.column('l'), r.key...), nil)
	}); err != nil || !deferred {
		return err
	}
	if err := p.collectGetters(ctx); err != nil {
		return err
	}
	prefix := p.column('l')
	return pendingVisit(ctx, p.b.pending, prefix, func(r row) error {
		old := r.key[len(prefix):]
		e, err := p.edge(old)
		if err != nil {
			return err
		}
		if e == nil {
			return invalid("Redux Thunk deferred Call missing")
		}
		binding, _ := e.Meta["store_binding"].(string)
		return p.planCall(old, e, p.rules.Stub(e), binding)
	})
}

func (p *rebuildReduxThunkPass) planCall(old []byte, e *graph.Edge, callee, binding string) error {
	pick, err := p.elect(e, callee, binding)
	if err != nil {
		return err
	}
	if pick.target != "" {
		p.stats.Resolved++
	}
	if !p.rules.Rebind(e, callee, pick.target, pick.target != "") {
		return nil
	}
	for _, key := range pick.inputs {
		if err := p.put(key, []byte{rebuildCapabilityWanted}); err != nil {
			return err
		}
	}
	if err := p.stage(old, e); err != nil {
		return err
	}
	p.stats.Moves++
	return nil
}

func (p *rebuildReduxThunkPass) state(key []byte) (byte, error) {
	return rebuildScratchState(p.b.pending, key, "Redux Thunk receipt proof")
}

func (p *rebuildReduxThunkPass) marked(key []byte) ([]byte, error) {
	return pendingGet(p.b.pending, key, func(val []byte) ([]byte, error) {
		if len(val) == 0 || val[0]&rebuildCapabilityWanted == 0 {
			return nil, invalid("Redux Thunk receipt claim")
		}
		return bytes.Clone(val), nil
	})
}

func (p *rebuildReduxThunkPass) claim(raw, old []byte) error {
	if raw == nil {
		raw = old
	}
	key := append(p.column('r'), raw...)
	claim, err := p.marked(key)
	if err != nil {
		return err
	}
	if claim != nil {
		return p.put(key, []byte{rebuildCapabilityWanted | rebuildCapabilityUnproved})
	}
	return p.put(key, append([]byte{rebuildCapabilityWanted}, old...))
}

func (p *rebuildReduxThunkPass) prove(ctx context.Context, receipts iter.Seq2[ParseFacts, error]) error {
	for facts, err := range receipts {
		if err != nil {
			return err
		}
		if err := p.record(ctx, facts); err != nil {
			return err
		}
	}
	anchor := false
	for _, pool := range [...]byte{'t', 'a', 'g'} {
		if err := pendingVisit(ctx, p.b.pending, p.poolKey(pool, 'c'), func(r row) error {
			if len(r.value) < 2 {
				return invalid("Redux Thunk candidate proof")
			}
			if r.value[0] == rebuildScratchExact {
				anchor = anchor || pool == 't'
				return nil
			}
			key := r.value[1:]
			state, err := p.state(key)
			if err != nil || state == 0 {
				return err
			}
			return noteRebuildScratchState(p.put, key, state, rebuildCapabilityUnproved)
		}); err != nil {
			return err
		}
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

func (p *rebuildReduxThunkPass) record(ctx context.Context, facts ParseFacts) error {
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
		key := append(p.column('r'), pendingEdgeKey(raw)...)
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
			return invalid("Redux Thunk recorder has no old Call")
		}
		rawBinding, _ := raw.Meta["store_binding"].(string)
		liveBinding, _ := live.Meta["store_binding"].(string)
		ok := lang != "" && raw.FilePath == file && p.rules.Stub(raw) == p.rules.Stub(live) && rawBinding == liveBinding
		if err := p.noteReceipt(key, claim, ok); err != nil {
			return err
		}
	}
	return nil
}

func (p *rebuildReduxThunkPass) recordNode(raw *graph.Node, file, lang string) error {
	key := keyStrings(p.column('n'), raw.ID)
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
	for _, pool := range [...]byte{'t', 'a', 'g'} {
		if err := p.recordCandidate(pool, raw, file, lang); err != nil {
			return err
		}
	}
	return nil
}

func (p *rebuildReduxThunkPass) recordCandidate(pool byte, raw *graph.Node, file, lang string) error {
	key := p.poolKey(pool, 'c', raw.ID)
	claim, err := p.marked(key)
	if err != nil || len(claim) == 0 {
		return err
	}
	// Even outside the affected cohorts, one exact thunk must prove
	// that native would not return before looking at Calls at all.
	if pool != 't' {
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
	if ok {
		switch pool {
		case 't':
			name := p.rules.Thunk(raw)
			ok = name != "" && raw.WorkspaceID == live.WorkspaceID && bytes.Equal(claim[1:], p.poolKey(pool, 'u', name))
		case 'a':
			binding, member := p.rules.Action(raw)
			ok = binding != "" && raw.WorkspaceID == live.WorkspaceID && bytes.Equal(claim[1:], p.poolKey(pool, 'u', member))
		case 'g':
			ok = p.rules.Getter(raw, raw.Name) && bytes.Equal(claim[1:], p.poolKey(pool, 'u', raw.Name))
		}
	}
	return noteRebuildScratchPayload(p.put, key, claim, rebuildScratchProofBit(ok))
}

func (p *rebuildReduxThunkPass) proved(old []byte, e *graph.Edge, anchor bool) (bool, error) {
	raw, err := rebuildReceiptKey(p.b.pending, old)
	if errors.Is(err, ErrRebuildUnknownProvenance) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	claim, err := p.marked(append(p.column('r'), raw...))
	if err != nil || len(claim) <= 1 || claim[0] != rebuildScratchExact || !bytes.Equal(claim[1:], old) || !anchor {
		return false, err
	}
	binding, _ := e.Meta["store_binding"].(string)
	pick, err := p.elect(e, p.rules.Stub(e), binding)
	if err != nil {
		return false, err
	}
	for _, key := range pick.inputs {
		state, err := p.state(key)
		want := byte(rebuildCapabilityWanted)
		if key[len(p.prefix)] == 'n' {
			want = rebuildScratchExact
		}
		if err != nil || state != want {
			return false, err
		}
	}
	return true, nil
}
