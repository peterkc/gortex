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

// ErrRebuildEventChannel identifies a refused private event-channel turn.
var ErrRebuildEventChannel = errors.New("pebble rebuild event channel stage failed")

var rebuildEventPrefix = []byte{'g', 'X'}

// RebuildEventChannelStats reports actual added pairs and scratch table bytes.
type RebuildEventChannelStats struct {
	Pairs, SpillBytes uint64
}

// RebuildEventChannel runs the cold event turn on the quiet private graph.
// Fanout and payload semantics are supplied by the resolver. No published
// state changes before the owning worker validates and offers the full tail.
func (b *BuildStore) RebuildEventChannel(ctx context.Context, receipts iter.Seq2[ParseFacts, error], spillAt int,
	isEvent func(string) bool, fanout func(event, transport string) int,
	makeCall func(event, transport string, emitter, listener *graph.Edge) *graph.Edge,
) (stats RebuildEventChannelStats, err error) {
	defer func() {
		if err != nil {
			stats = RebuildEventChannelStats{}
			err = fmt.Errorf("%w: %w", ErrRebuildEventChannel, err)
		}
	}()
	if spillAt < 1 || spillAt > goldBufferBytes || receipts == nil || isEvent == nil || fanout == nil || makeCall == nil {
		return stats, fmt.Errorf("%w: invalid event channel inputs", ErrRebuildLinkInputSize)
	}
	found := false
	if err := b.RebuildView().VisitNodes(ctx, func(n *graph.Node) error {
		if isEvent(n.ID) {
			found = true
		}
		return nil
	}); err != nil || !found {
		return stats, err
	}
	occupied, err := rebuildGuardHasPrefix(ctx, b.pending, rebuildEventPrefix)
	if err != nil {
		return stats, err
	}
	if occupied {
		return stats, invalid("event channel scratch range occupied")
	}
	defer func() {
		err = errors.Join(err, b.pending.DeleteRange(rebuildEventPrefix, prefixEnd(rebuildEventPrefix), pebble.NoSync))
	}()
	p := rebuildEventChannel{b: b, threshold: spillAt}
	if err := p.capture(ctx, isEvent); err != nil {
		return stats, err
	}
	if err := p.elect(ctx, fanout, makeCall); err != nil {
		return stats, err
	}
	if err := p.prove(ctx, receipts); err != nil {
		return stats, err
	}
	return p.stats, b.withWrites(func() error { return p.apply(ctx) })
}

type rebuildEventChannel struct {
	b         *BuildStore
	threshold int
	buffered  int
	stats     RebuildEventChannelStats
}

func eventScratch(col byte, fields ...string) []byte {
	return keyStrings([]byte{'g', 'X', col}, fields...)
}

func (p *rebuildEventChannel) put(key, value []byte) error {
	if len(key)+len(value) > 16<<20 {
		return fmt.Errorf("%w: event channel scratch row", ErrRebuildLinkInputSize)
	}
	if err := p.b.pending.Set(key, value, pebble.NoSync); err != nil {
		return err
	}
	p.buffered += len(key) + len(value)
	if p.buffered >= p.threshold {
		before := p.b.pending.Metrics().Levels[0].TableBytesFlushed
		if err := p.b.pending.Flush(); err != nil {
			return err
		}
		p.stats.SpillBytes += p.b.pending.Metrics().Levels[0].TableBytesFlushed - before
		p.buffered = 0
	}
	return nil
}

func (p *rebuildEventChannel) capture(ctx context.Context, isEvent func(string) bool) error {
	return p.b.RebuildView().visit(ctx, []byte{familyEdge, edgeOut}, func(_ *pebble.Snapshot, r row) error {
		kind, err := rebuildAttributionEdgeKind(r.key)
		if err != nil || kind != graph.EdgeEmits && kind != graph.EdgeListensOn {
			return err
		}
		if len(r.key)+len(r.value) > 16<<20 {
			return fmt.Errorf("%w: event channel input row", ErrRebuildLinkInputSize)
		}
		e, err := decodeEdge(r.value)
		if err != nil {
			return err
		}
		if !bytes.Equal(r.key, pendingEdgeKey(e)) {
			return invalid("event channel edge identity")
		}
		if e.From == "" || !isEvent(e.To) {
			return nil
		}
		col := byte('l')
		if kind == graph.EdgeEmits {
			col = 'e'
			if err := p.put(eventScratch('t', e.To), nil); err != nil {
				return err
			}
		}
		return p.put(append(eventScratch(col, e.To), r.key...), nil)
	})
}

func (p *rebuildEventChannel) sites(ctx context.Context, col byte, event string, visit func(*graph.Edge) error) error {
	prefix := eventScratch(col, event)
	return pendingVisit(ctx, p.b.pending, prefix, func(r row) error {
		e, err := p.b.pendingEdge(r.key[len(prefix):])
		if err != nil {
			return err
		}
		if e == nil {
			return invalid("event channel census lost input")
		}
		return visit(e)
	})
}

func (p *rebuildEventChannel) elect(ctx context.Context, fanout func(string, string) int,
	makeCall func(string, string, *graph.Edge, *graph.Edge) *graph.Edge,
) error {
	return pendingVisit(ctx, p.b.pending, eventScratch('t'), func(r row) error {
		event, tail, err := readKeyString(r.key[3:])
		if err != nil {
			return err
		}
		if len(tail) != 0 {
			return invalid("event channel topic key")
		}
		var emits, listens int
		var transport string
		var transportSource *graph.Edge
		if err := p.sites(ctx, 'e', event, func(e *graph.Edge) error {
			emits++
			if transport == "" {
				transport, _ = e.Meta["transport"].(string)
				if transport != "" {
					transportSource = e
				}
			}
			return nil
		}); err != nil {
			return err
		}
		cap := fanout(event, transport)
		if cap == 0 || emits > cap {
			return nil
		}
		if err := p.sites(ctx, 'l', event, func(*graph.Edge) error {
			listens++
			return nil
		}); err != nil || listens == 0 || listens > cap {
			return err
		}
		if err := p.sites(ctx, 'e', event, func(em *graph.Edge) error {
			a, err := p.b.RebuildView().Node(ctx, em.From)
			if err != nil || a == nil {
				return err
			}
			return p.sites(ctx, 'l', event, func(li *graph.Edge) error {
				if em.From == li.From {
					return nil
				}
				b, err := p.b.RebuildView().Node(ctx, li.From)
				if err != nil || b == nil || a.WorkspaceID != b.WorkspaceID {
					return err
				}
				return p.represent(event, em, li)
			})
		}); err != nil {
			return err
		}
		return pendingVisit(ctx, p.b.pending, eventScratch('r', event), func(r row) error {
			em, li, err := p.sitePair(r.value)
			if err != nil {
				return err
			}
			e := makeCall(event, transport, em, li)
			key := pendingEdgeKey(e)
			prior, err := p.b.pendingEdge(key)
			if err != nil || prior != nil {
				return err
			}
			raw, err := encodeEdge(e)
			if err != nil {
				return err
			}
			var selected string
			if transportSource != nil && !bytes.Equal(pendingEdgeKey(transportSource), pendingEdgeKey(em)) {
				selected = string(eventRecorder(transportSource))
			}
			proof := keyStrings(nil, string(eventRecorder(em)), string(eventRecorder(li)), selected)
			// The native batch overwrites its staged payload at a duplicate
			// key. Sorted topic order means the later topic wins.
			return p.put(append(eventScratch('w'), key...), append(proof, raw...))
		})
	})
}

func (p *rebuildEventChannel) represent(event string, em, li *graph.Edge) error {
	key := eventScratch('r', event, em.From, li.From)
	rep, err := pendingGet(p.b.pending, key, func(v []byte) (*graph.Edge, error) {
		prior, _, err := p.sitePair(v)
		return prior, err
	})
	if err != nil || rep != nil && rep.Line <= em.Line {
		return err
	}
	return p.put(key, keyStrings(nil, string(pendingEdgeKey(em)), string(pendingEdgeKey(li))))
}

func (p *rebuildEventChannel) sitePair(value []byte) (*graph.Edge, *graph.Edge, error) {
	emKey, tail, err := readKeyString(value)
	if err != nil {
		return nil, nil, err
	}
	liKey, _, err := readKeyString(tail)
	if err != nil {
		return nil, nil, err
	}
	em, err := p.b.pendingEdge([]byte(emKey))
	if err != nil {
		return nil, nil, err
	}
	li, err := p.b.pendingEdge([]byte(liKey))
	if err != nil {
		return nil, nil, err
	}
	if em == nil || li == nil {
		return nil, nil, invalid("event channel representative lost input")
	}
	return em, li, nil
}

func eventRecorder(e *graph.Edge) []byte {
	transport, _ := e.Meta["transport"].(string)
	return eventScratch('p', string(pendingEdgeKey(e)), transport)
}

func eventWinner(value []byte) (keys [3]string, e *graph.Edge, err error) {
	for i := range keys {
		keys[i], value, err = readKeyString(value)
		if err != nil {
			return keys, nil, err
		}
	}
	e, err = decodeEdge(value)
	return keys, e, err
}

func (p *rebuildEventChannel) prove(ctx context.Context, receipts iter.Seq2[ParseFacts, error]) error {
	wanted := false
	if err := pendingVisit(ctx, p.b.pending, eventScratch('w'), func(r row) error {
		wanted = true
		keys, e, err := eventWinner(r.value)
		if err != nil {
			return err
		}
		for _, key := range keys {
			if key != "" {
				if err := p.put([]byte(key), []byte{rebuildCapabilityWanted}); err != nil {
					return err
				}
			}
		}
		if err := p.put(eventScratch('n', e.From), []byte{rebuildCapabilityWanted}); err != nil {
			return err
		}
		return p.put(eventScratch('n', e.To), []byte{rebuildCapabilityWanted})
	}); err != nil || !wanted {
		return err
	}
	for f, err := range receipts {
		if err != nil {
			return err
		}
		file, lang := pathkey.Normalize(f.File.FilePath), f.EffectiveLanguage
		for _, n := range f.Nodes {
			if err := ctx.Err(); err != nil {
				return err
			}
			key := eventScratch('n', n.ID)
			state, err := p.state(key)
			if err != nil {
				return err
			}
			if state == 0 {
				continue
			}
			live, err := p.b.RebuildView().Node(ctx, n.ID)
			if err != nil {
				return err
			}
			ok := lang != "" && n.FilePath == file && live != nil && live.FilePath == file &&
				live.Kind == n.Kind && live.Language == n.Language && live.Name == n.Name && live.RepoPrefix == n.RepoPrefix
			if err := p.note(key, state, ok); err != nil {
				return err
			}
		}
		for _, e := range f.Edges {
			if err := ctx.Err(); err != nil {
				return err
			}
			if e.Kind != graph.EdgeEmits && e.Kind != graph.EdgeListensOn {
				continue
			}
			key := eventRecorder(e)
			state, err := p.state(key)
			if err != nil {
				return err
			}
			if err := p.note(key, state, lang != "" && e.FilePath == file); err != nil {
				return err
			}
		}
	}
	var refused RebuildUnknownProvenanceError
	if err := pendingVisit(ctx, p.b.pending, eventScratch('w'), func(r row) error {
		keys, e, err := eventWinner(r.value)
		if err != nil {
			return err
		}
		all := []string{keys[0], keys[1], keys[2], string(eventScratch('n', e.From)), string(eventScratch('n', e.To))}
		for _, key := range all {
			if key == "" {
				continue
			}
			state, err := p.state([]byte(key))
			if err != nil {
				return err
			}
			if state != rebuildCapabilityWanted|rebuildCapabilityProved {
				if refused.Count == 0 {
					refused.First = graph.EdgeIdentityFor(e)
				}
				refused.Count++
				break
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if refused.Count != 0 {
		return &refused
	}
	return nil
}

func (p *rebuildEventChannel) state(key []byte) (byte, error) {
	return pendingGet(p.b.pending, key, func(value []byte) (byte, error) {
		if len(value) != 1 {
			return 0, invalid("event channel receipt proof state")
		}
		return value[0], nil
	})
}

func (p *rebuildEventChannel) note(key []byte, state byte, ok bool) error {
	if state == 0 {
		return nil
	}
	bit := byte(rebuildCapabilityUnproved)
	if ok {
		bit = rebuildCapabilityProved
	}
	if state&bit == 0 {
		return p.put(key, []byte{state | bit})
	}
	return nil
}

func (p *rebuildEventChannel) apply(ctx context.Context) error {
	return pendingVisit(ctx, p.b.pending, eventScratch('w'), func(r row) error {
		_, e, err := eventWinner(r.value)
		if err != nil {
			return err
		}
		if err := p.b.putEdge(e); err != nil {
			return err
		}
		p.stats.Pairs++
		return nil
	})
}
