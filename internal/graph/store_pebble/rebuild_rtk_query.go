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

var ErrRebuildRTKQuery = errors.New("pebble rebuild RTK Query stage failed")

var rebuildRTKQueryPrefix = []byte{'g', 'q', 'T'}

// RebuildRTKQueryRules supplies the native marker and payload decisions.
type RebuildRTKQueryRules struct {
	Node   func(*graph.Node) (string, bool)
	Call   func(*graph.Edge) (string, bool)
	Getter func(*graph.Node, string) bool
	Rebind func(*graph.Edge, string, bool) bool
}

type RebuildRTKQueryStats struct {
	Resolved, Moves uint64
}

type rebuildRTKQueryPass struct {
	rebuildCallMoves
	rules RebuildRTKQueryRules
	stats RebuildRTKQueryStats
}

// RebuildRTKQuery runs against the quiet private view. On error the worker
// discards the build, including any earlier registry turns.
func (b *BuildStore) RebuildRTKQuery(ctx context.Context, receipts iter.Seq2[ParseFacts, error],
	rules RebuildRTKQueryRules,
) (stats RebuildRTKQueryStats, err error) {
	defer func() {
		if err != nil {
			stats = RebuildRTKQueryStats{}
			err = fmt.Errorf("%w: %w", ErrRebuildRTKQuery, err)
		}
	}()
	if receipts == nil || rules.Node == nil || rules.Call == nil || rules.Getter == nil || rules.Rebind == nil {
		return stats, fmt.Errorf("%w: invalid RTK Query inputs", ErrRebuildLinkInputSize)
	}
	prefix := rebuildRTKQueryPrefix
	occupied, err := rebuildGuardHasPrefix(ctx, b.pending, prefix)
	if err != nil {
		return stats, err
	}
	if occupied {
		return stats, invalid("RTK Query scratch range occupied")
	}
	defer func() {
		err = errors.Join(err, b.pending.DeleteRange(prefix, prefixEnd(prefix), pebble.NoSync))
	}()
	p := rebuildRTKQueryPass{
		rebuildCallMoves: rebuildCallMoves{b: b, prefix: prefix, rowBytes: 16 << 20, label: "RTK Query", inputRow: "edge row"},
		rules:            rules,
	}
	haveEndpoints, err := p.collect(ctx)
	if err != nil || !haveEndpoints {
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

func (p *rebuildRTKQueryPass) key(col byte, fields ...string) []byte {
	return keyStrings(p.column(col), fields...)
}

func (p *rebuildRTKQueryPass) get(key []byte) ([]byte, error) {
	return pendingGet(p.b.pending, key, copyValue)
}

func (p *rebuildRTKQueryPass) state(key []byte) (byte, error) {
	return rebuildScratchState(p.b.pending, key, "RTK Query receipt proof")
}

func (p *rebuildRTKQueryPass) node(id string) (*graph.Node, error) {
	key := pendingNodeKey(id)
	return pendingGet(p.b.pending, key, func(raw []byte) (*graph.Node, error) {
		if err := p.nodeInput(key, raw); err != nil {
			return nil, err
		}
		return checkedRebuildLightNode(row{key, raw}, p.label)
	})
}

func (p *rebuildRTKQueryPass) nodeInput(key, val []byte) error {
	if len(key)+len(val) > p.rowBytes {
		return fmt.Errorf("%w: %s node row", ErrRebuildLinkInputSize, p.label)
	}
	return nil
}

func (p *rebuildRTKQueryPass) unique(key []byte) (string, bool, error) {
	v, err := p.get(key)
	if len(v) == 0 || v[0] == 0 {
		return "", v != nil, err
	}
	return string(v[1:]), true, err
}

func (p *rebuildRTKQueryPass) addUnique(key []byte, id string) error {
	prev, seen, err := p.unique(key)
	if err != nil || seen && (prev == "" || prev == id) {
		return err
	}
	if seen {
		return p.put(key, []byte{0})
	}
	return p.put(key, append([]byte{1}, id...))
}

func (p *rebuildRTKQueryPass) collect(ctx context.Context) (bool, error) {
	haveEndpoints := false
	err := p.b.RebuildView().visit(ctx, []byte{familyNode}, func(_ *pebble.Snapshot, r row) error {
		light, err := checkedRebuildLightNode(r, p.label)
		if err != nil || light.ID == "" || light.Kind != graph.KindFunction {
			return err
		}
		n, err := decodeNode(r.value)
		if err != nil {
			return err
		}
		endpoint, hook := p.rules.Node(n)
		if endpoint == "" && !hook {
			return nil
		}
		if err := p.nodeInput(r.key, r.value); err != nil {
			return err
		}
		if endpoint != "" {
			haveEndpoints = true
			if err := p.put(p.key('d', n.ID), append([]byte{rebuildCapabilityWanted}, endpoint...)); err != nil {
				return err
			}
			key := p.key('e', n.FilePath, endpoint)
			prev, err := p.get(key)
			if err != nil {
				return err
			}
			// SQLite NodesByKinds explicitly sorts by ID; native keeps the last.
			if n.ID > string(prev) {
				if err := p.put(key, []byte(n.ID)); err != nil {
					return err
				}
			}
		}
		if !hook {
			return nil
		}
		cohort := p.key('u', n.Name)
		if err := p.put(p.key('c', n.ID), append([]byte{rebuildCapabilityWanted}, cohort...)); err != nil {
			return err
		}
		for _, bucket := range rebuildStoreScopeKeys(cohort, len(p.prefix), 'i', n) {
			if err := p.addUnique(bucket, n.ID); err != nil {
				return err
			}
		}
		return nil
	})
	return haveEndpoints, err
}

type rebuildRTKQueryPick struct {
	target      string
	caller      *graph.Node
	buckets     []rebuildStoreCandidateWitness
	getter      []byte // nonempty only when ambiguity consulted the binding getter
	getterProof bool   // getter file can change the pick
	ambiguous   bool   // the scoped pool must consult the binding and file
	callerFile  bool   // caller file can decide an untagged election
	file        string // file for a tagged endpoint lookup
}

func (p *rebuildRTKQueryPass) elect(e *graph.Edge, name string, tagged, resolveGetter bool) (pick rebuildRTKQueryPick, err error) {
	pick.caller, err = p.node(e.From)
	if err != nil {
		return pick, err
	}
	if pick.caller != nil {
		pick.file = pick.caller.FilePath
	}
	if tagged {
		id, err := p.get(p.key('e', pick.file, name))
		pick.target = string(id)
		return pick, err
	}
	scoped := p.key('i', name)
	if pick.caller != nil {
		scoped = p.key('w', name, pick.caller.WorkspaceID)
	}
	id, seen, err := p.unique(scoped)
	if err != nil {
		return pick, err
	}
	pick.ambiguous = seen && id == ""
	binding, _ := e.Meta["store_binding"].(string)
	if !resolveGetter && pick.ambiguous && binding != "" {
		pick.getter = p.key('v', binding)
		return pick, nil
	}
	var getterFile string
	decision, err := pickRebuildStoreCandidate(p.unique, scoped, id, seen, func() (string, error) {
		if binding == "" {
			return "", nil
		}
		pick.getter = p.key('v', binding)
		file, _, err := p.unique(p.key('t', binding))
		getterFile = file
		return file, err
	}, pick.file, func(file string) []byte {
		if pick.caller != nil {
			return p.key('s', name, pick.caller.WorkspaceID, file)
		}
		return p.key('f', name, file)
	})
	pick.target, pick.buckets = decision.target, decision.buckets
	pick.callerFile = pick.ambiguous && !decision.getterWinner
	pick.getterProof = pick.getter != nil && (getterFile == "" || decision.getterWinner)
	return pick, err
}

func (p *rebuildRTKQueryPass) plan(ctx context.Context) error {
	deferred := false
	if err := p.b.RebuildView().visit(ctx, []byte{familyEdge, edgeOut}, func(snap *pebble.Snapshot, r row) error {
		kind, err := rebuildAttributionEdgeKind(r.key)
		if err != nil || kind != graph.EdgeCalls {
			return err
		}
		// A stationary live row can also claim a raw identity. It must not
		// let a moved row obtain that identity's receipt position.
		raw, err := rebuildReceiptKey(snap, r.key)
		if err != nil && !errors.Is(err, ErrRebuildUnknownProvenance) {
			return err
		}
		if err == nil {
			key := append(p.column('r'), raw...)
			claim, err := p.get(key)
			if err != nil {
				return err
			}
			value := []byte{rebuildCapabilityWanted | rebuildCapabilityUnproved}
			if claim == nil {
				value = append([]byte{rebuildCapabilityWanted}, r.key...)
			}
			if err := p.put(key, value); err != nil {
				return err
			}
		}
		e, err := p.decodeKeyedEdge(r.key, r.value)
		if err != nil {
			return err
		}
		name, tagged := p.rules.Call(e)
		if !tagged {
			if !graph.IsUnresolvedTarget(e.To) {
				return nil
			}
			seen, err := p.present(p.key('i', name))
			if err != nil || !seen {
				return err
			}
		}
		if err := p.input(r.key, r.value); err != nil {
			return err
		}
		pick, err := p.elect(e, name, tagged, false)
		if err != nil {
			return err
		}
		if pick.getter == nil {
			return p.planCall(r.key, e, name, tagged, pick)
		}
		deferred = true
		binding, _ := e.Meta["store_binding"].(string)
		if err := p.put(p.key('b', binding), nil); err != nil {
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
			return invalid("RTK Query deferred Call missing")
		}
		name, tagged := p.rules.Call(e)
		pick, err := p.elect(e, name, tagged, true)
		if err != nil {
			return err
		}
		return p.planCall(old, e, name, tagged, pick)
	})
}

func (p *rebuildRTKQueryPass) collectGetters(ctx context.Context) error {
	return p.b.RebuildView().visit(ctx, []byte{familyNode}, func(_ *pebble.Snapshot, r row) error {
		light, err := checkedRebuildLightNode(r, p.label)
		if err != nil || !rebuildStoreFactoryGetterKind(light.Kind) || light.Name == "" || light.FilePath == "" {
			return err
		}
		wanted, err := p.present(p.key('b', light.Name))
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
		if err := p.put(p.key('g', n.ID), append([]byte{rebuildCapabilityWanted}, p.key('v', n.Name)...)); err != nil {
			return err
		}
		return p.addUnique(p.key('t', n.Name), n.FilePath)
	})
}

func (p *rebuildRTKQueryPass) planCall(old []byte, e *graph.Edge, name string, tagged bool, pick rebuildRTKQueryPick) error {
	if pick.target != "" {
		p.stats.Resolved++
	}
	if !p.rules.Rebind(e, pick.target, pick.target != "") {
		return nil
	}
	if pick.caller != nil {
		if err := p.put(p.key('n', pick.caller.ID), []byte{rebuildCapabilityWanted}); err != nil {
			return err
		}
	}
	if tagged {
		if err := p.put(p.key('q', pick.file, name), append([]byte{rebuildCapabilityWanted}, pick.target...)); err != nil {
			return err
		}
	}
	if pick.getter != nil {
		if err := p.put(pick.getter, []byte{rebuildCapabilityWanted}); err != nil {
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

func (p *rebuildRTKQueryPass) recordNode(raw *graph.Node, file, lang string) error {
	callerKey, endpointKey := p.key('n', raw.ID), p.key('d', raw.ID)
	hookKey, getterKey := p.key('c', raw.ID), p.key('g', raw.ID)
	caller, err := p.get(callerKey)
	if err != nil {
		return err
	}
	endpoint, err := p.get(endpointKey)
	if err != nil {
		return err
	}
	hook, err := p.get(hookKey)
	if err != nil {
		return err
	}
	getter, err := p.get(getterKey)
	if err != nil {
		return err
	}
	var ep string
	var generated bool
	if raw.Kind == graph.KindFunction {
		ep, generated = p.rules.Node(raw)
	}
	rawGetter := raw.Name != "" && rebuildStoreFactoryGetterKind(raw.Kind) && p.rules.Getter(raw, raw.Name)
	var getterWanted []byte
	var getterState byte
	if rawGetter {
		getterWanted = p.key('v', raw.Name)
		getterState, err = p.state(getterWanted)
		if err != nil {
			return err
		}
	}
	if caller == nil && endpoint == nil && hook == nil && getter == nil && ep == "" && !generated && getterState == 0 {
		return nil
	}
	live, err := p.node(raw.ID)
	if err != nil {
		return err
	}
	admitted := rebuildStoreNodeReceipt(raw, live, file, lang)
	if caller != nil {
		if err := p.noteStoreNodeProof(callerKey, caller, rebuildStoreCandidateProof(admitted, raw, live)); err != nil {
			return err
		}
	}
	if endpoint != nil {
		role := admitted && ep == string(endpoint[1:])
		proof := rebuildStoreCandidateProof(role, raw, live) | rebuildStoreAnchorProof(admitted && ep != "")
		if err := p.noteStoreNodeProof(endpointKey, endpoint, proof); err != nil {
			return err
		}
	}
	if admitted && ep != "" {
		key := p.key('q', raw.FilePath, ep)
		wanted, err := p.get(key)
		if err != nil {
			return err
		}
		if len(wanted) != 0 && raw.ID > string(wanted[1:]) {
			if err := noteRebuildScratchPayload(p.put, key, wanted, rebuildCapabilityUnproved); err != nil {
				return err
			}
		}
	}
	if hook != nil {
		ok := admitted && generated && raw.Name == live.Name
		if err := p.noteStoreNodeProof(hookKey, hook, rebuildStoreCandidateProof(ok, raw, live)); err != nil {
			return err
		}
	}
	if admitted && generated {
		cohort := p.key('u', raw.Name)
		if hook != nil {
			if err := p.noteCandidateCohortChange(hook[1:], cohort, len(p.prefix), 'i', raw, live); err != nil {
				return err
			}
		} else {
			for _, bucket := range rebuildStoreScopeKeys(cohort, len(p.prefix), 'i', raw) {
				key := p.candidateProofKey(bucket)
				state, err := p.state(key)
				if err != nil {
					return err
				}
				if state != 0 {
					if err := noteRebuildScratchState(p.put, key, state, rebuildStoreCandidateExtra); err != nil {
						return err
					}
				}
			}
		}
	}
	// A receipt can enter the binding even when its live node was not a getter.
	if admitted && getterState != 0 && (getter == nil || !bytes.Equal(getter[1:], getterWanted)) {
		getterFile, seen, err := p.unique(p.key('t', raw.Name))
		if err != nil {
			return err
		}
		if !seen || getterFile != "" && getterFile != raw.FilePath {
			if err := noteRebuildScratchState(p.put, getterWanted, getterState, rebuildCapabilityUnproved); err != nil {
				return err
			}
		}
	}
	if getter == nil {
		return nil
	}
	state, err := p.state(getter[1:])
	if err != nil || state == 0 {
		return err
	}
	role := rawGetter && bytes.Equal(getter[1:], getterWanted)
	if role {
		if err := p.noteGetterFileChange(getter[1:], state, raw, live, file, lang); err != nil {
			return err
		}
	}
	ok := admitted && role && raw.FilePath == live.FilePath
	return noteRebuildScratchPayload(p.put, getterKey, getter, rebuildScratchProofBit(ok))
}

func (p *rebuildRTKQueryPass) recordCall(raw *graph.Edge, file, lang string) error {
	key := append(p.column('r'), pendingEdgeKey(raw)...)
	claim, err := p.get(key)
	if err != nil || len(claim) <= 1 {
		return err
	}
	moving, err := p.present(append(p.column('m'), claim[1:]...))
	if err != nil || !moving {
		return err
	}
	live, err := p.edge(claim[1:])
	if err != nil {
		return err
	}
	if live == nil {
		return invalid("RTK Query recorder has no live Call")
	}
	name, tagged := p.rules.Call(raw)
	liveName, liveTagged := p.rules.Call(live)
	ok := lang != "" && raw.FilePath == file && name == liveName && tagged == liveTagged
	if !tagged {
		pick, err := p.elect(live, liveName, false, true)
		if err != nil {
			return err
		}
		binding, _ := raw.Meta["store_binding"].(string)
		liveBinding, _ := live.Meta["store_binding"].(string)
		ok = ok && graph.IsUnresolvedTarget(raw.To) && (!pick.ambiguous || binding == liveBinding)
	}
	return p.noteReceipt(key, claim, ok)
}

func (p *rebuildRTKQueryPass) prove(ctx context.Context, receipts iter.Seq2[ParseFacts, error]) error {
	for facts, err := range receipts {
		if err != nil {
			return err
		}
		file, lang := pathkey.Normalize(facts.File.FilePath), facts.EffectiveLanguage
		for _, n := range facts.Nodes {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := p.recordNode(n, file, lang); err != nil {
				return err
			}
		}
		for _, e := range facts.Edges {
			if err := ctx.Err(); err != nil {
				return err
			}
			if e.Kind == graph.EdgeCalls {
				if err := p.recordCall(e, file, lang); err != nil {
					return err
				}
			}
		}
	}
	anchor := false
	if err := pendingVisit(ctx, p.b.pending, p.column('d'), func(r row) error {
		anchor = anchor || rebuildStoreAnchorExact(r.value[0])
		return nil
	}); err != nil {
		return err
	}
	prefix := p.column('c')
	if err := pendingVisit(ctx, p.b.pending, prefix, func(r row) error {
		if !rebuildStoreCandidateExact(r.value[0]) {
			return nil
		}
		return p.foldExactCandidate(r, prefix, len(p.prefix), 'i', p.node)
	}); err != nil {
		return err
	}
	if err := p.foldGetterFiles(ctx, p.column('g'), p.node, p.addUnique); err != nil {
		return err
	}
	var refused rebuildScratchRefusals
	if err := p.moves(ctx, func(old []byte, next *graph.Edge) error {
		ok, err := p.proved(old, anchor)
		if err == nil && !ok {
			refused.add(graph.EdgeIdentityFor(next), pendingEdgeKey(next))
		}
		return err
	}); err != nil {
		return err
	}
	return refused.err()
}

func (p *rebuildRTKQueryPass) proved(old []byte, anchor bool) (bool, error) {
	if !anchor {
		return false, nil
	}
	raw, err := rebuildReceiptKey(p.b.pending, old)
	if errors.Is(err, ErrRebuildUnknownProvenance) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	claim, err := p.get(append(p.column('r'), raw...))
	if err != nil || len(claim) <= 1 || claim[0] != rebuildScratchExact || !bytes.Equal(claim[1:], old) {
		return false, err
	}
	// For an untagged move the old target, not the generated node ID,
	// supplies the unresolved name that native used to elect the hook.
	e, err := p.edge(old)
	if err != nil {
		return false, err
	}
	if e == nil {
		return false, invalid("RTK Query planned Call missing")
	}
	name, tagged := p.rules.Call(e)
	pick, err := p.elect(e, name, tagged, true)
	if err != nil {
		return false, err
	}
	if pick.caller != nil {
		state, err := p.state(p.key('n', pick.caller.ID))
		if err != nil || !rebuildStoreCandidateExact(state) {
			return false, err
		}
		if tagged && state&rebuildStoreOtherFile != 0 || !tagged && state&rebuildStoreOtherWorkspace != 0 {
			return false, nil
		}
		if !tagged && state&rebuildStoreOtherFile != 0 && pick.callerFile {
			return false, nil
		}
	}
	if tagged {
		wanted, err := p.get(p.key('q', pick.file, name))
		if err != nil || len(wanted) == 0 || wanted[0] != rebuildCapabilityWanted || string(wanted[1:]) != pick.target {
			return false, err
		}
		if pick.target == "" {
			return true, nil
		}
		proof, err := p.get(p.key('d', pick.target))
		return len(proof) != 0 && rebuildStoreCandidateExact(proof[0]) && proof[0]&rebuildStoreOtherFile == 0, err
	}
	if pick.getter != nil {
		var ok bool
		if pick.getterProof {
			binding, _ := e.Meta["store_binding"].(string)
			ok, err = p.getterFilesProved(p.key('t', binding), pick.getter, p.unique)
		} else {
			ok, err = p.getterFileUnchanged(pick.getter)
		}
		if err != nil || !ok {
			return false, err
		}
	}
	return p.candidateBucketsProved(pick.buckets)
}
