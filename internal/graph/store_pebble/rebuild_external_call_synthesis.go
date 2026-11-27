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
)

// ErrRebuildExternalCallSynthesis marks a failed private external-call pass.
var ErrRebuildExternalCallSynthesis = errors.New("rebuild external-call synthesis failed")

// RebuildExternalCallCandidate describes a native package vote for one site.
// OldTo is the terminal used in the native sort; empty means the edge's To.
type RebuildExternalCallCandidate struct {
	Node     *graph.Node
	Language string
	OldTo    string
}

// RebuildExternalCallRules supplies native classification and the uncertain
// effects of skipped producers. The callbacks do not mutate the working graph.
type RebuildExternalCallRules struct {
	Candidate func(*graph.Edge, *graph.Node) (RebuildExternalCallCandidate, bool)
	Maybe     func(e *graph.Edge, caller *graph.Node, receiptLanguage string, live bool) []RebuildExternalCallCandidate
	Rebind    func(*graph.Edge, string, string) graph.EdgeReindex
	Extra     func(context.Context, RebuildView, func(*graph.Edge, RebuildExternalCallCandidate) error) error
}

const rebuildExternalCallRowBytes = 16 << 20

const (
	externalCallCertain byte = iota + 1
	externalCallOptional
	externalCallPossible
)

type rebuildExternalCallPass struct {
	b     *BuildStore
	rules RebuildExternalCallRules
	count uint64
}

// RebuildExternalCallSynthesis runs under the worker's exclusive stage
// ownership. The pending index stores receipt keys, ordered votes and mutation
// plans on disk. All retained-effect proof finishes before any graph write.
// The count is native's selected rebind attempts, including occupied targets
// but not converging loser deletes or rows already pointing at a package node.
func (b *BuildStore) RebuildExternalCallSynthesis(ctx context.Context, receipts iter.Seq2[ParseFacts, error], rules RebuildExternalCallRules) (count uint64, err error) {
	defer func() {
		if err != nil {
			count, err = 0, fmt.Errorf("%w: %w", ErrRebuildExternalCallSynthesis, err)
		}
	}()
	if receipts == nil || rules.Candidate == nil || rules.Maybe == nil || rules.Rebind == nil {
		return 0, invalid("external-call synthesis requires receipts and rules")
	}
	view := b.RebuildView()
	if err := view.mutationError(); err != nil {
		return 0, err
	}
	prefix := rebuildExternalCallColumn(0)[:2]
	occupied, err := rebuildGuardHasPrefix(ctx, b.pending, prefix)
	if err != nil {
		return 0, err
	}
	if occupied {
		return 0, invalid("external-call synthesis scratch range occupied")
	}
	defer func() { err = errors.Join(err, b.pending.DeleteRange(prefix, prefixEnd(prefix), pebble.NoSync)) }()
	p := rebuildExternalCallPass{b: b, rules: rules}
	if err := p.receipts(ctx, receipts); err != nil {
		return 0, err
	}
	if err := p.live(ctx, view); err != nil {
		return 0, err
	}
	if err := p.receiptEntrants(ctx); err != nil {
		return 0, err
	}
	if rules.Extra != nil {
		if err := rules.Extra(ctx, view, p.possible); err != nil {
			return 0, err
		}
	}
	if err := p.groups(ctx); err != nil {
		return 0, err
	}
	if err := p.packages(ctx); err != nil {
		return 0, err
	}
	return p.count, b.withWrites(func() error { return p.apply(ctx) })
}

// Scratch columns: file authority f, exact receipts r, absent receipt inputs h,
// consumed raw keys c, site votes g, package votes p, retained packages t, and
// the checked mutation plan (node n, delete d, move m, custody l).
func rebuildExternalCallColumn(column byte) []byte { return []byte{'g', '9', column} }

func (p *rebuildExternalCallPass) put(key, value []byte) error {
	return p.b.putRebuildScratch(key, value, rebuildExternalCallRowBytes, "external-call synthesis")
}

func (p *rebuildExternalCallPass) present(key []byte) (bool, error) {
	return pendingGet(p.b.pending, key, fnPointerPresent)
}

func (p *rebuildExternalCallPass) visit(ctx context.Context, column byte, visit func(row) error) error {
	return pendingVisit(ctx, p.b.pending, rebuildExternalCallColumn(column), visit)
}

func rebuildExternalCallInput(key, value []byte) error {
	if len(key)+len(value) > rebuildExternalCallRowBytes {
		return fmt.Errorf("%w: external-call synthesis input", ErrRebuildLinkInputSize)
	}
	return nil
}

func rebuildExternalCallKind(kind graph.EdgeKind) bool {
	return kind == graph.EdgeCalls || kind == graph.EdgeReferences
}

// Only terminals and unresolved alternatives need a central edge payload.
// This is a key filter, not the native terminal parser or stdlib policy.
func rebuildExternalCallTarget(to string) bool {
	return !strings.HasPrefix(to, "external-call::") && (strings.HasPrefix(to, "dep::") ||
		graph.IsStdlibStub(to) || strings.HasPrefix(to, "external::") || graph.IsUnresolvedTarget(to))
}

func rebuildExternalCallRetained(lang string) bool {
	return lang == "go" || lang == "javascript" || lang == "typescript"
}

func (p *rebuildExternalCallPass) fileLanguage(file string) (string, bool, error) {
	raw, err := pendingGet(p.b.pending, keyStrings(rebuildExternalCallColumn('f'), file), copyValue)
	if err != nil || raw == nil {
		return "", false, err
	}
	return string(raw[1:]), true, nil
}

func (p *rebuildExternalCallPass) receipts(ctx context.Context, receipts iter.Seq2[ParseFacts, error]) error {
	var ordinal uint64
	for f, err := range receipts {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		lang := f.EffectiveLanguage
		prev, seen, err := p.fileLanguage(f.File.FilePath)
		if err != nil {
			return err
		}
		if seen && prev != lang {
			return invalid("conflicting external-call receipt file languages")
		}
		if !seen {
			if err := p.put(keyStrings(rebuildExternalCallColumn('f'), f.File.FilePath), append([]byte{1}, lang...)); err != nil {
				return err
			}
		}
		for _, e := range f.Edges {
			if err := ctx.Err(); err != nil {
				return err
			}
			if e == nil || !rebuildReceiptInputKind(e.Kind) {
				continue
			}
			key := pendingEdgeKey(e)
			if err := p.put(append(rebuildExternalCallColumn('r'), key...), nil); err != nil {
				return err
			}
			if !rebuildExternalCallKind(e.Kind) || !rebuildExternalCallTarget(e.To) {
				continue
			}
			exact, err := p.present(key)
			if err != nil {
				return err
			}
			if exact {
				// The live scan owns this incarnation, not a receipt ghost.
				continue
			}
			raw, err := encodeEdge(e)
			if err != nil {
				return err
			}
			// An ordinal preserves multiple absent witnesses at one raw key.
			key = binary.BigEndian.AppendUint64(rebuildExternalCallColumn('h'), ordinal)
			if err := p.put(key, append(appendKeyString(nil, lang), raw...)); err != nil {
				return err
			}
			ordinal++
		}
	}
	return ctx.Err()
}

func (p *rebuildExternalCallPass) node(id string, decode decodeRow[*graph.Node]) (*graph.Node, error) {
	key := pendingNodeKey(id)
	return pendingGet(p.b.pending, key, func(value []byte) (*graph.Node, error) {
		if err := rebuildExternalCallInput(key, value); err != nil {
			return nil, err
		}
		n, err := decode(value)
		if err == nil && n.ID != id {
			return nil, invalid("external-call node does not match key")
		}
		return n, err
	})
}

func (p *rebuildExternalCallPass) edge(key []byte) (*graph.Edge, error) {
	return pendingGet(p.b.pending, key, func(value []byte) (*graph.Edge, error) {
		if err := rebuildExternalCallInput(key, value); err != nil {
			return nil, err
		}
		e, err := decodeEdge(value)
		if err == nil && !bytes.Equal(key, pendingEdgeKey(e)) {
			return nil, invalid("external-call edge does not match key")
		}
		return e, err
	})
}

// A moved raw key has an actual successor. It is not a possible entrant
// merely because the original key is now absent. Named producer proofs bind
// to the live payload; untracked moves never prove receipt or producer custody.
func (p *rebuildExternalCallPass) custody(key []byte) (rebuildEdgeCustody, bool, error) {
	c, err := readRebuildEdgeCustody(p.b.pending, key)
	if err != nil || c.proof != 0 {
		return c, err == nil && c.proof != rebuildProofUntrackedMove, err
	}
	if c.raw == nil {
		c.raw = key
	}
	exact, err := p.present(append(rebuildExternalCallColumn('r'), c.raw...))
	if !exact {
		return rebuildEdgeCustody{proof: rebuildProofUntrackedMove}, false, err
	}
	return c, true, err
}

func (p *rebuildExternalCallPass) receiptEntrants(ctx context.Context) error {
	if err := pendingVisit(ctx, p.b.pending, rebuildLinkRecorderPrefix, func(r row) error {
		if len(r.value) < 2 || r.value[0] != familyEdge || r.value[1] != edgeOut {
			return nil
		}
		raw, err := decodeLinkRecorder(r.value)
		if err != nil || !rebuildExternalCallKind(raw.Kind) || !rebuildExternalCallTarget(raw.To) {
			return err
		}
		live, err := p.present(r.key[len(rebuildLinkRecorderPrefix):])
		if err != nil || !live {
			return err
		}
		return p.put(append(rebuildExternalCallColumn('c'), r.value...), nil)
	}); err != nil {
		return err
	}
	return p.visit(ctx, 'h', func(r row) error {
		lang, raw, err := readKeyString(r.value)
		if err != nil {
			return err
		}
		e, err := decodeEdge(raw)
		if err != nil {
			return err
		}
		consumed, err := p.present(append(rebuildExternalCallColumn('c'), pendingEdgeKey(e)...))
		if err != nil || consumed {
			return err
		}
		caller, err := p.node(e.From, decodeNodeLight)
		if err != nil {
			return err
		}
		for _, candidate := range p.rules.Maybe(e, caller, lang, false) {
			if err := p.possible(e, candidate); err != nil {
				return err
			}
		}
		return nil
	})
}

func (p *rebuildExternalCallPass) live(ctx context.Context, view RebuildView) error {
	return view.read(ctx, func(snap *pebble.Snapshot) (err error) {
		prefix := []byte{familyEdge, edgeOut}
		it, err := snap.NewIterWithContext(ctx, &pebble.IterOptions{LowerBound: prefix, UpperBound: prefixEnd(prefix)})
		if err != nil {
			return err
		}
		defer func() { err = errors.Join(err, it.Close()) }()
		for ok := it.First(); ok; ok = it.Next() {
			if err := ctx.Err(); err != nil {
				return err
			}
			_, rest, err := readKeyString(it.Key()[len(prefix):])
			if err != nil {
				return err
			}
			kind, rest, err := readKeyString(rest)
			if err != nil {
				return err
			}
			if !rebuildExternalCallKind(graph.EdgeKind(kind)) {
				continue
			}
			to, _, err := readKeyString(rest)
			if err != nil {
				return err
			}
			if !rebuildExternalCallTarget(to) {
				continue
			}
			value, err := it.ValueAndErr()
			if err != nil {
				return err
			}
			if err := rebuildExternalCallInput(it.Key(), value); err != nil {
				return err
			}
			e, err := decodeEdge(value)
			if err != nil {
				return err
			}
			if !bytes.Equal(it.Key(), pendingEdgeKey(e)) {
				return invalid("external-call live edge does not match key")
			}
			if err := p.liveEdge(it.Key(), e); err != nil {
				return err
			}
		}
		return errors.Join(ctx.Err(), it.Error())
	})
}

func (p *rebuildExternalCallPass) liveEdge(key []byte, e *graph.Edge) error {
	caller, err := p.node(e.From, decodeNodeLight)
	if err != nil {
		return err
	}
	lang, _, err := p.fileLanguage(e.FilePath)
	if err != nil {
		return err
	}
	actual, eligible := p.rules.Candidate(e, caller)
	alternates := p.rules.Maybe(e, caller, lang, true)
	if eligible {
		if actual.OldTo != "" && actual.OldTo != e.To {
			return invalid("external-call live terminal changed by candidate classifier")
		}
		_, exact, err := p.custody(key)
		if err != nil {
			return err
		}
		state := externalCallCertain
		if !exact || len(alternates) != 0 {
			state = externalCallOptional
		}
		if err := p.vote(e, actual, state); err != nil {
			return err
		}
	}
	for _, alt := range alternates {
		// A live Maybe at exactly the actual tuple described possible
		// disappearance above, not a second way to add that same row.
		if eligible && alt.Node != nil && alt.Node.ID == actual.Node.ID && alt.Language == actual.Language &&
			(alt.OldTo == "" || alt.OldTo == e.To) {
			continue
		}
		if err := p.possible(e, alt); err != nil {
			return err
		}
	}
	return nil
}

func (p *rebuildExternalCallPass) possible(e *graph.Edge, c RebuildExternalCallCandidate) error {
	return p.vote(e, c, externalCallPossible)
}

// Ordered as native: From, Kind, FilePath, signed Line, package ID, old To.
// The final flag and language do not take part in the native comparison.
func (p *rebuildExternalCallPass) vote(e *graph.Edge, c RebuildExternalCallCandidate, state byte) error {
	if e == nil || !rebuildExternalCallKind(e.Kind) || e.From == "" || c.Node == nil ||
		!strings.HasPrefix(c.Node.ID, "external-call::") || graph.IsProxyID(e.From) {
		return invalid("external-call candidate identity")
	}
	old := c.OldTo
	if old == "" {
		old = e.To
	}
	if old == "" || strings.HasPrefix(old, "external-call::") {
		return invalid("external-call candidate old terminal")
	}
	group := appendKeyLine(keyStrings(rebuildExternalCallColumn('g'), e.From, string(e.Kind), e.FilePath), e.Line)
	key := append(appendKeyString(keyStrings(group, c.Node.ID, old), c.Language), state)
	if state == externalCallPossible {
		// Receipts and Extra may independently offer an already certain row.
		// Match the whole native identity and language, not just the package.
		key[len(key)-1] = externalCallCertain
		exact, err := p.present(key)
		if err != nil || exact {
			return err
		}
		key[len(key)-1] = state
		return p.put(key, nil)
	}
	if c.Node.Language != c.Language || c.Node.Kind != graph.KindModule {
		return invalid("external-call module candidate")
	}
	raw, err := encodeNode(c.Node)
	if err != nil {
		return err
	}
	return p.put(key, raw)
}

type rebuildExternalCallVote struct {
	edge  graph.Edge
	group []byte
	order []byte
	node  string
	lang  string
	state byte
}

func readRebuildExternalCallVote(key []byte) (v rebuildExternalCallVote, err error) {
	tail := key
	var kind string
	for _, s := range []*string{&v.edge.From, &kind, &v.edge.FilePath} {
		*s, tail, err = readKeyString(tail)
		if err != nil {
			return v, err
		}
	}
	if len(tail) < 8 {
		return v, invalid("external-call vote line")
	}
	v.edge.Kind = graph.EdgeKind(kind)
	v.edge.Line = int(int64(binary.BigEndian.Uint64(tail) ^ (1 << 63)))
	v.node, tail, err = readKeyString(tail[8:])
	if err != nil {
		return v, err
	}
	v.group = key[:len(key)-len(tail)]
	v.edge.To, tail, err = readKeyString(tail)
	if err != nil {
		return v, err
	}
	v.order = key[:len(key)-len(tail)]
	v.lang, tail, err = readKeyString(tail)
	if err != nil {
		return v, err
	}
	if len(tail) != 1 || tail[0] < externalCallCertain || tail[0] > externalCallPossible {
		return v, invalid("external-call vote state")
	}
	v.state = tail[0]
	return v, nil
}

func (p *rebuildExternalCallPass) groups(ctx context.Context) error {
	var group []byte
	var winner, first bool
	var retained, occupied bool
	var firstOld string
	return p.visit(ctx, 'g', func(r row) error {
		v, err := readRebuildExternalCallVote(r.key[3:])
		if err != nil {
			return err
		}
		if !bytes.Equal(group, v.group) {
			group = bytes.Clone(v.group)
			winner, first, firstOld = false, true, ""
			lang, found, err := p.fileLanguage(v.edge.FilePath)
			if err != nil {
				return err
			}
			if !found {
				caller, err := p.node(v.edge.From, decodeNodeLight)
				if err != nil {
					return err
				}
				if caller != nil {
					lang = caller.Language
				}
			}
			retained = rebuildExternalCallRetained(lang)
			incumbent := v.edge
			incumbent.To = v.node
			key := pendingEdgeKey(&incumbent)
			stored, err := p.edge(key)
			if err != nil {
				return err
			}
			occupied = stored != nil
			if occupied && retained {
				_, exact, err := p.custody(key)
				if err != nil {
					return err
				}
				if !exact {
					return fmt.Errorf("unproved external-call incumbent: %+v", graph.EdgeIdentityFor(stored))
				}
			}
		}
		if retained {
			if v.state == externalCallOptional {
				return fmt.Errorf("uncertain retained external-call terminal: %+v", graph.EdgeIdentityFor(&v.edge))
			}
			if v.state == externalCallPossible && !occupied && (!winner || v.edge.To == firstOld) {
				return fmt.Errorf("uncertain retained external-call winner: %+v", graph.EdgeIdentityFor(&v.edge))
			}
			if first {
				if err := p.put(keyStrings(rebuildExternalCallColumn('t'), v.node), nil); err != nil {
					return err
				}
			}
		}
		if err := p.put(append(keyStrings(rebuildExternalCallColumn('p'), v.node), r.key[3:]...), r.value); err != nil {
			return err
		}
		first = false
		if v.state == externalCallPossible {
			return nil
		}
		old := pendingEdgeKey(&v.edge)
		if err := p.put(append(rebuildExternalCallColumn('d'), old...), nil); err != nil {
			return err
		}
		if winner {
			return nil
		}
		winner, firstOld = true, v.edge.To
		p.count++
		if occupied {
			return nil
		}
		return p.planMove(v, old)
	})
}

func (p *rebuildExternalCallPass) planMove(v rebuildExternalCallVote, old []byte) error {
	e, err := p.edge(old)
	if err != nil {
		return err
	}
	if e == nil {
		return invalid("external-call winner missing from central edge rows")
	}
	custody, _, err := p.custody(old)
	if err != nil {
		return err
	}
	if custody.proof != 0 && custody.proof != rebuildProofUntrackedMove {
		custody.proof = rebuildProofExternalCallSynthesis
	}
	rebind := p.rules.Rebind(e, v.node, v.lang)
	dst := v.edge
	dst.To = v.node
	key := pendingEdgeKey(&dst)
	if rebind.Edge == nil || rebind.OldTo != v.edge.To || rebind.OldFrom != "" || rebind.OldKind != "" || rebind.RefreshIdentity ||
		!bytes.Equal(key, pendingEdgeKey(rebind.Edge)) {
		return invalid("external-call rebind changed the site identity")
	}
	raw, err := encodeEdge(rebind.Edge)
	if err != nil {
		return err
	}
	if err := p.put(append(rebuildExternalCallColumn('m'), key...), raw); err != nil {
		return err
	}
	return p.put(append(rebuildExternalCallColumn('l'), key...), rebuildMoveCustodyBytes(custody))
}

// A possible first creator is any vote before (or at) the first certain
// actual row. Track the whole frontier: removing an optional early creator
// can expose a different language even with no entrant before the live winner.
func (p *rebuildExternalCallPass) packages(ctx context.Context) error {
	var id, lang string
	var existing, retained, voted, different, actual bool
	var certainOrder []byte
	finish := func() error {
		if retained && !existing && (!actual || certainOrder == nil || different) {
			return fmt.Errorf("uncertain retained external-call package creator: %q", id)
		}
		return nil
	}
	if err := p.visit(ctx, 'p', func(r row) error {
		node, rest, err := readKeyString(r.key[3:])
		if err != nil {
			return err
		}
		if node != id {
			if err := finish(); err != nil {
				return err
			}
			id, lang, certainOrder = node, "", nil
			voted, different, actual = false, false, false
			n, err := p.node(id, decodeNode)
			if err != nil {
				return err
			}
			existing = n != nil
			retained, err = p.present(keyStrings(rebuildExternalCallColumn('t'), id))
			if err != nil {
				return err
			}
		}
		if existing {
			return nil
		}
		v, err := readRebuildExternalCallVote(rest)
		if err != nil {
			return err
		}
		if certainOrder == nil || bytes.Equal(certainOrder, v.order) {
			if !voted {
				lang, voted = v.lang, true
			}
			different = different || v.lang != lang
			if v.state == externalCallCertain && certainOrder == nil {
				certainOrder = bytes.Clone(v.order)
			}
		}
		if !actual && v.state != externalCallPossible {
			actual = true
			return p.put(append(rebuildExternalCallColumn('n'), pendingNodeKey(id)...), r.value)
		}
		return nil
	}); err != nil {
		return err
	}
	return finish()
}

func (p *rebuildExternalCallPass) apply(ctx context.Context) error {
	if err := p.visit(ctx, 'n', func(r row) error {
		n, err := decodeNode(r.value)
		if err != nil {
			return err
		}
		return p.b.writeNode(n)
	}); err != nil {
		return err
	}
	if err := p.visit(ctx, 'd', func(r row) error {
		key := r.key[3:]
		e, err := p.edge(key)
		if err != nil {
			return err
		}
		if e == nil {
			return invalid("external-call planned deletion missing")
		}
		return p.b.deleteEdge(key, e)
	}); err != nil {
		return err
	}
	return p.visit(ctx, 'm', func(r row) error {
		key := r.key[3:]
		e, err := decodeEdge(r.value)
		if err != nil {
			return err
		}
		custody, err := pendingGet(p.b.pending, append(rebuildExternalCallColumn('l'), key...), decodeRebuildMoveCustody)
		if err != nil {
			return err
		}
		return p.b.putEdgeWithCustody(e, custody)
	})
}
