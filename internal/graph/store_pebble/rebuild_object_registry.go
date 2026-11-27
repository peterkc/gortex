package store_pebble

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"iter"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/pathkey"
)

var ErrRebuildObjectRegistry = errors.New("pebble rebuild Object Registry stage failed")

var rebuildObjectRegistryPrefix = []byte{'g', 'q', 'R'}

// RebuildObjectRegistryRules supplies the native marker, priority and payload rules.
type RebuildObjectRegistryRules struct {
	Call        func(*graph.Edge) (string, string)
	ClassName   func(string) string
	MethodOrder func(string) []string
	Getter      func(*graph.Node, string) bool
	Rebind      func(*graph.Edge, string, string, bool) bool
}

type RebuildObjectRegistryStats struct {
	Stubs, Resolved, Moves uint64
}

type rebuildObjectRegistryPass struct {
	rebuildCallMoves
	rules       RebuildObjectRegistryRules
	stats       RebuildObjectRegistryStats
	haveMembers bool
}

// RebuildObjectRegistry matches the direct SQLite path. The native mutation
// of detached dyn_shape edges does not persist a registry_claimed marker.
func (b *BuildStore) RebuildObjectRegistry(ctx context.Context, receipts iter.Seq2[ParseFacts, error],
	rules RebuildObjectRegistryRules,
) (stats RebuildObjectRegistryStats, err error) {
	defer func() {
		if err != nil {
			stats = RebuildObjectRegistryStats{}
			err = fmt.Errorf("%w: %w", ErrRebuildObjectRegistry, err)
		}
	}()
	if receipts == nil || rules.Call == nil || rules.ClassName == nil || rules.MethodOrder == nil || rules.Getter == nil || rules.Rebind == nil {
		return stats, fmt.Errorf("%w: invalid Object Registry inputs", ErrRebuildLinkInputSize)
	}
	prefix := rebuildObjectRegistryPrefix
	occupied, err := rebuildGuardHasPrefix(ctx, b.pending, prefix)
	if err != nil {
		return stats, err
	}
	if occupied {
		return stats, invalid("Object Registry scratch range occupied")
	}
	defer func() {
		err = errors.Join(err, b.pending.DeleteRange(prefix, prefixEnd(prefix), pebble.NoSync))
	}()
	p := rebuildObjectRegistryPass{
		rebuildCallMoves: rebuildCallMoves{
			b: b, prefix: prefix, rowBytes: 16 << 20, label: "Object Registry", inputRow: "edge row",
		},
		rules: rules,
	}
	if err := p.collectEdges(ctx, graph.EdgeCalls); err != nil || p.stats.Stubs == 0 {
		return p.stats, err
	}
	if err := p.collectEdges(ctx, graph.EdgeMemberOf); err != nil || !p.haveMembers {
		return p.stats, err
	}
	if err := p.orderMemberships(ctx, receipts); err != nil {
		return stats, err
	}
	haveMethods, err := p.collectCandidates(ctx)
	if err != nil || !haveMethods {
		return p.stats, err
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

func (p *rebuildObjectRegistryPass) state(key []byte) (byte, error) {
	return rebuildScratchState(p.b.pending, key, "Object Registry receipt proof")
}

func (p *rebuildObjectRegistryPass) node(id string) (*graph.Node, error) {
	key := pendingNodeKey(id)
	return pendingGet(p.b.pending, key, func(raw []byte) (*graph.Node, error) {
		if err := p.nodeInput(key, raw); err != nil {
			return nil, err
		}
		return checkedRebuildLightNode(row{key, raw}, p.label)
	})
}

func (p *rebuildObjectRegistryPass) nodeInput(key, val []byte) error {
	if len(key)+len(val) > p.rowBytes {
		return fmt.Errorf("%w: %s node row", ErrRebuildLinkInputSize, p.label)
	}
	return nil
}

func rebuildObjectRegistryCallable(kind graph.NodeKind) bool {
	return kind == graph.KindMethod || kind == graph.KindFunction
}

func (p *rebuildObjectRegistryPass) collectEdges(ctx context.Context, kind graph.EdgeKind) error {
	return p.b.RebuildView().visit(ctx, []byte{familyEdge, edgeOut}, func(snap *pebble.Snapshot, r row) error {
		rowKind, err := rebuildAttributionEdgeKind(r.key)
		if err != nil || rowKind != kind {
			return err
		}
		col := byte('h') // raw MemberOf -> live MemberOf
		if kind == graph.EdgeCalls {
			e, err := p.decodeKeyedEdge(r.key, r.value)
			if err != nil {
				return err
			}
			class, _ := p.rules.Call(e)
			if class == "" {
				return nil
			}
			if err := p.input(r.key, r.value); err != nil {
				return err
			}
			if err := p.put(append(p.column('l'), r.key...), nil); err != nil {
				return err
			}
			if err := p.put(p.key('k', class), nil); err != nil {
				return err
			}
			p.stats.Stubs++
			col = 'r' // shared move helper's Call recorder
		} else {
			e, err := guardLightEdge(r)
			if err != nil || e.From == "" || e.To == "" {
				return err
			}
			p.haveMembers = true
		}
		raw, err := rebuildReceiptKey(snap, r.key)
		if errors.Is(err, ErrRebuildUnknownProvenance) {
			return nil // potentially relevant later, but it has no raw position
		}
		if err != nil {
			return err
		}
		return p.claimKey(append(p.column(col), raw...), r.key)
	})
}

func (p *rebuildObjectRegistryPass) orderMemberships(ctx context.Context, receipts iter.Seq2[ParseFacts, error]) error {
	var ordinal uint64
	for facts, err := range receipts {
		if err != nil {
			return err
		}
		file := pathkey.Normalize(facts.File.FilePath)
		for _, e := range facts.Edges {
			if err := ctx.Err(); err != nil {
				return err
			}
			if e.Kind != graph.EdgeMemberOf {
				continue
			}
			raw := pendingEdgeKey(e)
			key := append(p.column('h'), raw...)
			claim, err := p.get(key)
			if err != nil {
				return err
			}
			if len(claim) <= 1 {
				continue
			}
			exact := facts.EffectiveLanguage != "" && e.FilePath == file
			if exact && claim[0] == rebuildCapabilityWanted {
				ordinal++
				// Only the first copy occupies a SQLite row position.
				if err := p.put(append(p.column('j'), claim[1:]...), binary.BigEndian.AppendUint64(nil, ordinal)); err != nil {
					return err
				}
			}
			if err := noteRebuildScratchPayload(p.put, key, claim, rebuildScratchProofBit(exact)); err != nil {
				return err
			}
		}
	}
	return ctx.Err()
}

func (p *rebuildObjectRegistryPass) membershipOrder(live []byte) (uint64, error) {
	raw, err := rebuildReceiptKey(p.b.pending, live)
	if errors.Is(err, ErrRebuildUnknownProvenance) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	claim, err := p.get(append(p.column('h'), raw...))
	if err != nil || len(claim) <= 1 || claim[0] != rebuildScratchExact || !bytes.Equal(claim[1:], live) {
		return 0, err
	}
	return pendingGet(p.b.pending, append(p.column('j'), live...), func(v []byte) (uint64, error) {
		if len(v) != 8 {
			return 0, invalid("Object Registry membership ordinal")
		}
		return binary.BigEndian.Uint64(v), nil
	})
}

func (p *rebuildObjectRegistryPass) memberships(ctx context.Context, id string, visit func(string, uint64) error) error {
	return pendingVisit(ctx, p.b.pending, guardOutgoing(id, graph.EdgeMemberOf), func(r row) error {
		e, err := guardLightEdge(r)
		if err != nil || e.To == "" {
			return err
		}
		order, err := p.membershipOrder(r.key)
		if err != nil {
			return err
		}
		return visit(p.rules.ClassName(e.To), order)
	})
}

func (p *rebuildObjectRegistryPass) collectCandidates(ctx context.Context) (bool, error) {
	haveMethods := false
	err := p.b.RebuildView().visit(ctx, []byte{familyNode}, func(_ *pebble.Snapshot, r row) error {
		n, err := checkedRebuildLightNode(r, p.label)
		if err != nil {
			return err
		}
		if !rebuildObjectRegistryCallable(n.Kind) || n.ID == "" {
			return nil
		}
		var class string
		var last uint64
		var member, unpositioned bool
		if err := p.memberships(ctx, n.ID, func(next string, order uint64) error {
			if !member || order > last {
				class, last = next, order
			}
			member = true
			unpositioned = unpositioned || order == 0
			return nil
		}); err != nil || !member {
			return err
		}
		// Without a receipt position, a different class could supersede
		// this one. Extra buckets cannot certify a unique or empty pool.
		certain := last != 0
		hasClass := class != ""
		anchorPossible := certain && hasClass
		if unpositioned {
			if err := p.memberships(ctx, n.ID, func(other string, order uint64) error {
				if order != 0 || other == class {
					return nil
				}
				certain = false
				anchorPossible = anchorPossible && other != ""
				hasClass = hasClass || other != ""
				for _, possible := range [...]string{class, other} {
					if possible == "" {
						continue
					}
					wanted, err := p.present(p.key('k', possible))
					if err != nil {
						return err
					}
					if !wanted {
						continue
					}
					cohort := p.key('u', possible, n.Name)
					for _, bucket := range rebuildStoreScopeKeys(cohort, len(p.prefix), 'i', n) {
						if err := p.put(p.key('e', string(bucket)), nil); err != nil {
							return err
						}
					}
				}
				return nil
			}); err != nil {
				return err
			}
		}
		if !hasClass {
			return nil
		}
		haveMethods = true
		cohort := p.key('u', class, n.Name)
		state := byte(rebuildCapabilityWanted)
		if !certain {
			state |= rebuildCapabilityUnproved
		}
		if !anchorPossible {
			state |= rebuildStoreAnchorIneligible
		}
		if err := p.put(p.key('c', n.ID), append([]byte{state}, cohort...)); err != nil {
			return err
		}
		wanted, err := p.present(p.key('k', class))
		if err != nil || !wanted {
			return err
		}
		for _, key := range rebuildStoreScopeKeys(cohort, len(p.prefix), 'i', n) {
			if err := p.addUnique(key, n.ID); err != nil {
				return err
			}
		}
		return nil
	})
	return haveMethods, err
}

func (p *rebuildObjectRegistryPass) collectGetters(ctx context.Context) error {
	return p.b.RebuildView().visit(ctx, []byte{familyNode}, func(_ *pebble.Snapshot, r row) error {
		n, err := checkedRebuildLightNode(r, p.label)
		if err != nil || !rebuildStoreFactoryGetterKind(n.Kind) || n.FilePath == "" {
			return err
		}
		wanted, err := p.present(p.key('b', n.Name))
		if err != nil || !wanted {
			return err
		}
		full, err := decodeNode(r.value)
		if err != nil || !p.rules.Getter(full, n.Name) {
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

type rebuildObjectRegistryPick struct {
	target       string
	caller       *graph.Node
	buckets      []rebuildStoreCandidateWitness
	nonempty     bool
	readsBinding bool
	getterProof  bool
	deferred     bool
}

func (p *rebuildObjectRegistryPass) elect(e *graph.Edge, resolveGetter bool) (pick rebuildObjectRegistryPick, err error) {
	class, method := p.rules.Call(e)
	binding, _ := e.Meta["store_binding"].(string)
	var callerRead bool
	for _, name := range p.rules.MethodOrder(method) {
		scoped := p.key('i', class, name)
		id, seen, err := p.unique(scoped)
		if err != nil {
			return pick, err
		}
		if !seen {
			pick.buckets = append(pick.buckets, rebuildStoreCandidateWitness{scoped, 0})
			continue
		}
		if !callerRead {
			pick.caller, err = p.node(e.From)
			if err != nil {
				return pick, err
			}
			callerRead = true
		}
		var callerFile string
		if pick.caller != nil {
			callerFile = pick.caller.FilePath
			scoped = p.key('w', class, name, pick.caller.WorkspaceID)
			id, seen, err = p.unique(scoped)
			if err != nil {
				return pick, err
			}
		}
		if seen && id == "" {
			pick.readsBinding = true
			if !resolveGetter && binding != "" {
				pick.deferred = true
				return pick, nil
			}
		}
		var getterFile string
		decision, err := pickRebuildStoreCandidate(p.unique, scoped, id, seen, func() (string, error) {
			if binding == "" {
				return "", nil
			}
			file, _, err := p.unique(p.key('t', binding))
			getterFile = file
			return file, err
		}, callerFile, func(file string) []byte {
			if pick.caller != nil {
				return p.key('s', class, name, pick.caller.WorkspaceID, file)
			}
			return p.key('f', class, name, file)
		})
		if err != nil {
			return pick, err
		}
		if seen && id == "" && binding != "" {
			pick.getterProof = pick.getterProof || getterFile == "" || decision.getterWinner
		}
		pick.nonempty = pick.nonempty || decision.nonempty
		pick.buckets = append(pick.buckets, decision.buckets...)
		if decision.target != "" {
			pick.target = decision.target
			break
		}
	}
	return pick, nil
}

func (p *rebuildObjectRegistryPass) plan(ctx context.Context) error {
	var refused rebuildScratchRefusals
	deferred := false
	for _, col := range [...]byte{'l', 'd'} {
		if col == 'd' {
			if !deferred {
				break
			}
			if err := p.collectGetters(ctx); err != nil {
				return err
			}
		}
		prefix := p.column(col)
		if err := pendingVisit(ctx, p.b.pending, prefix, func(r row) error {
			old := r.key[len(prefix):]
			e, err := p.edge(old)
			if err != nil {
				return err
			}
			if e == nil {
				return invalid("Object Registry Call missing")
			}
			pick, err := p.elect(e, col == 'd')
			if err != nil {
				return err
			}
			if pick.deferred {
				deferred = true
				binding, _ := e.Meta["store_binding"].(string)
				if err := p.put(p.key('b', binding), nil); err != nil {
					return err
				}
				return p.put(append(p.column('d'), old...), nil)
			}
			return p.planCall(old, e, pick, &refused)
		}); err != nil {
			return err
		}
	}
	return refused.err()
}

func (p *rebuildObjectRegistryPass) planCall(old []byte, e *graph.Edge, pick rebuildObjectRegistryPick, refused *rebuildScratchRefusals) error {
	if pick.target != "" {
		p.stats.Resolved++
	}
	class, _ := p.rules.Call(e)
	if !p.rules.Rebind(e, class, pick.target, pick.target != "") {
		// A no-op still needs a stable class membership election.
		for _, witness := range pick.buckets {
			uncertain, err := p.present(p.key('e', string(witness.bucket)))
			if err != nil {
				return err
			}
			if uncertain {
				refused.add(graph.EdgeIdentityFor(e), old)
				break
			}
		}
		return nil
	}
	if pick.caller != nil {
		if err := p.put(p.key('n', pick.caller.ID), []byte{rebuildCapabilityWanted}); err != nil {
			return err
		}
	}
	if binding, _ := e.Meta["store_binding"].(string); binding != "" && pick.readsBinding {
		if err := p.put(p.key('v', binding), []byte{rebuildCapabilityWanted}); err != nil {
			return err
		}
	}
	if err := p.wantCandidateBuckets(pick.buckets); err != nil {
		return err
	}
	for _, witness := range pick.buckets {
		extra, err := p.present(p.key('e', string(witness.bucket)))
		if err != nil {
			return err
		}
		if extra {
			if err := p.put(p.candidateProofKey(witness.bucket), []byte{1 | rebuildStoreCandidateExtra}); err != nil {
				return err
			}
		}
	}
	if err := p.stage(old, e); err != nil {
		return err
	}
	p.stats.Moves++
	return nil
}

func (p *rebuildObjectRegistryPass) recordNode(ctx context.Context, raw *graph.Node, file, lang string) error {
	callerKey, candidateKey, getterKey := p.key('n', raw.ID), p.key('c', raw.ID), p.key('g', raw.ID)
	caller, err := p.state(callerKey)
	if err != nil {
		return err
	}
	candidate, err := p.get(candidateKey)
	if err != nil {
		return err
	}
	var wanted, files []byte
	if rebuildStoreFactoryGetterKind(raw.Kind) && p.rules.Getter(raw, raw.Name) {
		wanted, files = p.key('v', raw.Name), p.key('t', raw.Name)
	}
	if err := p.recordStoreGetter(getterKey, wanted, files, raw, file, lang, p.node); err != nil || (caller == 0 && candidate == nil) {
		return err
	}
	live, err := p.node(raw.ID)
	if err != nil {
		return err
	}
	admitted := rebuildStoreNodeReceipt(raw, live, file, lang)
	if caller != 0 {
		ok := rebuildStoreCallerReceipt(raw, live, file, lang)
		if err := noteRebuildScratchState(p.put, callerKey, caller, rebuildScratchProofBit(ok)); err != nil {
			return err
		}
	}
	if candidate != nil {
		eligible := admitted && rebuildObjectRegistryCallable(raw.Kind)
		if eligible {
			if err := p.noteCandidateChange(candidate[1:], raw, live); err != nil {
				return err
			}
			if candidate[0]&rebuildCapabilityUnproved != 0 &&
				(raw.Name != live.Name || raw.FilePath != live.FilePath || raw.WorkspaceID != live.WorkspaceID) {
				// Every unpositioned class may receive the changed declaration.
				if err := p.memberships(ctx, raw.ID, func(class string, order uint64) error {
					if order != 0 || class == "" {
						return nil
					}
					return p.noteCandidateChange(p.key('u', class, live.Name), raw, live)
				}); err != nil {
					return err
				}
			}
		}
		ok := eligible && raw.Name == live.Name
		proof := rebuildStoreCandidateProof(ok, raw, live) | rebuildStoreAnchorProof(eligible)
		if err := p.noteStoreNodeProof(candidateKey, candidate, proof); err != nil {
			return err
		}
	}
	return nil
}

func (p *rebuildObjectRegistryPass) noteCandidateChange(cohort []byte, raw, live *graph.Node) error {
	if raw.Name == live.Name {
		return p.noteCandidateScopeChange(cohort, len(p.prefix), 'i', raw, live)
	}
	class, _, err := readKeyString(cohort[len(p.prefix)+1:])
	if err != nil {
		return err
	}
	return p.noteCandidateCohortChange(cohort, p.key('u', class, raw.Name), len(p.prefix), 'i', raw, live)
}

func (p *rebuildObjectRegistryPass) recordCall(raw *graph.Edge, file, lang string) error {
	key := append(p.column('r'), pendingEdgeKey(raw)...)
	claim, err := p.get(key)
	if err != nil || len(claim) <= 1 {
		return err
	}
	live, err := p.edge(claim[1:])
	if err != nil {
		return err
	}
	if live == nil {
		return invalid("Object Registry recorder has no live Call")
	}
	class, method := p.rules.Call(raw)
	liveClass, liveMethod := p.rules.Call(live)
	binding, _ := raw.Meta["store_binding"].(string)
	liveBinding, _ := live.Meta["store_binding"].(string)
	exact := lang != "" && raw.FilePath == file && class == liveClass && method == liveMethod
	if exact && binding != liveBinding {
		pick, err := p.elect(live, true)
		if err != nil {
			return err
		}
		exact = !pick.readsBinding
	}
	return p.noteReceipt(key, claim, exact)
}

func (p *rebuildObjectRegistryPass) prove(ctx context.Context, receipts iter.Seq2[ParseFacts, error]) error {
	for facts, err := range receipts {
		if err != nil {
			return err
		}
		file, lang := pathkey.Normalize(facts.File.FilePath), facts.EffectiveLanguage
		for _, n := range facts.Nodes {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := p.recordNode(ctx, n, file, lang); err != nil {
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
	prefix := p.column('c')
	if err := pendingVisit(ctx, p.b.pending, prefix, func(r row) error {
		anchor = anchor || rebuildStoreAnchorExact(r.value[0])
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

func (p *rebuildObjectRegistryPass) proved(old []byte, e *graph.Edge, anchor bool) (bool, error) {
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
	pick, err := p.elect(e, true)
	if err != nil {
		return false, err
	}
	if !pick.nonempty && !anchor {
		return false, nil
	}
	if pick.caller != nil {
		state, err := p.state(p.key('n', pick.caller.ID))
		if err != nil || state != rebuildScratchExact {
			return false, err
		}
	}
	if binding, _ := e.Meta["store_binding"].(string); binding != "" && pick.readsBinding {
		wanted := p.key('v', binding)
		var ok bool
		if pick.getterProof {
			ok, err = p.getterFilesProved(p.key('t', binding), wanted, p.unique)
		} else {
			ok, err = p.getterFallbackProved(p.key('t', binding), wanted)
		}
		if err != nil || !ok {
			return false, err
		}
	}
	return p.candidateBucketsProved(pick.buckets)
}
