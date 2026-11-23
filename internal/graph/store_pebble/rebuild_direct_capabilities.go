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

const rebuildCapabilityRowLimit = 16 << 20

// RebuildDirectCapabilities derives the native direct rows from a quiet
// working graph. First elect the native winners without using receipt proof
// as a filter. Refuse before writing if any winner's proof is missing.
func (b *BuildStore) RebuildDirectCapabilities(ctx context.Context, receipts iter.Seq2[ParseFacts, error],
	processMechanism func(string) string, processImport func(string, string) (path, symbol string),
) (err error) {
	prefix := []byte{'g', 'c'}
	occupied, err := rebuildGuardHasPrefix(ctx, b.pending, prefix)
	if err != nil {
		return err
	}
	if occupied {
		return invalid("direct capability scratch range occupied")
	}
	defer func() { err = errors.Join(err, b.pending.DeleteRange(prefix, prefixEnd(prefix), pebble.NoSync)) }()
	s := rebuildCapabilityScratch{db: b.pending}
	if err := s.elect(ctx, b.RebuildView(), processMechanism); err != nil {
		return err
	}
	winners := []byte{'g', 'c', 'w'}
	hasWinners, err := rebuildGuardHasPrefix(ctx, b.pending, winners)
	if err != nil || !hasWinners {
		return err
	}
	visit := func(visit func(*graph.Edge) error) error {
		return pendingVisit(ctx, b.pending, winners, func(r row) error {
			input, err := decodeEdgeLight(r.value)
			if err != nil {
				return err
			}
			return visit(input)
		})
	}
	if err := visit(func(e *graph.Edge) error {
		keys := [][]byte{rebuildCapabilityKey('n', e.From), rebuildCapabilityKey('n', e.To), rebuildCapabilityRecorder(e, true)}
		if e.Kind != graph.EdgeReadsConfig {
			keys = append(keys, rebuildCapabilityRecorder(e, false))
		}
		for _, k := range keys {
			if err := s.put(k, []byte{rebuildCapabilityWanted}); err != nil {
				return err
			}
		}
		if e.Kind == graph.EdgeCalls {
			return s.put(rebuildCapabilityExternal(e, e.To), []byte{rebuildCapabilityWanted})
		}
		return nil
	}); err != nil {
		return err
	}
	for facts, err := range receipts {
		if err != nil {
			return err
		}
		if err := s.record(ctx, facts, processImport); err != nil {
			return err
		}
	}
	var first *graph.Edge
	var count uint64
	if err := visit(func(e *graph.Edge) error {
		proved, err := s.proves(e, processImport)
		if err != nil || proved {
			return err
		}
		if first == nil || rebuildCapabilityLess(e, first) {
			first = e
		}
		count++
		return nil
	}); err != nil {
		return err
	}
	if count != 0 {
		return &RebuildUnknownProvenanceError{Count: count, First: graph.EdgeIdentityFor(first)}
	}
	return b.withWrites(func() error {
		// The native matcher returns one of a fixed set of mechanisms.
		written := make(map[string]bool)
		return visit(func(input *graph.Edge) error {
			e, mechanism := rebuildDirectCapability(input, processMechanism)
			if mechanism != "" && !written[mechanism] {
				n := &graph.Node{ID: e.To, Kind: graph.KindString, Name: mechanism,
					Meta: map[string]any{"context": "process", "mechanism": mechanism}}
				if err := b.writeNode(n); err != nil {
					return err
				}
				written[mechanism] = true
			}
			return b.addEdge(e)
		})
	})
}

type rebuildCapabilityScratch struct{ db *pebble.DB }

const (
	rebuildCapabilityWanted byte = 1 << iota
	rebuildCapabilityProved
	rebuildCapabilityUnproved
)

func rebuildCapabilityKey(column byte, parts ...string) []byte {
	return keyStrings([]byte{'g', 'c', column}, parts...)
}

func rebuildCapabilityRecorder(e *graph.Edge, exact bool) []byte {
	column := byte('r')
	if exact {
		column = 'e'
	}
	k := rebuildCapabilityKey(column, e.From, string(e.Kind), e.FilePath)
	if exact {
		k = appendKeyString(k, e.To)
	}
	return appendKeyLine(k, e.Line)
}

func rebuildCapabilityExternal(e *graph.Edge, target string) []byte {
	return appendKeyLine(rebuildCapabilityKey('x', e.From, e.FilePath, target), e.Line)
}

func (s rebuildCapabilityScratch) put(k, v []byte) error {
	if len(k)+len(v) > rebuildCapabilityRowLimit {
		return fmt.Errorf("%w: direct capability scratch row", ErrRebuildLinkInputSize)
	}
	return s.db.Set(k, v, pebble.NoSync)
}

func (s rebuildCapabilityScratch) state(k []byte) (byte, error) {
	return pendingGet(s.db, k, func(v []byte) (byte, error) {
		if len(v) != 1 {
			return 0, invalid("direct capability evidence")
		}
		return v[0], nil
	})
}

func (s rebuildCapabilityScratch) note(k []byte, proved bool) error {
	prior, err := s.state(k)
	if err != nil || prior == 0 {
		return err
	}
	bit := rebuildCapabilityUnproved
	if proved {
		bit = rebuildCapabilityProved
	}
	if prior&bit != 0 {
		return nil
	}
	return s.put(k, []byte{prior | bit})
}

func (s rebuildCapabilityScratch) node(id string, decode func([]byte) (*graph.Node, error)) (*graph.Node, error) {
	k := pendingNodeKey(id)
	return pendingGet(s.db, k, func(v []byte) (*graph.Node, error) {
		if len(k)+len(v) > rebuildCapabilityRowLimit {
			return nil, fmt.Errorf("%w: direct capability endpoint", ErrRebuildLinkInputSize)
		}
		n, err := decode(v)
		if err == nil && n.ID != id {
			return nil, invalid("direct capability node identity does not match key")
		}
		return n, err
	})
}

func (s rebuildCapabilityScratch) elect(ctx context.Context, view RebuildView, process func(string) string) error {
	return view.visit(ctx, []byte{familyEdge, edgeOut}, func(_ *pebble.Snapshot, r row) error {
		kind, err := rebuildAttributionEdgeKind(r.key)
		if err != nil || !rebuildDirectCapabilityKind(kind) {
			return err
		}
		if len(r.key)+len(r.value) > rebuildCapabilityRowLimit {
			return fmt.Errorf("%w: direct capability input", ErrRebuildLinkInputSize)
		}
		e, err := guardLightEdge(r)
		if err != nil {
			return err
		}
		out, _ := rebuildDirectCapability(e, process)
		if out == nil {
			return nil
		}
		if out.Kind == graph.EdgeAccessesField {
			n, err := s.node(e.To, decodeNodeLight)
			if err != nil || n == nil || n.Kind != graph.KindField {
				return err
			}
		}
		k := rebuildCapabilityKey('w', out.From, out.To, string(out.Kind))
		prior, err := pendingGet(s.db, k, decodeEdgeLight)
		if err != nil || prior != nil && !rebuildCapabilityLess(e, prior) {
			return err
		}
		v, err := encodeEdge(e)
		if err != nil {
			return err
		}
		return s.put(k, v)
	})
}

func (s rebuildCapabilityScratch) record(ctx context.Context, facts ParseFacts, processImport func(string, string) (string, string)) error {
	file, language := pathkey.Normalize(facts.File.FilePath), facts.EffectiveLanguage
	supported := language == "go" || language == "javascript" || language == "typescript"
	// Borrow only these structured import paths from one bounded receipt.
	var imports map[string]bool
	for _, n := range facts.Nodes {
		if err := ctx.Err(); err != nil {
			return err
		}
		if supported && n.Kind == graph.KindImport && n.FilePath == file && n.Language == language {
			if path, ok := n.Meta["path"].(string); ok {
				if imports == nil {
					imports = make(map[string]bool)
				}
				imports[path] = true
			}
		}
		k := rebuildCapabilityKey('n', n.ID)
		state, err := s.state(k)
		if err != nil {
			return err
		}
		if state == 0 || state&rebuildCapabilityUnproved != 0 {
			continue
		}
		if !supported || n.FilePath != file {
			if err := s.note(k, false); err != nil {
				return err
			}
			continue
		}
		live, err := s.node(n.ID, decodeNodeLight)
		if err != nil {
			return err
		}
		// Test stamping may have changed metadata. Prove the definition
		// attributes this pass reads, not a stale copy of the entire node.
		if live != nil && live.FilePath == file && live.Kind == n.Kind && live.Name == n.Name &&
			live.Language == n.Language && live.RepoPrefix == n.RepoPrefix {
			if err := s.note(k, true); err != nil {
				return err
			}
		}
	}
	for _, e := range facts.Edges {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !rebuildDirectCapabilityKind(e.Kind) {
			continue
		}
		proved := supported && e.FilePath == file
		if err := s.note(rebuildCapabilityRecorder(e, true), proved); err != nil {
			return err
		}
		// Calls and field accesses can move from a pending target without
		// changing the recorder. An unrelated stationary target cannot.
		if e.Kind != graph.EdgeReadsConfig && graph.IsUnresolvedTarget(e.To) {
			if err := s.note(rebuildCapabilityRecorder(e, false), proved); err != nil {
				return err
			}
		}
		if !proved || e.Kind != graph.EdgeCalls {
			continue
		}
		// A stationary raw call cannot certify attribution of the same key.
		if err := s.note(rebuildCapabilityExternal(e, e.To), false); err != nil {
			return err
		}
		path, symbol := processImport(language, e.To)
		if !imports[path] {
			continue
		}
		caller, err := s.node(e.From, decodeNodeLight)
		if err != nil {
			return err
		}
		if caller == nil || caller.Language != language {
			continue
		}
		target := graph.StubID(caller.RepoPrefix, graph.StubKindStdlib, path, symbol)
		if err := s.note(rebuildCapabilityExternal(e, target), true); err != nil {
			return err
		}
	}
	return nil
}

func (s rebuildCapabilityScratch) proves(e *graph.Edge, processImport func(string, string) (string, string)) (bool, error) {
	const exact = rebuildCapabilityWanted | rebuildCapabilityProved
	owner, err := s.state(rebuildCapabilityKey('n', e.From))
	if err != nil || owner != exact {
		return false, err
	}
	recorder, err := s.state(rebuildCapabilityRecorder(e, true))
	if err != nil {
		return false, err
	}
	if e.Kind != graph.EdgeReadsConfig {
		moving, err := s.state(rebuildCapabilityRecorder(e, false))
		if err != nil {
			return false, err
		}
		recorder |= moving
	}
	if recorder != exact {
		return false, nil
	}
	target, err := s.state(rebuildCapabilityKey('n', e.To))
	if err != nil || e.Kind != graph.EdgeCalls {
		return target == exact, err
	}
	n, err := s.node(e.To, decodeNode)
	if err != nil {
		return false, err
	}
	if target == exact && n != nil && (n.Kind == graph.KindFunction || n.Kind == graph.KindMethod) {
		return true, nil
	}
	proof, err := s.state(rebuildCapabilityExternal(e, e.To))
	if err != nil || proof != exact || target != rebuildCapabilityWanted {
		return false, err
	}
	caller, err := s.node(e.From, decodeNodeLight)
	if err != nil || caller == nil {
		return false, err
	}
	// Decode the same exact settled spelling that the receipt matched. Go
	// additionally requires the live symbol materialized by the prior tail.
	prefix := graph.StubID(caller.RepoPrefix, graph.StubKindStdlib) + "::"
	name, ok := strings.CutPrefix(e.To, prefix)
	if !ok {
		return false, nil
	}
	path, symbol := processImport(caller.Language, "unresolved::extern::"+name)
	if path == "" {
		return false, nil
	}
	if caller.Language != "go" {
		// Node stdlib externs are node-less at this point in the cold tail.
		return n == nil, nil
	}
	return n != nil && n.Kind == graph.KindFunction && n.FilePath == "" && n.Name == symbol &&
		n.Language == "go" && n.RepoPrefix == caller.RepoPrefix && n.Meta["external"] == true &&
		n.Meta["module_path"] == path && n.Meta["module_role"] == "stdlib", nil
}

func rebuildDirectCapabilityKind(k graph.EdgeKind) bool {
	return k == graph.EdgeReadsConfig || k == graph.EdgeReads || k == graph.EdgeWrites || k == graph.EdgeCalls
}

func rebuildDirectCapability(input *graph.Edge, process func(string) string) (*graph.Edge, string) {
	e := &graph.Edge{From: input.From, To: input.To, FilePath: input.FilePath, Line: input.Line, Origin: graph.OriginASTResolved}
	switch input.Kind {
	case graph.EdgeReadsConfig:
		if !strings.Contains(input.To, "cfg::env::") {
			return nil, ""
		}
		e.Kind = graph.EdgeReadsEnv
	case graph.EdgeReads:
		e.Kind, e.Meta = graph.EdgeAccessesField, map[string]any{"access": "read"}
	case graph.EdgeWrites:
		e.Kind, e.Meta = graph.EdgeAccessesField, map[string]any{"access": "write"}
	case graph.EdgeCalls:
		mechanism := process(input.To)
		if mechanism == "" {
			return nil, ""
		}
		e.Kind, e.To, e.Origin = graph.EdgeExecutesProcess, "string::process::"+mechanism, graph.OriginASTInferred
		return e, mechanism
	}
	return e, ""
}

func rebuildCapabilityLess(a, b *graph.Edge) bool {
	if a.From != b.From {
		return a.From < b.From
	}
	if a.To != b.To {
		return a.To < b.To
	}
	if a.Kind != b.Kind {
		return a.Kind < b.Kind
	}
	if a.FilePath != b.FilePath {
		return a.FilePath < b.FilePath
	}
	return a.Line < b.Line
}
