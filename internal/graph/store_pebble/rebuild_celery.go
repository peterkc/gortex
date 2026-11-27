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

var ErrRebuildCelery = errors.New("pebble rebuild Celery stage failed")

var rebuildCeleryPrefix = []byte{'g', 'q', 'C'}

// RebuildCeleryRules supplies the native metadata, cap and payload decisions.
type RebuildCeleryRules struct {
	Node      func(*graph.Node) (string, string)
	Call      func(*graph.Edge) (string, string)
	Getter    func(*graph.Node, string) bool
	Rebind    func(*graph.Edge, string, string, bool) bool
	FanoutCap byte
}

type RebuildCeleryStats = RebuildStoreFactoryStats

type rebuildCeleryPass struct {
	rebuildStoreFactoryPass // shared scratch, getter census and scoped election
	celery                  RebuildCeleryRules
}

func (b *BuildStore) RebuildCelery(ctx context.Context, receipts iter.Seq2[ParseFacts, error],
	rules RebuildCeleryRules,
) (stats RebuildCeleryStats, err error) {
	defer func() {
		if err != nil {
			stats = RebuildCeleryStats{}
			err = fmt.Errorf("%w: %w", ErrRebuildCelery, err)
		}
	}()
	if receipts == nil || rules.Node == nil || rules.Call == nil || rules.Getter == nil || rules.Rebind == nil ||
		rules.FanoutCap == 0 || rules.FanoutCap == 255 {
		return stats, fmt.Errorf("%w: invalid Celery inputs", ErrRebuildLinkInputSize)
	}
	prefix := rebuildCeleryPrefix
	occupied, err := rebuildGuardHasPrefix(ctx, b.pending, prefix)
	if err != nil {
		return stats, err
	}
	if occupied {
		return stats, invalid("Celery scratch range occupied")
	}
	defer func() {
		err = errors.Join(err, b.pending.DeleteRange(prefix, prefixEnd(prefix), pebble.NoSync))
	}()
	p := rebuildCeleryPass{
		rebuildStoreFactoryPass: rebuildStoreFactoryPass{
			rebuildCallMoves: rebuildCallMoves{
				b: b, prefix: prefix, rowBytes: 16 << 20, label: "Celery", inputRow: "edge row",
			},
			rules: rebuildStoreDispatchRules{getter: rules.Getter},
		},
		celery: rules,
	}
	haveTasks, err := p.collect(ctx)
	if err != nil || !haveTasks {
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

var rebuildCeleryGroups = [...]struct {
	name string
	col  byte
}{{"name", 'N'}, {"registered", 'R'}}

func rebuildCeleryCallable(kind graph.NodeKind) bool {
	return kind == graph.KindFunction || kind == graph.KindMethod
}

func (p *rebuildCeleryPass) bump(key []byte) error {
	n, err := p.state(key)
	if err != nil || n > p.celery.FanoutCap {
		return err
	}
	return p.put(key, []byte{n + 1})
}

func (p *rebuildCeleryPass) collect(ctx context.Context) (bool, error) {
	haveTasks := false
	err := p.b.RebuildView().visit(ctx, []byte{familyNode}, func(_ *pebble.Snapshot, r row) error {
		light, err := checkedRebuildLightNode(r, p.label)
		if err != nil || !rebuildCeleryCallable(light.Kind) {
			return err
		}
		n, err := decodeNode(r.value)
		if err != nil {
			return err
		}
		task, registered := p.celery.Node(n)
		if task == "" && registered == "" {
			return nil
		}
		if err := p.nodeInput(r.key, r.value); err != nil {
			return err
		}
		haveTasks = haveTasks || task != ""
		for i, name := range [...]string{task, registered} {
			if name == "" {
				continue
			}
			if n.ID == "" {
				return invalid("Celery candidate has no ID")
			}
			group := rebuildCeleryGroups[i]
			if err := p.bump(p.key('k', group.name, name)); err != nil {
				return err
			}
			cohort := p.key('u', group.name, name)
			if err := p.put(p.key(group.col, n.ID), append([]byte{rebuildCapabilityWanted}, cohort...)); err != nil {
				return err
			}
			for _, key := range rebuildStoreScopeKeys(cohort, len(p.prefix), 'a', n) {
				if err := p.addUnique(key, n.ID); err != nil {
					return err
				}
			}
		}
		return nil
	})
	return haveTasks, err
}

func (p *rebuildCeleryPass) call(e *graph.Edge) rebuildStoreDispatchCall {
	task, registered := p.celery.Call(e)
	if task == "" {
		return rebuildStoreDispatchCall{}
	}
	binding, _ := e.Meta["store_binding"].(string)
	call := rebuildStoreDispatchCall{group: "name", member: task, getter: binding, reset: task}
	if registered != "" {
		call.group, call.member = "registered", registered
	}
	return call
}

func (p *rebuildCeleryPass) elect(e *graph.Edge, call rebuildStoreDispatchCall, getters bool) (rebuildStoreFactoryElection, error) {
	n, err := p.state(p.key('k', call.group, call.member))
	if err != nil || n > p.celery.FanoutCap {
		// Native does not read the caller or any scoped pool above the cap.
		return rebuildStoreFactoryElection{}, err
	}
	return p.rebuildStoreFactoryPass.elect(e.From, call, getters)
}

func (p *rebuildCeleryPass) plan(ctx context.Context) error {
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
		call := p.call(e)
		if call.group == "" {
			return nil
		}
		if err := p.input(r.key, r.value); err != nil {
			return err
		}
		p.stats.Stubs++
		raw, err := rebuildReceiptKey(snap, r.key)
		if err != nil && !errors.Is(err, ErrRebuildUnknownProvenance) {
			return err
		}
		if err == nil {
			if err := p.claim(raw, r.key); err != nil {
				return err
			}
		}
		pick, err := p.elect(e, call, false)
		if err != nil {
			return err
		}
		if !pick.getter {
			return p.planCall(r.key, e, call, pick)
		}
		deferred = true
		if err := p.put(p.key('b', call.getter), nil); err != nil {
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
			return invalid("Celery deferred Call missing")
		}
		call := p.call(e)
		pick, err := p.elect(e, call, true)
		if err != nil {
			return err
		}
		return p.planCall(old, e, call, pick)
	})
}

func (p *rebuildCeleryPass) planCall(old []byte, e *graph.Edge, call rebuildStoreDispatchCall, pick rebuildStoreFactoryElection) error {
	if pick.target != "" {
		p.stats.Resolved++
	}
	if !p.celery.Rebind(e, call.reset, pick.target, pick.target != "") {
		return nil
	}
	if err := p.want(p.key('u', call.group, call.member)); err != nil {
		return err
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

func (p *rebuildCeleryPass) recordCeleryNode(raw *graph.Node, file, lang string) error {
	if err := p.recordNode(raw, file, lang); err != nil {
		return err
	}
	var task, registered string
	if rebuildCeleryCallable(raw.Kind) {
		task, registered = p.celery.Node(raw)
	}
	for i, name := range [...]string{task, registered} {
		group := rebuildCeleryGroups[i]
		key := p.key(group.col, raw.ID)
		claim, err := p.marked(key)
		if err != nil {
			return err
		}
		if name == "" && claim == nil {
			continue
		}
		live, err := p.node(raw.ID)
		if err != nil {
			return err
		}
		admitted := rebuildStoreNodeReceipt(raw, live, file, lang)
		cohort := p.key('u', group.name, name)
		same := claim != nil && name != "" && bytes.Equal(claim[1:], cohort)
		if claim != nil {
			proof := rebuildStoreCandidateProof(admitted && same, raw, live)
			if i == 0 {
				proof |= rebuildStoreAnchorProof(admitted && name != "")
			}
			if err := p.noteStoreNodeProof(key, claim, proof); err != nil {
				return err
			}
		}
		if !admitted || name == "" {
			continue
		}
		wanted, err := p.state(cohort)
		if err != nil {
			return err
		}
		if wanted == 0 {
			continue
		}
		before := p.key('u', "", "")
		if claim != nil {
			before = claim[1:]
		}
		if err := p.noteCandidateCohortChange(before, cohort, len(p.prefix), 'a', raw, live); err != nil {
			return err
		}
		if !same {
			// A contradictory receipt can add one distinct member to the
			// complete pool, even when it cannot win the scoped election.
			id := p.key('z', group.name, name, raw.ID)
			seen, err := p.present(id)
			if err != nil {
				return err
			}
			if !seen {
				if err := p.put(id, nil); err != nil {
					return err
				}
				if err := p.bump(p.key('y', group.name, name)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (p *rebuildCeleryPass) recordCeleryCall(raw *graph.Edge, file, lang string) error {
	key := append(p.column('r'), pendingEdgeKey(raw)...)
	claim, err := p.marked(key)
	if err != nil || len(claim) <= 1 {
		return err
	}
	live, err := p.edge(claim[1:])
	if err != nil {
		return err
	}
	if live == nil {
		return invalid("Celery recorder has no old Call")
	}
	call, current := p.call(raw), p.call(live)
	ok := lang != "" && raw.FilePath == file && call == current
	return p.noteReceipt(key, claim, ok)
}

func (p *rebuildCeleryPass) prove(ctx context.Context, receipts iter.Seq2[ParseFacts, error]) error {
	for facts, err := range receipts {
		if err != nil {
			return err
		}
		file, lang := pathkey.Normalize(facts.File.FilePath), facts.EffectiveLanguage
		for _, n := range facts.Nodes {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := p.recordCeleryNode(n, file, lang); err != nil {
				return err
			}
		}
		for _, edge := range facts.Edges {
			if err := ctx.Err(); err != nil {
				return err
			}
			if edge.Kind == graph.EdgeCalls {
				if err := p.recordCeleryCall(edge, file, lang); err != nil {
					return err
				}
			}
		}
	}
	anchor := false
	for _, group := range rebuildCeleryGroups {
		prefix := p.column(group.col)
		if err := pendingVisit(ctx, p.b.pending, prefix, func(r row) error {
			if len(r.value) <= len(p.prefix)+1 {
				return invalid("Celery candidate proof")
			}
			proof, cohort := r.value[0], r.value[1:]
			anchor = anchor || group.col == 'N' && rebuildStoreAnchorExact(proof)
			if !rebuildStoreCandidateExact(proof) {
				return nil
			}
			wanted, err := p.state(cohort)
			if err != nil || wanted == 0 {
				return err
			}
			// Scope mismatch flags do not change the total before filtering.
			k := append(p.column('K'), cohort[len(p.prefix)+1:]...)
			if err := p.bump(k); err != nil {
				return err
			}
			return p.foldExactCandidate(r, prefix, len(p.prefix), 'a', p.node)
		}); err != nil {
			return err
		}
	}
	if err := p.foldGetterFiles(ctx, p.column('g'), p.node, p.addUnique); err != nil {
		return err
	}
	var refused rebuildScratchRefusals
	if err := p.moves(ctx, func(old []byte, next *graph.Edge) error {
		ok, err := p.proved(old, next, anchor)
		if err == nil && !ok {
			refused.add(graph.EdgeIdentityFor(next), pendingEdgeKey(next))
		}
		return err
	}); err != nil {
		return err
	}
	return refused.err()
}

func (p *rebuildCeleryPass) proved(old []byte, e *graph.Edge, anchor bool) (bool, error) {
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
	claim, err := p.marked(append(p.column('r'), raw...))
	if err != nil || len(claim) <= 1 || claim[0] != rebuildScratchExact || !bytes.Equal(claim[1:], old) {
		return false, err
	}
	call := p.call(e)
	live, err := p.state(p.key('k', call.group, call.member))
	if err != nil {
		return false, err
	}
	if live > p.celery.FanoutCap {
		count, err := p.state(p.key('K', call.group, call.member))
		return count > p.celery.FanoutCap, err
	}
	extra, err := p.state(p.key('y', call.group, call.member))
	if err != nil || int(live)+int(extra) > int(p.celery.FanoutCap) {
		return false, err
	}
	pick, err := p.elect(e, call, true)
	if err != nil {
		return false, err
	}
	if ok, err := p.candidateBucketsProved(pick.buckets); err != nil || !ok {
		return false, err
	}
	if pick.getter {
		wanted := p.key('v', call.getter)
		var ok bool
		if pick.getterProof {
			ok, err = p.getterFilesProved(p.key('t', call.getter), wanted, p.unique)
		} else {
			ok, err = p.getterFallbackProved(p.key('t', call.getter), wanted)
		}
		if err != nil || !ok {
			return false, err
		}
	}
	if pick.caller != nil {
		state, err := p.state(p.key('n', pick.caller.ID))
		return state == rebuildScratchExact, err
	}
	return true, nil
}
