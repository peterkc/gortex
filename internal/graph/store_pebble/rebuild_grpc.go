package store_pebble

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"iter"
	"strings"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/pathkey"
)

var ErrRebuildGRPC = errors.New("pebble rebuild gRPC stage failed")

var rebuildGRPCPrefix = []byte{'g', 'r'}

// Bound the encoded rows retained by the gRPC turn.
const rebuildGRPCRowLimit = 16 << 20

// RebuildGRPCStats reports candidates and mutations in the first registry turn.
type RebuildGRPCStats struct {
	Stubs, Registrations, Resolved, Moves uint64
}

type rebuildGRPC struct {
	b           *BuildStore
	contractIDs map[string]string
	stats       RebuildGRPCStats
}

// RebuildGRPC runs on a quiet private graph at the first framework turn.
// rebind computes the native payload for one typed stub; it returns false
// for a stationary row. The worker must discard the graph on any error.
func (b *BuildStore) RebuildGRPC(ctx context.Context, contractIDs map[string]string, receipts iter.Seq2[ParseFacts, error],
	rebind func(e *graph.Edge, service, method, handler, origin string) bool,
) (stats RebuildGRPCStats, err error) {
	defer func() {
		if err != nil {
			stats = RebuildGRPCStats{}
			err = fmt.Errorf("%w: %w", ErrRebuildGRPC, err)
		}
	}()
	if rebind == nil || receipts == nil {
		return stats, fmt.Errorf("%w: invalid gRPC stage inputs", ErrRebuildLinkInputSize)
	}
	occupied, err := rebuildGuardHasPrefix(ctx, b.pending, rebuildGRPCPrefix)
	if err != nil {
		return stats, err
	}
	if occupied {
		return stats, invalid("gRPC scratch range occupied")
	}
	defer func() {
		err = errors.Join(err, b.pending.DeleteRange(rebuildGRPCPrefix, prefixEnd(rebuildGRPCPrefix), pebble.NoSync))
	}()
	p := rebuildGRPC{b: b, contractIDs: contractIDs}
	if err := p.capture(ctx); err != nil {
		return stats, err
	}
	if p.stats.Stubs == 0 {
		return p.stats, nil
	}
	if err := p.order(ctx, receipts); err != nil {
		return p.stats, err
	}
	if err := p.reduce(ctx, rebind); err != nil {
		return stats, err
	}
	if p.stats.Moves == 0 {
		return p.stats, nil
	}
	if err := p.prove(ctx, receipts); err != nil {
		return stats, err
	}
	return p.stats, b.withWrites(func() error { return p.apply(ctx) })
}

func rebuildGRPCKey(column byte, fields ...string) []byte {
	return keyStrings([]byte{rebuildGRPCPrefix[0], rebuildGRPCPrefix[1], column}, fields...)
}

func (p *rebuildGRPC) put(key, value []byte) error {
	return p.b.putRebuildScratch(key, value, rebuildGRPCRowLimit, "gRPC")
}

func rebuildGRPCInput(key, value []byte) error {
	if len(key)+len(value) > rebuildGRPCRowLimit {
		return fmt.Errorf("%w: gRPC input row", ErrRebuildLinkInputSize)
	}
	return nil
}

func rebuildGRPCStub(e *graph.Edge) (service, method string) {
	if via, _ := e.Meta["via"].(string); via == "grpc.stub" {
		service, _ = e.Meta["grpc_service"].(string)
		method, _ = e.Meta["grpc_method"].(string)
	}
	return service, method
}

func (p *rebuildGRPC) capture(ctx context.Context) error {
	return p.b.RebuildView().visit(ctx, []byte{familyEdge, edgeOut}, func(_ *pebble.Snapshot, row row) error {
		kind, err := rebuildAttributionEdgeKind(row.key)
		if err != nil || kind != graph.EdgeCalls {
			return err
		}
		if err := rebuildGRPCInput(row.key, row.value); err != nil {
			return err
		}
		e, err := decodeEdge(row.value)
		if err != nil {
			return err
		}
		if !bytes.Equal(row.key, pendingEdgeKey(e)) {
			return invalid("gRPC census call does not match key")
		}
		if e.From == "" {
			return nil
		}
		if service, method := rebuildGRPCStub(e); service != "" && method != "" {
			p.stats.Stubs++
			if err := p.put(append(rebuildGRPCKey('s'), row.key...), nil); err != nil {
				return err
			}
		}
		service, _ := e.Meta["grpc_register_service"].(string)
		impl, _ := e.Meta["grpc_register_impl"].(string)
		if service == "" {
			return nil
		}
		p.stats.Registrations++
		if impl == "" {
			return nil
		}
		// Neither the registration target nor site participates in type election.
		return p.put(rebuildGRPCKey('r', service, e.From, impl), nil)
	})
}

func (p *rebuildGRPC) node(reader pebble.Reader, id string) (*graph.Node, error) {
	key := pendingNodeKey(id)
	return pendingGet(reader, key, func(value []byte) (*graph.Node, error) {
		if err := rebuildGRPCInput(key, value); err != nil {
			return nil, err
		}
		n, err := decodeNodeLight(value)
		if err == nil && n.ID != id {
			return nil, invalid("gRPC node does not match key")
		}
		return n, err
	})
}

func (p *rebuildGRPC) edge(reader pebble.Reader, key []byte) (*graph.Edge, error) {
	return pendingGet(reader, key, func(value []byte) (*graph.Edge, error) {
		if err := rebuildGRPCInput(key, value); err != nil {
			return nil, err
		}
		e, err := decodeEdge(value)
		if err == nil && !bytes.Equal(key, pendingEdgeKey(e)) {
			return nil, invalid("gRPC call does not match key")
		}
		return e, err
	})
}

func (p *rebuildGRPC) names(ctx context.Context, snap *pebble.Snapshot, name string, kind graph.NodeKind, visit func(*graph.Node) error) error {
	return pendingVisit(ctx, snap, keyStrings([]byte{familyName, nameRebuildScoped}, name), func(r row) error {
		if err := rebuildGRPCInput(r.key, r.value); err != nil {
			return err
		}
		_, _, id, _, err := readRebuildCandidateIndex(nameRebuildScoped, r.key, r.value)
		if err != nil {
			return err
		}
		n, err := p.node(snap, id)
		if err != nil {
			return err
		}
		if n == nil || !bytes.Equal(r.key, rebuildCandidateKey(nameRebuildScoped, n)) {
			return invalid("gRPC name index has no matching node")
		}
		if n.Kind == kind {
			return visit(n)
		}
		return nil
	})
}

func (p *rebuildGRPC) incoming(ctx context.Context, snap *pebble.Snapshot, target string, kind graph.EdgeKind, visit func(string) error) error {
	prefix := keyStrings([]byte{familyEdge, edgeIn}, target)
	return pendingVisit(ctx, snap, prefix, func(r row) error {
		if err := rebuildGRPCInput(r.key, r.value); err != nil {
			return err
		}
		if len(r.value) < 2 || !bytes.Equal(r.key[len(prefix):], r.value) || !bytes.Equal(r.value[:2], []byte{familyEdge, edgeOut}) {
			return invalid("gRPC reverse edge index identity")
		}
		edgeKind, err := rebuildAttributionEdgeKind(r.value)
		if err != nil || edgeKind != kind {
			return err
		}
		e, err := p.edge(snap, r.value)
		if err != nil {
			return err
		}
		if e == nil || e.To != target {
			return invalid("gRPC reverse edge index has no matching row")
		}
		return visit(e.From)
	})
}

// A repeated ID from registrations, membership sites or interfaces is one
// method. Two distinct IDs suffice to disprove uniqueness in a bucket.
type rebuildGRPCChoice struct {
	id        string
	ambiguous bool
}

func (c *rebuildGRPCChoice) add(id string) {
	if c.id == "" {
		c.id = id
	} else if c.id != id {
		c.ambiguous = true
	}
}

func (c rebuildGRPCChoice) unique() string {
	if c.ambiguous {
		return ""
	}
	return c.id
}

func rebuildGRPCDir(path string) string {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		return path[:i]
	}
	return ""
}

func (p *rebuildGRPC) registeredType(ctx context.Context, snap *pebble.Snapshot, impl, source string) (string, error) {
	var repo, dir, nearby string
	n, err := p.node(snap, source)
	if err != nil {
		return "", err
	}
	if n != nil {
		repo, dir = n.RepoPrefix, rebuildGRPCDir(n.FilePath)
	}
	var all, local rebuildGRPCChoice
	err = p.names(ctx, snap, impl, graph.KindType, func(n *graph.Node) error {
		all.add(n.ID)
		if dir != "" && rebuildGRPCDir(n.FilePath) == dir && (nearby == "" || n.ID < nearby) {
			// SQLite NodesByKinds is ordered by ID, even with the native cache.
			nearby = n.ID
		}
		if repo != "" && n.RepoPrefix == repo {
			local.add(n.ID)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if id := all.unique(); id != "" {
		return id, nil
	}
	if nearby != "" {
		return nearby, nil
	}
	return local.unique(), nil
}

func (p *rebuildGRPC) handlers(ctx context.Context, snap *pebble.Snapshot, service, method, repo string, registration bool) (string, error) {
	var all, local rebuildGRPCChoice
	members := func(id string) error {
		return p.incoming(ctx, snap, id, graph.EdgeMemberOf, func(id string) error {
			n, err := p.node(snap, id)
			if err != nil || n == nil || n.Kind != graph.KindMethod || n.Name != method {
				return err
			}
			all.add(n.ID)
			if repo != "" && n.RepoPrefix == repo {
				local.add(n.ID)
			}
			return nil
		})
	}
	var err error
	if registration {
		prefix := rebuildGRPCKey('r', service)
		err = pendingVisit(ctx, snap, prefix, func(r row) error {
			source, rest, err := readKeyString(r.key[len(prefix):])
			if err != nil {
				return err
			}
			impl, rest, err := readKeyString(rest)
			if err != nil || len(rest) != 0 {
				return errors.Join(err, invalid("gRPC registration scratch key"))
			}
			id, err := p.registeredType(ctx, snap, impl, source)
			if err != nil || id == "" {
				return err
			}
			return members(id)
		})
	} else {
		err = p.names(ctx, snap, service+"Server", graph.KindInterface, func(n *graph.Node) error {
			return p.incoming(ctx, snap, n.ID, graph.EdgeImplements, func(id string) error {
				n, err := p.node(snap, id)
				if err != nil || n == nil || strings.HasPrefix(n.Name, "Unimplemented") {
					return err
				}
				return members(id)
			})
		})
	}
	if err != nil {
		return "", err
	}
	if local.id != "" {
		return local.unique(), nil
	}
	return all.unique(), nil
}

// order replays exact old call keys in receipt order, including the extractor's
// order within each file. This is native SQLite's order only on its direct
// write path, where a cold build inserts edges in receipt order and the global
// pass reads them ORDER BY id. On the in-memory shadow path, DrainEdgeBatches
// sorts each batch and the inline pass reindexes without 5,000-move windows, so
// native can keep a different mover there. The rebuild always uses this order;
// the VM proof labels such files as native-defect differences. Do not infer an
// ordinal from the live Pebble key or from an unrelated same-site Call.
func (p *rebuildGRPC) order(ctx context.Context, receipts iter.Seq2[ParseFacts, error]) error {
	stubs := rebuildGRPCKey('s')
	ordered := rebuildGRPCKey('o')
	recorders := rebuildGRPCKey('i')
	// Known limit: unlike MyBatis, gRPC refuses duplicate receipt claims
	// before election, even if every claimant would stay stationary and
	// native would write nothing.
	if err := pendingVisit(ctx, p.b.pending, stubs, func(r row) error {
		live := r.key[len(stubs):]
		custody, err := readRebuildEdgeCustody(p.b.pending, live)
		if errors.Is(err, ErrRebuildUnknownProvenance) {
			return nil // A stale producer stamp cannot supply a raw ordinal.
		}
		if err != nil || custody.proof != 0 {
			// A producer record cannot supply a raw ordinal. The unordered
			// tail still elects this stub; a move needs proof during reduce.
			return err
		}
		raw := custody.raw
		if raw == nil {
			raw = live
		}
		key := append(bytes.Clone(recorders), raw...)
		present, err := pendingGet(p.b.pending, key, func([]byte) (bool, error) { return true, nil })
		if err != nil {
			return err
		}
		if present {
			id, err := decodeLinkRecorder(live)
			if err != nil {
				return err
			}
			return &RebuildUnknownProvenanceError{Count: 1, First: id}
		}
		return p.put(key, live)
	}); err != nil {
		return err
	}
	var ordinal uint64
	for facts, err := range receipts {
		if err != nil {
			return err
		}
		for _, e := range facts.Edges {
			if err := ctx.Err(); err != nil {
				return err
			}
			if e.Kind != graph.EdgeCalls || e.From == "" {
				continue
			}
			service, method := rebuildGRPCStub(e)
			if service == "" || method == "" {
				continue
			}
			raw := pendingEdgeKey(e)
			old, err := pendingGet(p.b.pending, append(bytes.Clone(recorders), raw...), func(value []byte) ([]byte, error) {
				return bytes.Clone(value), nil
			})
			if err != nil {
				return err
			}
			if old == nil {
				continue
			}
			key := append(bytes.Clone(stubs), old...)
			ready, err := pendingGet(p.b.pending, key, func(raw []byte) (bool, error) {
				return len(raw) == 0, nil
			})
			if err != nil {
				return err
			}
			if !ready {
				continue
			}
			live, err := p.edge(p.b.pending, old)
			if err != nil {
				return err
			}
			if live == nil {
				return invalid("gRPC census order lost a live call")
			}
			if gotService, gotMethod := rebuildGRPCStub(live); gotService != service || gotMethod != method {
				continue
			}
			if err := p.put(binary.BigEndian.AppendUint64(bytes.Clone(ordered), ordinal), old); err != nil {
				return err
			}
			if err := p.put(key, []byte{1}); err != nil {
				return err
			}
			ordinal++
		}
	}
	// Elect the remaining stubs after the receipt-ordered rows. A stationary
	// stub can stay unchanged. For a move, wantProvenance refuses stale or
	// producer custody, and prove requires an exact receipt.
	return pendingVisit(ctx, p.b.pending, stubs, func(r row) error {
		if len(r.value) != 0 {
			return nil
		}
		err := p.put(binary.BigEndian.AppendUint64(bytes.Clone(ordered), ordinal), r.key[len(stubs):])
		ordinal++
		return err
	})
}

// election memoizes successful lookups, including an absent winner. Keep the
// memo on disk; the number of service/method/repository tuples is input-sized.
func (p *rebuildGRPC) election(ctx context.Context, snap *pebble.Snapshot, service, method, repo string) (string, string, error) {
	key := rebuildGRPCKey('c', service, method, repo)
	cached, err := pendingGet(p.b.pending, key, func(value []byte) ([]byte, error) {
		return bytes.Clone(value), nil
	})
	if err != nil {
		return "", "", err
	}
	if cached != nil {
		handler, tail, err := readKeyString(cached)
		return handler, string(tail), err
	}
	handler, err := p.handlers(ctx, snap, service, method, repo, true)
	if err != nil {
		return "", "", err
	}
	origin := graph.OriginASTResolved
	if handler == "" {
		origin = graph.OriginASTInferred
		handler, err = p.handlers(ctx, snap, service, method, repo, false)
		if err != nil {
			return "", "", err
		}
	}
	return handler, origin, p.put(key, append(appendKeyString(nil, handler), origin...))
}

func (p *rebuildGRPC) reduce(ctx context.Context, rebind func(e *graph.Edge, service, method, handler, origin string) bool) error {
	prefix := rebuildGRPCKey('o')
	return p.b.RebuildView().visit(ctx, prefix, func(snap *pebble.Snapshot, r row) error {
		old := r.value
		e, err := p.edge(snap, old)
		if err != nil {
			return err
		}
		if e == nil {
			return invalid("gRPC census lost a live call at its first turn")
		}
		service, method := rebuildGRPCStub(e)
		var repo string
		caller, err := p.node(snap, e.From)
		if err != nil {
			return err
		}
		if caller != nil {
			repo = caller.RepoPrefix
		}
		handler, origin, err := p.election(ctx, snap, service, method, repo)
		if err != nil {
			return err
		}
		if handler != "" {
			p.stats.Resolved++
		}
		if !rebind(e, service, method, handler, origin) {
			return nil
		}
		if err := p.wantProvenance(e, old, handler); err != nil {
			return err
		}
		group := p.stats.Moves / graph.SQLiteReindexChunkSize
		key := binary.BigEndian.AppendUint64(rebuildGRPCMoveKey(group), p.stats.Moves)
		p.stats.Moves++
		value, err := encodeEdge(e)
		if err != nil {
			return err
		}
		return p.put(key, append(keyStrings(nil, string(old), handler), value...))
	})
}

func rebuildGRPCMoveKey(group uint64) []byte {
	return binary.BigEndian.AppendUint64(rebuildGRPCKey('m'), group)
}

func (p *rebuildGRPC) apply(ctx context.Context) error {
	for group := range 1 + (p.stats.Moves-1)/graph.SQLiteReindexChunkSize {
		prefix := rebuildGRPCMoveKey(group)
		// Read the immutable move window twice. Removing all old keys first
		// distinguishes stationary destinations from moving siblings.
		if err := pendingVisit(ctx, p.b.pending, prefix, func(r row) error {
			old, _, err := readKeyString(r.value)
			if err != nil {
				return err
			}
			key := []byte(old)
			e, err := p.edge(p.b.pending, key)
			if err != nil {
				return err
			}
			if e == nil {
				return invalid("gRPC move has no exact old row")
			}
			custody, err := readRebuildEdgeCustody(p.b.pending, key)
			if err != nil {
				return err
			}
			// Copy only inherited custody before deleteEdge removes it. The
			// exact old key proved by this turn is already in the move row.
			if saved := rebuildMoveCustodyBytes(custody); len(saved) != 0 {
				if err := p.put(rebuildGRPCMoveCustodyKey(r.key), saved); err != nil {
					return err
				}
			}
			return p.b.deleteEdge(key, e)
		}); err != nil {
			return err
		}
		if err := pendingVisit(ctx, p.b.pending, prefix, func(r row) error {
			old, _, e, err := rebuildGRPCMove(r.value)
			if err != nil {
				return err
			}
			prior, err := p.edge(p.b.pending, pendingEdgeKey(e))
			if err != nil || prior != nil {
				return err
			}
			custody, err := pendingGet(p.b.pending, rebuildGRPCMoveCustodyKey(r.key), decodeRebuildMoveCustody)
			if err != nil {
				return err
			}
			if len(custody.raw) == 0 && custody.proof == 0 {
				custody.raw = []byte(old)
			}
			return p.b.addEdgeWithCustody(e, custody)
		}); err != nil {
			return err
		}
	}
	return nil
}

func rebuildGRPCMoveCustodyKey(move []byte) []byte {
	key := bytes.Clone(move)
	key[2] = 'l'
	return key
}

// Only a checked custody value or a stage proof constant enters move scratch.
// A raw out-key is at least two bytes; a synthetic proof is one byte. The
// eventual writer binds that proof to the new winner's encoded edge.
func rebuildMoveCustodyBytes(custody rebuildEdgeCustody) []byte {
	if custody.proof != 0 {
		return []byte{byte(custody.proof)}
	}
	return custody.raw
}

func decodeRebuildMoveCustody(value []byte) (rebuildEdgeCustody, error) {
	if len(value) == 1 {
		return rebuildEdgeCustody{proof: rebuildEdgeProof(value[0])}, nil
	}
	return rebuildEdgeCustody{raw: bytes.Clone(value)}, nil
}

func rebuildGRPCMove(value []byte) (string, string, *graph.Edge, error) {
	old, tail, err := readKeyString(value)
	if err != nil {
		return "", "", nil, err
	}
	handler, raw, err := readKeyString(tail)
	if err != nil {
		return "", "", nil, err
	}
	e, err := decodeEdge(raw)
	return old, handler, e, err
}

func rebuildGRPCRecorder(old []byte, e *graph.Edge) []byte {
	service, method := rebuildGRPCStub(e)
	return rebuildGRPCKey('e', string(old), service, method)
}

func (p *rebuildGRPC) wantProvenance(e *graph.Edge, old []byte, handler string) error {
	raw, err := rebuildReceiptKey(p.b.pending, old)
	if err != nil {
		return err
	}
	if err := p.put(rebuildGRPCRecorder(raw, e), []byte{rebuildCapabilityWanted}); err != nil {
		return err
	}
	// The early set already proves a replaced caller. Its displaced parse
	// receipt cannot prove the live contract row, but the call still needs
	// its exact original recorder and the selected handler needs its node.
	if _, contract := p.contractIDs[e.From]; !contract {
		if err := p.put(rebuildGRPCKey('n', e.From), []byte{rebuildCapabilityWanted}); err != nil {
			return err
		}
	}
	if handler != "" {
		return p.put(rebuildGRPCKey('n', handler), []byte{rebuildCapabilityWanted})
	}
	return nil
}

func (p *rebuildGRPC) note(key []byte, proved bool) error {
	state, err := p.state(key)
	if err != nil || state == 0 {
		return err
	}
	return noteRebuildScratchState(p.put, key, state, rebuildScratchProofBit(proved))
}

func (p *rebuildGRPC) state(key []byte) (byte, error) {
	return rebuildScratchState(p.b.pending, key, "gRPC receipt proof")
}

func (p *rebuildGRPC) record(ctx context.Context, facts ParseFacts) error {
	file := pathkey.Normalize(facts.File.FilePath)
	lang := facts.EffectiveLanguage
	supported := lang == "go" || lang == "typescript" || lang == "javascript"
	for _, n := range facts.Nodes {
		if err := ctx.Err(); err != nil {
			return err
		}
		key := rebuildGRPCKey('n', n.ID)
		state, err := p.state(key)
		if err != nil {
			return err
		}
		if state == 0 || state&rebuildCapabilityUnproved != 0 {
			continue
		}
		if !supported || n.FilePath != file {
			if err := p.note(key, false); err != nil {
				return err
			}
			continue
		}
		live, err := p.node(p.b.pending, n.ID)
		if err != nil {
			return err
		}
		if live != nil && live.FilePath == file && live.Language == n.Language && live.Kind == n.Kind &&
			live.Name == n.Name && live.RepoPrefix == n.RepoPrefix {
			if err := p.note(key, true); err != nil {
				return err
			}
		}
	}
	for _, e := range facts.Edges {
		if err := ctx.Err(); err != nil {
			return err
		}
		if e.Kind == graph.EdgeCalls {
			if service, method := rebuildGRPCStub(e); service != "" && method != "" {
				if err := p.note(rebuildGRPCRecorder(pendingEdgeKey(e), e), supported && e.FilePath == file); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (p *rebuildGRPC) prove(ctx context.Context, receipts iter.Seq2[ParseFacts, error]) error {
	for facts, err := range receipts {
		if err != nil {
			return err
		}
		if err := p.record(ctx, facts); err != nil {
			return err
		}
	}
	var refused rebuildScratchRefusals
	if err := pendingVisit(ctx, p.b.pending, rebuildGRPCKey('m'), func(r row) error {
		old, handler, e, err := rebuildGRPCMove(r.value)
		if err != nil {
			return err
		}
		raw, err := rebuildReceiptKey(p.b.pending, []byte(old))
		if err != nil {
			return err
		}
		keys := [][]byte{rebuildGRPCRecorder(raw, e)}
		if _, contract := p.contractIDs[e.From]; !contract {
			keys = append(keys, rebuildGRPCKey('n', e.From))
		}
		if handler != "" {
			keys = append(keys, rebuildGRPCKey('n', handler))
		}
		for _, key := range keys {
			state, err := p.state(key)
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
