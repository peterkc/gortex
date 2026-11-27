package store_pebble

import (
	"context"
	"errors"
	"fmt"
	"iter"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/pathkey"
)

var ErrRebuildSidekiq = errors.New("pebble rebuild Sidekiq stage failed")

var rebuildSidekiqPrefix = []byte{'g', 'q', 'K'}

type RebuildSidekiqStats = RebuildStoreFactoryStats

// RebuildSidekiqRules supplies the native marker, alias and payload decisions.
type RebuildSidekiqRules struct {
	Worker func(*graph.Node) string
	Stub   func(*graph.Edge) string
	Simple func(string) string
	Getter func(*graph.Node, string) bool
	Rebind func(*graph.Edge, string, bool) bool
}

type rebuildSidekiqPass struct {
	rebuildStoreFactoryPass
	sidekiq RebuildSidekiqRules
}

// RebuildSidekiq runs on the private graph at framework position 28.
func (b *BuildStore) RebuildSidekiq(ctx context.Context, receipts iter.Seq2[ParseFacts, error],
	rules RebuildSidekiqRules,
) (stats RebuildSidekiqStats, err error) {
	defer func() {
		if err != nil {
			stats = RebuildSidekiqStats{}
			err = fmt.Errorf("%w: %w", ErrRebuildSidekiq, err)
		}
	}()
	if receipts == nil || rules.Worker == nil || rules.Stub == nil || rules.Simple == nil || rules.Getter == nil || rules.Rebind == nil {
		return stats, fmt.Errorf("%w: invalid Sidekiq inputs", ErrRebuildLinkInputSize)
	}
	prefix := rebuildSidekiqPrefix
	occupied, err := rebuildGuardHasPrefix(ctx, b.pending, prefix)
	if err != nil {
		return stats, err
	}
	if occupied {
		return stats, invalid("Sidekiq scratch range occupied")
	}
	defer func() {
		err = errors.Join(err, b.pending.DeleteRange(prefix, prefixEnd(prefix), pebble.NoSync))
	}()
	read := func(e *graph.Edge) rebuildStoreDispatchCall {
		binding, _ := e.Meta["store_binding"].(string)
		return rebuildStoreDispatchCall{group: "full", member: rules.Stub(e), getter: binding}
	}
	p := rebuildSidekiqPass{
		rebuildStoreFactoryPass: rebuildStoreFactoryPass{
			rebuildCallMoves: rebuildCallMoves{b: b, prefix: prefix, rowBytes: 16 << 20, label: "Sidekiq", inputRow: "edge row"},
			rules: rebuildStoreDispatchRules{
				call:   read,
				getter: rules.Getter,
				rebind: func(e *graph.Edge, _, id string, matched bool) bool { return rules.Rebind(e, id, matched) },
			},
		},
		sidekiq: rules,
	}
	active, err := p.collect(ctx)
	if err != nil || !active {
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

func rebuildSidekiqCallable(kind graph.NodeKind) bool {
	return kind == graph.KindMethod || kind == graph.KindFunction
}

// c records full-name roles and d simple-name roles. Both use the shared
// candidate scopes with distinct groups; i proves relevant full-name presence.
func (p *rebuildSidekiqPass) aliases(name string) [2][]byte {
	if name == "" {
		return [2][]byte{}
	}
	return [2][]byte{p.key('u', "full", name), p.key('u', "simple", p.sidekiq.Simple(name))}
}

func (p *rebuildSidekiqPass) collect(ctx context.Context) (bool, error) {
	active := false
	err := p.b.RebuildView().visit(ctx, []byte{familyNode}, func(_ *pebble.Snapshot, r row) error {
		light, err := checkedRebuildLightNode(r, p.label)
		if err != nil || !rebuildSidekiqCallable(light.Kind) {
			return err
		}
		n, err := decodeNode(r.value)
		if err != nil {
			return err
		}
		name := p.sidekiq.Worker(n)
		if name == "" {
			return nil
		}
		if n.ID == "" {
			return invalid("Sidekiq worker has no ID")
		}
		if err := p.nodeInput(r.key, r.value); err != nil {
			return err
		}
		active = true
		for i, cohort := range p.aliases(name) {
			if err := p.put(p.key("cd"[i], n.ID), append([]byte{rebuildCapabilityWanted}, cohort...)); err != nil {
				return err
			}
			for _, bucket := range rebuildStoreScopeKeys(cohort, len(p.prefix), 'a', n) {
				if err := p.addUnique(bucket, n.ID); err != nil {
					return err
				}
			}
		}
		return nil
	})
	return active, err
}

func (p *rebuildSidekiqPass) fullPresence(cohort []byte) []byte {
	return append(p.key('i'), cohort[len(p.prefix)+1:]...)
}

func (p *rebuildSidekiqPass) elect(e *graph.Edge, call rebuildStoreDispatchCall, resolveGetter bool) (rebuildStoreDispatchCall, rebuildStoreFactoryElection, error) {
	full := p.key('a', "full", call.member)
	_, seen, err := p.unique(full)
	if err != nil {
		return call, rebuildStoreFactoryElection{}, err
	}
	// Native chooses the alias before filtering by caller workspace. A
	// qualified match outside the workspace still prevents simple lookup.
	if !seen {
		call.group, call.member = "simple", p.sidekiq.Simple(call.member)
	}
	pick, err := p.rebuildStoreFactoryPass.elect(e.From, call, resolveGetter)
	if !seen {
		pick.buckets = append(pick.buckets, rebuildStoreCandidateWitness{bucket: full})
	}
	return call, pick, err
}

func (p *rebuildSidekiqPass) planCall(old []byte, e *graph.Edge, call, selected rebuildStoreDispatchCall, pick rebuildStoreFactoryElection) error {
	moves := p.stats.Moves
	if err := p.rebuildStoreFactoryPass.planCall(old, e, selected, pick); err != nil || p.stats.Moves == moves {
		return err
	}
	full := p.key('u', "full", call.member)
	if err := p.want(full); err != nil {
		return err
	}
	if selected.group == "full" && !pick.nonempty {
		return p.want(p.fullPresence(full))
	}
	return nil
}

func (p *rebuildSidekiqPass) plan(ctx context.Context) error {
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
		selected, pick, err := p.elect(e, call, false)
		if err != nil {
			return err
		}
		if !pick.getter {
			return p.planCall(r.key, e, call, selected, pick)
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
		selected, pick, err := p.elect(e, call, true)
		if err != nil {
			return err
		}
		return p.planCall(old, e, call, selected, pick)
	})
}

func (p *rebuildSidekiqPass) prove(ctx context.Context, receipts iter.Seq2[ParseFacts, error]) error {
	for facts, err := range receipts {
		if err != nil {
			return err
		}
		if err := p.record(ctx, facts); err != nil {
			return err
		}
		file, lang := pathkey.Normalize(facts.File.FilePath), facts.EffectiveLanguage
		for _, raw := range facts.Nodes {
			if err := ctx.Err(); err != nil {
				return err
			}
			var after [2][]byte
			if rebuildSidekiqCallable(raw.Kind) {
				after = p.aliases(p.sidekiq.Worker(raw))
			}
			for i, cohort := range after {
				if err := p.recordStoreCandidate(p.key("cd"[i], raw.ID), cohort, len(p.prefix), 'a', i == 0 && p.needsAnchor, raw, file, lang); err != nil {
					return err
				}
			}
		}
	}
	anchor, err := p.proveAliases(ctx)
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

func (p *rebuildSidekiqPass) proveAliases(ctx context.Context) (bool, error) {
	anchor := false
	for _, col := range [...]byte{'c', 'd'} {
		prefix := p.key(col)
		if err := pendingVisit(ctx, p.b.pending, prefix, func(r row) error {
			if len(r.value) <= len(p.prefix)+1 {
				return invalid("Sidekiq worker proof")
			}
			anchor = anchor || col == 'c' && rebuildStoreAnchorExact(r.value[0])
			if !rebuildStoreCandidateExact(r.value[0]) {
				return nil
			}
			cohort := r.value[1:]
			state, err := p.state(cohort)
			if err != nil || state == 0 {
				return err
			}
			if col == 'c' {
				key := p.fullPresence(cohort)
				state, err := p.state(key)
				if err != nil {
					return err
				}
				if state == rebuildCapabilityWanted {
					if err := p.put(key, []byte{rebuildScratchExact}); err != nil {
						return err
					}
				}
			}
			return p.foldExactCandidate(r, prefix, len(p.prefix), 'a', p.node)
		}); err != nil {
			return false, err
		}
	}
	return anchor, nil
}

func (p *rebuildSidekiqPass) proved(old []byte, e *graph.Edge, anchor bool) (bool, error) {
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
	call := p.rules.call(e)
	selected, pick, err := p.elect(e, call, true)
	if err != nil {
		return false, err
	}
	if !pick.nonempty {
		if selected.group == "full" {
			// The full-name worker still blocks fallback, even out of scope.
			state, err := p.state(p.fullPresence(p.key('u', "full", call.member)))
			if err != nil || state != rebuildScratchExact {
				return false, err
			}
		} else if !anchor {
			return false, nil
		}
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
