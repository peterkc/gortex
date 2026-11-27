package store_pebble

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"iter"
	"slices"
	"strings"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/pathkey"
)

// ErrRebuildEventChannel identifies a refused private event-channel turn.
var ErrRebuildEventChannel = errors.New("pebble rebuild event channel stage failed")

var rebuildEventPrefix = []byte{'g', 'X'}

const eventUnorderedSite = ^uint64(0)

// RebuildEventChannelStats reports actual added pairs.
type RebuildEventChannelStats struct {
	Pairs uint64
}

// RebuildEventChannel runs the cold event turn on the quiet private graph.
// Fanout and payload semantics are supplied by the resolver. No published
// state changes before the owning worker validates and offers the full tail.
func (b *BuildStore) RebuildEventChannel(ctx context.Context, receipts iter.Seq2[ParseFacts, error],
	topicPrefixes []string, channelPolicy func(event, label string) (transport string, fanoutCap int),
	makeCall func(event, transport string, emitter, listener *graph.Edge) *graph.Edge,
) (stats RebuildEventChannelStats, err error) {
	defer func() {
		if err != nil {
			stats = RebuildEventChannelStats{}
			err = fmt.Errorf("%w: %w", ErrRebuildEventChannel, err)
		}
	}()
	if receipts == nil || len(topicPrefixes) == 0 || channelPolicy == nil || makeCall == nil {
		return stats, fmt.Errorf("%w: invalid event channel inputs", ErrRebuildLinkInputSize)
	}
	found, err := b.hasRebuildEventTopic(ctx, topicPrefixes)
	if err != nil || !found {
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
	p := rebuildEventChannel{b: b}
	if err := p.capture(ctx, topicPrefixes); err != nil {
		return stats, err
	}
	for _, col := range [...]byte{'t', 'l'} {
		found, err := rebuildGuardHasPrefix(ctx, b.pending, eventScratch(col))
		if err != nil || !found {
			return stats, err
		}
	}
	if err := p.order(ctx, receipts); err != nil {
		return stats, err
	}
	if err := p.elect(ctx, channelPolicy, makeCall); err != nil {
		return stats, err
	}
	if err := p.prove(ctx, receipts); err != nil {
		return stats, err
	}
	err = b.withWrites(func() error { return p.apply(ctx) })
	return p.stats, err
}

// The native framework gate needs only a topic ID prefix. Seek the node
// keys directly and check the first match without decoding unrelated nodes.
func (b *BuildStore) hasRebuildEventTopic(ctx context.Context, prefixes []string) (found bool, err error) {
	err = b.RebuildView().read(ctx, func(snap *pebble.Snapshot) (err error) {
		it, err := snap.NewIterWithContext(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, it.Close()) }()
		for _, topic := range prefixes {
			key := pendingNodeKey(topic)
			prefix := key[:len(key)-2] // drop the ID terminator, not escaped ID bytes
			if it.SeekGE(prefix) && bytes.HasPrefix(it.Key(), prefix) {
				raw, err := it.ValueAndErr()
				if err != nil {
					return err
				}
				if len(it.Key())+len(raw) > 16<<20 {
					return fmt.Errorf("%w: event channel topic probe", ErrRebuildLinkInputSize)
				}
				n, err := decodeNodeLight(raw)
				if err != nil {
					return err
				}
				if !bytes.Equal(it.Key(), pendingNodeKey(n.ID)) {
					return invalid("event channel topic probe node identity does not match key")
				}
				found = true
				return nil
			}
			if err := it.Error(); err != nil {
				return err
			}
		}
		return nil
	})
	return found, err
}

type rebuildEventChannel struct {
	b     *BuildStore
	stats RebuildEventChannelStats
}

func eventScratch(col byte, fields ...string) []byte {
	return keyStrings([]byte{'g', 'X', col}, fields...)
}

func (p *rebuildEventChannel) put(key, value []byte) error {
	return p.b.putRebuildScratch(key, value, 16<<20, "event channel")
}

func (p *rebuildEventChannel) capture(ctx context.Context, topicPrefixes []string) error {
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
		if e.From == "" {
			return nil
		}
		if !slices.ContainsFunc(topicPrefixes, func(prefix string) bool {
			return strings.HasPrefix(e.To, prefix)
		}) {
			return nil
		}
		col := byte('l')
		if kind == graph.EdgeEmits {
			col = 'e'
			if err := p.put(eventScratch('t', e.To), nil); err != nil {
				return err
			}
		}
		return p.put(eventSiteKey(col, e, eventUnorderedSite), nil)
	})
}

func eventSiteKey(col byte, e *graph.Edge, ordinal uint64) []byte {
	key := binary.BigEndian.AppendUint64(eventScratch(col, e.To), ordinal)
	return append(key, pendingEdgeKey(e)...)
}

// order gives live sites their first frozen receipt position. Like the gRPC
// tail, an unmatched site still enters election, but cannot pass receipt
// proof if it supplies a winning row. Native direct SQLite uses rowid order.
func (p *rebuildEventChannel) order(ctx context.Context, receipts iter.Seq2[ParseFacts, error]) error {
	var ordinal uint64
	for facts, err := range receipts {
		if err != nil {
			return err
		}
		for _, e := range facts.Edges {
			if err := ctx.Err(); err != nil {
				return err
			}
			var col byte
			switch e.Kind {
			case graph.EdgeEmits:
				col = 'e'
			case graph.EdgeListensOn:
				col = 'l'
			default:
				continue
			}
			unordered := eventSiteKey(col, e, eventUnorderedSite)
			present, err := pendingGet(p.b.pending, unordered, func([]byte) (bool, error) { return true, nil })
			if err != nil {
				return err
			}
			if !present {
				continue
			}
			if err := p.put(eventSiteKey(col, e, ordinal), nil); err != nil {
				return err
			}
			ordinal++
			if err := p.b.pending.Delete(unordered, pebble.NoSync); err != nil {
				return err
			}
		}
	}
	return ctx.Err()
}

func (p *rebuildEventChannel) sites(ctx context.Context, col byte, event string, visit func(*graph.Edge) error) error {
	prefix := eventScratch(col, event)
	return pendingVisit(ctx, p.b.pending, prefix, func(r row) error {
		if len(r.key) <= len(prefix)+8 {
			return invalid("event channel site key")
		}
		e, err := p.b.pendingEdge(r.key[len(prefix)+8:])
		if err != nil {
			return err
		}
		if e == nil {
			return invalid("event channel census lost input")
		}
		return visit(e)
	})
}

func (p *rebuildEventChannel) elect(ctx context.Context, channelPolicy func(event, label string) (transport string, fanoutCap int),
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
		transport, fanoutCap := channelPolicy(event, transport)
		if fanoutCap == 0 || emits > fanoutCap {
			return nil
		}
		if err := p.sites(ctx, 'l', event, func(*graph.Edge) error {
			listens++
			return nil
		}); err != nil || listens == 0 || listens > fanoutCap {
			return err
		}
		if err := p.sites(ctx, 'e', event, func(em *graph.Edge) error {
			emitter, err := p.b.RebuildView().Node(ctx, em.From)
			if err != nil || emitter == nil {
				return err
			}
			return p.sites(ctx, 'l', event, func(li *graph.Edge) error {
				if em.From == li.From {
					return nil
				}
				listener, err := p.b.RebuildView().Node(ctx, li.From)
				if err != nil || listener == nil || emitter.WorkspaceID != listener.WorkspaceID {
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
	var refused rebuildScratchRefusals
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
			if state != rebuildScratchExact {
				refused.add(graph.EdgeIdentityFor(e), nil)
				break
			}
		}
		return nil
	}); err != nil {
		return err
	}
	return refused.err()
}

func (p *rebuildEventChannel) state(key []byte) (byte, error) {
	return rebuildScratchState(p.b.pending, key, "event channel receipt proof state")
}

func (p *rebuildEventChannel) note(key []byte, state byte, ok bool) error {
	if state == 0 {
		return nil
	}
	return noteRebuildScratchState(p.put, key, state, rebuildScratchProofBit(ok))
}

func (p *rebuildEventChannel) apply(ctx context.Context) error {
	return applyRebuildScratchEdges(ctx, p.visit, p.b, rebuildProofEventChannel, &p.stats.Pairs)
}

func (p *rebuildEventChannel) visit(ctx context.Context, yield func(*graph.Edge) error) error {
	return pendingVisit(ctx, p.b.pending, eventScratch('w'), func(r row) error {
		_, e, err := eventWinner(r.value)
		if err != nil {
			return err
		}
		return yield(e)
	})
}
