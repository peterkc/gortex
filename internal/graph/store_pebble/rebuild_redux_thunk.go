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
			b: b, prefix: prefix, rowBytes: rebuildReduxThunkRowBytes, label: "Redux Thunk", inputRow: "edge row",
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
// names, l deferred Calls, n caller scope proof, p election witnesses, x exact getter files.
// The move helper uses r/m/o.
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
		if err != nil || (light.Kind != graph.KindMethod && !rebuildStoreFactoryGetterKind(light.Kind)) {
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
	cohort := p.poolKey(pool, 'u', name)
	if err := p.put(p.poolKey(pool, 'c', n.ID), append([]byte{rebuildCapabilityWanted}, cohort...)); err != nil {
		return err
	}
	for _, key := range rebuildStoreScopeKeys(cohort, len(p.prefix)+1, 'i', n) {
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

type rebuildReduxThunkPick struct {
	target         string
	inputs         [][]byte                       // consulted pool markers and caller scopes
	buckets        []rebuildStoreCandidateWitness // deciding candidate buckets
	getter         bool                           // a pool consulted the getter
	getterProof    bool                           // the getter file can change a pool's decision
	getterWinner   bool                           // a getter selected a different winner from file fallback
	bindingMatters bool                           // a consulted pool can use a binding
	callerFile     bool                           // a pool can fall back to the caller file
}

func (p *rebuildReduxThunkPass) elect(e *graph.Edge, callee, binding string, resolveGetter bool) (pick rebuildReduxThunkPick, err error) {
	var caller *graph.Node
	var haveCaller bool
	var getterFile string
	for _, pool := range [...]byte{'t', 'a'} {
		scoped := p.poolKey(pool, 'i', callee)
		id, seen, err := p.unique(scoped)
		if err != nil {
			return pick, err
		}
		pick.inputs = append(pick.inputs, p.poolKey(pool, 'u', callee))
		if !seen {
			pick.buckets = append(pick.buckets, rebuildStoreCandidateWitness{scoped, 0})
			continue
		}
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
		var callerFile string
		if caller != nil {
			scoped = p.poolKey(pool, 'w', callee, caller.WorkspaceID)
			id, seen, err = p.unique(scoped)
			if err != nil {
				return pick, err
			}
			callerFile = caller.FilePath
		}
		if seen && id == "" {
			pick.bindingMatters = true
		}
		if !resolveGetter && binding != "" && seen && id == "" {
			pick.getter = true
			return pick, nil
		}
		chosen, err := pickRebuildStoreCandidate(p.unique, scoped, id, seen, func() (string, error) {
			if binding != "" && !pick.getter {
				file, _, err := p.unique(p.poolKey('g', 'i', binding))
				if err != nil {
					return "", err
				}
				getterFile, pick.getter = file, true
			}
			return getterFile, nil
		}, callerFile, func(file string) []byte {
			if caller != nil {
				return p.poolKey(pool, 's', callee, caller.WorkspaceID, file)
			}
			return p.poolKey(pool, 'f', callee, file)
		})
		if err != nil {
			return pick, err
		}
		if seen && id == "" {
			if binding != "" {
				pick.getterProof = pick.getterProof || getterFile == "" || chosen.getterWinner
				pick.getterWinner = pick.getterWinner || chosen.getterWinner
			}
			// A distinct winning getter has its own proof and masks caller
			// file. Without it, even an empty caller file can affect fallback.
			pick.callerFile = pick.callerFile || caller != nil && !chosen.getterWinner
		}
		pick.buckets = append(pick.buckets, chosen.buckets...)
		if chosen.target != "" {
			pick.target = chosen.target
			return pick, nil
		}
	}
	return pick, nil
}

func (p *rebuildReduxThunkPass) plan(ctx context.Context) error {
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
		callee := p.rules.Stub(e)
		if callee == "" {
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
		// Valid stationary rows compete for the raw claim too. A stale
		// stamp cannot claim a receipt and cannot prove a move.
		if err == nil && custody.proof == 0 {
			if err := p.claim(custody.raw, r.key); err != nil {
				return err
			}
		}
		binding, _ := e.Meta["store_binding"].(string)
		pick, err := p.elect(e, callee, binding, false)
		if err != nil {
			return err
		}
		if !pick.getter {
			return p.planCall(r.key, e, callee, binding, pick)
		}
		// Only a many-candidate pool with a binding consults the getter.
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
		callee := p.rules.Stub(e)
		pick, err := p.elect(e, callee, binding, true)
		if err != nil {
			return err
		}
		return p.planCall(old, e, callee, binding, pick)
	})
}

func (p *rebuildReduxThunkPass) planCall(old []byte, e *graph.Edge, callee, binding string, pick rebuildReduxThunkPick) error {
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
	if pick.getter {
		if err := p.put(p.poolKey('g', 'u', binding), []byte{rebuildCapabilityWanted}); err != nil {
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

func (p *rebuildReduxThunkPass) state(key []byte) (byte, error) {
	return rebuildScratchState(p.b.pending, key, "Redux Thunk receipt proof")
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
	for _, pool := range [...]byte{'t', 'a'} {
		prefix := p.poolKey(pool, 'c')
		if err := pendingVisit(ctx, p.b.pending, prefix, func(r row) error {
			if len(r.value) <= len(p.prefix)+2 {
				return invalid("Redux Thunk candidate proof")
			}
			anchor = anchor || pool == 't' && rebuildStoreAnchorExact(r.value[0])
			if !rebuildStoreCandidateExact(r.value[0]) {
				return nil
			}
			state, err := p.state(r.value[1:])
			if err != nil || state == 0 {
				return err
			}
			return p.foldExactCandidate(r, prefix, len(p.prefix)+1, 'i', p.node)
		}); err != nil {
			return err
		}
	}
	if err := p.foldGetterFiles(ctx, p.poolKey('g', 'c'), p.node, p.addUnique); err != nil {
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
		callee := p.rules.Stub(live)
		ok := lang != "" && raw.FilePath == file && p.rules.Stub(raw) == callee
		if ok {
			rawBinding, _ := raw.Meta["store_binding"].(string)
			liveBinding, _ := live.Meta["store_binding"].(string)
			if rawBinding != liveBinding {
				pick, err := p.elect(live, callee, liveBinding, true)
				if err != nil {
					return err
				}
				// Empty raw binding takes the same proven caller-file fallback
				// unless a distinct getter winner overrides it. Other bindings
				// can name an unconsulted getter when a pool is ambiguous.
				ok = !pick.bindingMatters || rawBinding == "" && !pick.getterWinner
			}
		}
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
		valid := rebuildStoreNodeReceipt(raw, live, file, lang)
		proof := rebuildScratchProofBit(valid && raw.WorkspaceID == live.WorkspaceID)
		if valid && raw.FilePath != live.FilePath {
			proof |= rebuildStoreOtherFile
		}
		if proof&^state != 0 {
			if err := p.put(key, []byte{state | proof}); err != nil {
				return err
			}
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
	if pool == 'g' {
		var wanted, files []byte
		if rebuildStoreFactoryGetterKind(raw.Kind) && p.rules.Getter(raw, raw.Name) {
			wanted, files = p.poolKey(pool, 'u', raw.Name), p.poolKey(pool, 'i', raw.Name)
		}
		return p.recordStoreGetter(key, wanted, files, raw, file, lang)
	}
	var name string
	if raw.Kind == graph.KindMethod || rebuildStoreFactoryGetterKind(raw.Kind) {
		if pool == 't' {
			name = p.rules.Thunk(raw)
		} else if binding, member := p.rules.Action(raw); binding != "" {
			name = member
		}
	}
	var cohort []byte
	if name != "" {
		cohort = p.poolKey(pool, 'u', name)
	}
	// One live eligible thunk proves native reaches the Call scan, even
	// if that thunk's cohort does not take part in the election.
	return p.recordStoreCandidate(key, cohort, len(p.prefix)+1, 'i', pool == 't', raw, file, lang)
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
	pick, err := p.elect(e, p.rules.Stub(e), binding, true)
	if err != nil {
		return false, err
	}
	for _, key := range pick.inputs {
		state, err := p.state(key)
		want := byte(rebuildCapabilityWanted)
		if key[len(p.prefix)] == 'n' {
			want = rebuildScratchExact
			if !pick.callerFile {
				state &^= rebuildStoreOtherFile
			}
		}
		if err != nil || state != want {
			return false, err
		}
	}
	if pick.getter {
		wanted := p.poolKey('g', 'u', binding)
		var ok bool
		if pick.getterProof {
			ok, err = p.getterFilesProved(p.poolKey('g', 'i', binding), wanted, p.unique)
		} else {
			ok, err = p.getterFileUnchanged(wanted)
		}
		if err != nil || !ok {
			return false, err
		}
	}
	return p.candidateBucketsProved(pick.buckets)
}
