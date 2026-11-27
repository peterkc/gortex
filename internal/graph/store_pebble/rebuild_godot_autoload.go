package store_pebble

import (
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
	Decl       func(*graph.Node) (name, script string)
	MemberFile func(*graph.Node) (string, bool)
	Call       func(*graph.Edge) (receiver, member string)
	Rebind     func(*graph.Edge, string) graph.EdgeReindex
}

type rebuildGodotAutoload struct {
	rebuildGodotScripts
	rules RebuildGodotAutoloadRules
	moves uint64
}

// The two Godot Call turns use the same script, member and Call proofs.
type rebuildGodotScripts struct {
	rebuildCallMoves
	memberFileRule func(*graph.Node) (string, bool)
	callKey        func(*graph.Edge) (receiver, member string)
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
	if receipts == nil || rules.Decl == nil || rules.MemberFile == nil || rules.Call == nil || rules.Rebind == nil {
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
			rebuildCallMoves: rebuildCallMoves{b: b, prefix: prefix, rowBytes: 16 << 20, label: "godot autoload", inputRow: "edge row"},
			memberFileRule:   rules.MemberFile,
			callKey:          rules.Call,
		},
		rules: rules,
	}
	active, err := p.captureDecls(ctx)
	if err != nil || !active {
		return 0, err
	}
	if err := p.captureScripts(ctx); err != nil {
		return 0, err
	}
	if err := p.recordNodes(ctx, receipts); err != nil {
		return 0, err
	}
	if err := p.markUnproved(ctx); err != nil {
		return 0, err
	}
	if err := p.plan(ctx); err != nil || p.moves == 0 {
		return 0, err
	}
	if err := p.recordCalls(ctx, receipts); err != nil {
		return 0, err
	}
	return p.moves, b.withWrites(func() error {
		return p.apply(ctx, p.moves)
	})
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

// a/A elect the unique manifest path. s holds all requested paths, even
// conflicting ones that could win if a manifest receipt is missing.
func (p *rebuildGodotAutoload) captureDecls(ctx context.Context) (bool, error) {
	active := false
	err := p.visitNodes(ctx, func(n *graph.Node, r row) error {
		if n.Language != "godot_project" || n.Kind != graph.KindType {
			return nil
		}
		n, err := decodeNode(r.value)
		if err != nil {
			return err
		}
		name, script := p.rules.Decl(n)
		if script == "" {
			return nil
		}
		active = true
		if err := p.addUnique(p.key('a', name), script); err != nil {
			return err
		}
		return p.put(p.key('s', script), nil)
	})
	return active, err
}

// f/F elect the unique indexed file ID for one manifest path. Enumerating a
// file's slash-boundary suffixes is the inverse of native gdPathMatches.
func (p *rebuildGodotScripts) noteScript(n *graph.Node, col byte) error {
	for tail := strings.ReplaceAll(n.FilePath, "\\", "/"); tail != ""; {
		wanted, err := p.present(p.key('s', tail))
		if err != nil {
			return err
		}
		if wanted {
			if err := p.addUnique(p.key(col, tail), n.ID); err != nil {
				return err
			}
			if col == 'f' {
				if err := p.put(p.key('i', n.ID), nil); err != nil {
					return err
				}
			}
		}
		_, tail, _ = strings.Cut(tail, "/")
	}
	return nil
}

func (p *rebuildGodotScripts) captureScripts(ctx context.Context) error {
	return p.visitNodes(ctx, func(n *graph.Node, _ row) error {
		if n.Kind == graph.KindFile && n.Language == "gdscript" {
			return p.noteScript(n, 'f')
		}
		return nil
	})
}

func (p *rebuildGodotScripts) memberFile(n *graph.Node) (string, bool, error) {
	if n.Kind != graph.KindMethod && n.Kind != graph.KindFunction {
		return "", false, nil
	}
	file, ok := p.memberFileRule(n)
	if !ok {
		return "", false, nil
	}
	wanted, err := p.present(p.key('i', file))
	return file, wanted, err
}

func (p *rebuildGodotAutoload) recordNodes(ctx context.Context, receipts iter.Seq2[ParseFacts, error]) error {
	for facts, err := range receipts {
		if err != nil {
			return err
		}
		file := pathkey.Normalize(facts.File.FilePath)
		for _, n := range facts.Nodes {
			if err := ctx.Err(); err != nil {
				return err
			}
			if (facts.EffectiveLanguage != "gdscript" && facts.EffectiveLanguage != "godot_project") || n.FilePath != file {
				continue
			}
			if err := p.recordNode(n, file); err != nil {
				return err
			}
		}
	}
	return ctx.Err()
}

func (p *rebuildGodotAutoload) recordNode(raw *graph.Node, file string) error {
	if raw.Language != "godot_project" && raw.Language != "gdscript" {
		return nil
	}
	live, err := p.b.pendingNode(raw.ID)
	if err != nil || !sameRebuildLanguageMarker(raw, live, file) || raw.Kind != live.Kind {
		return err
	}
	if name, script := p.rules.Decl(raw); script != "" {
		liveName, liveScript := p.rules.Decl(live)
		if name == liveName && script == liveScript {
			return p.addUnique(p.key('A', name), script)
		}
	}
	return p.recordScriptNode(raw, live)
}

func (p *rebuildGodotScripts) recordScriptNode(raw, live *graph.Node) error {
	if raw.Kind == graph.KindFile && raw.Language == "gdscript" {
		return p.noteScript(live, 'F')
	}
	id, wanted, err := p.memberFile(live)
	if err != nil || !wanted || raw.Name != live.Name {
		return err
	}
	if err := p.put(p.key('p', raw.ID), nil); err != nil {
		return err
	}
	return p.noteMember('w', id, live)
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

func (p *rebuildGodotAutoload) markUnproved(ctx context.Context) error {
	return p.visitNodes(ctx, func(n *graph.Node, _ row) error {
		return p.markUnprovedMember(n)
	})
}

func (p *rebuildGodotScripts) markUnprovedMember(n *graph.Node) error {
	file, wanted, err := p.memberFile(n)
	if err != nil || !wanted {
		return err
	}
	proved, err := p.present(p.key('p', n.ID))
	if err != nil || proved {
		return err
	}
	return p.noteMember('u', file, n)
}

func (p *rebuildGodotAutoload) target(recv, member string, proved bool) (id string, unproved bool, err error) {
	decl, file := byte('a'), byte('f')
	if proved {
		decl, file = 'A', 'F'
	}
	script, _, err := p.unique(p.key(decl, recv))
	if err != nil || script == "" {
		return "", false, err
	}
	fileID, _, err := p.unique(p.key(file, script))
	if err != nil || fileID == "" {
		return "", false, err
	}
	return p.targetMember(fileID, member, proved)
}

func (p *rebuildGodotScripts) targetMember(fileID, member string, proved bool) (string, bool, error) {
	// Method priority is native. An unproved member can matter only at the
	// first nonempty kind, before or instead of that kind's proved winner.
	for _, kind := range [...]graph.NodeKind{graph.KindMethod, graph.KindFunction} {
		val, err := p.get(p.key('w', fileID, member, string(kind)))
		if err != nil {
			return "", false, err
		}
		if !proved {
			missing, err := p.get(p.key('u', fileID, member, string(kind)))
			if err != nil || missing != nil && (val == nil || string(missing) < string(val)) {
				return "", missing != nil, err
			}
		}
		if val != nil {
			return string(val), false, nil
		}
	}
	return "", false, nil
}

func (p *rebuildGodotAutoload) plan(ctx context.Context) error {
	var refused rebuildScratchRefusals
	if err := p.b.RebuildView().visit(ctx, []byte{familyEdge, edgeOut}, func(snap *pebble.Snapshot, r row) error {
		id, err := decodeLinkRecorder(r.key)
		if err != nil || id.Kind != graph.EdgeCalls || !graph.IsUnresolvedTarget(id.To) {
			return err
		}
		e, err := p.decodeKeyedEdge(r.key, r.value)
		if err != nil {
			return err
		}
		recv, member := p.rules.Call(e)
		if recv == "" {
			return nil
		}
		live, missing, err := p.target(recv, member, false)
		if err != nil {
			return err
		}
		proof, _, err := p.target(recv, member, true)
		if err != nil {
			return err
		}
		if missing || live != proof {
			refused.add(graph.EdgeIdentityFor(e), nil)
			return nil
		}
		if live == "" {
			return nil
		}
		if err := p.input(r.key, r.value); err != nil {
			return err
		}
		raw, err := rebuildReceiptKey(snap, r.key)
		if errors.Is(err, ErrRebuildUnknownProvenance) {
			refused.add(graph.EdgeIdentityFor(e), nil)
			return nil
		}
		if err != nil {
			return err
		}
		if err := p.claim(raw, r.key); err != nil {
			return err
		}
		if err := p.put(append(p.column('h'), r.key...), raw); err != nil {
			return err
		}
		p.rules.Rebind(e, live)
		p.moves++
		return p.stage(r.key, e)
	}); err != nil {
		return err
	}
	return refused.err()
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
		if err != nil || len(claim) != 0 && claim[0] == rebuildScratchExact {
			return err
		}
		e, err := p.edge(r.key[len(prefix):])
		if err != nil || e == nil {
			return errors.Join(err, invalid("godot autoload missing call"))
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
	live, err := p.edge(claim[1:])
	if err != nil || live == nil {
		return errors.Join(err, invalid("godot autoload receipt has no call"))
	}
	recv, member := p.callKey(raw)
	liveRecv, liveMember := p.callKey(live)
	return p.noteReceipt(key, claim, recv == liveRecv && member == liveMember)
}
