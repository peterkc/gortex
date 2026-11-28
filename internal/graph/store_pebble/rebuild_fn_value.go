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

var ErrRebuildFnValue = errors.New("pebble rebuild function-value callbacks failed")

var rebuildFnValuePrefix = []byte{'g', 'q', 'v'}

// RebuildFnValueSite is the native callback classification and output stamp.
type RebuildFnValueSite struct {
	Name, Receiver    string
	Ungated, SkipGate bool
}

type RebuildFnValueRules struct {
	Site         func(*graph.Edge) RebuildFnValueSite
	Prototype    func(*graph.Node) bool
	LocalLinkage func(*graph.Node) bool
	Registration func(*graph.Edge, string, string, float64, string, int) *graph.Edge
	Choose       func(string, bool, bool, graph.FnValueLookups) (float64, string, int)
}

type rebuildFnValuePass struct {
	rebuildCallMoves
	rules RebuildFnValueRules
}

// RebuildFnValueCallbacks proves additions at the live callback turn. It never
// removes its input placeholder or replaces an occupied registration key.
func (b *BuildStore) RebuildFnValueCallbacks(ctx context.Context, receipts iter.Seq2[ParseFacts, error], rules RebuildFnValueRules) (count uint64, err error) {
	defer func() {
		if err != nil {
			count = 0
			err = fmt.Errorf("%w: %w", ErrRebuildFnValue, err)
		}
	}()
	if receipts == nil || rules.Site == nil || rules.Prototype == nil || rules.LocalLinkage == nil || rules.Registration == nil || rules.Choose == nil {
		return 0, invalid("function-value callback inputs")
	}
	prefix := rebuildFnValuePrefix
	busy, err := rebuildGuardHasPrefix(ctx, b.pending, prefix)
	if err != nil {
		return 0, err
	}
	if busy {
		return 0, invalid("function-value callback scratch occupied")
	}
	defer func() { err = errors.Join(err, b.pending.DeleteRange(prefix, prefixEnd(prefix), pebble.NoSync)) }()
	p := rebuildFnValuePass{
		rebuildCallMoves: rebuildCallMoves{b: b, prefix: prefix, rowBytes: 16 << 20, label: "function-value callback", inputRow: "edge row"},
		rules:            rules,
	}
	if err := p.capture(ctx); err != nil {
		return 0, err
	}
	if err := p.recordSites(ctx, receipts); err != nil {
		return 0, err
	}
	active, err := rebuildGuardHasPrefix(ctx, b.pending, p.column('h'))
	if err != nil || !active {
		return 0, err
	}
	if err := p.recordNodes(ctx, receipts); err != nil {
		return 0, err
	}
	if err := p.prove(ctx); err != nil {
		return 0, err
	}
	err = b.withWrites(func() error {
		return visitRebuildScratchKeyedEdges(ctx, b.pending, p.column('w'), 0, "function-value registration key", func(e *graph.Edge) error {
			if err := b.addEdgeWithCustody(e, rebuildEdgeCustody{proof: rebuildProofUntrackedMove}); err != nil {
				return err
			}
			count++
			return nil
		})
	})
	return count, err
}

// Register custody before classifying the live callback, including settled
// rows that still own the same placeholder receipt as a live candidate.
func (p *rebuildFnValuePass) capture(ctx context.Context) error {
	return p.b.RebuildView().visit(ctx, []byte{familyEdge, edgeOut}, func(snap *pebble.Snapshot, r row) error {
		id, err := decodeLinkRecorder(r.key)
		if err != nil || id.Kind != graph.EdgeReferences && !graph.IsFnValuePlaceholder(id.To) {
			return err
		}
		raw, err := rebuildReceiptKey(snap, r.key)
		if err != nil && !errors.Is(err, ErrRebuildUnknownProvenance) {
			return err
		}
		if raw != nil {
			original, err := decodeLinkRecorder(raw)
			if err != nil {
				return err
			}
			if graph.IsFnValuePlaceholder(original.To) {
				if err := p.claim(raw, r.key); err != nil {
					return err
				}
			}
		}
		if !graph.IsFnValuePlaceholder(id.To) {
			return nil
		}
		e, err := p.decodeKeyedEdge(r.key, r.value)
		if err != nil {
			return err
		}
		site := p.rules.Site(e)
		if site.Name == "" {
			return nil
		}
		if err := p.input(r.key, r.value); err != nil {
			return err
		}
		return p.wantSite(r.key, e.From, site)
	})
}

func (p *rebuildFnValuePass) wantSite(key []byte, from string, site RebuildFnValueSite) error {
	if err := p.put(p.key('n', site.Name), nil); err != nil {
		return err
	}
	if site.Receiver == "<self>" && !site.SkipGate {
		if err := p.put(p.key('s', from), nil); err != nil {
			return err
		}
	}
	return p.put(append(p.column('h'), key...), nil)
}

// Replay edge roles first: receipt-only names must be requested before the
// node replay, regardless of which input file supplied the declaration.
func (p *rebuildFnValuePass) recordSites(ctx context.Context, receipts iter.Seq2[ParseFacts, error]) error {
	var ordinal uint64
	for facts, err := range receipts {
		if err != nil {
			return err
		}
		file := pathkey.Normalize(facts.File.FilePath)
		for _, raw := range facts.Edges {
			if err := ctx.Err(); err != nil {
				return err
			}
			if !graph.IsFnValuePlaceholder(raw.To) || facts.EffectiveLanguage == "" || raw.FilePath != file {
				continue
			}
			key := pendingEdgeKey(raw)
			claim, err := p.marked(append(p.column('r'), key...))
			if err != nil {
				return err
			}
			if len(claim) > 1 {
				key = claim[1:]
				live, err := decodeLinkRecorder(key)
				if err != nil {
					return err
				}
				if !graph.IsFnValuePlaceholder(live.To) {
					continue // A settled predecessor still owns this receipt.
				}
			}
			proof := append(p.column('H'), key...)
			seen, err := p.present(proof)
			if err != nil {
				return err
			}
			if seen {
				continue // Native retains the first receipt at a complete key.
			}
			site := p.rules.Site(raw)
			if site.Name == "" {
				if err := p.put(proof, []byte{0}); err != nil {
					return err
				}
				continue
			}
			val, err := encodeEdge(raw)
			if err != nil {
				return err
			}
			if err := p.input(key, val); err != nil {
				return err
			}
			body := append(binary.BigEndian.AppendUint64([]byte{1}, ordinal), val...)
			if len(claim) <= 1 {
				body[0] = 2 // Raw-only or shared claim; never authorizes a write.
			}
			if err := p.put(proof, body); err != nil {
				return err
			}
			if err := p.wantSite(key, raw.From, site); err != nil {
				return err
			}
			ordinal++
		}
	}
	return ctx.Err()
}

func fnValueReceiver(n *graph.Node) string {
	if n == nil {
		return ""
	}
	recv, _ := n.Meta["receiver"].(string)
	return recv
}

func fnValueCallable(n *graph.Node) bool {
	return n.Kind == graph.KindMethod || n.Kind == graph.KindFunction
}

func (p *rebuildFnValuePass) recordNodes(ctx context.Context, receipts iter.Seq2[ParseFacts, error]) error {
	for facts, err := range receipts {
		if err != nil {
			return err
		}
		file := pathkey.Normalize(facts.File.FilePath)
		for _, n := range facts.Nodes {
			if err := ctx.Err(); err != nil {
				return err
			}
			if facts.EffectiveLanguage == "" || n.FilePath != file || n.ID == "" {
				continue
			}
			source, err := p.present(p.key('s', n.ID))
			if err != nil {
				return err
			}
			if source {
				if err := p.put(p.key('S', n.ID), appendKeyString(nil, fnValueReceiver(n))); err != nil {
					return err
				}
			}
			wanted, err := p.present(p.key('n', n.Name))
			if err != nil {
				return err
			}
			if !wanted || !fnValueCallable(n) {
				continue
			}
			value := p.nodeFact(n)
			key := p.key('N', n.Name, n.ID)
			prior, err := p.get(key)
			if err != nil {
				return err
			}
			if prior != nil {
				if !bytes.Equal(prior, value) {
					if err := p.put(p.key('C', n.Name, n.ID), nil); err != nil {
						return err
					}
				}
				continue
			}
			if err := p.put(key, value); err != nil {
				return err
			}
		}
	}
	return ctx.Err()
}

func (p *rebuildFnValuePass) nodeFact(n *graph.Node) []byte {
	var flags byte
	if n.Kind == graph.KindMethod {
		flags |= 1
	}
	if p.rules.Prototype(n) {
		flags |= 2
	}
	if p.rules.LocalLinkage(n) {
		flags |= 4
	}
	return keyStrings([]byte{flags}, n.FilePath, fnValueReceiver(n))
}

type rebuildFnValueNode struct {
	file, receiver           string
	method, prototype, local bool
}

func decodeFnValueNode(value []byte) (rebuildFnValueNode, error) {
	if len(value) < 1 {
		return rebuildFnValueNode{}, invalid("function-value node fact")
	}
	file, tail, err := readKeyString(value[1:])
	if err != nil {
		return rebuildFnValueNode{}, err
	}
	recv, tail, err := readKeyString(tail)
	if err != nil || len(tail) != 0 {
		return rebuildFnValueNode{}, invalid("function-value node receiver")
	}
	flags := value[0]
	return rebuildFnValueNode{file: file, receiver: recv, method: flags&1 != 0, prototype: flags&2 != 0, local: flags&4 != 0}, nil
}

// Only candidates of the current lookup participate. Live facts lacking a
// matching receipt remain possible members, never evidence of absence.
func (p *rebuildFnValuePass) lookup(ctx context.Context, name string, raw, prototypeMatters bool, admits func(rebuildFnValueNode) bool,
	visit func(string, rebuildFnValueNode, bool) error,
) error {
	if raw {
		prefix := p.key('N', name)
		return pendingVisit(ctx, p.b.pending, prefix, func(r row) error {
			id, tail, err := readKeyString(r.key[len(prefix):])
			if err != nil || len(tail) != 0 {
				return invalid("function-value receipt candidate")
			}
			n, err := decodeFnValueNode(r.value)
			if err != nil || !admits(n) {
				return err
			}
			conflict, err := p.present(p.key('C', name, id))
			if err != nil {
				return err
			}
			return visit(id, n, !conflict)
		})
	}
	return p.b.RebuildView().VisitNameCandidates(ctx, name, nil, func(n *graph.Node) error {
		if !fnValueCallable(n) {
			return nil
		}
		project, err := decodeFnValueNode(p.nodeFact(n))
		if err != nil || !admits(project) {
			return err
		}
		if err := p.capNode(n.ID); err != nil {
			return err
		}
		v, err := p.get(p.key('N', name, n.ID))
		if err != nil {
			return err
		}
		if v == nil {
			return visit(n.ID, project, false)
		}
		other, err := decodeFnValueNode(v)
		if err != nil {
			return err
		}
		conflict, err := p.present(p.key('C', name, n.ID))
		if err != nil {
			return err
		}
		exact := !conflict && admits(other) && (!prototypeMatters || project.prototype == other.prototype)
		return visit(n.ID, project, exact)
	})
}

type rebuildFnValueBucket struct {
	first, proto             string
	count, protos            int
	unknownDefs, unknownPros int
	firstUnknown             bool
	unknown                  bool
}

type rebuildFnValueMode byte

const (
	fnValueSameFile rebuildFnValueMode = iota
	fnValueMember
	fnValueUniqueMember
	fnValueGlobal
	fnValueCrossFile
)

func (p *rebuildFnValuePass) bucket(ctx context.Context, name, scope string, mode rebuildFnValueMode, raw bool,
	yield func(string) error,
) (b rebuildFnValueBucket, err error) {
	admits := func(n rebuildFnValueNode) bool {
		switch mode {
		case fnValueSameFile:
			return n.file == scope
		case fnValueMember, fnValueUniqueMember:
			return n.method && n.receiver == scope
		case fnValueCrossFile:
			return !n.local
		default:
			return true
		}
	}
	global := mode == fnValueGlobal || mode == fnValueCrossFile
	err = p.lookup(ctx, name, raw, global, admits, func(id string, n rebuildFnValueNode, exact bool) error {
		if global && n.prototype {
			b.proto = id
			b.protos++
			if !exact {
				b.unknownPros++
			}
			return nil
		}
		// Native's SQLite file index scans equal file keys by node ID. The VM proof
		// labels any divergence from that order as a native-defect difference.
		if b.count == 0 || id < b.first {
			b.first, b.firstUnknown = id, !exact
		}
		b.count++
		if !exact {
			b.unknownDefs++
		}
		if yield != nil {
			return yield(id)
		}
		return nil
	})
	switch mode {
	case fnValueSameFile:
		b.unknown = b.firstUnknown
	case fnValueMember:
		b.unknown = b.unknownDefs != 0
	case fnValueUniqueMember:
		b.unknown = b.unknownDefs != 0 && b.count-b.unknownDefs < 2
	default:
		// Two exact definitions block every optional definition or prototype.
		// Likewise, an exact definition makes prototype uncertainty irrelevant.
		exact := b.count - b.unknownDefs
		b.unknown = exact < 2 && b.unknownDefs != 0 || exact == 0 && b.unknownPros != 0 && b.protos-b.unknownPros < 2
	}
	return b, err
}

func (p *rebuildFnValuePass) sourceReceiver(id string, raw bool) (string, error) {
	key := p.key('S', id)
	value, err := p.get(key)
	if err != nil {
		return "", err
	}
	if raw {
		if value == nil {
			return "", nil
		}
		recv, _, err := readKeyString(value)
		return recv, err
	}
	n, err := pendingGet(p.b.pending, pendingNodeKey(id), decodeNode)
	if err != nil || n == nil {
		return "", err
	}
	if err := p.capNode(id); err != nil {
		return "", err
	}
	return fnValueReceiver(n), nil
}

func (p *rebuildFnValuePass) plan(ctx context.Context, e *graph.Edge, raw bool, receiver *string, emit func(*graph.Edge) error) (uncertain bool, err error) {
	site := p.rules.Site(e)
	if site.Name == "" {
		return false, nil
	}
	var selected rebuildFnValueBucket
	var mode rebuildFnValueMode
	var scope string
	choose := func(nextScope string, nextMode rebuildFnValueMode) int {
		if err != nil {
			return 0
		}
		mode, scope = nextMode, nextScope
		selected, err = p.bucket(ctx, site.Name, scope, mode, raw, nil)
		uncertain = uncertain || selected.unknown
		if err != nil {
			return 0
		}
		switch mode {
		case fnValueMember:
			return selected.count
		case fnValueSameFile:
			if selected.count > 0 {
				return 1
			}
		case fnValueGlobal, fnValueCrossFile:
			if selected.count == 0 && selected.protos == 1 {
				selected.first = selected.proto
				return 1
			}
			fallthrough
		case fnValueUniqueMember:
			if selected.count == 1 {
				return 1
			}
		}
		return 0
	}
	self := func() int {
		if receiver != nil {
			if *receiver == "" {
				return 0
			}
			return choose(*receiver, fnValueMember)
		}
		var recv string
		recv, err = p.sourceReceiver(e.From, raw)
		if err != nil || recv == "" {
			return 0
		}
		return choose(recv, fnValueMember)
	}
	confidence, origin, count := p.rules.Choose(site.Receiver, site.Ungated, site.SkipGate, graph.FnValueLookups{
		UniqueMember: func(recv string) int { return choose(recv, fnValueUniqueMember) },
		Members:      func(recv string) int { return choose(recv, fnValueMember) },
		SelfMembers:  self,
		SameFile:     func() int { return choose(e.FilePath, fnValueSameFile) },
		Global:       func() int { return choose("", fnValueGlobal) },
		CrossFile:    func() int { return choose("", fnValueCrossFile) },
	})
	if err != nil || count == 0 {
		return uncertain, err
	}
	land := func(id string) error {
		if id == "" || id == e.From {
			return nil
		}
		return emit(p.rules.Registration(e, site.Name, id, confidence, origin, count))
	}
	if count == 1 {
		return uncertain, land(selected.first)
	}
	_, err = p.bucket(ctx, site.Name, scope, mode, raw, land)
	return uncertain, err
}

func (p *rebuildFnValuePass) prove(ctx context.Context) error {
	var refused rebuildScratchRefusals
	prefix := p.column('h')
	if err := pendingVisit(ctx, p.b.pending, prefix, func(r row) error {
		old := r.key[len(prefix):]
		id, err := decodeLinkRecorder(old)
		if err != nil {
			return err
		}
		live, err := pendingGet(p.b.pending, old, func(val []byte) (*graph.Edge, error) {
			return p.decodeKeyedEdge(old, val)
		})
		if err != nil {
			return err
		}
		proof, err := p.get(append(p.column('H'), old...))
		if err != nil {
			return err
		}
		var raw *graph.Edge
		var order []byte
		if len(proof) >= 9 {
			raw, err = decodeEdge(proof[9:])
			if err != nil {
				return err
			}
			group := byte(0)
			if !strings.HasPrefix(id.To, graph.FnValuePlaceholderMarker) {
				group = 1
			}
			order = append(appendKeyString([]byte{group}, id.To), proof[1:9]...)
		}
		temp := p.column('x')
		var rawCount, liveCount uint64
		rawUnknown, err := p.plan(ctx, raw, true, nil, func(e *graph.Edge) error {
			key := pendingEdgeKey(e)
			occupied, err := p.present(key)
			if err != nil || occupied {
				return err
			}
			val, err := encodeEdge(e)
			if err != nil {
				return err
			}
			rawCount++
			return p.put(append(bytes.Clone(temp), key...), val)
		})
		if err != nil {
			return err
		}
		changed := false
		unknown, err := p.plan(ctx, live, false, nil, func(e *graph.Edge) error {
			key := pendingEdgeKey(e)
			occupied, err := p.present(key)
			if err != nil || occupied {
				return err
			}
			val, err := encodeEdge(e)
			if err != nil {
				return err
			}
			liveCount++
			rawValue, err := p.get(append(bytes.Clone(temp), key...))
			if err != nil {
				return err
			}
			if !bytes.Equal(val, rawValue) || len(proof) < 9 || proof[0] != 1 {
				changed = true
				return nil
			}
			return p.offer(key, order, val)
		})
		if err != nil {
			return err
		}
		selfChanged, err := p.alternateSelfReceiver(ctx, live, raw, temp, rawCount)
		if err != nil {
			return err
		}
		if unknown || rawUnknown || changed || selfChanged || liveCount != rawCount {
			refused.add(id, bytes.Clone(old))
		}
		if rawCount == 0 {
			return nil
		}
		return p.b.pending.DeleteRange(temp, prefixEnd(temp), pebble.NoSync)
	}); err != nil {
		return err
	}
	return refused.err()
}

// If the source receiver differs, either value can change which member
// bucket is consulted. Compare both cross-projections to the raw output.
func (p *rebuildFnValuePass) alternateSelfReceiver(ctx context.Context, live, raw *graph.Edge, prefix []byte, wantCount uint64) (bool, error) {
	self := func(e *graph.Edge) bool {
		site := p.rules.Site(e)
		return site.Name != "" && site.Receiver == "<self>" && !site.SkipGate
	}
	if live == nil || raw == nil || !self(live) && !self(raw) {
		return false, nil
	}
	liveRecv, err := p.sourceReceiver(live.From, false)
	if err != nil {
		return false, err
	}
	rawRecv, err := p.sourceReceiver(raw.From, true)
	if err != nil || liveRecv == rawRecv {
		return false, err
	}
	for _, check := range []struct {
		edge     *graph.Edge
		receiver string
		raw      bool
	}{
		{live, rawRecv, false},
		{raw, liveRecv, true},
	} {
		if !self(check.edge) {
			continue
		}
		var count uint64
		var changed bool
		unknown, err := p.plan(ctx, check.edge, check.raw, &check.receiver, func(e *graph.Edge) error {
			key := pendingEdgeKey(e)
			occupied, err := p.present(key)
			if err != nil || occupied {
				return err
			}
			value, err := encodeEdge(e)
			if err != nil {
				return err
			}
			want, err := p.get(append(bytes.Clone(prefix), key...))
			changed = changed || !bytes.Equal(value, want)
			count++
			return err
		})
		if err != nil || unknown || changed || count != wantCount {
			return true, err
		}
	}
	return false, nil
}

func (p *rebuildFnValuePass) offer(key, order, val []byte) error {
	// SQLite visits bare placeholders before prefixed ones. Within each
	// scan, the target index breaks ties by frozen receipt ordinal.
	orderKey := append(p.column('W'), key...)
	prior, err := p.get(orderKey)
	if err != nil || prior != nil && bytes.Compare(prior, order) <= 0 {
		return err
	}
	if err := p.put(orderKey, order); err != nil {
		return err
	}
	return p.put(append(p.column('w'), key...), val)
}
