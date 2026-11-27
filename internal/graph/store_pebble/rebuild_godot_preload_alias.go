package store_pebble

import (
	"context"
	"errors"
	"fmt"
	"iter"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/graph"
)

var ErrRebuildGodotPreloadAlias = errors.New("pebble rebuild Godot preload alias failed")

var rebuildGodotPreloadAliasPrefix = []byte{'g', 'q', 'Q'}

// RebuildGodotPreloadAliasRules supplies the native alias and Call decisions.
type RebuildGodotPreloadAliasRules struct {
	Decl            func(*graph.Node) (fileID, path string)
	MemberFile      func(*graph.Node) (string, bool)
	Call            func(*graph.Edge) (receiver, member string)
	SourceFile      func(*graph.Edge) string
	Rebind          func(*graph.Edge, string) graph.EdgeReindex
	DeclCandidate   func(*graph.Node) bool
	ScriptFile      func(*graph.Node) bool
	MemberKinds     [2]graph.NodeKind
	ReceiptLanguage func(string) bool
}

type rebuildGodotPreloadAlias struct {
	rebuildGodotScripts
	declRule   func(*graph.Node) (fileID, path string)
	sourceFile func(*graph.Edge) string
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
	if receipts == nil || rules.Decl == nil || rules.MemberFile == nil || rules.Call == nil || rules.SourceFile == nil || rules.Rebind == nil ||
		rules.DeclCandidate == nil || rules.ScriptFile == nil || rules.ReceiptLanguage == nil ||
		rules.MemberKinds[0] == "" || rules.MemberKinds[1] == "" {
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
			rebuildCallMoves: rebuildCallMoves{b: b, prefix: prefix, rowBytes: 16 << 20, label: "godot preload alias", inputRow: "input row"},
			memberFileRule:   rules.MemberFile,
			callKey:          rules.Call,
			declCandidate:    rules.DeclCandidate,
			scriptFile:       rules.ScriptFile,
			memberKinds:      rules.MemberKinds,
			receiptLanguage:  rules.ReceiptLanguage,
		},
		declRule:   rules.Decl,
		sourceFile: rules.SourceFile,
	}
	return p.run(ctx, receipts, p.noteNodeDecl, p.target, rules.Rebind)
}

func (p *rebuildGodotPreloadAlias) noteNodeDecl(n *graph.Node, col byte) (string, func() error) {
	file, path := p.declRule(n)
	if path == "" {
		return "", nil
	}
	return path, func() error { return p.noteDecl(col, file, n.Name, n.ID, path) }
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

func (p *rebuildGodotPreloadAlias) target(e *graph.Edge, recv, member string, proved bool) (string, error) {
	source := p.sourceFile(e)
	decl, file := byte('a'), byte('f')
	if proved {
		decl, file = 'A', 'F'
	}
	_, path, err := p.decl(p.key(decl, source, recv))
	if err != nil || path == "" {
		return "", err
	}
	id, _, err := p.unique(p.key(file, path))
	if err != nil || id == "" || id == source {
		return "", err
	}
	return p.targetMember(id, member, proved)
}
