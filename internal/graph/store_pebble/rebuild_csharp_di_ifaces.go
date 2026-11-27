package store_pebble

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"iter"
	"math"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/pathkey"
)

var ErrRebuildCSharpDIIfaces = errors.New("pebble rebuild C# DI interfaces failed")

var rebuildCSharpDIPrefix = []byte{'g', 'q', 'B'}

// RebuildCSharpDIRules carries the native registration and payload decisions.
type RebuildCSharpDIRules struct {
	Name     func(string) string
	Key      func(string, string, string) string
	Impl     func(*graph.Node) bool
	Iface    func(*graph.Node) bool
	Declared func(*graph.Edge) bool
	Emit     func(string, string, *graph.Node) *graph.Edge
}

type rebuildCSharpDI struct {
	rebuildCallMoves
	rules   RebuildCSharpDIRules
	adds    uint64
	refused rebuildScratchRefusals
}

// RebuildCSharpDIIfaces expands Autofac markers on the settled private graph.
// All receipts and planned winners are checked before any graph add.
func (b *BuildStore) RebuildCSharpDIIfaces(ctx context.Context, receipts iter.Seq2[ParseFacts, error],
	rules RebuildCSharpDIRules,
) (adds uint64, err error) {
	defer func() {
		if err != nil {
			adds = 0
			err = fmt.Errorf("%w: %w", ErrRebuildCSharpDIIfaces, err)
		}
	}()
	if receipts == nil || rules.Name == nil || rules.Key == nil || rules.Impl == nil || rules.Iface == nil || rules.Declared == nil || rules.Emit == nil {
		return 0, invalid("C# DI inputs")
	}
	prefix := rebuildCSharpDIPrefix
	busy, err := rebuildGuardHasPrefix(ctx, b.pending, prefix)
	if err != nil {
		return 0, err
	}
	if busy {
		return 0, invalid("C# DI scratch range occupied")
	}
	defer func() { err = errors.Join(err, b.pending.DeleteRange(prefix, prefixEnd(prefix), pebble.NoSync)) }()
	p := rebuildCSharpDI{
		rebuildCallMoves: rebuildCallMoves{
			b: b, prefix: prefix, rowBytes: 16 << 20, label: "C# DI interfaces", inputRow: "row",
		},
		rules: rules,
	}
	if err := p.captureProvides(ctx); err != nil {
		return 0, err
	}
	if err := p.captureCandidates(ctx); err != nil {
		return 0, err
	}
	have, err := rebuildGuardHasPrefix(ctx, b.pending, p.key('a'))
	if err != nil || !have {
		return 0, err
	}
	if err := p.record(ctx, receipts); err != nil {
		return 0, err
	}
	if err := p.plan(ctx); err != nil || p.adds == 0 {
		return 0, err
	}
	err = b.withWrites(func() error {
		return visitRebuildScratchKeyedEdges(ctx, b.pending, p.key('w'), 8, "C# DI output identity", func(e *graph.Edge) error {
			return b.addEdgeWithCustody(e, rebuildEdgeCustody{proof: rebuildProofUntrackedMove})
		})
	})
	return p.adds, err
}

func (p *rebuildCSharpDI) visit(ctx context.Context, prefix []byte, yield func(row) error) error {
	return pendingVisit(ctx, p.b.pending, prefix, yield)
}

func (p *rebuildCSharpDI) node(id string, reached bool) (*graph.Node, error) {
	key := pendingNodeKey(id)
	return pendingGet(p.b.pending, key, func(raw []byte) (*graph.Node, error) {
		if reached && len(key)+len(raw) > p.rowBytes {
			return nil, fmt.Errorf("%w: csharp DI node row", ErrRebuildLinkInputSize)
		}
		return checkedRebuildLightNode(row{key, raw}, p.label)
	})
}

func csharpDIBinding(e *graph.Edge) string {
	binding, _ := e.Meta[graph.MetaDIBinding].(string)
	return binding
}

func (p *rebuildCSharpDI) captureProvides(ctx context.Context) error {
	return p.b.RebuildView().visit(ctx, []byte{familyEdge, edgeOut}, func(snap *pebble.Snapshot, r row) error {
		kind, err := rebuildAttributionEdgeKind(r.key)
		if err != nil || kind != graph.EdgeProvides {
			return err
		}
		e, err := p.decodeKeyedEdge(r.key, r.value)
		if err != nil {
			return err
		}
		name := p.rules.Name(e.To)
		switch csharpDIBinding(e) {
		case graph.DIBindingUseClass:
			iface, _ := e.Meta[graph.MetaDIProvidesFor].(string)
			return p.put(p.key('u', p.rules.Key(e.From, iface, name)), nil)
		case graph.DIBindingAsImplemented:
			if name == "" {
				return nil
			}
			if err := p.input(r.key, r.value); err != nil {
				return err
			}
			if err := p.want(snap, r.key, name); err != nil {
				return err
			}
			return p.put(append(p.key('m', e.From, name), r.key...), nil)
		default:
			return nil
		}
	})
}

// k stores a live row's raw key and r requests that receipt. name is the
// original registration name for marker requests, empty for hierarchy.
func (p *rebuildCSharpDI) want(snap *pebble.Snapshot, live []byte, name string) error {
	raw, err := rebuildReceiptKey(snap, live)
	if errors.Is(err, ErrRebuildUnknownProvenance) {
		return nil
	}
	if err != nil {
		return err
	}
	id, err := decodeLinkRecorder(raw)
	if err != nil || (name == "" && id.Kind != graph.EdgeImplements) || (name != "" && id.Kind != graph.EdgeProvides) {
		return err
	}
	if err := p.put(append(p.key('k'), live...), raw); err != nil {
		return err
	}
	return p.put(append(p.key('r'), raw...), append([]byte{0}, name...))
}

func (p *rebuildCSharpDI) selectNode(ctx context.Context, name, repo string, accept func(*graph.Node) bool) (*graph.Node, error) {
	var selected *graph.Node
	ambiguous := false
	err := p.b.RebuildView().VisitNameCandidates(ctx, name, []string{"csharp"}, func(n *graph.Node) error {
		if n.RepoPrefix == repo && accept(n) {
			ambiguous = ambiguous || selected != nil
			selected = n
		}
		return nil
	})
	if err != nil || ambiguous {
		return nil, err
	}
	return selected, nil
}

// All markers with the same From and native impl name expand the same
// registration. Neither their receipt order nor site can affect the output.
func (p *rebuildCSharpDI) captureCandidates(ctx context.Context) error {
	prefix := p.key('m')
	return p.visit(ctx, prefix, func(r row) error {
		from, tail, err := readKeyString(r.key[len(prefix):])
		if err != nil {
			return err
		}
		name, _, err := readKeyString(tail)
		if err != nil {
			return err
		}
		reg := p.key('g', from, name)
		seen, err := p.present(reg)
		if err != nil || seen {
			return err
		}
		if err := p.put(reg, nil); err != nil {
			return err
		}
		source, err := p.node(from, false)
		if err != nil {
			return err
		}
		var repo string
		if source != nil {
			repo = source.RepoPrefix
		}
		impl, err := p.selectNode(ctx, name, repo, p.rules.Impl)
		if err != nil || impl == nil {
			return err
		}
		prior := keyStrings([]byte{familyEdge, edgeOut}, impl.ID, string(graph.EdgeImplements))
		return p.b.RebuildView().visit(ctx, prior, func(snap *pebble.Snapshot, r row) error {
			e, err := p.decodeKeyedEdge(r.key, r.value)
			if err != nil || !p.rules.Declared(e) {
				return err
			}
			var iface *graph.Node
			if graph.IsUnresolvedTarget(e.To) {
				if n := graph.UnresolvedName(e.To); n != "" {
					iface, err = p.selectNode(ctx, n, impl.RepoPrefix, p.rules.Iface)
				}
			} else {
				iface, err = p.node(e.To, false)
			}
			if err != nil || !p.rules.Iface(iface) {
				return err
			}
			bound, err := p.present(p.key('u', p.rules.Key(from, iface.Name, name)))
			if err != nil || bound {
				return err
			}
			// Only check the rows of a registration that can offer an edge.
			for _, id := range [...]string{from, impl.ID, iface.ID} {
				if _, err := p.node(id, true); err != nil {
					return err
				}
			}
			if err := p.input(r.key, r.value); err != nil {
				return err
			}
			if err := p.want(snap, r.key, ""); err != nil {
				return err
			}
			if source != nil {
				if err := p.put(p.key('s', from), []byte{0}); err != nil {
					return err
				}
			}
			for _, id := range [...]string{impl.ID, iface.ID} {
				if err := p.put(p.key('n', id), []byte{0}); err != nil {
					return err
				}
			}
			key := p.key('a', from, name, string(r.key))
			return p.put(key, keyStrings(nil, impl.ID, iface.ID))
		})
	})
}

func (p *rebuildCSharpDI) record(ctx context.Context, receipts iter.Seq2[ParseFacts, error]) error {
	var ordinal uint64
	for facts, err := range receipts {
		if err != nil {
			return err
		}
		file, lang := pathkey.Normalize(facts.File.FilePath), facts.EffectiveLanguage
		for _, n := range facts.Nodes {
			if err := ctx.Err(); err != nil {
				return err
			}
			key, source := p.key('n', n.ID), p.key('s', n.ID)
			wanted, err := p.present(key)
			if err != nil {
				return err
			}
			sourceWanted, err := p.present(source)
			if err != nil {
				return err
			}
			if !wanted && !sourceWanted {
				continue
			}
			live, err := p.node(n.ID, true)
			if err != nil {
				return err
			}
			if sourceWanted {
				good := lang != "" && n.FilePath == file && live != nil && live.RepoPrefix == n.RepoPrefix
				state, err := rebuildScratchState(p.b.pending, source, "csharp DI source proof")
				if err != nil {
					return err
				}
				if err := noteRebuildScratchState(p.put, source, state, rebuildScratchProofBit(good)); err != nil {
					return err
				}
			}
			if !wanted {
				continue
			}
			good := lang != "" && sameRebuildLanguageMarker(n, live, file) && live.Kind == n.Kind && live.Name == n.Name &&
				live.RepoPrefix == n.RepoPrefix && (!p.rules.Iface(live) || live.StartLine == n.StartLine)
			state, err := rebuildScratchState(p.b.pending, key, "C# DI node proof")
			if err != nil {
				return err
			}
			if err := noteRebuildScratchState(p.put, key, state, rebuildScratchProofBit(good)); err != nil {
				return err
			}
		}
		for _, e := range facts.Edges {
			if err := ctx.Err(); err != nil {
				return err
			}
			if lang != "" && e.FilePath == file && (e.Kind == graph.EdgeProvides || e.Kind == graph.EdgeImplements) {
				key := append(p.key('r'), pendingEdgeKey(e)...)
				claim, err := p.get(key)
				if err != nil {
					return err
				}
				if len(claim) != 0 && claim[0] == 0 {
					match := p.rules.Declared(e) || (csharpDIBinding(e) == graph.DIBindingAsImplemented && p.rules.Name(e.To) == string(claim[1:]))
					if match {
						if err := p.put(key, binary.BigEndian.AppendUint64([]byte{1}, ordinal)); err != nil {
							return err
						}
					}
				}
			}
			ordinal++
		}
	}
	return nil
}

func (p *rebuildCSharpDI) order(live []byte) (uint64, error) {
	raw, err := p.get(append(p.key('k'), live...))
	if err != nil || raw == nil {
		return math.MaxUint64, err
	}
	claim, err := p.get(append(p.key('r'), raw...))
	if err != nil || len(claim) != 9 || claim[0] != 1 {
		return math.MaxUint64, err
	}
	return binary.BigEndian.Uint64(claim[1:]), nil
}

func (p *rebuildCSharpDI) plan(ctx context.Context) error {
	prefix := p.key('a')
	if err := p.visit(ctx, prefix, func(r row) error {
		from, tail, err := readKeyString(r.key[len(prefix):])
		if err != nil {
			return err
		}
		name, tail, err := readKeyString(tail)
		if err != nil {
			return err
		}
		site, _, err := readKeyString(tail)
		if err != nil {
			return err
		}
		id, err := decodeLinkRecorder([]byte(site))
		if err != nil {
			return err
		}
		ordinal, err := p.order([]byte(site))
		if err != nil {
			return err
		}
		// SQLite GetOutEdges explicitly sorts by line; rowid ties use the
		// frozen receipt order. Different registrations cannot share a key.
		key := binary.BigEndian.AppendUint64(appendKeyLine(p.key('o', from, name), id.Line), ordinal)
		return p.put(append(key, site...), r.value)
	}); err != nil {
		return err
	}
	prefix = p.key('o')
	if err := p.visit(ctx, prefix, func(r row) error {
		from, tail, err := readKeyString(r.key[len(prefix):])
		if err != nil {
			return err
		}
		name, tail, err := readKeyString(tail)
		if err != nil {
			return err
		}
		impl, value, err := readKeyString(r.value)
		if err != nil {
			return err
		}
		ifaceID, _, err := readKeyString(value)
		if err != nil {
			return err
		}
		iface, err := p.node(ifaceID, true)
		if err != nil || iface == nil {
			return errors.Join(err, invalid("C# DI interface disappeared"))
		}
		line, order := tail[:8], binary.BigEndian.Uint64(tail[8:16])
		return p.offer(ctx, p.rules.Emit(from, name, iface), from, name, impl, iface, line, order)
	}); err != nil {
		return err
	}
	return p.refused.err()
}

func (p *rebuildCSharpDI) offer(ctx context.Context, e *graph.Edge, from, name, impl string, iface *graph.Node, line []byte, ordinal uint64) error {
	out := pendingEdgeKey(e)
	semantic := p.key('y', p.rules.Key(from, iface.Name, name))
	physical := append(p.key('d'), out...)
	raw, err := encodeEdge(e)
	if err != nil {
		return err
	}
	occupied, err := p.present(out)
	if err != nil {
		return err
	}
	physicalWinner, err := p.get(physical)
	if err != nil {
		return err
	}
	prior, err := p.get(semantic)
	if err != nil {
		return err
	}
	const exact, installed = byte(1), byte(2)
	if prior != nil {
		if bytes.Equal(raw, prior[9:]) {
			return nil
		}
		unknownOrder := ordinal == math.MaxUint64 && bytes.Equal(line, prior[1:9])
		if !occupied && (prior[0]&exact == 0 || unknownOrder) {
			p.refused.add(graph.EdgeIdentityFor(e), out)
		} else if occupied && unknownOrder && prior[0]&installed != 0 {
			// An occupied offer still claims the semantic slot. If it ran
			// first, the free row already planned for that slot would vanish.
			winner, err := decodeEdge(prior[9:])
			if err != nil {
				return err
			}
			p.refused.add(graph.EdgeIdentityFor(winner), pendingEdgeKey(winner))
		}
		return nil
	}
	proved, err := p.prove(ctx, from, name, impl, iface.ID)
	if err != nil {
		return err
	}
	proved = proved && ordinal != math.MaxUint64
	var state byte
	if proved {
		state |= exact
		if !occupied && physicalWinner == nil {
			state |= installed
		}
	}
	// Native records semantic dedup even when the physical key is blocked.
	if err := p.put(semantic, append(append([]byte{state}, line...), raw...)); err != nil {
		return err
	}
	if occupied {
		return nil
	}
	if physicalWinner != nil {
		if ordinal == math.MaxUint64 && bytes.Equal(line, physicalWinner[:8]) && !bytes.Equal(raw, physicalWinner[8:]) {
			p.refused.add(graph.EdgeIdentityFor(e), out)
		}
		return nil
	}
	if !proved {
		p.refused.add(graph.EdgeIdentityFor(e), out)
		return nil
	}
	if err := p.put(physical, append(bytes.Clone(line), raw...)); err != nil {
		return err
	}
	key := binary.BigEndian.AppendUint64(p.key('w'), p.adds)
	p.adds++
	return p.put(append(key, out...), raw)
}

func (p *rebuildCSharpDI) prove(ctx context.Context, from, name, impl, iface string) (bool, error) {
	// A missing registration source selects the empty repo natively.
	found, err := p.present(pendingNodeKey(from))
	if err != nil {
		return false, err
	}
	if found {
		state, err := rebuildScratchState(p.b.pending, p.key('s', from), "csharp DI source proof")
		if err != nil || state != rebuildCapabilityProved {
			return false, err
		}
	}
	for _, id := range [...]string{impl, iface} {
		state, err := rebuildScratchState(p.b.pending, p.key('n', id), "C# DI declaration proof")
		if err != nil || state != rebuildCapabilityProved {
			return false, err
		}
	}
	key := p.key('p', from, name)
	state, err := rebuildScratchState(p.b.pending, key, "C# DI registration proof")
	if err != nil || state != 0 {
		return state == 1, err
	}
	prefix := p.key('m', from, name)
	proved := false
	if err := p.visit(ctx, prefix, func(r row) error {
		if proved {
			return nil
		}
		order, err := p.order(r.key[len(prefix):])
		proved = order != math.MaxUint64
		return err
	}); err != nil {
		return false, err
	}
	if proved {
		state = 1
	} else {
		state = 2
	}
	return proved, p.put(key, []byte{state})
}
