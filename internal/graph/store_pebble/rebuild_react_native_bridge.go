package store_pebble

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"iter"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/pathkey"
)

var (
	ErrRebuildReactNativeBridge = errors.New("pebble rebuild React Native bridge stage failed")
	ErrRebuildExpoModuleBridge  = errors.New("pebble rebuild Expo module bridge stage failed")
)

var rebuildReactNativePrefix = []byte{'g', 'K'}

// RebuildReactNativeBridgeStats describes installed Calls.
type RebuildReactNativeBridgeStats struct {
	Edges uint64
}

// RebuildReactNativeBridge joins live markers and methods at the fifth
// framework turn. Frozen receipt order elects the first site per output pair.
func (b *BuildStore) RebuildReactNativeBridge(ctx context.Context, receipts iter.Seq2[ParseFacts, error],
	inFamily func(string) bool, isCall func(*graph.Edge) bool, buildCall func(*graph.Edge, *graph.Node) *graph.Edge,
) (RebuildReactNativeBridgeStats, error) {
	var family func(string) uint8
	if inFamily != nil {
		family = func(language string) uint8 {
			if inFamily(language) {
				return 1
			}
			return 0
		}
	}
	return b.rebuildNativeModuleBridge(ctx, receipts, rebuildNativeModuleBridgePolicy{
		stage:    ErrRebuildReactNativeBridge,
		label:    "React Native bridge",
		proof:    rebuildProofRNBridge,
		names:    rebuildRNNames,
		family:   family,
		required: 1,
	}, isCall, buildCall)
}

// RebuildExpoModuleBridgeStats reports the same counters as the React Native turn.
type RebuildExpoModuleBridgeStats = RebuildReactNativeBridgeStats

// RebuildExpoModuleBridge joins live RN call markers to Expo declarations.
// The family census applies to the whole pass, not individual endpoints.
// The resolver supplies one required bit per native registry family group.
func (b *BuildStore) RebuildExpoModuleBridge(ctx context.Context, receipts iter.Seq2[ParseFacts, error],
	required uint8, family func(string) uint8, isCall func(*graph.Edge) bool, buildCall func(*graph.Edge, *graph.Node) *graph.Edge,
) (RebuildExpoModuleBridgeStats, error) {
	return b.rebuildNativeModuleBridge(ctx, receipts, rebuildNativeModuleBridgePolicy{
		stage:    ErrRebuildExpoModuleBridge,
		label:    "Expo module bridge",
		proof:    rebuildProofExpoModules,
		names:    rebuildExpoNames,
		family:   family,
		required: required,
	}, isCall, buildCall)
}

type rebuildNativeModuleBridgePolicy struct {
	stage    error
	label    string
	proof    rebuildEdgeProof
	names    func(map[string]any) (string, string)
	family   func(string) uint8
	required uint8
}

func (b *BuildStore) rebuildNativeModuleBridge(ctx context.Context, receipts iter.Seq2[ParseFacts, error],
	policy rebuildNativeModuleBridgePolicy, isCall func(*graph.Edge) bool, buildCall func(*graph.Edge, *graph.Node) *graph.Edge,
) (stats RebuildReactNativeBridgeStats, err error) {
	defer func() {
		if err != nil {
			stats = RebuildReactNativeBridgeStats{}
			err = fmt.Errorf("%w: %w", policy.stage, err)
		}
	}()
	if receipts == nil || policy.family == nil || isCall == nil || buildCall == nil {
		return stats, fmt.Errorf("%w: invalid %s inputs", ErrRebuildLinkInputSize, policy.label)
	}
	busy, err := rebuildGuardHasPrefix(ctx, b.pending, rebuildReactNativePrefix)
	if err != nil {
		return stats, err
	}
	if busy {
		return stats, invalid(policy.label + " scratch range occupied")
	}
	if policy.required != 0 {
		var families uint8
		ready, err := b.RebuildView().hasLightNode(ctx, policy.label, func(n *graph.Node) bool {
			families |= policy.family(n.Language)
			return families&policy.required == policy.required
		})
		if err != nil || !ready {
			return stats, err
		}
	}
	defer func() {
		err = errors.Join(err, b.pending.DeleteRange(rebuildReactNativePrefix, prefixEnd(rebuildReactNativePrefix), pebble.NoSync))
	}()
	p := rebuildReactNativeBridge{b: b, policy: policy}
	ready, err := p.capture(ctx, isCall)
	if err != nil || !ready {
		return p.stats, err
	}
	if err := p.order(ctx, receipts, isCall); err != nil {
		return stats, err
	}
	if err := p.elect(ctx, buildCall); err != nil {
		return stats, err
	}
	if err := p.prove(ctx, receipts); err != nil {
		return stats, err
	}
	err = b.withWrites(func() error { return p.apply(ctx) })
	return p.stats, err
}

type rebuildReactNativeBridge struct {
	b       *BuildStore
	changes bool
	stats   RebuildReactNativeBridgeStats
	policy  rebuildNativeModuleBridgePolicy
}

// Scratch columns (all under g/K):
// b: module,method -> 1+sole ID, or 2 for several candidates.
// n: module,method,native ID -> empty candidate membership.
// r: raw edge key -> proof-state byte + live edge key.
// l: live edge key -> empty marker for a claim under a different raw key.
// o: big-endian receipt ordinal -> live edge key.
// g: caller,module,method -> 1 (first site already elected).
// w: caller,native ID -> 0 for a stationary key, 1+encoded edge for a
// proposal; before a capture or order refusal it holds affected output keys.
// d: declaration ID -> proof-state byte.
func rebuildRNKey(col byte, fields ...string) []byte {
	return keyStrings([]byte{'g', 'K', col}, fields...)
}

func rebuildRNNames(meta map[string]any) (module, method string) {
	module, _ = meta["rn_module"].(string)
	method, _ = meta["rn_method"].(string)
	return module, method
}

func rebuildExpoNames(meta map[string]any) (module, method string) {
	module, _ = meta["expo_module"].(string)
	method, _ = meta["expo_method"].(string)
	return module, method
}

func (p *rebuildReactNativeBridge) put(key, value []byte) error {
	return p.b.putRebuildScratch(key, value, 16<<20, p.policy.label)
}

func (p *rebuildReactNativeBridge) get(key []byte) ([]byte, error) {
	return pendingGet(p.b.pending, key, copyValue)
}

// hasLightNode stops the live node scan when match reports enough evidence.
func (v RebuildView) hasLightNode(ctx context.Context, label string, match func(*graph.Node) bool) (bool, error) {
	var found bool
	err := v.read(ctx, func(snap *pebble.Snapshot) (err error) {
		prefix := []byte{familyNode}
		it, err := snap.NewIterWithContext(ctx, &pebble.IterOptions{LowerBound: prefix, UpperBound: prefixEnd(prefix)})
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, it.Close()) }()
		for ok := it.First(); ok; ok = it.Next() {
			if err := ctx.Err(); err != nil {
				return err
			}
			value, err := it.ValueAndErr()
			if err != nil {
				return err
			}
			n, err := checkedRebuildLightNode(row{it.Key(), value}, label)
			if err != nil {
				return err
			}
			if match(n) {
				found = true
				break
			}
		}
		return it.Error()
	})
	return found, err
}

func checkedRebuildLightNode(r row, label string) (*graph.Node, error) {
	n, err := decodeNodeLight(r.value)
	if err == nil && !bytes.Equal(r.key, pendingNodeKey(n.ID)) {
		err = invalid(label + " node identity")
	}
	return n, err
}

func (p *rebuildReactNativeBridge) capture(ctx context.Context, isCall func(*graph.Edge) bool) (bool, error) {
	view := p.b.RebuildView()
	var methods bool
	if err := view.VisitNodes(ctx, func(n *graph.Node) error {
		mod, method := p.policy.names(n.Meta)
		if n.Kind != graph.KindMethod || n.ID == "" || mod == "" || method == "" {
			return nil
		}
		methods = true
		key := rebuildRNKey('b', mod, method)
		state, err := p.get(key)
		if err != nil {
			return err
		}
		if state == nil {
			if err := p.put(key, append([]byte{1}, n.ID...)); err != nil {
				return err
			}
		} else if state[0] == 1 {
			if err := p.put(key, []byte{2}); err != nil {
				return err
			}
		}
		return p.put(rebuildRNKey('n', mod, method, n.ID), nil)
	}); err != nil || !methods {
		return false, err
	}
	err := view.VisitEdges(ctx, func(e *graph.Edge) error {
		if !isCall(e) {
			return nil
		}
		mod, method := rebuildRNNames(e.Meta)
		if mod == "" || method == "" {
			return nil
		}
		// With no distinct candidate the call cannot participate in an
		// election, so it requires no order or ownership proof.
		bucket, err := p.get(rebuildRNKey('b', mod, method))
		if err != nil || len(bucket) == 0 || (bucket[0] == 1 && string(bucket[1:]) == e.From) {
			return err
		}
		live := pendingEdgeKey(e)
		custody, err := readRebuildEdgeCustody(p.b.pending, live)
		if err != nil {
			if errors.Is(err, ErrRebuildUnknownProvenance) {
				return p.affected(ctx, e)
			}
			return err
		}
		raw := custody.raw
		if raw == nil {
			// Synthetic and untracked moves have no raw receipt ordinal.
			if custody.proof != 0 {
				return p.affected(ctx, e)
			}
			raw = live
		}
		key := rebuildRNKey('r', string(raw))
		prior, err := p.get(key)
		if err != nil {
			return err
		}
		if prior != nil {
			previous, err := p.b.pendingEdge(prior[1:])
			if err != nil {
				return err
			}
			if previous == nil {
				return invalid(p.policy.label + " census lost a call")
			}
			if err := p.affected(ctx, previous); err != nil {
				return err
			}
			return p.affected(ctx, e)
		}
		if err := p.put(key, append([]byte{rebuildCapabilityWanted}, live...)); err != nil {
			return err
		}
		if !bytes.Equal(raw, live) {
			return p.put(append(rebuildRNKey('l'), live...), nil)
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	if err := p.refusedOutputs(ctx); err != nil {
		return false, err
	}
	// Replay receipts even without a live marker: a duplicate payload may
	// have hidden the only eligible call.
	return true, nil
}

func (p *rebuildReactNativeBridge) order(ctx context.Context, receipts iter.Seq2[ParseFacts, error], isCall func(*graph.Edge) bool) error {
	var ordinal uint64
	for facts, err := range receipts {
		if err != nil {
			return err
		}
		file := pathkey.Normalize(facts.File.FilePath)
		for _, e := range facts.Edges {
			if err := ctx.Err(); err != nil {
				return err
			}
			receiptKey := pendingEdgeKey(e)
			key := rebuildRNKey('r', string(receiptKey))
			state, err := p.get(key)
			if err != nil {
				return err
			}
			if len(state) == 0 {
				if !isCall(e) {
					continue
				}
				// A receipt at a moved call's live key is not its recorder.
				// The claim under its original key still controls its ordinal.
				inherited, err := pendingGet(p.b.pending, append(rebuildRNKey('l'), receiptKey...),
					func([]byte) (bool, error) { return true, nil })
				if err != nil {
					return err
				}
				if !inherited {
					if err := p.affected(ctx, e); err != nil {
						return err
					}
				}
				continue
			}
			live, err := p.b.pendingEdge(state[1:])
			if err != nil {
				return err
			}
			if live == nil {
				return invalid(p.policy.label + " census lost a call")
			}
			mod, method := rebuildRNNames(e.Meta)
			liveMod, liveMethod := rebuildRNNames(live.Meta)
			bridgeCall := isCall(e)
			if !bridgeCall || mod != liveMod || method != liveMethod || facts.EffectiveLanguage == "" || e.FilePath != file {
				// A raw key may be repeated with different metadata. Even a
				// row that fails the bridge predicate must participate in proof.
				if bridgeCall && mod != "" && method != "" && (mod != liveMod || method != liveMethod) {
					if err := p.affected(ctx, e); err != nil {
						return err
					}
				}
				if err := p.put(key, append([]byte{state[0] | rebuildCapabilityUnproved}, state[1:]...)); err != nil {
					return err
				}
				continue
			}
			if state[0]&rebuildCapabilityProved == 0 {
				ordered := binary.BigEndian.AppendUint64(rebuildRNKey('o'), ordinal)
				ordinal++
				if err := p.put(ordered, state[1:]); err != nil {
					return err
				}
				if err := p.put(key, append([]byte{state[0] | rebuildCapabilityProved}, state[1:]...)); err != nil {
					return err
				}
			}
		}
	}
	if err := pendingVisit(ctx, p.b.pending, rebuildRNKey('r'), func(r row) error {
		if len(r.value) < 2 {
			return invalid(p.policy.label + " recorder key")
		}
		if r.value[0] == rebuildScratchExact {
			return nil
		}
		e, err := p.b.pendingEdge(r.value[1:])
		if err != nil {
			return err
		}
		if e == nil {
			return invalid(p.policy.label + " census lost a call")
		}
		return p.affected(ctx, e)
	}); err != nil {
		return err
	}
	return p.refusedOutputs(ctx)
}

// Before election, an unproved order can change the representative site for
// every distinct candidate pair, even when one site hits a stationary key.
// Reuse the still-empty winner column to count pairs, not recorder claims.
// No caller can proceed to election with these diagnostic rows present.
func (p *rebuildReactNativeBridge) affected(ctx context.Context, e *graph.Edge) error {
	mod, method := rebuildRNNames(e.Meta)
	prefix := rebuildRNKey('n', mod, method)
	return pendingVisit(ctx, p.b.pending, prefix, func(r row) error {
		id, _, err := readKeyString(r.key[len(prefix):])
		if err != nil || id == e.From {
			return err
		}
		output := pendingEdgeKey(&graph.Edge{From: e.From, To: id, Kind: graph.EdgeCalls, FilePath: e.FilePath, Line: e.Line})
		key := rebuildRNKey('w', e.From, id)
		prior, err := p.get(key)
		if err != nil || prior != nil && bytes.Compare(prior, output) <= 0 {
			return err
		}
		return p.put(key, output)
	})
}

func (p *rebuildReactNativeBridge) refusedOutputs(ctx context.Context) error {
	var refused RebuildUnknownProvenanceError
	var first []byte
	if err := pendingVisit(ctx, p.b.pending, rebuildRNKey('w'), func(r row) error {
		if first == nil || bytes.Compare(r.value, first) < 0 {
			first = bytes.Clone(r.value)
		}
		refused.Count++
		return nil
	}); err != nil || refused.Count == 0 {
		return err
	}
	id, err := decodeLinkRecorder(first)
	if err != nil {
		return err
	}
	refused.First = id
	return &refused
}

func (p *rebuildReactNativeBridge) elect(ctx context.Context, buildCall func(*graph.Edge, *graph.Node) *graph.Edge) error {
	view := p.b.RebuildView()
	return pendingVisit(ctx, p.b.pending, rebuildRNKey('o'), func(r row) error {
		e, err := p.b.pendingEdge(r.value)
		if err != nil {
			return err
		}
		if e == nil {
			return invalid(p.policy.label + " census lost a call")
		}
		mod, method := rebuildRNNames(e.Meta)
		// One method belongs to exactly one module/method bucket. Its first
		// call for this caller is therefore also the first call of the bucket.
		key := rebuildRNKey('g', e.From, mod, method)
		prior, err := p.get(key)
		if err != nil || prior != nil {
			return err
		}
		if err := p.put(key, []byte{1}); err != nil {
			return err
		}
		prefix := rebuildRNKey('n', mod, method)
		return pendingVisit(ctx, p.b.pending, prefix, func(r row) error {
			id, _, err := readKeyString(r.key[len(prefix):])
			if err != nil || id == e.From {
				return err
			}
			key := rebuildRNKey('w', e.From, id)
			native, err := view.Node(ctx, id)
			if err != nil {
				return err
			}
			if native == nil {
				return invalid(p.policy.label + " census lost a method")
			}
			out := buildCall(e, native)
			old, err := p.b.pendingEdge(pendingEdgeKey(out))
			if err != nil {
				return err
			}
			if old != nil {
				// Stationary still consumes the first-site pair.
				return p.put(key, []byte{0})
			}
			encoded, err := encodeEdge(out)
			if err != nil {
				return err
			}
			for _, endpoint := range [...]string{out.From, out.To} {
				if err := p.put(rebuildRNKey('d', endpoint), []byte{rebuildCapabilityWanted}); err != nil {
					return err
				}
			}
			p.changes = true
			return p.put(key, append([]byte{1}, encoded...))
		})
	})
}

func (p *rebuildReactNativeBridge) prove(ctx context.Context, receipts iter.Seq2[ParseFacts, error]) error {
	if !p.changes {
		return nil
	}
	view := p.b.RebuildView()
	for facts, err := range receipts {
		if err != nil {
			return err
		}
		file := pathkey.Normalize(facts.File.FilePath)
		for _, n := range facts.Nodes {
			if err := ctx.Err(); err != nil {
				return err
			}
			key := rebuildRNKey('d', n.ID)
			state, err := p.get(key)
			if err != nil {
				return err
			}
			if len(state) == 0 {
				continue
			}
			live, err := view.Node(ctx, n.ID)
			if err != nil {
				return err
			}
			mod, method := p.policy.names(n.Meta)
			bit := byte(rebuildCapabilityUnproved)
			// HTML receipts can contain delegated JavaScript callers.
			// D71 still defers HTML in the build worker's main-link caller
			// selection. This proof does not admit HTML links for publication.
			if live != nil && n.FilePath == file && live.FilePath == file && facts.EffectiveLanguage != "" &&
				live.Language == n.Language && live.Kind == n.Kind && live.Name == n.Name && live.RepoPrefix == n.RepoPrefix {
				liveMod, liveMethod := p.policy.names(live.Meta)
				if liveMod == mod && liveMethod == method {
					bit = rebuildCapabilityProved
				}
			}
			if err := noteRebuildScratchState(p.put, key, state[0], bit); err != nil {
				return err
			}
		}
	}
	return proveRebuildScratchEndpoints(ctx, p.visit, func(id string) (bool, error) {
		state, err := p.get(rebuildRNKey('d', id))
		return len(state) == 1 && state[0] == rebuildScratchExact, err
	}, nil)
}

func (p *rebuildReactNativeBridge) visit(ctx context.Context, yield func(*graph.Edge) error) error {
	return pendingVisit(ctx, p.b.pending, rebuildRNKey('w'), func(r row) error {
		if len(r.value) == 1 && r.value[0] == 0 {
			return nil
		}
		if len(r.value) < 2 || r.value[0] != 1 {
			return invalid(p.policy.label + " winner row")
		}
		e, err := decodeEdge(r.value[1:])
		if err != nil {
			return err
		}
		return yield(e)
	})
}

func (p *rebuildReactNativeBridge) apply(ctx context.Context) error {
	return applyRebuildScratchEdges(ctx, p.visit, p.b, p.policy.proof, &p.stats.Edges)
}
