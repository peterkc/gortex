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

// RebuildTestSymbols runs after RebuildTestFiles on the quiet private graph.
// Deferred parse facts can veto a test-to-test edge but are not stamped here.
func (b *BuildStore) RebuildTestSymbols(ctx context.Context, receipts iter.Seq2[ParseFacts, error],
	isTestFile func(string) bool, fileRole, annotationRole func(string, string) string, annotationRunner func(string) string,
) (err error) {
	const rowLimit = 16 << 20
	const supported, deferred, unproved = byte(1), byte(2), byte(4)
	prefix := []byte{'g', 'S'}
	occupied, err := rebuildGuardHasPrefix(ctx, b.pending, prefix)
	if err != nil {
		return err
	}
	if occupied {
		return invalid("test symbol scratch range occupied")
	}
	defer func() { err = errors.Join(err, b.pending.DeleteRange(prefix, prefixEnd(prefix), pebble.NoSync)) }()
	column := func(c byte) []byte { return []byte{'g', 'S', c} }
	key := func(c byte, parts ...string) []byte { return keyStrings(column(c), parts...) }
	put := func(k, v []byte) error {
		if len(k)+len(v) > rowLimit {
			return fmt.Errorf("%w: test projection scratch row", ErrRebuildLinkInputSize)
		}
		return b.pending.Set(k, v, pebble.NoSync)
	}
	state := func(k []byte) (byte, error) {
		return pendingGet(b.pending, k, func(v []byte) (byte, error) {
			if len(v) != 1 {
				return 0, invalid("test projection proof")
			}
			return v[0], nil
		})
	}
	mark := func(k []byte, bit byte) error {
		prior, err := state(k)
		if err != nil || prior&bit != 0 {
			return err
		}
		return put(k, []byte{prior | bit})
	}
	readNode := func(id string) (*graph.Node, error) {
		return pendingGet(b.pending, pendingNodeKey(id), func(raw []byte) (*graph.Node, error) {
			if len(raw) > rowLimit {
				return nil, fmt.Errorf("%w: test projection node", ErrRebuildLinkInputSize)
			}
			n, err := decodeNode(raw)
			if err == nil && n.ID != id {
				return nil, invalid("working test node identity does not match key")
			}
			return n, err
		})
	}
	callable := func(n *graph.Node) bool { return n.Kind == graph.KindFunction || n.Kind == graph.KindMethod }
	callSite := func(e *graph.Edge) []byte { return appendKeyLine(key('c', e.From, e.FilePath), e.Line) }
	annotationEdge := func(e *graph.Edge) []byte {
		return appendKeyLine(key('a', e.From, e.To, e.FilePath), e.Line)
	}
	if err := b.visitRebuildDataflowKind(ctx, graph.EdgeAnnotated, decodeEdgeLight, func(e *graph.Edge) error {
		return mark(key('a', e.To), unproved)
	}); err != nil {
		return err
	}
	for facts, err := range receipts {
		if err != nil {
			return err
		}
		file := pathkey.Normalize(facts.File.FilePath)
		producer := deferred
		switch facts.EffectiveLanguage {
		case "go", "javascript", "typescript":
			producer = supported
		}
		proof := func(recording string) byte {
			if recording != file {
				return unproved
			}
			return producer
		}
		for _, n := range facts.Nodes {
			if err := ctx.Err(); err != nil {
				return err
			}
			if callable(n) {
				want, err := encodeNode(n)
				if err != nil {
					return err
				}
				match, err := pendingGet(b.pending, pendingNodeKey(n.ID), func(raw []byte) (bool, error) { return bytes.Equal(raw, want), nil })
				if err != nil {
					return err
				}
				bit := unproved
				if match {
					bit = proof(n.FilePath)
				}
				if err := mark(key('n', n.ID), bit); err != nil {
					return err
				}
			}
			isTarget, err := state(key('a', n.ID))
			if err != nil {
				return err
			}
			if isTarget != 0 {
				live, err := readNode(n.ID)
				if err != nil {
					return err
				}
				bit := unproved
				if live != nil && live.Name == n.Name && live.Language == n.Language {
					bit = proof(n.FilePath)
				}
				if err := mark(key('t', n.ID), bit); err != nil {
					return err
				}
			}
		}
		for _, e := range facts.Edges {
			if err := ctx.Err(); err != nil {
				return err
			}
			switch e.Kind {
			case graph.EdgeAnnotated:
				if err := mark(annotationEdge(e), proof(e.FilePath)); err != nil {
					return err
				}
			case graph.EdgeCalls:
				// Main, the cross-package guard and Go attribution can
				// change To. No earlier pass changes the call's source/site.
				if err := mark(callSite(e), proof(e.FilePath)); err != nil {
					return err
				}
			}
		}
	}

	view := b.RebuildView()
	var unknown RebuildUnknownProvenanceError
	refuse := func(id graph.EdgeIdentity) {
		if unknown.Count == 0 {
			unknown.First = id
		}
		unknown.Count++
	}
	preferRole := func(old, next string) string {
		if old == "" || old == "benchmark" && next == "test" {
			return next
		}
		return old
	}
	if err := view.visit(ctx, []byte{familyNode}, func(_ *pebble.Snapshot, r row) error {
		light, err := decodeNodeLight(r.value)
		if err != nil || !callable(light) {
			return err
		}
		if !bytes.Equal(r.key, pendingNodeKey(light.ID)) {
			return invalid("working test symbol identity does not match key")
		}
		producer, err := state(key('n', light.ID))
		if err != nil {
			return err
		}
		var file *graph.Node
		if light.FilePath != "" && isTestFile(light.FilePath) {
			file, err = readNode(light.FilePath)
			if err != nil {
				return err
			}
		}
		var role, runner, provedRole string
		if file != nil && file.Kind == graph.KindFile && isTestFile(file.FilePath) {
			// The preceding pass already proved the exact test-shaped file.
			role = fileRole(light.Name, light.Language)
			if role == "" {
				role = "test"
			}
			provedRole = role
			runner, _ = file.Meta["test_runner"].(string)
		} else {
			if err := view.VisitGuardOutgoing(ctx, light.ID, graph.EdgeAnnotated, func(e *graph.Edge) error {
				target, err := readNode(e.To)
				if err != nil || target == nil {
					return err
				}
				candidate := annotationRole(target.Language, target.Name)
				if candidate == "" {
					return nil
				}
				role = preferRole(role, candidate)
				ap, err := state(annotationEdge(e))
				if err != nil {
					return err
				}
				tp, err := state(key('t', e.To))
				if (ap == supported || ap == deferred) && tp != 0 && tp&unproved == 0 {
					provedRole = preferRole(provedRole, candidate)
				}
				return err
			}); err != nil {
				return err
			}
			runner = annotationRunner(light.Language)
		}
		if role == "" {
			return nil
		}
		if producer == deferred && provedRole == "" {
			// This only matters if an otherwise retained Call needs this
			// target's membership to decide whether to emit its Tests row.
			return mark(key('m', light.ID), unproved)
		}
		if producer != deferred && (producer != supported || role != provedRole) {
			refuse(graph.EdgeIdentity{From: light.ID, FilePath: light.FilePath})
			return nil
		}
		if err := mark(key('m', light.ID), producer); err != nil || producer == deferred {
			return err
		}
		if len(r.key)+len(r.value) > rowLimit {
			return fmt.Errorf("%w: test symbol row", ErrRebuildLinkInputSize)
		}
		n, err := decodeNode(r.value)
		if err != nil {
			return err
		}
		changed := setRebuildTestMetadata(n, "is_test", true)
		changed = setRebuildTestMetadata(n, "test_role", role) || changed
		if runner != "" {
			changed = setRebuildTestMetadata(n, "test_runner", runner) || changed
		}
		if !changed {
			return nil
		}
		value, err := encodeNode(n)
		if err != nil {
			return err
		}
		return put(key('w', n.ID), value)
	}); err != nil {
		return err
	}
	if unknown.Count != 0 {
		return &unknown
	}

	// The complete edge key starts with this pair within EdgeCalls. Once an
	// offer wins, later sites cannot change the output for that pair.
	var last graph.EdgeEndpoint
	if err := b.visitRebuildDataflowKind(ctx, graph.EdgeCalls, decodeEdgeLight, func(e *graph.Edge) error {
		pair := graph.EdgeEndpoint{From: e.From, To: e.To}
		if pair == last || graph.IsUnresolvedTarget(e.To) {
			return nil
		}
		from, err := state(key('m', e.From))
		if err != nil || from != supported {
			return err
		}
		last = pair
		to, err := state(key('m', e.To))
		if err != nil || to == supported || to == deferred {
			return err
		}
		site, err := state(callSite(e))
		if err != nil {
			return err
		}
		if to == unproved || site != supported {
			refuse(graph.EdgeIdentity{From: e.From, To: e.To, Kind: e.Kind, FilePath: e.FilePath, Line: e.Line})
			return nil
		}
		test := &graph.Edge{From: e.From, To: e.To, Kind: graph.EdgeTests, FilePath: e.FilePath, Line: e.Line, Origin: graph.OriginASTInferred}
		value, err := encodeEdge(test)
		if err != nil {
			return err
		}
		return put(append(column('e'), pendingEdgeKey(test)...), value)
	}); err != nil {
		return err
	}
	if unknown.Count != 0 {
		return &unknown
	}
	return b.withWrites(func() error {
		if err := pendingVisit(ctx, b.pending, column('w'), func(r row) error {
			n, err := decodeNode(r.value)
			if err != nil {
				return err
			}
			return b.writeNode(n)
		}); err != nil {
			return err
		}
		return pendingVisit(ctx, b.pending, column('e'), func(r row) error {
			e, err := decodeEdge(r.value)
			if err != nil {
				return err
			}
			return b.addEdge(e)
		})
	})
}

func setRebuildTestMetadata(n *graph.Node, key string, value any) bool {
	if n.Meta[key] == value {
		return false
	}
	if n.Meta == nil {
		n.Meta = make(map[string]any)
	}
	n.Meta[key] = value
	return true
}
