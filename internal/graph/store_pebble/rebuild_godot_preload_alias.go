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

var ErrRebuildGodotPreloadAlias = errors.New("pebble rebuild Godot preload alias failed")

var rebuildGodotPreloadAliasPrefix = []byte{'g', 'q', 'Q'}

// RebuildGodotPreloadAliasRules supplies the native alias and Call decisions.
type RebuildGodotPreloadAliasRules struct {
	Decl       func(*graph.Node) (fileID, path string)
	MemberFile func(*graph.Node) (string, bool)
	Call       func(*graph.Edge) (receiver, member string)
	SourceFile func(*graph.Edge) string
	Rebind     func(*graph.Edge, string) graph.EdgeReindex
}

type rebuildGodotPreloadAlias struct {
	rebuildGodotScripts
	rules RebuildGodotPreloadAliasRules
	moves uint64
}

// RebuildGodotPreloadAlias binds file-local aliases on the live residual
// Calls after autoload. It stages moves only when the deciding facts agree
// with their receipts, then applies them in receipt order.
func (b *BuildStore) RebuildGodotPreloadAlias(ctx context.Context, receipts iter.Seq2[ParseFacts, error], rules RebuildGodotPreloadAliasRules) (count uint64, err error) {
	defer func() {
		if err != nil {
			count = 0
			err = fmt.Errorf("%w: %w", ErrRebuildGodotPreloadAlias, err)
		}
	}()
	if receipts == nil || rules.Decl == nil || rules.MemberFile == nil || rules.Call == nil || rules.SourceFile == nil || rules.Rebind == nil {
		return 0, invalid("godot preload alias inputs")
	}
	prefix := rebuildGodotPreloadAliasPrefix
	busy, err := rebuildGuardHasPrefix(ctx, b.pending, prefix)
	if err != nil {
		return 0, err
	}
	if busy {
		return 0, invalid("godot preload alias scratch occupied")
	}
	defer func() {
		err = errors.Join(err, b.pending.DeleteRange(prefix, prefixEnd(prefix), pebble.NoSync))
	}()
	p := rebuildGodotPreloadAlias{
		rebuildGodotScripts: rebuildGodotScripts{
			rebuildCallMoves: rebuildCallMoves{b: b, prefix: prefix, rowBytes: 16 << 20, label: "godot preload alias", inputRow: "edge row"},
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

func (p *rebuildGodotPreloadAlias) visitDecl(n *graph.Node, r row, visit func(*graph.Node, string, string) error) error {
	if n.Kind != graph.KindVariable || n.Language != "gdscript" {
		return nil
	}
	n, err := decodeNode(r.value)
	if err != nil {
		return err
	}
	file, path := p.rules.Decl(n)
	if path == "" {
		return nil
	}
	return visit(n, file, path)
}

func (p *rebuildGodotPreloadAlias) captureDecls(ctx context.Context) (bool, error) {
	active := false
	err := p.visitNodes(ctx, func(n *graph.Node, r row) error {
		return p.visitDecl(n, r, func(n *graph.Node, file, path string) error {
			active = true
			if err := p.noteDecl('a', file, n.Name, n.ID, path); err != nil {
				return err
			}
			return p.put(p.key('s', path), nil)
		})
	})
	return active, err
}

func (p *rebuildGodotPreloadAlias) recordNodes(ctx context.Context, receipts iter.Seq2[ParseFacts, error]) error {
	for facts, err := range receipts {
		if err != nil {
			return err
		}
		if facts.EffectiveLanguage != "gdscript" {
			continue
		}
		file := pathkey.Normalize(facts.File.FilePath)
		for _, raw := range facts.Nodes {
			if err := ctx.Err(); err != nil {
				return err
			}
			if raw.Language != "gdscript" || raw.FilePath != file {
				continue
			}
			live, err := p.b.pendingNode(raw.ID)
			if err != nil {
				return err
			}
			if !sameRebuildLanguageMarker(raw, live, file) || raw.Kind != live.Kind {
				continue
			}
			if raw.Kind == graph.KindVariable {
				if err := p.recordDecl(raw, live); err != nil {
					return err
				}
				continue
			}
			if err := p.recordScriptNode(raw, live); err != nil {
				return err
			}
		}
	}
	return ctx.Err()
}

func (p *rebuildGodotPreloadAlias) recordDecl(raw, live *graph.Node) error {
	file, path := p.rules.Decl(raw)
	if path == "" {
		return nil
	}
	liveFile, livePath := p.rules.Decl(live)
	if file != liveFile || path != livePath || raw.Name != live.Name {
		return nil
	}
	return p.noteDecl('A', file, raw.Name, raw.ID, path)
}

// SQLite's KindVariable scan uses the node index's ID tiebreaker, not
// receipt arrival. The native alias map retains the last declaration.
func (p *rebuildGodotPreloadAlias) noteDecl(col byte, file, name, id, path string) error {
	key := p.key(col, file, name)
	prior, _, err := p.decl(key)
	if err != nil || id <= prior {
		return err
	}
	return p.put(key, append(appendKeyString(nil, id), path...))
}

func (p *rebuildGodotPreloadAlias) decl(key []byte) (id, path string, err error) {
	val, err := p.get(key)
	if err != nil || val == nil {
		return "", "", err
	}
	id, rest, err := readKeyString(val)
	if err != nil || id == "" || len(rest) == 0 {
		return "", "", invalid("godot preload alias declaration")
	}
	return id, string(rest), nil
}

func (p *rebuildGodotPreloadAlias) markUnproved(ctx context.Context) error {
	return p.visitNodes(ctx, func(n *graph.Node, _ row) error {
		return p.markUnprovedMember(n)
	})
}

func (p *rebuildGodotPreloadAlias) target(source, path, member string, proved bool) (string, bool, error) {
	if path == "" {
		return "", false, nil
	}
	col := byte('f')
	if proved {
		col = 'F'
	}
	file, _, err := p.unique(p.key(col, path))
	if err != nil || file == "" || file == source {
		return "", false, err
	}
	return p.targetMember(file, member, proved)
}

func (p *rebuildGodotPreloadAlias) plan(ctx context.Context) error {
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
		recv, member := p.callKey(e)
		if recv == "" {
			return nil
		}
		source := p.rules.SourceFile(e)
		_, path, err := p.decl(p.key('a', source, recv))
		if err != nil || path == "" {
			return err
		}
		live, missing, err := p.target(source, path, member, false)
		if err != nil {
			return err
		}
		_, provedPath, err := p.decl(p.key('A', source, recv))
		if err != nil {
			return err
		}
		proof, _, err := p.target(source, provedPath, member, true)
		if err != nil {
			return err
		}
		if missing || live != proof {
			refused.add(id, nil)
			return nil
		}
		if live == "" {
			return nil
		}
		raw, err := rebuildReceiptKey(snap, r.key)
		if errors.Is(err, ErrRebuildUnknownProvenance) {
			refused.add(id, nil)
			return nil
		}
		if err != nil {
			return err
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
