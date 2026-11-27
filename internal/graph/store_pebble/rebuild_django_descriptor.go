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

var ErrRebuildDjangoDescriptor = errors.New("pebble rebuild Django descriptor claiming failed")

var rebuildDjangoPrefix = []byte{'g', 'q', 51}

// RebuildDjangoRules keeps the descriptor semantics in the resolver.
type RebuildDjangoRules struct {
	ClaimsTo     func(string) bool
	Rebind       func(*graph.Edge, string)
	DefaultClass string
}

type rebuildDjango struct {
	rebuildCallMoves
	rules RebuildDjangoRules
}

// RebuildDjangoDescriptor claims live residual Calls and References. Parse
// receipts order the proved old edge identities; they do not re-offer edges.
func (b *BuildStore) RebuildDjangoDescriptor(ctx context.Context, receipts iter.Seq2[ParseFacts, error], rules RebuildDjangoRules) (count uint64, err error) {
	defer func() {
		if err != nil {
			count = 0
			err = fmt.Errorf("%w: %w", ErrRebuildDjangoDescriptor, err)
		}
	}()
	if receipts == nil || rules.ClaimsTo == nil || rules.Rebind == nil || rules.DefaultClass == "" {
		return 0, invalid("Django descriptor inputs")
	}
	prefix := rebuildDjangoPrefix
	busy, err := rebuildGuardHasPrefix(ctx, b.pending, prefix)
	if err != nil {
		return 0, err
	}
	if busy {
		return 0, invalid("Django descriptor scratch occupied")
	}
	defer func() { err = errors.Join(err, b.pending.DeleteRange(prefix, prefixEnd(prefix), pebble.NoSync)) }()
	p := rebuildDjango{
		rebuildCallMoves: rebuildCallMoves{b: b, prefix: prefix, rowBytes: 16 << 20, label: "Django descriptor", inputRow: "input row"},
		rules:            rules,
	}
	// The node table and native name index order tied names by ascending ID
	// on the cold SQLite access path. Index only eligible descriptor methods.
	if err := b.RebuildView().visit(ctx, []byte{familyNode}, func(_ *pebble.Snapshot, r row) error {
		n, err := checkedRebuildLightNode(r, p.label)
		if err != nil || n.Kind != graph.KindMethod || n.Name != "__iter__" {
			return err
		}
		n, err = b.rebuildNativeBridgeNode(ctx, n.ID, p.label)
		if err != nil {
			return err
		}
		receiver, _ := n.Meta["receiver"].(string)
		if receiver == "" {
			return nil
		}
		key := p.key('i', receiver)
		present, err := p.present(key)
		if err != nil || present {
			return err
		}
		return p.put(key, []byte(n.ID))
	}); err != nil {
		return 0, err
	}
	active, err := rebuildGuardHasPrefix(ctx, b.pending, p.column('i'))
	if err != nil || !active {
		return 0, err
	}
	if err := b.RebuildView().visit(ctx, []byte{familyEdge, edgeOut}, func(snap *pebble.Snapshot, r row) error {
		id, err := decodeLinkRecorder(r.key)
		if err != nil || id.Kind != graph.EdgeCalls && id.Kind != graph.EdgeReferences || !p.rules.ClaimsTo(id.To) {
			return err
		}
		target, err := p.target(ctx, id.From)
		if err != nil || target == "" || target == id.To {
			return err
		}
		if err := p.input(r.key, r.value); err != nil {
			return err
		}
		e, err := p.decodeKeyedEdge(r.key, r.value)
		if err != nil {
			return err
		}
		raw, err := rebuildReceiptKey(snap, r.key)
		if err != nil {
			return err
		}
		if err := p.put(append(p.column('a'), raw...), nil); err != nil {
			return err
		}
		if err := p.put(append(p.column('b'), r.key...), r.value); err != nil {
			return err
		}
		p.rules.Rebind(e, target)
		if err := p.stage(r.key, e); err != nil {
			return err
		}
		count++
		return nil
	}); err != nil || count == 0 {
		return 0, err
	}
	if err := p.registerClaims(ctx); err != nil {
		return 0, err
	}
	if err := p.order(ctx, receipts); err != nil {
		return 0, err
	}
	return count, b.withWrites(func() error {
		before := p.column('b')
		if err := pendingVisit(ctx, b.pending, before, func(r row) error {
			return b.checkRebuildFamilyDrop(r.key[len(before):], r.value)
		}); err != nil {
			return err
		}
		return p.apply(ctx, count)
	})
}

func (p *rebuildDjango) target(ctx context.Context, from string) (string, error) {
	source, err := p.b.rebuildNativeBridgeNode(ctx, from, p.label)
	if err != nil {
		return "", err
	}
	var receiver string
	if source != nil {
		receiver, _ = source.Meta["receiver"].(string)
	}
	class := p.rules.DefaultClass
	if receiver != "" {
		key := p.key('c', receiver)
		cached, err := p.get(key)
		if err != nil {
			return "", err
		}
		if cached == nil {
			first := ""
			if err := p.b.RebuildView().visit(ctx, keyStrings([]byte{familyName, nameRebuildScoped}, receiver), func(snap *pebble.Snapshot, r row) error {
				_, _, id, _, err := readRebuildCandidateIndex(nameRebuildScoped, r.key, r.value)
				if err != nil {
					return err
				}
				n, err := pendingGet(snap, pendingNodeKey(id), decodeNodeLight)
				if err != nil {
					return err
				}
				if n == nil || n.ID != id || n.Name != receiver {
					return invalid("Django descriptor class index identity")
				}
				if n.Kind != graph.KindType || first != "" && id >= first {
					return nil
				}
				n, err = p.b.rebuildNativeBridgeNode(ctx, id, p.label)
				if err != nil {
					return err
				}
				if hint, _ := n.Meta["django_iterable_class"].(string); hint != "" {
					first, class = id, hint
				}
				return nil
			}); err != nil {
				return "", err
			}
			if err := p.put(key, []byte(class)); err != nil {
				return "", err
			}
		} else {
			class = string(cached)
		}
	}
	target, err := p.get(p.key('i', class))
	return string(target), err
}

func (p *rebuildDjango) registerClaims(ctx context.Context) error {
	return p.b.RebuildView().visit(ctx, []byte{familyEdge, edgeOut}, func(snap *pebble.Snapshot, r row) error {
		id, err := decodeLinkRecorder(r.key)
		if err != nil || id.Kind != graph.EdgeCalls && id.Kind != graph.EdgeReferences {
			return err
		}
		raw, err := rebuildReceiptKey(snap, r.key)
		if errors.Is(err, ErrRebuildUnknownProvenance) {
			// This row cannot claim a receipt. A mover with this custody
			// already refused at its capture boundary.
			return nil
		}
		if err != nil {
			return err
		}
		wanted, err := p.present(append(p.column('a'), raw...))
		if err != nil || !wanted {
			return err
		}
		return p.claim(raw, r.key)
	})
}

func (p *rebuildDjango) order(ctx context.Context, receipts iter.Seq2[ParseFacts, error]) error {
	for f, err := range receipts {
		if err != nil {
			return err
		}
		file := pathkey.Normalize(f.File.FilePath)
		for _, e := range f.Edges {
			if err := ctx.Err(); err != nil {
				return err
			}
			if f.EffectiveLanguage == "" || e.FilePath != file || e.Kind != graph.EdgeCalls && e.Kind != graph.EdgeReferences {
				continue
			}
			key := append(p.column('r'), pendingEdgeKey(e)...)
			claim, err := p.marked(key)
			if err != nil {
				return err
			}
			if len(claim) > 1 && claim[0] == rebuildCapabilityWanted {
				if err := p.noteReceipt(key, claim, true); err != nil {
					return err
				}
			}
		}
	}
	var refused rebuildScratchRefusals
	before := p.column('b')
	if err := pendingVisit(ctx, p.b.pending, before, func(r row) error {
		old := r.key[len(before):]
		raw, err := rebuildReceiptKey(p.b.pending, old)
		if err != nil {
			return err
		}
		claim, err := p.marked(append(p.column('r'), raw...))
		if err != nil {
			return err
		}
		if len(claim) <= 1 || claim[0] != rebuildScratchExact {
			id, err := decodeLinkRecorder(old)
			if err != nil {
				return err
			}
			refused.add(id, nil)
		}
		return nil
	}); err != nil {
		return err
	}
	return refused.err()
}
