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

// RebuildReturnsTo runs one cold source lift after the private resolver and
// argument lift. The owner keeps graph rows unchanged until the plan applies.
// Receipt proof covers possible producers of settled rows, not the effect of
// any deferred pass that was not run.
func (b *BuildStore) RebuildReturnsTo(ctx context.Context, receipts iter.Seq2[ParseFacts, error],
	request func(*graph.Edge) (caller string, line int, name string, ok bool), targetName func(string) string,
) (err error) {
	const maxRowBytes = 16 << 20
	prefix := []byte{'g', 't'}
	occupied, err := rebuildGuardHasPrefix(ctx, b.pending, prefix)
	if err != nil {
		return err
	}
	if occupied {
		return invalid("return lift scratch range occupied")
	}
	defer func() { err = errors.Join(err, b.pending.DeleteRange(prefix, prefixEnd(prefix), pebble.NoSync)) }()
	read := func(key []byte) ([]byte, error) { return pendingGet(b.pending, key, copyValue) }
	write := func(key, value []byte) error {
		if len(key)+len(value) > maxRowBytes {
			return fmt.Errorf("%w: return lift scratch row", ErrRebuildLinkInputSize)
		}
		return b.pending.Set(key, value, pebble.NoSync)
	}
	bucket := func(caller, name string, line int) []byte {
		return appendKeyLine(keyStrings([]byte{'g', 't', 'b'}, caller, name), line)
	}
	demand := false
	if err := b.visitRebuildDataflowKind(ctx, graph.EdgeReturnsTo, decodeEdge, func(e *graph.Edge) error {
		caller, line, name, ok := request(e)
		if !ok {
			return nil
		}
		demand = true
		return write(bucket(caller, name, line), []byte{'?'})
	}); err != nil || !demand {
		return err
	}
	// Earlier passes can change To, but preserve source, site and request.
	returnProof := func(e *graph.Edge, line int, name string) []byte {
		key := appendKeyLine(keyStrings([]byte{'g', 't', 'r'}, e.From, e.FilePath, name), e.Line)
		return appendKeyLine(key, line)
	}
	callProof := func(e *graph.Edge) []byte {
		return appendKeyLine(keyStrings([]byte{'g', 't', 'c'}, e.From, e.FilePath), e.Line)
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
			var key []byte
			switch e.Kind {
			case graph.EdgeCalls:
				key = callProof(e)
			case graph.EdgeReturnsTo:
				if _, line, name, ok := request(e); ok {
					key = returnProof(e, line, name)
				}
			}
			if key == nil {
				continue
			}
			state := byte('n')
			if supported && path == e.FilePath {
				prior, err := read(key)
				if err != nil {
					return err
				}
				if prior != nil {
					continue // a good row cannot clear another possible producer
				}
				state = 'y'
			}
			if err := write(key, []byte{state}); err != nil {
				return err
			}
		}
	}
	const targetStart = 9 // proof byte, then signed line, then verbatim To
	if err := b.visitRebuildDataflowKind(ctx, graph.EdgeCalls, decodeEdgeLight, func(e *graph.Edge) error {
		if e.To == "" || strings.HasPrefix(e.To, "unresolved::") {
			return nil
		}
		proof, err := read(callProof(e))
		if err != nil {
			return err
		}
		state := byte('n')
		if bytes.Equal(proof, []byte{'y'}) {
			state = 'y'
		}
		ranked := append(appendKeyLine([]byte{state}, e.Line), e.To...)
		name := targetName(e.To)
		for _, line := range [...]int{0, e.Line} {
			for _, match := range [...]string{"", name} {
				key := bucket(e.From, match, line)
				prior, err := read(key)
				if err != nil {
					return err
				}
				if prior != nil {
					best := ranked
					if len(prior) > targetStart {
						if bytes.Compare(prior[1:], ranked[1:]) < 0 {
							best = prior
						}
						best = bytes.Clone(best)
						if prior[0] == 'n' || state == 'n' {
							best[0] = 'n'
						}
					} else if !bytes.Equal(prior, []byte{'?'}) {
						return invalid("return lift bucket shape")
					}
					if err := write(key, best); err != nil {
						return err
					}
				}
				if match == name {
					break // do not add an empty name twice
				}
			}
			if line == e.Line {
				break // do not add a line-zero call twice
			}
		}
		return nil
	}); err != nil {
		return err
	}
	var refused RebuildUnknownProvenanceError
	_, err = b.rebuildAttribution(ctx, func(e *graph.Edge) (string, error) {
		caller, line, name, ok := request(e)
		if !ok {
			return "", nil
		}
		best, err := read(bucket(caller, name, line))
		if err != nil || bytes.Equal(best, []byte{'?'}) {
			return "", err // native no match (including a named miss)
		}
		if len(best) <= targetStart || (best[0] != 'y' && best[0] != 'n') {
			return "", invalid("return lift bucket missing or malformed")
		}
		proof, err := read(returnProof(e, line, name))
		if err != nil {
			return "", err
		}
		if best[0] == 'n' || !bytes.Equal(proof, []byte{'y'}) {
			if refused.Count == 0 {
				refused.First = graph.EdgeIdentityFor(e)
			}
			refused.Count++
			return "", nil
		}
		return string(best[targetStart:]), nil
	}, func() error {
		if refused.Count != 0 {
			return &refused
		}
		return nil
	}, nil, graph.EdgeReturnsTo)
	return err
}

func (b *BuildStore) visitRebuildDataflowKind(ctx context.Context, kind graph.EdgeKind,
	decode func([]byte) (*graph.Edge, error), yield func(*graph.Edge) error,
) error {
	return b.RebuildView().visit(ctx, []byte{familyEdge, edgeOut}, func(_ *pebble.Snapshot, r row) error {
		saved, err := rebuildAttributionEdgeKind(r.key)
		if err != nil || saved != kind {
			return err
		}
		if len(r.key)+len(r.value) > 16<<20 {
			return fmt.Errorf("%w: return lift edge", ErrRebuildLinkInputSize)
		}
		e, err := decode(r.value)
		if err != nil {
			return err
		}
		if !bytes.Equal(r.key, pendingEdgeKey(e)) {
			return invalid("return lift edge identity does not match key")
		}
		return yield(e)
	})
}
