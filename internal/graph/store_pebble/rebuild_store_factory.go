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

// rebuildStoreDispatchCall separates the candidate cohort from the optional
// getter binding and the name used when resetting a stale target.
type rebuildStoreDispatchCall struct {
	group, member, getter, reset string
}

type rebuildStoreDispatchRules struct {
	kind   func(graph.NodeKind) bool
	action func(*graph.Node) (string, string)
	call   func(*graph.Edge) rebuildStoreDispatchCall
	getter func(*graph.Node, string) bool
	rebind func(*graph.Edge, string, string, bool) bool
}

type rebuildStoreFactoryPass struct {
	rebuildCallMoves
	rules            rebuildStoreDispatchRules
	needsAnchor      bool
	hasLargeHandlers bool // MediatR checks handler size at the deciding bucket.
	stats            RebuildStoreFactoryStats
}

// RebuildStoreFactory runs on the quiet private graph after SQL callsites.
// The worker must discard the build if applying the moves fails.
func (b *BuildStore) RebuildStoreFactory(ctx context.Context, receipts iter.Seq2[ParseFacts, error],
	rules RebuildStoreFactoryRules,
) (RebuildStoreFactoryStats, error) {
	var read func(*graph.Edge) rebuildStoreDispatchCall
	if rules.Stub != nil {
		read = func(e *graph.Edge) rebuildStoreDispatchCall {
			binding, member := rules.Stub(e)
			return rebuildStoreDispatchCall{group: binding, member: member, getter: binding, reset: member}
		}
	}
	p := rebuildStoreFactoryPass{
		rebuildCallMoves: rebuildCallMoves{
			b: b, prefix: rebuildStoreFactoryPrefix, rowBytes: rebuildStoreFactoryRowBytes, label: "Store Factory", inputRow: "edge row",
		},
		rules: rebuildStoreDispatchRules{
			kind:   func(kind graph.NodeKind) bool { return kind == graph.KindFunction },
			action: rules.Action, call: read, getter: rules.Getter, rebind: rules.Rebind,
		},
	}
	return p.run(ctx, receipts, ErrRebuildStoreFactory)
}

func (p *rebuildStoreFactoryPass) run(ctx context.Context, receipts iter.Seq2[ParseFacts, error], stageErr error) (stats RebuildStoreFactoryStats, err error) {
	defer func() {
		if err != nil {
			stats = RebuildStoreFactoryStats{}
			err = fmt.Errorf("%w: %w", stageErr, err)
		}
	}()
	if receipts == nil || p.rules.action == nil || p.rules.call == nil || p.rules.getter == nil || p.rules.rebind == nil {
		return stats, fmt.Errorf("%w: invalid %s inputs", ErrRebuildLinkInputSize, p.label)
	}
	prefix := p.prefix
	occupied, err := rebuildGuardHasPrefix(ctx, p.b.pending, prefix)
	if err != nil {
		return stats, err
	}
	if occupied {
		return stats, invalid(p.label + " scratch range occupied")
	}
	defer func() {
		err = errors.Join(err, p.b.pending.DeleteRange(prefix, prefixEnd(prefix), pebble.NoSync))
	}()
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
	err = p.b.withWrites(func() error { return p.apply(ctx, p.stats.Moves) })
	return p.stats, err
}

// b: needed getter bindings; c/g: action/getter proof with affected key by ID.
// a/w: global/workspace action singleton; f/s: their file singletons.
// t: live getter files, x: exact getter files. u/v: wanted action/getter cohorts.
// l: deferred Calls; n: live caller proof; R: receipt-only caller scope.
// p: election witnesses; r: raw-key claims;
// m: new payload by old key; o: raw receipt key in move order.
func (p *rebuildStoreFactoryPass) key(col byte, fields ...string) []byte {
	return keyStrings(p.column(col), fields...)
}

func (p *rebuildStoreFactoryPass) state(key []byte) (byte, error) {
	return rebuildScratchState(p.b.pending, key, p.label+" receipt proof")
}

func (p *rebuildStoreFactoryPass) collect(ctx context.Context) (bool, error) {
	haveActions := false
	err := p.b.RebuildView().visit(ctx, []byte{familyNode}, func(_ *pebble.Snapshot, r row) error {
		light, err := checkedRebuildLightNode(r, p.label)
		if err != nil || !p.rules.kind(light.Kind) {
			return err
		}
		n, err := decodeNode(r.value)
		if err != nil {
			return err
		}
		binding, member := p.rules.action(n)
		if binding == "" {
			return nil
		}
		if n.ID == "" {
			return invalid(p.label + " action has no ID")
		}
		if err := p.nodeInput(r.key, r.value); err != nil {
			return err
		}
		haveActions = true
		cohort := p.key('u', binding, member)
		if err := p.put(p.key('c', n.ID), append([]byte{rebuildCapabilityWanted}, cohort...)); err != nil {
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
		light, err := checkedRebuildLightNode(r, p.label)
		if err != nil || !rebuildStoreFactoryGetterKind(light.Kind) || light.Name == "" || light.FilePath == "" {
			return err
		}
		wanted, err := p.present(p.key('b', light.Name))
		if err != nil || !wanted {
			return err
		}
		n, err := decodeNode(r.value)
		if err != nil || !p.rules.getter(n, light.Name) {
			return err
		}
		if err := p.nodeInput(r.key, r.value); err != nil {
			return err
		}
		if err := p.put(p.key('g', n.ID),
			append([]byte{rebuildCapabilityWanted}, p.key('v', n.Name)...)); err != nil {
			return err
		}
		return p.addUnique(p.key('t', n.Name), n.FilePath)
	})
}

func (p *rebuildStoreFactoryPass) nodeInput(key, val []byte) error {
	if len(key)+len(val) > p.rowBytes {
		return fmt.Errorf("%w: %s node row", ErrRebuildLinkInputSize, p.label)
	}
	return nil
}

func (p *rebuildStoreFactoryPass) node(id string) (*graph.Node, error) {
	key := pendingNodeKey(id)
	return pendingGet(p.b.pending, key, func(val []byte) (*graph.Node, error) {
		if err := p.nodeInput(key, val); err != nil {
			return nil, err
		}
		return checkedRebuildLightNode(row{key, val}, p.label)
	})
}

// Action proof reads only live ID, workspace and file. A caller or getter
// with the same ID still goes through the normal capped node lookup.
func (p *rebuildStoreFactoryPass) actionNode(id string) (*graph.Node, error) {
	if p.hasLargeHandlers {
		handler, err := p.present(p.key('c', id))
		if err != nil {
			return nil, err
		}
		if handler {
			return p.storeReceiptNode(id)
		}
	}
	return p.node(id)
}

type rebuildStoreFactoryElection struct {
	rebuildStoreCandidatePick
	caller      *graph.Node
	getter      bool // the filtered cohort needs the getter/fallback decision
	getterProof bool // the getter file can change the decision
}

func (p *rebuildStoreFactoryPass) elect(from string, call rebuildStoreDispatchCall, resolveGetter bool) (rebuildStoreFactoryElection, error) {
	return p.electWithCaller(call, resolveGetter, func() (*graph.Node, error) { return p.node(from) })
}

func (p *rebuildStoreFactoryPass) electWithCaller(call rebuildStoreDispatchCall, resolveGetter bool,
	caller func() (*graph.Node, error),
) (pick rebuildStoreFactoryElection, err error) {
	scoped := p.key('a', call.group, call.member)
	id, seen, err := p.unique(scoped)
	if err != nil {
		return pick, err
	}
	if !seen {
		pick.buckets = []rebuildStoreCandidateWitness{{scoped, 0}}
		return pick, nil
	}
	pick.caller, err = caller()
	if err != nil {
		return pick, err
	}
	var callerFile string
	if pick.caller != nil {
		scoped = p.key('w', call.group, call.member, pick.caller.WorkspaceID)
		id, seen, err = p.unique(scoped)
		if err != nil {
			return pick, err
		}
		callerFile = pick.caller.FilePath
	}
	if !resolveGetter && call.getter != "" && seen && id == "" {
		pick.getter = true
		return pick, nil
	}
	var getterFile string
	pick.rebuildStoreCandidatePick, err = pickRebuildStoreCandidate(p.unique, scoped, id, seen, func() (string, error) {
		if call.getter == "" {
			return "", nil
		}
		pick.getter = true
		file, _, err := p.unique(p.key('t', call.getter))
		getterFile = file
		return file, err
	}, callerFile, func(file string) []byte {
		if pick.caller != nil {
			return p.key('s', call.group, call.member, pick.caller.WorkspaceID, file)
		}
		return p.key('f', call.group, call.member, file)
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
		call := p.rules.call(e)
		if call.group == "" || call.member == "" {
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
		pick, err := p.elect(e.From, call, false)
		if err != nil {
			return err
		}
		if !pick.getter {
			return p.planScopedCall(r.key, e, call, pick)
		}
		// Only a many-candidate scoped election consults the getter.
		deferred = true
		if err := p.put(p.key('b', call.getter), nil); err != nil {
			return err
		}
		return p.put(append(p.key('l'), r.key...), nil)
	}); err != nil || !deferred {
		return err
	}
	if err := p.collectGetters(ctx); err != nil {
		return err
	}
	prefix := p.key('l')
	return pendingVisit(ctx, p.b.pending, prefix, func(r row) error {
		old := r.key[len(prefix):]
		e, err := p.edge(old)
		if err != nil {
			return err
		}
		if e == nil {
			return invalid(p.label + " deferred Call missing")
		}
		call := p.rules.call(e)
		pick, err := p.elect(e.From, call, true)
		if err != nil {
			return err
		}
		return p.planScopedCall(old, e, call, pick)
	})
}

func (p *rebuildStoreFactoryPass) planCall(old []byte, e *graph.Edge, call rebuildStoreDispatchCall, pick rebuildStoreFactoryElection) error {
	if pick.target != "" {
		p.stats.Resolved++
	}
	if !p.rules.rebind(e, call.reset, pick.target, pick.target != "") {
		return nil
	}
	if err := p.want(p.key('u', call.group, call.member)); err != nil {
		return err
	}
	if !pick.nonempty {
		p.needsAnchor = true
	}
	if pick.getter {
		if err := p.want(p.key('v', call.getter)); err != nil {
			return err
		}
	}
	if pick.caller != nil {
		if err := p.want(p.key('n', pick.caller.ID)); err != nil {
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

// Only moving Calls with a nonempty global cohort can change their
// election when receipts introduce an otherwise missing caller.
func (p *rebuildStoreFactoryPass) planScopedCall(old []byte, e *graph.Edge, call rebuildStoreDispatchCall, pick rebuildStoreFactoryElection) error {
	moves := p.stats.Moves
	if err := p.planCall(old, e, call, pick); err != nil {
		return err
	}
	if p.stats.Moves != moves && pick.nonempty && pick.caller == nil {
		// A receipt caller can leave an already ambiguous cohort empty.
		p.needsAnchor = p.needsAnchor || pick.target == ""
		return p.put(p.key('R', e.From), nil)
	}
	return nil
}

// Mark the deciding receipt-only caller buckets before recording candidate
// receipts. A scoped decision does not need proof of an unrelated global row.
func (p *rebuildStoreFactoryPass) prepareReceiptCallerProof(ctx context.Context, receipts iter.Seq2[ParseFacts, error]) error {
	wanted, err := rebuildGuardHasPrefix(ctx, p.b.pending, p.key('R'))
	if err != nil || !wanted {
		return err
	}
	for facts, err := range receipts {
		if err != nil {
			return err
		}
		if facts.EffectiveLanguage == "" {
			continue
		}
		file := pathkey.Normalize(facts.File.FilePath)
		for _, n := range facts.Nodes {
			if err := ctx.Err(); err != nil {
				return err
			}
			if n == nil || n.FilePath != file {
				continue
			}
			key := p.key('R', n.ID)
			wanted, err := p.present(key)
			if err != nil {
				return err
			}
			if wanted {
				// Like the native node upsert, keep the last admitted row.
				if err := p.put(key, keyStrings(nil, n.WorkspaceID, file)); err != nil {
					return err
				}
			}
		}
	}
	return p.moves(ctx, func(_ []byte, e *graph.Edge) error {
		call := p.rules.call(e)
		pick, err := p.receiptCallerPick(e.From, call)
		if err != nil || pick.caller == nil {
			return err
		}
		return p.wantCandidateBuckets(pick.buckets)
	})
}

func (p *rebuildStoreFactoryPass) receiptCallerPick(from string, call rebuildStoreDispatchCall) (rebuildStoreFactoryElection, error) {
	val, err := p.get(p.key('R', from))
	if err != nil || len(val) == 0 {
		return rebuildStoreFactoryElection{}, err
	}
	return p.electWithCaller(call, true, func() (*graph.Node, error) {
		workspace, tail, err := readKeyString(val)
		if err != nil {
			return nil, err
		}
		file, _, err := readKeyString(tail)
		if err != nil {
			return nil, err
		}
		return &graph.Node{WorkspaceID: workspace, FilePath: file}, nil
	})
}

func (p *rebuildStoreFactoryPass) prove(ctx context.Context, receipts iter.Seq2[ParseFacts, error]) error {
	if err := p.prepareReceiptCallerProof(ctx, receipts); err != nil {
		return err
	}
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
	if err := p.foldGetterFiles(ctx, p.key('g'), p.node, p.addUnique); err != nil {
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
		key := append(p.key('r'), pendingEdgeKey(raw)...)
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
			return invalid(p.label + " recorder has no old call")
		}
		call, liveCall := p.rules.call(raw), p.rules.call(live)
		ok := lang != "" && raw.FilePath == file && call == liveCall
		if err := p.noteReceipt(key, claim, ok); err != nil {
			return err
		}
	}
	return nil
}

func (p *rebuildStoreFactoryPass) recordNode(raw *graph.Node, file, lang string) error {
	key := p.key('n', raw.ID)
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
	key := p.key(col, raw.ID)
	if col == 'c' {
		// Celery records its task and registration cohorts separately.
		if p.rules.action == nil {
			return nil
		}
		var cohort []byte
		if p.rules.kind(raw.Kind) {
			if binding, member := p.rules.action(raw); binding != "" {
				cohort = p.key('u', binding, member)
			}
		}
		return p.recordStoreCandidate(key, cohort, len(p.prefix), 'a', p.needsAnchor, raw, file, lang, p.actionNode)
	}
	var wanted, files []byte
	if rebuildStoreFactoryGetterKind(raw.Kind) && p.rules.getter(raw, raw.Name) {
		wanted, files = p.key('v', raw.Name), p.key('t', raw.Name)
	}
	return p.recordStoreGetter(key, wanted, files, raw, file, lang, p.node)
}

func (p *rebuildStoreFactoryPass) proveActions(ctx context.Context) (anchor bool, err error) {
	prefix := p.key('c')
	err = pendingVisit(ctx, p.b.pending, prefix, func(r row) error {
		if len(r.value) <= len(p.prefix)+1 {
			return invalid(p.label + " candidate proof")
		}
		anchor = anchor || rebuildStoreAnchorExact(r.value[0])
		if !rebuildStoreCandidateExact(r.value[0]) {
			return nil
		}
		state, err := p.state(r.value[1:])
		if err != nil || state == 0 {
			return err // no deciding buckets in this cohort
		}
		return p.foldExactCandidate(r, prefix, len(p.prefix), 'a', p.actionNode)
	})
	return anchor, err
}

func (p *rebuildStoreFactoryPass) callProved(old []byte) (bool, error) {
	raw, err := rebuildReceiptKey(p.b.pending, old)
	if errors.Is(err, ErrRebuildUnknownProvenance) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	claim, err := p.marked(append(p.key('r'), raw...))
	if err != nil || len(claim) <= 1 || claim[0] != rebuildScratchExact || !bytes.Equal(claim[1:], old) {
		return false, err
	}
	return true, nil
}

func (p *rebuildStoreFactoryPass) proved(old []byte, e *graph.Edge, anchor bool) (bool, error) {
	ok, err := p.callProved(old)
	if err != nil || !ok {
		return ok, err
	}
	call := p.rules.call(e)
	pick, err := p.elect(e.From, call, true)
	if err != nil {
		return false, err
	}
	if pick.caller == nil && pick.nonempty {
		receipt, err := p.receiptCallerPick(e.From, call)
		if err != nil {
			return false, err
		}
		if receipt.caller != nil {
			if receipt.target != pick.target {
				return false, nil
			}
			return p.electionProved(call, receipt, anchor)
		}
	}
	if ok, err := p.electionProved(call, pick, anchor); err != nil || !ok {
		return false, err
	}
	if pick.caller != nil {
		state, err := p.state(p.key('n', pick.caller.ID))
		return state == rebuildScratchExact, err
	}
	return true, nil
}

func (p *rebuildStoreFactoryPass) electionProved(call rebuildStoreDispatchCall, pick rebuildStoreFactoryElection, anchor bool) (bool, error) {
	if !pick.nonempty && !anchor {
		// An empty scoped cohort has no candidate witness of its own.
		return false, nil
	}
	if ok, err := p.candidateBucketsProved(pick.buckets); err != nil || !ok {
		return false, err
	}
	if pick.getter {
		wanted := p.key('v', call.getter)
		var (
			ok  bool
			err error
		)
		if pick.getterProof {
			ok, err = p.getterFilesProved(p.key('t', call.getter), wanted, p.unique)
		} else {
			ok, err = p.getterFallbackProved(p.key('t', call.getter), wanted)
		}
		if err != nil || !ok {
			return false, err
		}
	}
	return true, nil
}
