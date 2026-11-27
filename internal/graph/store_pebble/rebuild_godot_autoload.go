package store_pebble

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"iter"
	"strings"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/pathkey"
)

var ErrRebuildGodotAutoload = errors.New("pebble rebuild Godot autoload failed")

var rebuildGodotAutoloadPrefix = []byte{'g', 'q', 'O'}

// RebuildGodotAutoloadRules keeps the native node and Call predicates in the resolver.
type RebuildGodotAutoloadRules struct {
	Decl            func(*graph.Node) (name, script string)
	MemberFile      func(*graph.Node) (string, bool)
	Call            func(*graph.Edge) (receiver, member string)
	Rebind          func(*graph.Edge, string) graph.EdgeReindex
	DeclCandidate   func(*graph.Node) bool
	ScriptFile      func(*graph.Node) bool
	MemberKinds     [2]graph.NodeKind
	ReceiptLanguage func(string) bool
}

type rebuildGodotAutoload struct {
	rebuildGodotScripts
	decl func(*graph.Node) (name, script string)
}

// The two Godot Call turns use the same script, member and Call proofs.
type rebuildGodotScripts struct {
	rebuildCallMoves
	memberFileRule  func(*graph.Node) (string, bool)
	callKey         func(*graph.Edge) (receiver, member string)
	declCandidate   func(*graph.Node) bool
	scriptFile      func(*graph.Node) bool
	memberKinds     [2]graph.NodeKind
	receiptLanguage func(string) bool
}

// RebuildGodotAutoload binds the live residual Calls. It compares the exact
// live and receipt-proved winners before applying any moves.
func (b *BuildStore) RebuildGodotAutoload(ctx context.Context, receipts iter.Seq2[ParseFacts, error], rules RebuildGodotAutoloadRules) (count uint64, err error) {
	defer func() {
		if err != nil {
			count = 0
			err = fmt.Errorf("%w: %w", ErrRebuildGodotAutoload, err)
		}
	}()
	if receipts == nil || rules.Decl == nil || rules.MemberFile == nil || rules.Call == nil || rules.Rebind == nil ||
		rules.DeclCandidate == nil || rules.ScriptFile == nil || rules.ReceiptLanguage == nil ||
		rules.MemberKinds[0] == "" || rules.MemberKinds[1] == "" {
		return 0, invalid("godot autoload inputs")
	}
	prefix := rebuildGodotAutoloadPrefix
	busy, err := rebuildGuardHasPrefix(ctx, b.pending, prefix)
	if err != nil {
		return 0, err
	}
	if busy {
		return 0, invalid("godot autoload scratch occupied")
	}
	defer func() {
		err = errors.Join(err, b.pending.DeleteRange(prefix, prefixEnd(prefix), pebble.NoSync))
	}()
	p := rebuildGodotAutoload{
		rebuildGodotScripts: rebuildGodotScripts{
			rebuildCallMoves: rebuildCallMoves{b: b, prefix: prefix, rowBytes: 16 << 20, label: "godot autoload", inputRow: "input row"},
			memberFileRule:   rules.MemberFile,
			callKey:          rules.Call,
			declCandidate:    rules.DeclCandidate,
			scriptFile:       rules.ScriptFile,
			memberKinds:      rules.MemberKinds,
			receiptLanguage:  rules.ReceiptLanguage,
		},
		decl: rules.Decl,
	}
	return p.run(ctx, receipts, p.noteDecl, p.target, rules.Rebind)
}

func (p *rebuildGodotScripts) visitNodes(ctx context.Context, visit func(*graph.Node, row) error) error {
	return p.b.RebuildView().visit(ctx, []byte{familyNode}, func(_ *pebble.Snapshot, r row) error {
		n, err := checkedRebuildLightNode(r, p.label)
		if err != nil {
			return err
		}
		return visit(n, r)
	})
}

func (p *rebuildGodotAutoload) noteDecl(n *graph.Node, col byte) (string, func() error) {
	name, script := p.decl(n)
	if script == "" {
		return "", nil
	}
	return script, func() error { return p.addUnique(p.key(col, name), script) }
}

func (p *rebuildGodotScripts) run(ctx context.Context, receipts iter.Seq2[ParseFacts, error], noteDecl func(*graph.Node, byte) (string, func() error),
	target func(*graph.Edge, string, string, bool) (string, error), rebind func(*graph.Edge, string) graph.EdgeReindex,
) (uint64, error) {
	if err := p.visitNodes(ctx, func(n *graph.Node, r row) error {
		if !p.declCandidate(n) {
			return nil
		}
		full, err := decodeNode(r.value)
		if err != nil {
			return err
		}
		script, note := noteDecl(full, 'a')
		if note == nil {
			return nil
		}
		if err := p.input(r.key, r.value); err != nil {
			return err
		}
		if err := note(); err != nil {
			return err
		}
		return p.put(p.key('s', script), nil)
	}); err != nil {
		return 0, err
	}
	// Replay declarations before matching paths. Script file facts are small
	// projections keyed by Node ID, not copies of the receipt payload.
	if err := p.visitReceiptNodes(ctx, receipts, func(raw *graph.Node) error {
		if p.scriptFile(raw) {
			return p.put(p.key('D', raw.ID), []byte(raw.FilePath))
		}
		if !p.declCandidate(raw) {
			return nil
		}
		script, note := noteDecl(raw, 'A')
		if note == nil {
			return nil
		}
		return p.withReceiptNode(raw.ID, func() error {
			if err := note(); err != nil {
				return err
			}
			return p.put(p.key('s', script), nil)
		})
	}); err != nil {
		return 0, err
	}
	active, err := rebuildGuardHasPrefix(ctx, p.b.pending, p.column('s'))
	if err != nil || !active {
		return 0, err
	}
	if err := p.captureScripts(ctx); err != nil {
		return 0, err
	}
	if err := p.captureMembers(ctx, receipts); err != nil {
		return 0, err
	}
	moves, err := p.plan(ctx, target, rebind)
	if err != nil || moves == 0 {
		return 0, err
	}
	if err := p.recordCalls(ctx, receipts); err != nil {
		return 0, err
	}
	return moves, p.b.withWrites(func() error { return p.apply(ctx, moves) })
}

func (p *rebuildGodotScripts) visitReceiptNodes(ctx context.Context, receipts iter.Seq2[ParseFacts, error], visit func(*graph.Node) error) error {
	for facts, err := range receipts {
		if err != nil {
			return err
		}
		if !p.receiptLanguage(facts.EffectiveLanguage) {
			continue
		}
		file := pathkey.Normalize(facts.File.FilePath)
		for _, n := range facts.Nodes {
			if err := ctx.Err(); err != nil {
				return err
			}
			if n.FilePath != file {
				continue
			}
			if err := visit(n); err != nil {
				return err
			}
		}
	}
	return ctx.Err()
}

func (p *rebuildGodotScripts) withReceiptNode(id string, note func() error) error {
	key := pendingNodeKey(id)
	_, err := pendingGet(p.b.pending, key, func(val []byte) (bool, error) {
		if err := p.input(key, val); err != nil {
			return false, err
		}
		if _, err := checkedRebuildLightNode(row{key, val}, p.label); err != nil {
			return false, err
		}
		return true, note()
	})
	return err
}

// Enumerating slash-boundary suffixes is the inverse of gdPathMatches.
func (p *rebuildGodotScripts) noteScript(id, path string, col byte, note func(func() error) error) error {
	for tail := strings.ReplaceAll(path, "\\", "/"); tail != ""; {
		wanted, err := p.present(p.key('s', tail))
		if err != nil {
			return err
		}
		if wanted {
			if err := note(func() error {
				if err := p.addUnique(p.key(col, tail), id); err != nil {
					return err
				}
				return p.put(p.key('i', id), nil)
			}); err != nil {
				return err
			}
		}
		_, tail, _ = strings.Cut(tail, "/")
	}
	return nil
}

func (p *rebuildGodotScripts) captureScripts(ctx context.Context) error {
	if err := p.visitNodes(ctx, func(n *graph.Node, r row) error {
		if !p.scriptFile(n) {
			return nil
		}
		return p.noteScript(n.ID, n.FilePath, 'f', func(note func() error) error {
			if err := p.input(r.key, r.value); err != nil {
				return err
			}
			return note()
		})
	}); err != nil {
		return err
	}
	prefix := p.column('D')
	return pendingVisit(ctx, p.b.pending, prefix, func(r row) error {
		id, rest, err := readKeyString(r.key[len(prefix):])
		if err != nil || len(rest) != 0 {
			return invalid(p.label + " script receipt row")
		}
		return p.noteScript(id, string(r.value), 'F', func(note func() error) error {
			return p.withReceiptNode(id, note)
		})
	})
}

func (p *rebuildGodotScripts) memberFile(n *graph.Node) (string, bool, error) {
	if n.Kind != p.memberKinds[0] && n.Kind != p.memberKinds[1] {
		return "", false, nil
	}
	file, ok := p.memberFileRule(n)
	if !ok {
		return "", false, nil
	}
	wanted, err := p.present(p.key('i', file))
	return file, wanted, err
}

func (p *rebuildGodotScripts) captureMembers(ctx context.Context, receipts iter.Seq2[ParseFacts, error]) error {
	if err := p.visitReceiptNodes(ctx, receipts, func(n *graph.Node) error {
		id, wanted, err := p.memberFile(n)
		if err != nil || !wanted {
			return err
		}
		return p.withReceiptNode(n.ID, func() error { return p.noteMember('W', id, n) })
	}); err != nil {
		return err
	}
	return p.visitNodes(ctx, func(n *graph.Node, r row) error {
		id, wanted, err := p.memberFile(n)
		if err != nil || !wanted {
			return err
		}
		if err := p.input(r.key, r.value); err != nil {
			return err
		}
		return p.noteMember('w', id, n)
	})
}

// Within each kind, SQLite's node index visits IDs in ascending order.
func (p *rebuildGodotScripts) noteMember(col byte, file string, n *graph.Node) error {
	key := p.key(col, file, n.Name, string(n.Kind))
	prior, err := p.get(key)
	if err != nil || prior != nil && string(prior) <= n.ID {
		return err
	}
	return p.put(key, []byte(n.ID))
}

func (p *rebuildGodotAutoload) target(_ *graph.Edge, recv, member string, proved bool) (string, error) {
	decl, file := byte('a'), byte('f')
	if proved {
		decl, file = 'A', 'F'
	}
	script, _, err := p.unique(p.key(decl, recv))
	if err != nil || script == "" {
		return "", err
	}
	fileID, _, err := p.unique(p.key(file, script))
	if err != nil || fileID == "" {
		return "", err
	}
	return p.targetMember(fileID, member, proved)
}

func (p *rebuildGodotScripts) targetMember(fileID, member string, proved bool) (string, error) {
	col := byte('w')
	if proved {
		col = 'W'
	}
	for _, kind := range p.memberKinds {
		val, err := p.get(p.key(col, fileID, member, string(kind)))
		if err != nil {
			return "", err
		}
		if val != nil {
			return string(val), nil
		}
	}
	return "", nil
}

func (p *rebuildGodotScripts) plan(ctx context.Context, target func(*graph.Edge, string, string, bool) (string, error),
	rebind func(*graph.Edge, string) graph.EdgeReindex,
) (uint64, error) {
	var moves uint64
	var refused rebuildScratchRefusals
	if err := p.b.RebuildView().visit(ctx, []byte{familyEdge, edgeOut}, func(snap *pebble.Snapshot, r row) error {
		id, err := decodeLinkRecorder(r.key)
		if err != nil || id.Kind != graph.EdgeCalls {
			return err
		}
		// A settled or unrelated Call can share a receipt with a mover.
		raw, err := rebuildReceiptKey(snap, r.key)
		if err != nil && !errors.Is(err, ErrRebuildUnknownProvenance) {
			return err
		}
		if raw != nil {
			if err := p.claim(raw, r.key); err != nil {
				return err
			}
		}
		if !graph.IsUnresolvedTarget(id.To) {
			return nil
		}
		e, err := p.decodeKeyedEdge(r.key, r.value)
		if err != nil {
			return err
		}
		recv, member := p.callKey(e)
		if recv == "" {
			return nil
		}
		live, err := target(e, recv, member, false)
		if err != nil {
			return err
		}
		proof, err := target(e, recv, member, true)
		if err != nil {
			return err
		}
		if live != proof {
			refused.add(id, nil)
			return nil
		}
		if live == "" {
			return nil
		}
		if raw == nil {
			refused.add(id, nil)
			return nil
		}
		original, err := decodeLinkRecorder(raw)
		if err != nil {
			return err
		}
		if original.Kind != id.Kind || original.From != id.From || original.FilePath != id.FilePath || original.Line != id.Line {
			refused.add(id, nil)
			return nil
		}
		if err := p.input(r.key, r.value); err != nil {
			return err
		}
		if err := p.put(append(p.column('h'), r.key...), raw); err != nil {
			return err
		}
		rebind(e, live)
		moves++
		return p.stage(r.key, e)
	}); err != nil {
		return 0, err
	}
	return moves, refused.err()
}

func (p *rebuildGodotScripts) recordCalls(ctx context.Context, receipts iter.Seq2[ParseFacts, error]) error {
	for facts, err := range receipts {
		if err != nil {
			return err
		}
		file := pathkey.Normalize(facts.File.FilePath)
		for _, raw := range facts.Edges {
			if err := ctx.Err(); err != nil {
				return err
			}
			if facts.EffectiveLanguage == "" || raw.Kind != graph.EdgeCalls || raw.FilePath != file {
				continue
			}
			if err := p.recordCall(raw); err != nil {
				return err
			}
		}
	}
	var refused rebuildScratchRefusals
	prefix := p.column('h')
	if err := pendingVisit(ctx, p.b.pending, prefix, func(r row) error {
		claim, err := p.marked(append(p.column('r'), r.value...))
		if err != nil || len(claim) > 1 && claim[0] == rebuildScratchExact && bytes.Equal(claim[1:], r.key[len(prefix):]) {
			return err
		}
		e, err := p.edge(r.key[len(prefix):])
		if err != nil || e == nil {
			return errors.Join(err, invalid(p.label+" missing call"))
		}
		refused.add(graph.EdgeIdentityFor(e), nil)
		return nil
	}); err != nil {
		return err
	}
	return refused.err()
}

func (p *rebuildGodotScripts) recordCall(raw *graph.Edge) error {
	key := append(p.column('r'), pendingEdgeKey(raw)...)
	claim, err := p.marked(key)
	if err != nil || len(claim) < 2 {
		return err
	}
	moving, err := p.present(append(p.column('h'), claim[1:]...))
	if err != nil || !moving {
		return err
	}
	live, err := p.edge(claim[1:])
	if err != nil || live == nil {
		return errors.Join(err, invalid(p.label+" receipt has no call"))
	}
	recv, member := p.callKey(raw)
	liveRecv, liveMember := p.callKey(live)
	return p.noteReceipt(key, claim, recv == liveRecv && member == liveMember)
}
