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

// ErrRebuildUnknownProvenance refuses a D72 lift when its input cannot be
// proved independent of deferred or cross-recording-path receipt rows.
var ErrRebuildUnknownProvenance = errors.New("rebuild: unknown provenance")

// RebuildUnknownProvenanceError counts distinct affected output rows, not
// possible producers or contenders. First names the lowest affected key.
// If a pass must prove inputs but cannot elect an output without guessing,
// Count and First describe the unproved inputs instead.
type RebuildUnknownProvenanceError struct {
	Count uint64
	First graph.EdgeIdentity
}

func (e *RebuildUnknownProvenanceError) Unwrap() error { return ErrRebuildUnknownProvenance }

func (e *RebuildUnknownProvenanceError) Error() string {
	return fmt.Sprintf("%s: count=%d first=%s", ErrRebuildUnknownProvenance, e.Count, attributionIdentityText(e.First))
}

func rebuildAttributionEdgeKind(key []byte) (graph.EdgeKind, error) {
	_, rest, err := readKeyString(key[2:])
	if err != nil {
		return "", err
	}
	kind, _, err := readKeyString(rest)
	return graph.EdgeKind(kind), err
}

// RebuildArgOf lifts settled arg_of targets to the native argument slot.
// The worker owns a quiet graph and supplies all its immutable parse receipts.
// Native callbacks keep metadata coercion and rewrite eligibility unchanged.
func (b *BuildStore) RebuildArgOf(ctx context.Context, receipts iter.Seq2[ParseFacts, error],
	arg func(*graph.Edge) (string, int, bool), position func(map[string]any, string) (int, bool),
) (err error) {
	const (
		maxRowBytes = 16 << 20
		proved      = 'y'
		unproved    = 'n'
	)
	prefix := []byte{'g', 'p'}
	occupied, err := rebuildGuardHasPrefix(ctx, b.pending, prefix)
	if err != nil {
		return err
	}
	if occupied {
		return invalid("argument lift scratch range occupied")
	}
	defer func() { err = errors.Join(err, b.pending.DeleteRange(prefix, prefixEnd(prefix), pebble.NoSync)) }()
	read := func(key []byte) (string, error) {
		return pendingGet(b.pending, key, func(raw []byte) (string, error) { return string(raw), nil })
	}
	record := func(key []byte, good bool) error {
		if len(key) > maxRowBytes {
			return fmt.Errorf("%w: argument lift receipt key", ErrRebuildLinkInputSize)
		}
		state := byte(unproved)
		if good {
			state = proved
			prior, err := read(key)
			if err != nil || prior != "" {
				return err // never erase a possible deferred contributor
			}
		}
		return b.pending.Set(key, []byte{state}, pebble.NoSync)
	}
	argKey := func(e *graph.Edge) []byte {
		// The link passes can replace both endpoints but never create an
		// arg_of or change its recorded site or typed argument position.
		key := appendKeyLine(keyStrings([]byte{'g', 'p', 'a'}, e.FilePath), e.Line)
		key, _ = appendMetaValue(key, e.Meta["arg_position"], 1) // numeric by caller
		return key
	}
	paramKey := func(e *graph.Edge) []byte { return append([]byte{'g', 'p', 'e'}, pendingEdgeKey(e)...) }
	nodeKey := func(n *graph.Node) []byte {
		key := keyStrings([]byte{'g', 'p', 'n'}, n.ID)
		key, _ = appendMetaValue(key, n.Meta["position"], 1) // numeric by caller
		return key
	}
	for facts, err := range receipts {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		path := pathkey.Normalize(facts.File.FilePath)
		supported := facts.EffectiveLanguage == "go" || facts.EffectiveLanguage == "javascript" || facts.EffectiveLanguage == "typescript"
		for _, e := range facts.Edges {
			if err := ctx.Err(); err != nil {
				return err
			}
			switch e.Kind {
			case graph.EdgeArgOf:
				if _, ok := position(e.Meta, "arg_position"); ok {
					if err := record(argKey(e), supported && path == e.FilePath); err != nil {
						return err
					}
				}
			case graph.EdgeParamOf:
				if err := record(paramKey(e), supported && path == e.FilePath); err != nil {
					return err
				}
			}
		}
		for _, n := range facts.Nodes {
			if err := ctx.Err(); err != nil {
				return err
			}
			if n.Kind == graph.KindParam {
				if _, ok := position(n.Meta, "position"); ok {
					if err := record(nodeKey(n), supported && path == n.FilePath); err != nil {
						return err
					}
				}
			}
		}
	}

	slotKey := func(owner string, pos int) []byte {
		return appendKeyLine(keyStrings([]byte{'g', 'p', 's'}, owner), pos)
	}
	haveSlots := false
	if err := b.RebuildView().visit(ctx, []byte{familyEdge, edgeOut}, func(snap *pebble.Snapshot, r row) error {
		kind, err := rebuildAttributionEdgeKind(r.key)
		if err != nil || kind != graph.EdgeParamOf {
			return err
		}
		if len(r.key)+len(r.value) > maxRowBytes {
			return fmt.Errorf("%w: argument lift parameter edge", ErrRebuildLinkInputSize)
		}
		e, err := guardLightEdge(r)
		if err != nil || e.From == "" {
			return err
		}
		n, err := pendingGet(snap, pendingNodeKey(e.From), func(raw []byte) (*graph.Node, error) {
			if len(raw) > maxRowBytes {
				return nil, fmt.Errorf("%w: argument lift parameter node", ErrRebuildLinkInputSize)
			}
			return decodeNode(raw)
		})
		if err != nil || n == nil {
			return err
		}
		if n.ID != e.From {
			return invalid("argument lift parameter node identity does not match key")
		}
		pos, ok := position(n.Meta, "position")
		if n.Kind != graph.KindParam || !ok {
			return nil
		}
		haveSlots = true
		key := slotKey(e.To, pos)
		prior, err := read(key)
		if err != nil || prior == string(unproved) {
			return err
		}
		// A stationary receipt row can only be overwritten at assembly.
		// Generic main can also make plugin-shaped pending param_of moves;
		// without an exact stationary of record their origin is unknown.
		edgeProof, err := read(paramKey(e))
		if err != nil {
			return err
		}
		nodeProof, err := read(nodeKey(n))
		if err != nil {
			return err
		}
		if edgeProof != string(proved) || linkPendingTarget(e.To) || nodeProof != string(proved) {
			return b.pending.Set(key, []byte{unproved}, pebble.NoSync)
		}
		if prior == "" {
			// Forward order is complete edge-key order, not native arrival
			// order. Every contender can still invalidate the proved slot.
			if len(key)+len(n.ID)+1 > maxRowBytes {
				return fmt.Errorf("%w: argument lift parameter slot", ErrRebuildLinkInputSize)
			}
			return b.pending.Set(key, append([]byte{proved}, n.ID...), pebble.NoSync)
		}
		return nil
	}); err != nil || !haveSlots {
		return err
	}
	var refused RebuildUnknownProvenanceError
	_, err = b.rebuildAttribution(ctx, func(e *graph.Edge) (string, error) {
		owner, pos, ok := arg(e)
		if !ok {
			return "", nil
		}
		slot, err := read(slotKey(owner, pos))
		if err != nil || slot == "" {
			return "", err // native no match
		}
		argProof, err := read(argKey(e))
		if err != nil {
			return "", err
		}
		if slot == string(unproved) || argProof != string(proved) {
			if refused.Count == 0 {
				refused.First = graph.EdgeIdentityFor(e)
			}
			refused.Count++
			return "", nil
		}
		return slot[1:], nil
	}, func() error {
		if refused.Count > 0 {
			return &refused
		}
		return nil
	}, nil, graph.EdgeArgOf)
	return err
}
