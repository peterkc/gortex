package store_pebble

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"iter"
	"os"
	"path/filepath"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/pathkey"
)

var ErrRebuildMacroExpansion = errors.New("pebble rebuild macro expansion stage failed")

var rebuildMacroExpansionPrefix = []byte{'g', 'q', 'X'}

// RebuildMacroExpansionRules supplies the native macro roles and Call payload.
type RebuildMacroExpansionRules struct {
	CFamily func(string) bool
	Macro   func(*graph.Node) bool
	Body    func(*graph.Edge) string
	MayAdd  func(*graph.Edge) bool
	Add     func(caller, callee, file string, line int, name string) *graph.Edge
}

// RebuildMacroExpansionStats distinguishes native owned proposals from new rows.
type RebuildMacroExpansionStats struct {
	Sites, Owned, Adds uint64
}

type rebuildMacroExpansion struct {
	rebuildCallMoves
	rules   RebuildMacroExpansionRules
	stats   RebuildMacroExpansionStats
	cProved bool
}

func rebuildMacroExpansionScratch(b *BuildStore) rebuildCallMoves {
	return rebuildCallMoves{b: b, prefix: rebuildMacroExpansionPrefix, rowBytes: 16 << 20,
		label: "macro expansion", inputRow: "edge row"}
}

// FreezeMacroExpansionInputs mirrors the native macro-node snapshot and its
// pre-loop Call identity collector. The body edges remain live.
func (b *BuildStore) FreezeMacroExpansionInputs(ctx context.Context, macro func(*graph.Node) bool) (err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("%w: %w", ErrRebuildMacroExpansion, err)
		}
	}()
	p := rebuildMacroExpansionScratch(b)
	busy, err := rebuildGuardHasPrefix(ctx, b.pending, p.prefix)
	if err != nil {
		return err
	}
	if busy || macro == nil {
		return invalid("macro expansion census inputs")
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, b.pending.DeleteRange(p.prefix, prefixEnd(p.prefix), pebble.NoSync))
		}
	}()
	var eligible bool
	if err := b.RebuildView().visit(ctx, []byte{familyNode}, func(_ *pebble.Snapshot, r row) error {
		light, err := checkedRebuildLightNode(r, p.label)
		if err != nil || light.Kind != graph.KindMacro {
			return err
		}
		n, err := decodeNode(r.value)
		if err != nil || !macro(n) {
			return err
		}
		eligible = true
		if err := p.put(p.key('n', n.ID), r.value); err != nil {
			return err
		}
		return p.put(p.key('v', n.Name), nil)
	}); err != nil {
		return err
	}
	if eligible {
		if err := b.RebuildView().visit(ctx, []byte{familyEdge, edgeOut}, func(_ *pebble.Snapshot, r row) error {
			kind, err := rebuildAttributionEdgeKind(r.key)
			if err != nil || kind != graph.EdgeCalls {
				return err
			}
			e, err := guardLightEdge(r)
			if err != nil || e.To == "" {
				return err
			}
			key := p.key('n', e.To)
			if graph.IsUnresolvedTarget(e.To) {
				key = p.key('v', graph.UnresolvedName(e.To))
			}
			candidate, err := p.present(key)
			if err != nil || !candidate {
				return err
			}
			return p.put(append(p.column('i'), r.key...), nil)
		}); err != nil {
			return err
		}
	}
	return p.put(p.column('I'), nil)
}

// RebuildMacroExpansion adds proved missing Calls at framework position #31.
func (b *BuildStore) RebuildMacroExpansion(ctx context.Context, receipts iter.Seq2[ParseFacts, error], rules RebuildMacroExpansionRules) (stats RebuildMacroExpansionStats, err error) {
	defer func() {
		if err != nil {
			stats = RebuildMacroExpansionStats{}
			err = fmt.Errorf("%w: %w", ErrRebuildMacroExpansion, err)
		}
	}()
	p := rebuildMacroExpansion{rebuildCallMoves: rebuildMacroExpansionScratch(b), rules: rules}
	frozen, err := p.present(p.column('I'))
	if err != nil {
		return stats, err
	}
	if !frozen {
		return stats, invalid("macro expansion census missing")
	}
	defer func() {
		err = errors.Join(err, b.pending.DeleteRange(p.prefix, prefixEnd(p.prefix), pebble.NoSync))
	}()
	if rules.CFamily == nil || rules.Macro == nil || rules.Body == nil || rules.MayAdd == nil || rules.Add == nil || receipts == nil {
		return stats, invalid("macro expansion inputs")
	}
	uses, err := rebuildGuardHasPrefix(ctx, b.pending, p.column('i'))
	if err != nil || !uses {
		return stats, err
	}
	if err := p.collectBodies(ctx); err != nil {
		return stats, err
	}
	active, err := rebuildGuardHasPrefix(ctx, b.pending, p.column('a'))
	if err != nil || !active {
		return stats, err
	}
	if err := p.collectUses(ctx); err != nil {
		return stats, err
	}
	if err := p.plan(ctx); err != nil {
		return stats, err
	}
	ambiguous, err := rebuildGuardHasPrefix(ctx, b.pending, p.column('L'))
	if err != nil {
		return stats, err
	}
	if p.stats.Adds == 0 && !ambiguous {
		callers, err := rebuildGuardHasPrefix(ctx, b.pending, p.column('h'))
		if err != nil || !callers {
			return p.stats, err
		}
	}
	if ambiguous {
		if err := p.ambiguousCohorts(ctx, func(cohort []byte) error {
			return p.cohortMacros(ctx, cohort, func(id string) error {
				return p.put(p.key('m', id), []byte{rebuildCapabilityWanted})
			})
		}); err != nil {
			return stats, err
		}
	}
	if err := p.wantFinalProof(ctx); err != nil {
		return stats, err
	}
	if err := p.record(ctx, receipts); err != nil {
		return stats, err
	}
	if ambiguous {
		if err := p.proveAmbiguity(ctx); err != nil {
			return stats, err
		}
	}
	if err := p.prove(ctx); err != nil || p.stats.Adds == 0 {
		return p.stats, err
	}
	if !p.cProved {
		return stats, fmt.Errorf("%w: macro expansion lacks a C-family receipt", ErrRebuildUnknownProvenance)
	}
	return p.stats, p.write(ctx)
}

func (p *rebuildMacroExpansion) visit(ctx context.Context, key []byte, yield func(row) error) error {
	return pendingVisit(ctx, p.b.pending, key, yield)
}

func (p *rebuildMacroExpansion) macro(id string) (*graph.Node, error) {
	return pendingGet(p.b.pending, p.key('n', id), decodeNode)
}

func (p *rebuildMacroExpansion) body(ctx context.Context, id string, yield func(row, string) error) error {
	return p.visit(ctx, guardOutgoing(id, graph.EdgeCalls), func(r row) error {
		e, err := guardLightEdge(r)
		if err != nil {
			return err
		}
		if callee := p.rules.Body(e); callee != "" {
			if err := p.input(r.key, r.value); err != nil {
				return err
			}
			return yield(r, callee)
		}
		return nil
	})
}

func (p *rebuildMacroExpansion) collectBodies(ctx context.Context) error {
	return p.visit(ctx, p.column('n'), func(r row) error {
		n, err := decodeNode(r.value)
		if err != nil {
			return err
		}
		var active bool
		if err := p.body(ctx, n.ID, func(r row, callee string) error {
			active = true
			if err := p.put(p.key('b', n.ID, callee), nil); err != nil {
				return err
			}
			if err := p.put(append(p.column('c'), r.key...), nil); err != nil {
				return err
			}
			return p.claimInput(r.key)
		}); err != nil || !active {
			return err
		}
		if err := p.put(p.key('A', n.RepoPrefix, n.Name, n.ID), nil); err != nil {
			return err
		}
		return p.addUnique(p.key('a', n.RepoPrefix, n.Name), n.ID)
	})
}

func (p *rebuildMacroExpansion) claimInput(live []byte) error {
	raw, err := rebuildReceiptKey(p.b.pending, live)
	if errors.Is(err, ErrRebuildUnknownProvenance) {
		return nil
	}
	if err != nil {
		return err
	}
	if rawID, err := decodeLinkRecorder(raw); err != nil {
		return err
	} else if id, err := decodeLinkRecorder(live); err != nil {
		return err
	} else if rawID.Kind != id.Kind || rawID.From != id.From || rawID.FilePath != id.FilePath || rawID.Line != id.Line {
		return nil
	}
	key := append(p.column('r'), raw...)
	claim, err := p.marked(key)
	if err != nil || len(claim) > 1 && bytes.Equal(claim[1:], live) {
		return err
	}
	return p.claim(raw, live)
}

func (p *rebuildMacroExpansion) readCaller(id string) (*graph.Node, bool, error) {
	key := pendingNodeKey(id)
	var oversized bool
	n, err := pendingGet(p.b.pending, key, func(value []byte) (*graph.Node, error) {
		oversized = len(key)+len(value) > p.rowBytes
		return checkedRebuildLightNode(row{key, value}, p.label)
	})
	return n, oversized, err
}

func (p *rebuildMacroExpansion) collectUses(ctx context.Context) error {
	prefix := p.column('i')
	return p.visit(ctx, prefix, func(r row) error {
		old := r.key[len(prefix):]
		e, err := p.edge(old)
		if err != nil || e == nil {
			return err
		}
		if err := p.claimInput(old); err != nil || e.From == "" || e.To == "" {
			return err
		}
		var repo, name string
		var oversizedCaller bool
		unresolved := graph.IsUnresolvedTarget(e.To)
		if unresolved {
			if err := p.put(p.key('h', e.From), []byte{rebuildCapabilityWanted}); err != nil {
				return err
			}
			if err := p.put(append(p.key('j', e.From), old...), nil); err != nil {
				return err
			}
			caller, oversized, err := p.readCaller(e.From)
			if err != nil || caller == nil {
				return err
			}
			repo, name = caller.RepoPrefix, graph.UnresolvedName(e.To)
			oversizedCaller = oversized
		} else {
			n, err := p.macro(e.To)
			if err != nil || n == nil {
				return err
			}
			repo, name = n.RepoPrefix, n.Name
		}
		id, seen, err := p.unique(p.key('a', repo, name))
		if err != nil || !seen {
			return err
		}
		if oversizedCaller {
			return fmt.Errorf("%w: %s caller node", ErrRebuildLinkInputSize, p.label)
		}
		if id == "" {
			if err := p.put(p.key('L', repo, name), nil); err != nil {
				return err
			}
			return p.put(append(p.key('t', repo, name), old...), nil)
		}
		if !unresolved && id != e.To {
			return nil
		}
		p.stats.Sites++
		key := appendKeyLine(p.key('s', e.From, e.FilePath), e.Line)
		return p.put(append(appendKeyString(key, id), old...), []byte(id))
	})
}

func (p *rebuildMacroExpansion) plan(ctx context.Context) error {
	return p.visit(ctx, p.column('s'), func(r row) error {
		id := string(r.value)
		// s stores (caller, file, line, macro ID, live use key).
		_, rest, err := readKeyString(r.key[len(p.prefix)+1:])
		if err != nil {
			return err
		}
		_, rest, err = readKeyString(rest)
		if err != nil || len(rest) < 8 {
			return invalid("macro expansion site key")
		}
		_, old, err := readKeyString(rest[8:])
		if err != nil {
			return err
		}
		e, err := p.edge(old)
		if err != nil || e == nil {
			return errors.Join(err, invalid("macro expansion use missing"))
		}
		n, err := p.macro(id)
		if err != nil || n == nil {
			return errors.Join(err, invalid("macro expansion declaration missing"))
		}
		prefix := p.key('b', id)
		return p.visit(ctx, prefix, func(r row) error {
			callee, rest, err := readKeyString(r.key[len(prefix):])
			if err != nil || callee == "" || len(rest) != 0 {
				return invalid("macro expansion callee key")
			}
			if callee == e.From {
				return nil
			}
			add := p.rules.Add(e.From, callee, e.FilePath, e.Line, n.Name)
			key := pendingEdgeKey(add)
			existing, err := p.edge(key)
			if err != nil || !p.rules.MayAdd(existing) {
				return err
			}
			p.stats.Owned++
			if existing != nil {
				return nil // native AddBatch preserves an occupied macro key too
			}
			return p.offer(add, key, keyStrings(nil, string(old), n.ID, callee))
		})
	})
}

// Each saved identity needs its first position and last payload, not the
// proposals between them. Collect receipt demands after both are known.
func (p *rebuildMacroExpansion) wantFinalProof(ctx context.Context) error {
	for _, col := range []byte{'p', 'P'} {
		if err := p.visit(ctx, p.column(col), func(r row) error {
			_, rest, err := readKeyString(r.value)
			if err != nil {
				return err
			}
			id, _, err := readKeyString(rest)
			if err != nil {
				return err
			}
			n, err := p.macro(id)
			if err != nil || n == nil {
				return errors.Join(err, invalid("macro expansion proof declaration missing"))
			}
			if err := p.put(p.key('m', n.ID), []byte{rebuildCapabilityWanted}); err != nil {
				return err
			}
			return p.put(p.key('q', n.RepoPrefix, n.Name), nil)
		}); err != nil {
			return err
		}
	}
	return nil
}

func (p *rebuildMacroExpansion) offer(add *graph.Edge, key, proof []byte) error {
	ordinalKey := append(p.column('d'), key...)
	ordinal, err := p.get(ordinalKey)
	if err != nil {
		return err
	}
	col := byte('P')
	if ordinal == nil {
		col = 'p'
		ordinal = binary.BigEndian.AppendUint64(nil, p.stats.Adds)
		if err := p.put(ordinalKey, ordinal); err != nil {
			return err
		}
		p.stats.Adds++
	}
	value, err := encodeEdge(add)
	if err != nil {
		return err
	}
	if err := p.input(key, value); err != nil {
		return err
	}
	if err := p.put(append(p.column(col), key...), proof); err != nil {
		return err
	}
	return p.put(append(p.column('o'), ordinal...), value)
}

func (p *rebuildMacroExpansion) record(ctx context.Context, receipts iter.Seq2[ParseFacts, error]) error {
	for facts, err := range receipts {
		if err != nil {
			return err
		}
		file, language := pathkey.Normalize(facts.File.FilePath), facts.EffectiveLanguage
		for _, n := range facts.Nodes {
			if err := ctx.Err(); err != nil {
				return err
			}
			if !p.cProved && p.rules.CFamily(n.Language) {
				proved, err := p.b.rebuildReceiptLanguageMarker(n, file)
				if err != nil {
					return err
				}
				p.cProved = proved
			}
			if err := p.recordNode(ctx, n, file, language); err != nil {
				return err
			}
		}
		for _, e := range facts.Edges {
			if err := ctx.Err(); err != nil {
				return err
			}
			if e.Kind != graph.EdgeCalls {
				continue
			}
			key := append(p.column('r'), pendingEdgeKey(e)...)
			claim, err := p.marked(key)
			if err != nil {
				return err
			}
			if len(claim) > 1 {
				if err := noteRebuildScratchPayload(p.put, key, claim, rebuildScratchProofBit(language != "" && e.FilePath == file)); err != nil {
					return err
				}
			}
		}
	}
	prefix := p.column('c')
	return p.visit(ctx, prefix, func(r row) error {
		old := r.key[len(prefix):]
		exact, err := p.inputProved(old)
		if err != nil || !exact {
			return err
		}
		id, err := decodeLinkRecorder(old)
		if err != nil {
			return err
		}
		return p.put(p.key('e', id.From, id.To), nil)
	})
}

func (p *rebuildMacroExpansion) recordNode(ctx context.Context, n *graph.Node, file, language string) error {
	callerKey := p.key('h', n.ID)
	caller, err := rebuildScratchState(p.b.pending, callerKey, "macro expansion caller proof")
	if err != nil {
		return err
	}
	if caller != 0 {
		live, oversized, err := p.readCaller(n.ID)
		if err != nil {
			return err
		}
		admitted := language != "" && n.FilePath == file
		exact := admitted && live != nil && live.FilePath == file && n.RepoPrefix == live.RepoPrefix
		if admitted && (live == nil || n.RepoPrefix != live.RepoPrefix) {
			if err := p.noteRawCallerOutputs(ctx, n, oversized); err != nil {
				return err
			}
		}
		if err := noteRebuildScratchState(p.put, callerKey, caller, rebuildScratchProofBit(exact)); err != nil {
			return err
		}
	}
	macroKey := p.key('m', n.ID)
	claim, err := rebuildScratchState(p.b.pending, macroKey, "macro expansion declaration proof")
	if err != nil {
		return err
	}
	rawRole := n.Kind == graph.KindMacro && p.rules.Macro(n)
	if !rawRole && claim == 0 {
		return nil
	}
	frozen, err := p.macro(n.ID)
	if err != nil {
		return err
	}
	admitted := language != "" && n.FilePath == file
	exact := rawRole && admitted && frozen != nil && frozen.FilePath == file && n.RepoPrefix == frozen.RepoPrefix && n.Name == frozen.Name
	if claim != 0 {
		if err := noteRebuildScratchState(p.put, macroKey, claim, rebuildScratchProofBit(exact)); err != nil {
			return err
		}
	}
	if !rawRole || !admitted || exact {
		return nil
	}
	// A known raw declaration can make a chosen name ambiguous, even if
	// its live node had another name or no macro role at freeze.
	wanted, err := p.present(p.key('q', n.RepoPrefix, n.Name))
	if err != nil || !wanted {
		return err
	}
	return p.body(ctx, n.ID, func(_ row, _ string) error {
		return p.put(p.key('x', n.RepoPrefix, n.Name), nil)
	})
}

// A caller receipt in another repo can select a macro the live use missed.
// Check only its missing outputs; existing rows suppress native writes.
func (p *rebuildMacroExpansion) noteRawCallerOutputs(ctx context.Context, n *graph.Node, oversizedCaller bool) error {
	uses := p.key('j', n.ID)
	return p.visit(ctx, uses, func(r row) error {
		e, err := p.edge(r.key[len(uses):])
		if err != nil || e == nil {
			return errors.Join(err, invalid("macro expansion use missing"))
		}
		name := graph.UnresolvedName(e.To)
		id, _, err := p.unique(p.key('a', n.RepoPrefix, name))
		if err != nil || id == "" {
			return err
		}
		prefix := p.key('b', id)
		return p.visit(ctx, prefix, func(r row) error {
			callee, rest, err := readKeyString(r.key[len(prefix):])
			if err != nil || callee == "" || len(rest) != 0 {
				return invalid("macro expansion callee key")
			}
			if callee == e.From {
				return nil
			}
			add := p.rules.Add(e.From, callee, e.FilePath, e.Line, name)
			key := pendingEdgeKey(add)
			exists, err := p.present(key)
			if err != nil || exists {
				return err
			}
			if oversizedCaller {
				return fmt.Errorf("%w: %s caller node", ErrRebuildLinkInputSize, p.label)
			}
			return p.put(append(p.column('y'), key...), nil)
		})
	})
}

func (p *rebuildMacroExpansion) ambiguousCohorts(ctx context.Context, visit func([]byte) error) error {
	prefix := p.column('L')
	return p.visit(ctx, prefix, func(r row) error { return visit(r.key[len(prefix):]) })
}

func (p *rebuildMacroExpansion) cohortMacros(ctx context.Context, cohort []byte, visit func(string) error) error {
	prefix := append(p.column('A'), cohort...)
	return p.visit(ctx, prefix, func(r row) error {
		id, rest, err := readKeyString(r.key[len(prefix):])
		if err != nil || id == "" || len(rest) != 0 {
			return invalid("macro expansion cohort member")
		}
		return visit(id)
	})
}

func (p *rebuildMacroExpansion) proveAmbiguity(ctx context.Context) error {
	return p.ambiguousCohorts(ctx, func(cohort []byte) error {
		var exactID string
		var exactCount int
		if err := p.cohortMacros(ctx, cohort, func(id string) error {
			if exactCount == 2 {
				return nil
			}
			state, err := rebuildScratchState(p.b.pending, p.key('m', id), "macro expansion declaration proof")
			if err != nil || state != rebuildScratchExact {
				return err
			}
			body, err := rebuildGuardHasPrefix(ctx, p.b.pending, p.key('e', id))
			if err == nil && body {
				exactID = id
				exactCount++
			}
			return err
		}); err != nil || exactCount == 2 {
			return err
		}
		// Only a member that can be the sole survivor can change a use.
		return p.cohortMacros(ctx, cohort, func(id string) error {
			if exactCount == 1 && id != exactID {
				return nil
			}
			return p.noteAmbiguousOutputs(ctx, cohort, id)
		})
	})
}

func (p *rebuildMacroExpansion) noteAmbiguousOutputs(ctx context.Context, cohort []byte, id string) error {
	n, err := p.macro(id)
	if err != nil || n == nil {
		return errors.Join(err, invalid("macro expansion declaration missing"))
	}
	uses := append(p.column('t'), cohort...)
	return p.visit(ctx, uses, func(r row) error {
		e, err := p.edge(r.key[len(uses):])
		if err != nil || e == nil {
			return errors.Join(err, invalid("macro expansion use missing"))
		}
		if e.To != id && !graph.IsUnresolvedTarget(e.To) {
			return nil
		}
		return p.body(ctx, id, func(_ row, callee string) error {
			if callee == e.From {
				return nil
			}
			key := pendingEdgeKey(p.rules.Add(e.From, callee, e.FilePath, e.Line, n.Name))
			exists, err := p.present(key)
			if err != nil || exists {
				return err
			}
			return p.put(append(p.column('y'), key...), nil)
		})
	})
}

func (p *rebuildMacroExpansion) inputProved(old []byte) (bool, error) {
	raw, err := rebuildReceiptKey(p.b.pending, old)
	if errors.Is(err, ErrRebuildUnknownProvenance) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	claim, err := p.marked(append(p.column('r'), raw...))
	return len(claim) > 1 && claim[0] == rebuildScratchExact && bytes.Equal(claim[1:], old), err
}

func (p *rebuildMacroExpansion) prove(ctx context.Context) error {
	var refused rebuildScratchRefusals
	rawOnly := p.column('y')
	if err := p.visit(ctx, rawOnly, func(r row) error {
		key := r.key[len(rawOnly):]
		id, err := decodeLinkRecorder(key)
		if err == nil {
			refused.add(id, bytes.Clone(key))
		}
		return err
	}); err != nil {
		return err
	}
	prefix := p.column('p')
	if err := p.visit(ctx, prefix, func(r row) error {
		key := r.key[len(prefix):]
		alreadyRefused, err := p.present(append(rawOnly, key...))
		if err != nil || alreadyRefused {
			return err
		}
		first, err := p.proposalProved(r.value)
		if err != nil {
			return err
		}
		last, err := p.get(append(p.column('P'), key...))
		if err != nil {
			return err
		}
		lastProved := true
		if last != nil {
			lastProved, err = p.proposalProved(last)
			if err != nil {
				return err
			}
		}
		if first && lastProved {
			return nil
		}
		id, err := decodeLinkRecorder(key)
		if err == nil {
			refused.add(id, bytes.Clone(key))
		}
		return err
	}); err != nil {
		return err
	}
	return refused.err()
}

func (p *rebuildMacroExpansion) proposalProved(proof []byte) (bool, error) {
	old, rest, err := readKeyString(proof)
	if err != nil {
		return false, err
	}
	macro, rest, err := readKeyString(rest)
	if err != nil {
		return false, err
	}
	callee, rest, err := readKeyString(rest)
	if err != nil || len(rest) != 0 {
		return false, invalid("macro expansion output proof")
	}
	e, err := p.edge([]byte(old))
	if err != nil || e == nil {
		return false, errors.Join(err, invalid("macro expansion proof use missing"))
	}
	n, err := p.macro(macro)
	if err != nil || n == nil {
		return false, errors.Join(err, invalid("macro expansion proof declaration missing"))
	}
	return p.outputProved(e, n, callee, []byte(old))
}

func (p *rebuildMacroExpansion) outputProved(e *graph.Edge, n *graph.Node, callee string, old []byte) (bool, error) {
	if exact, err := p.inputProved(old); err != nil || !exact {
		return false, err
	}
	if state, err := rebuildScratchState(p.b.pending, p.key('m', n.ID), "macro expansion declaration proof"); err != nil || state != rebuildScratchExact {
		return false, err
	}
	if changed, err := p.present(p.key('x', n.RepoPrefix, n.Name)); err != nil || changed {
		return false, err
	}
	if graph.IsUnresolvedTarget(e.To) {
		if state, err := rebuildScratchState(p.b.pending, p.key('h', e.From), "macro expansion caller proof"); err != nil || state != rebuildScratchExact {
			return false, err
		}
	}
	return p.present(p.key('e', n.ID, callee))
}

func (p *rebuildMacroExpansion) write(ctx context.Context) (err error) {
	dir, err := os.MkdirTemp(filepath.Join(p.b.scratchParent, "pending"), "macro-expansion-")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(dir)) }()
	path := filepath.Join(dir, "adds.sst")
	if err := p.prepareRun(ctx, path); err != nil {
		return err
	}
	return p.b.withWrites(func() error {
		return visitLinkRun(ctx, path, nil, func(r row) error {
			e, err := decodeEdge(r.value)
			if err != nil {
				return err
			}
			return p.b.addEdgeWithCustody(e, rebuildEdgeCustody{proof: rebuildProofMacroExpansion})
		})
	})
}

func (p *rebuildMacroExpansion) prepareRun(ctx context.Context, path string) (err error) {
	w, err := newLinkTable(path)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, w.Close()) }()
	prefix := p.column('o')
	return p.visit(ctx, prefix, func(r row) error {
		return w.Set(r.key[len(prefix):], r.value)
	})
}
