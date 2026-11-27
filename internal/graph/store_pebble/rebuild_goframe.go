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

var ErrRebuildGoFrame = errors.New("pebble rebuild GoFrame route calls failed")

var rebuildGoFramePrefix = []byte{'g', 'q', 'j'}

// RebuildGoFrameRules supplies the native route/handler readers and payload.
type RebuildGoFrameRules struct {
	Route   func(*graph.Edge) (request, pkg, route string)
	Handler func(*graph.Node) (request, pkg string, bound bool)
	Dir     func(string) string
	Rank    func(source *graph.Node, routePkg, routeDir, workspace, pkg, dir string, bound bool) int
	Rebind  func(*graph.Edge, string, *graph.Node) (graph.EdgeReindex, *graph.Edge)
}

type rebuildGoFrame struct {
	rebuildCallMoves
	rules RebuildGoFrameRules
}

// RebuildGoFrame reconciles the live tagged Calls, then offers their mirrors.
func (b *BuildStore) RebuildGoFrame(ctx context.Context, receipts iter.Seq2[ParseFacts, error], rules RebuildGoFrameRules) (moves uint64, err error) {
	defer func() {
		if err != nil {
			moves = 0
			err = fmt.Errorf("%w: %w", ErrRebuildGoFrame, err)
		}
	}()
	if receipts == nil || rules.Route == nil || rules.Handler == nil || rules.Dir == nil || rules.Rank == nil || rules.Rebind == nil {
		return 0, invalid("goframe inputs")
	}
	prefix := rebuildGoFramePrefix
	busy, err := rebuildGuardHasPrefix(ctx, b.pending, prefix)
	if err != nil {
		return 0, err
	}
	if busy {
		return 0, invalid("goframe scratch range occupied")
	}
	defer func() { err = errors.Join(err, b.pending.DeleteRange(prefix, prefixEnd(prefix), pebble.NoSync)) }()
	p := rebuildGoFrame{
		rebuildCallMoves: rebuildCallMoves{b: b, prefix: prefix, rowBytes: 16 << 20, label: "GoFrame", inputRow: "input row"},
		rules:            rules,
	}
	// Index tagged receipt keys before scanning live claims. Untagged moves
	// do not need scratch state unless a tagged receipt can reach them.
	if err := visitGoFrameReceipts(ctx, receipts, func(e *graph.Edge) error {
		request, _, _ := p.rules.Route(e)
		if request == "" {
			return nil
		}
		return p.put(append(p.column('q'), pendingEdgeKey(e)...), nil)
	}); err != nil {
		return 0, err
	}
	if err := p.capture(ctx); err != nil {
		return 0, err
	}
	if err := p.record(ctx, receipts); err != nil {
		return 0, err
	}
	moves, err = p.plan(ctx)
	if err != nil || moves == 0 {
		return moves, err
	}
	if err := p.order(ctx, receipts); err != nil {
		return 0, err
	}
	return moves, b.withWrites(func() error {
		if err := p.apply(ctx, moves); err != nil {
			return err
		}
		return visitRebuildScratchKeyedEdges(ctx, b.pending, p.column('w'), 0, "goframe mirror key", func(e *graph.Edge) error {
			occupied, err := p.present(pendingEdgeKey(e))
			if err != nil || occupied {
				return err
			}
			return b.addEdge(e)
		})
	})
}

func (p *rebuildGoFrame) visit(ctx context.Context, prefix []byte, visit func(row) error) error {
	return pendingVisit(ctx, p.b.pending, prefix, visit)
}

func (p *rebuildGoFrame) saveHandler(col byte, n *graph.Node) error {
	if n.Kind != graph.KindMethod && n.Kind != graph.KindFunction {
		return nil
	}
	request, pkg, bound := p.rules.Handler(n)
	if request == "" {
		return nil
	}
	var boundByte byte
	if bound {
		boundByte = 1
	}
	if col == 'h' {
		if err := p.put(p.key('i', n.ID), nil); err != nil {
			return err
		}
	}
	return p.put(p.key(col, request, n.ID), keyStrings([]byte{boundByte}, pkg, n.WorkspaceID, p.rules.Dir(n.FilePath)))
}

func (p *rebuildGoFrame) capture(ctx context.Context) error {
	view := p.b.RebuildView()
	if err := view.visit(ctx, []byte{familyNode}, func(_ *pebble.Snapshot, r row) error {
		n, err := checkedRebuildLightNode(r, p.label)
		if err != nil || n.Kind != graph.KindMethod && n.Kind != graph.KindFunction {
			return err
		}
		if len(r.key)+len(r.value) > p.rowBytes {
			return fmt.Errorf("%w: goframe handler node row", ErrRebuildLinkInputSize)
		}
		n, err = decodeNode(r.value)
		if err != nil {
			return err
		}
		return p.saveHandler('h', n)
	}); err != nil {
		return err
	}
	return view.visit(ctx, []byte{familyEdge, edgeOut}, func(snap *pebble.Snapshot, r row) error {
		kind, err := rebuildAttributionEdgeKind(r.key)
		if err != nil || kind != graph.EdgeCalls {
			return err
		}
		custody, err := readRebuildEdgeCustody(snap, r.key)
		if err != nil && !errors.Is(err, ErrRebuildUnknownProvenance) {
			return err
		}
		var raw []byte
		if err == nil && custody.proof == 0 {
			raw = custody.raw
			if raw == nil {
				raw = r.key
			}
		}
		var taggedReceipt bool
		if raw != nil {
			taggedReceipt, err = p.present(append(p.column('q'), raw...))
			if err != nil {
				return err
			}
		}
		e, err := p.decodeKeyedEdge(r.key, r.value)
		if err != nil {
			return err
		}
		request, _, _ := p.rules.Route(e)
		if request == "" && !taggedReceipt {
			return nil
		}
		if err := p.input(r.key, r.value); err != nil {
			return err
		}
		if err := p.put(append(p.column('s'), r.key...), raw); err != nil {
			return err
		}
		if raw == nil {
			return nil
		}
		if taggedReceipt {
			if err := p.put(p.key('f', e.From), nil); err != nil {
				return err
			}
		}
		return p.claim(raw, r.key)
	})
}

// visitGoFrameReceipts keeps role discovery, recording and ordering on the same
// receipt owner and language rules.
func visitGoFrameReceipts(ctx context.Context, receipts iter.Seq2[ParseFacts, error], visit func(*graph.Edge) error) error {
	for facts, err := range receipts {
		if err != nil {
			return err
		}
		if facts.EffectiveLanguage == "" {
			continue
		}
		if err := visitGoFrameReceiptCalls(ctx, facts.Edges, pathkey.Normalize(facts.File.FilePath), visit); err != nil {
			return err
		}
	}
	return nil
}

func visitGoFrameReceiptCalls(ctx context.Context, edges []*graph.Edge, file string, visit func(*graph.Edge) error) error {
	for _, e := range edges {
		if err := ctx.Err(); err != nil {
			return err
		}
		if e != nil && e.Kind == graph.EdgeCalls && e.FilePath == file {
			if err := visit(e); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *rebuildGoFrame) record(ctx context.Context, receipts iter.Seq2[ParseFacts, error]) error {
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
			source, err := p.present(p.key('f', n.ID))
			if err != nil {
				return err
			}
			if source {
				if err := p.put(p.key('F', n.ID), append([]byte{1}, n.WorkspaceID...)); err != nil {
					return err
				}
			}
			request, _, _ := p.rules.Handler(n)
			known, err := p.present(p.key('i', n.ID))
			if err != nil {
				return err
			}
			if known || request != "" && (n.Kind == graph.KindMethod || n.Kind == graph.KindFunction) {
				role := &graph.Node{ID: n.ID, Kind: n.Kind, WorkspaceID: n.WorkspaceID, FilePath: n.FilePath}
				if request != "" {
					_, pkg, bound := p.rules.Handler(n)
					role.Meta = map[string]any{"goframe_request_type": request, "goframe_request_pkg": pkg, "goframe_bound": bound}
				}
				data, err := encodeNode(role)
				if err != nil {
					return err
				}
				if err := p.put(p.key('I', n.ID), data); err != nil {
					return err
				}
			}
		}
		if err := visitGoFrameReceiptCalls(ctx, facts.Edges, file, p.recordCall); err != nil {
			return err
		}
	}
	return p.visit(ctx, p.column('I'), func(r row) error {
		n, err := decodeNode(r.value)
		if err != nil {
			return err
		}
		return p.saveHandler('H', n)
	})
}

// recordCall retains the first payload at a relevant raw key. A later receipt
// can disprove its route role, but cannot replace that payload or its position.
func (p *rebuildGoFrame) recordCall(e *graph.Edge) error {
	raw := pendingEdgeKey(e)
	claimed, err := p.present(append(p.column('r'), raw...))
	if err != nil || !claimed {
		return err
	}
	key := append(p.column('j'), raw...)
	first, err := pendingGet(p.b.pending, key, decodeEdge)
	if err != nil {
		return err
	}
	if first != nil {
		if !p.sameRouteRole(first, e) {
			return p.put(append(p.column('d'), raw...), nil)
		}
		return nil
	}
	if request, _, _ := p.rules.Route(e); request == "" {
		e = &graph.Edge{Kind: graph.EdgeCalls}
	}
	data, err := encodeEdge(e)
	if err != nil {
		return err
	}
	return p.put(key, data)
}

func (p *rebuildGoFrame) sameRouteRole(a, b *graph.Edge) bool {
	ar, ap, am := p.rules.Route(a)
	br, bp, bm := p.rules.Route(b)
	return ar == br && (ar == "" || ap == bp && am == bm)
}

type rebuildGoFrameWinner struct {
	id    string
	rank  int
	count int
}

func (w *rebuildGoFrameWinner) offer(id string, rank int) {
	if w.count == 0 || rank > w.rank {
		w.id, w.rank, w.count = id, rank, 1
	} else if rank == w.rank {
		w.count = 2
	}
}

func (w rebuildGoFrameWinner) node() *graph.Node {
	if w.count == 1 {
		return &graph.Node{ID: w.id}
	}
	return nil
}

func (p *rebuildGoFrame) elect(ctx context.Context, col byte, request, pkg, dir string, source *graph.Node, skipKnown bool) (rebuildGoFrameWinner, error) {
	var best rebuildGoFrameWinner
	prefix := p.key(col, request)
	err := p.visit(ctx, prefix, func(r row) error {
		id, _, err := readKeyString(r.key[len(prefix):])
		if err != nil || len(r.value) == 0 {
			return errors.Join(err, invalid("goframe handler posting"))
		}
		if skipKnown {
			known, err := p.present(p.key('I', id))
			if err != nil || known {
				return err
			}
		}
		candidatePkg, tail, err := readKeyString(r.value[1:])
		if err != nil {
			return err
		}
		workspace, tail, err := readKeyString(tail)
		if err != nil {
			return err
		}
		candidateDir, _, err := readKeyString(tail)
		if err != nil {
			return err
		}
		if rank := p.rules.Rank(source, pkg, dir, workspace, candidatePkg, candidateDir, r.value[0] != 0); rank >= 0 {
			best.offer(id, rank)
		}
		return nil
	})
	return best, err
}

func (p *rebuildGoFrame) plan(ctx context.Context) (uint64, error) {
	liveOn, err := rebuildGuardHasPrefix(ctx, p.b.pending, p.column('h'))
	if err != nil {
		return 0, err
	}
	rawOn, err := rebuildGuardHasPrefix(ctx, p.b.pending, p.column('H'))
	if err != nil {
		return 0, err
	}
	// The native nonempty guard is global, including other request types.
	var optionalCount int
	if !rawOn && liveOn {
		prefix := p.column('i')
		if err := p.visit(ctx, prefix, func(r row) error {
			if optionalCount == 2 {
				return nil
			}
			id, _, err := readKeyString(r.key[len(prefix):])
			if err != nil {
				return err
			}
			known, err := p.present(p.key('I', id))
			if !known {
				optionalCount++
			}
			return err
		}); err != nil {
			return 0, err
		}
	}
	var moves uint64
	var refused rebuildScratchRefusals
	prefix := p.column('s')
	err = p.visit(ctx, prefix, func(r row) error {
		key := r.key[len(prefix):]
		live, err := p.edge(key)
		if err != nil || live == nil {
			return errors.Join(err, invalid("goframe call disappeared"))
		}
		source, err := pendingGet(p.b.pending, pendingNodeKey(live.From), decodeNodeLight)
		if err != nil {
			return err
		}
		var move graph.EdgeReindex
		var mirror *graph.Edge
		if request, pkg, _ := p.rules.Route(live); liveOn && request != "" {
			winner, err := p.elect(ctx, 'h', request, pkg, p.rules.Dir(live.FilePath), source, false)
			if err != nil {
				return err
			}
			move, mirror = p.rules.Rebind(live, request, winner.node())
		}
		proved, err := p.compareCall(ctx, r.value, live, source, rawOn, optionalCount, move.Edge, mirror)
		if err != nil {
			return err
		}
		if !proved {
			refused.add(graph.EdgeIdentityFor(live), bytes.Clone(key))
			return nil
		}
		if move.Edge == nil {
			return nil
		}
		if err := p.stage(key, move.Edge); err != nil {
			return err
		}
		moves++
		if mirror == nil {
			return nil
		}
		payload, err := encodeEdge(mirror)
		if err != nil {
			return err
		}
		return p.put(append(p.column('w'), pendingEdgeKey(mirror)...), payload)
	})
	return moves, errors.Join(err, refused.err())
}

// compareCall tests the raw role at the current turn's site and target. A
// shared receipt can be attached to either claimant only when neither world
// changes the output; it can never authorize a move on either row.
func (p *rebuildGoFrame) compareCall(ctx context.Context, rawKey []byte, live *graph.Edge, source *graph.Node,
	rawOn bool, optionalCount int, move, mirror *graph.Edge,
) (bool, error) {
	var raw *graph.Edge
	var ambiguous bool
	if len(rawKey) != 0 {
		claim, err := p.marked(append(p.column('r'), rawKey...))
		if err != nil {
			return false, err
		}
		ambiguous = len(claim) == 1
		conflict, err := p.present(append(p.column('d'), rawKey...))
		if err != nil || conflict {
			return false, err
		}
		raw, err = pendingGet(p.b.pending, append(p.column('j'), rawKey...), decodeEdge)
		if err != nil {
			return false, err
		}
	}
	if move != nil && (raw == nil || ambiguous) {
		return false, nil
	}
	if raw == nil {
		raw = live
	} else {
		// The receipt supplies the first role. Earlier turns supply the
		// current key; the original raw key only determines move order.
		raw.From, raw.To, raw.FilePath, raw.Line = live.From, live.To, live.FilePath, live.Line
	}
	workspace, err := p.get(p.key('F', raw.From))
	if err != nil {
		return false, err
	}
	return p.matches(ctx, raw, workspace, source, rawOn, optionalCount, move, mirror)
}

func (p *rebuildGoFrame) matches(ctx context.Context, raw *graph.Edge, workspace []byte, source *graph.Node,
	rawOn bool, optionalCount int, live, mirror *graph.Edge,
) (bool, error) {
	request, pkg, _ := p.rules.Route(raw)
	if request == "" || !rawOn && optionalCount == 0 {
		return live == nil, nil
	}
	if !rawOn && live != nil {
		return false, nil // without an exact handler, the entire turn can be inert
	}
	sources := [2]*graph.Node{source, nil}
	count := 1
	if workspace != nil {
		sources[0] = &graph.Node{WorkspaceID: string(workspace[1:])}
	} else if source != nil {
		count = 2 // native admits every workspace if the source is absent
	}
	for _, boundary := range sources[:count] {
		dir := p.rules.Dir(raw.FilePath)
		winner, err := p.elect(ctx, 'H', request, pkg, dir, boundary, false)
		if err != nil {
			return false, err
		}
		optional, err := p.elect(ctx, 'h', request, pkg, dir, boundary, true)
		if err != nil {
			return false, err
		}
		if !rawOn && optionalCount == 1 {
			// The sole optional handler must be present for the turn to
			// run at all. Compare its active world as well as the no-op.
			winner = optional
		} else if optional.count != 0 && (winner.count == 0 || optional.rank > winner.rank || optional.rank == winner.rank && winner.count == 1) {
			return false, nil
		}
		move, reflected := p.rules.Rebind(raw, request, winner.node())
		for _, pair := range [...][2]*graph.Edge{{live, move.Edge}, {mirror, reflected}} {
			equal, err := rebuildGoFrameEdgeEqual(pair[0], pair[1])
			if err != nil || !equal {
				return false, err
			}
		}
	}
	return true, nil
}

func rebuildGoFrameEdgeEqual(a, b *graph.Edge) (bool, error) {
	if a == nil || b == nil {
		return a == b, nil
	}
	x, err := encodeEdge(a)
	if err != nil {
		return false, err
	}
	y, err := encodeEdge(b)
	return bytes.Equal(x, y), err
}

func (p *rebuildGoFrame) order(ctx context.Context, receipts iter.Seq2[ParseFacts, error]) error {
	return visitGoFrameReceipts(ctx, receipts, func(raw *graph.Edge) error {
		key := append(p.column('r'), pendingEdgeKey(raw)...)
		claim, err := p.marked(key)
		if err != nil || len(claim) <= 1 {
			return err
		}
		first, err := pendingGet(p.b.pending, append(p.column('j'), key[len(p.prefix)+1:]...), decodeEdge)
		if err != nil {
			return err
		}
		return p.noteReceipt(key, claim, first != nil && p.sameRouteRole(first, raw))
	})
}
