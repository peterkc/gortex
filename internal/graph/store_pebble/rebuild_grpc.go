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

// SQLite elects destinations in windows of 5,000 moves, not inputs.
const rebuildGRPCWindow = 5000

const rebuildGRPCRowLimit = 16 << 20

// RebuildGRPCStats describe the candidate and mutation rows at the first
// registry turn, and actual table bytes written at its scratch threshold.
type RebuildGRPCStats struct {
	Stubs, Registrations, Resolved, Moves, SpillBytes uint64
}

type rebuildGRPC struct {
	b         *BuildStore
	threshold int
	unflushed int
	stats     RebuildGRPCStats
}

// RebuildGRPC runs on a quiet private graph at the first framework turn.
// rebind computes the native payload for one typed stub; it returns false
// for a stationary row. The worker must discard the graph on any error.
func (b *BuildStore) RebuildGRPC(ctx context.Context, receipts iter.Seq2[ParseFacts, error], spillAt int,
	rebind func(e *graph.Edge, service, method, handler, origin string) bool,
) (stats RebuildGRPCStats, err error) {
	defer func() {
		if err != nil {
			stats = RebuildGRPCStats{}
			err = fmt.Errorf("%w: %w", ErrRebuildGRPC, err)
		}
	}()
	if spillAt < 1 || spillAt > goldBufferBytes || rebind == nil || receipts == nil {
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
	p := rebuildGRPC{b: b, threshold: spillAt}
	if err := p.capture(ctx); err != nil {
		return stats, err
	}
	if p.stats.Stubs == 0 {
		return p.stats, nil
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
	if len(key)+len(value) > rebuildGRPCRowLimit {
		return fmt.Errorf("%w: gRPC scratch row", ErrRebuildLinkInputSize)
	}
	if err := p.b.pending.Set(key, value, pebble.NoSync); err != nil {
		return err
	}
	p.unflushed += len(key) + len(value)
	if p.unflushed >= p.threshold {
		before := p.b.pending.Metrics().Levels[0].TableBytesFlushed
		if err := p.b.pending.Flush(); err != nil {
			return err
		}
		p.stats.SpillBytes += p.b.pending.Metrics().Levels[0].TableBytesFlushed - before
		p.unflushed = 0
	}
	return nil
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

func (p *rebuildGRPC) reduce(ctx context.Context, rebind func(e *graph.Edge, service, method, handler, origin string) bool) error {
	prefix := rebuildGRPCKey('s')
	return p.b.RebuildView().visit(ctx, prefix, func(snap *pebble.Snapshot, r row) error {
		old := r.key[len(prefix):]
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
		handler, err := p.handlers(ctx, snap, service, method, repo, true)
		if err != nil {
			return err
		}
		origin := graph.OriginASTResolved
		if handler == "" {
			origin = graph.OriginASTInferred
			handler, err = p.handlers(ctx, snap, service, method, repo, false)
			if err != nil {
				return err
			}
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
		group := p.stats.Moves / rebuildGRPCWindow
		p.stats.Moves++
		key := append(rebuildGRPCMoveKey(group), old...)
		value, err := encodeEdge(e)
		if err != nil {
			return err
		}
		return p.put(key, append(appendKeyString(nil, handler), value...))
	})
}

func rebuildGRPCMoveKey(group uint64) []byte {
	return binary.BigEndian.AppendUint64(rebuildGRPCKey('m'), group)
}

func (p *rebuildGRPC) apply(ctx context.Context) error {
	for group := range 1 + (p.stats.Moves-1)/rebuildGRPCWindow {
		prefix := rebuildGRPCMoveKey(group)
		// Read the immutable move window twice. Removing all old keys first
		// distinguishes stationary destinations from moving siblings.
		if err := pendingVisit(ctx, p.b.pending, prefix, func(r row) error {
			key := r.key[len(prefix):]
			e, err := p.edge(p.b.pending, key)
			if err != nil {
				return err
			}
			if e == nil {
				return invalid("gRPC move has no exact old row")
			}
			return p.b.deleteEdge(key, e)
		}); err != nil {
			return err
		}
		if err := pendingVisit(ctx, p.b.pending, prefix, func(r row) error {
			_, e, err := rebuildGRPCMove(r.value)
			if err != nil {
				return err
			}
			prior, err := p.edge(p.b.pending, pendingEdgeKey(e))
			if err != nil || prior != nil {
				return err
			}
			return p.b.addEdge(e)
		}); err != nil {
			return err
		}
	}
	return nil
}

func rebuildGRPCMove(value []byte) (string, *graph.Edge, error) {
	handler, raw, err := readKeyString(value)
	if err != nil {
		return "", nil, err
	}
	e, err := decodeEdge(raw)
	return handler, e, err
}

func rebuildGRPCRecorder(old []byte, e *graph.Edge) []byte {
	service, method := rebuildGRPCStub(e)
	return rebuildGRPCKey('e', string(old), service, method)
}

func (p *rebuildGRPC) wantProvenance(e *graph.Edge, old []byte, handler string) error {
	if err := p.put(rebuildGRPCRecorder(old, e), []byte{rebuildCapabilityWanted}); err != nil {
		return err
	}
	if err := p.put(rebuildGRPCKey('n', e.From), []byte{rebuildCapabilityWanted}); err != nil {
		return err
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
	bit := rebuildCapabilityUnproved
	if proved {
		bit = rebuildCapabilityProved
	}
	if state&bit != 0 {
		return nil
	}
	return p.put(key, []byte{state | bit})
}

func (p *rebuildGRPC) state(key []byte) (byte, error) {
	return pendingGet(p.b.pending, key, func(value []byte) (byte, error) {
		if len(value) != 1 {
			return 0, invalid("gRPC receipt proof")
		}
		return value[0], nil
	})
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
	var refused RebuildUnknownProvenanceError
	if err := pendingVisit(ctx, p.b.pending, rebuildGRPCKey('m'), func(r row) error {
		handler, e, err := rebuildGRPCMove(r.value)
		if err != nil {
			return err
		}
		old := r.key[len(rebuildGRPCMoveKey(0)):]
		keys := [][]byte{rebuildGRPCRecorder(old, e), rebuildGRPCKey('n', e.From)}
		if handler != "" {
			keys = append(keys, rebuildGRPCKey('n', handler))
		}
		for _, key := range keys {
			state, err := p.state(key)
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
