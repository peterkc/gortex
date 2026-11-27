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

var ErrRebuildExpress = errors.New("pebble rebuild Express stage failed")

var rebuildExpressPrefix = []byte{'g', 'q', 'Y'}

// RebuildExpressRules supplies native input roles, path decisions and the move payload.
type RebuildExpressRules struct {
	Handler    func(*graph.Edge) (class, method, name string, ok bool)
	Owner      func(*graph.Edge) (class string, ok bool)
	SimpleName func(string) string
	Candidate  func(*graph.Node) bool
	Names      func(name, suffix string) []string
	Preferred  func(path, class string) bool
	Directory  func(string) string
	Rebind     func(*graph.Edge, string) bool
}

// RebuildExpressStats counts owned placeholders and planned reindexes.
type RebuildExpressStats struct {
	Stubs, Moves uint64
}

type rebuildExpress struct {
	rebuildCallMoves
	rules RebuildExpressRules
	stats RebuildExpressStats
}

type rebuildExpressQuery struct {
	class, method, name string
}

func (q rebuildExpressQuery) convention() (name, suffix string) {
	if q.class != "" {
		return q.class, ""
	}
	return q.name, "Middleware"
}

func (p *rebuildExpress) query(e *graph.Edge) (rebuildExpressQuery, bool) {
	class, method, name, ok := p.rules.Handler(e)
	return rebuildExpressQuery{class, method, name}, ok
}

// RebuildExpress reads the settled Calls and MemberOf after Gin middleware.
func (b *BuildStore) RebuildExpress(ctx context.Context, receipts iter.Seq2[ParseFacts, error], rules RebuildExpressRules) (stats RebuildExpressStats, err error) {
	defer func() {
		if err != nil {
			stats = RebuildExpressStats{}
			err = fmt.Errorf("%w: %w", ErrRebuildExpress, err)
		}
	}()
	if rules.Handler == nil || rules.Owner == nil || rules.SimpleName == nil || rules.Candidate == nil ||
		rules.Names == nil || rules.Preferred == nil || rules.Directory == nil || rules.Rebind == nil || receipts == nil {
		return stats, invalid("express rebuild inputs")
	}
	prefix := rebuildExpressPrefix
	busy, err := rebuildGuardHasPrefix(ctx, b.pending, prefix)
	if err != nil {
		return stats, err
	}
	if busy {
		return stats, invalid("express scratch range occupied")
	}
	defer func() {
		err = errors.Join(err, b.pending.DeleteRange(prefix, prefixEnd(prefix), pebble.NoSync))
	}()
	p := rebuildExpress{
		rebuildCallMoves: rebuildCallMoves{b: b, prefix: prefix, rowBytes: 16 << 20, label: "express", inputRow: "edge row"},
		rules:            rules,
	}
	if err := p.captureCalls(ctx); err != nil || p.stats.Stubs == 0 {
		return p.stats, err
	}
	if err := p.captureNodes(ctx); err != nil {
		return stats, err
	}
	if err := p.record(ctx, receipts); err != nil {
		return stats, err
	}
	if err := p.joinMembers(ctx); err != nil {
		return stats, err
	}
	if err := p.plan(ctx); err != nil || p.stats.Moves == 0 {
		return p.stats, err
	}
	if err := p.order(ctx); err != nil {
		return stats, err
	}
	return p.stats, b.withWrites(func() error { return p.apply(ctx, p.stats.Moves) })
}

func (p *rebuildExpress) visit(ctx context.Context, prefix []byte, yield func(row) error) error {
	return pendingVisit(ctx, p.b.pending, prefix, yield)
}

func (p *rebuildExpress) inputKey(live []byte) ([]byte, error) {
	raw, err := rebuildReceiptKey(p.b.pending, live)
	if errors.Is(err, ErrRebuildUnknownProvenance) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	a, err := decodeLinkRecorder(live)
	if err != nil {
		return nil, err
	}
	b, err := decodeLinkRecorder(raw)
	if err != nil || a.Kind != b.Kind || a.From != b.From || a.FilePath != b.FilePath || a.Line != b.Line {
		return nil, err
	}
	return raw, nil
}

func (p *rebuildExpress) captureCalls(ctx context.Context) error {
	return p.b.RebuildView().visit(ctx, []byte{familyEdge, edgeOut}, func(snap *pebble.Snapshot, r row) error {
		kind, err := rebuildAttributionEdgeKind(r.key)
		if err != nil || kind != graph.EdgeCalls {
			return err
		}
		raw, err := rebuildReceiptKey(snap, r.key)
		if err != nil && !errors.Is(err, ErrRebuildUnknownProvenance) {
			return err
		}
		if raw != nil {
			if err := p.claim(raw, r.key); err != nil {
				return err
			}
		}
		e, err := p.decodeKeyedEdge(r.key, r.value)
		if err != nil {
			return err
		}
		q, ok := p.query(e)
		if !ok || q.class != "" && q.method == "" || q.class == "" && q.name == "" {
			return nil
		}
		if err := p.input(r.key, r.value); err != nil {
			return err
		}
		name, suffix := q.convention()
		for _, name := range p.rules.Names(name, suffix) {
			if err := p.put(p.key('q', name), nil); err != nil {
				return err
			}
		}
		if q.class != "" {
			if err := p.put(p.key('c', q.class), nil); err != nil {
				return err
			}
			if err := p.put(p.key('v', q.class), nil); err != nil {
				return err
			}
			if err := p.put(p.key('w', q.method), nil); err != nil {
				return err
			}
		}
		p.stats.Stubs++
		if err := p.put(p.key('n', e.From), []byte{rebuildCapabilityWanted}); err != nil {
			return err
		}
		raw, err = p.inputKey(r.key)
		if err != nil {
			return err
		}
		return p.put(append(p.column('h'), r.key...), raw)
	})
}

func expressMethodKind(kind graph.NodeKind) bool {
	return kind == graph.KindMethod || kind == graph.KindFunction
}

func (p *rebuildExpress) nodeRoles(n *graph.Node) (candidate, method bool, err error) {
	if p.rules.Candidate(n) {
		candidate, err = p.present(p.key('q', n.Name))
		if err != nil {
			return false, false, err
		}
	}
	if expressMethodKind(n.Kind) {
		method, err = p.present(p.key('w', n.Name))
	}
	return candidate, method, err
}

func (p *rebuildExpress) saveCandidate(n *graph.Node, col byte) error {
	class, err := p.present(p.key('c', n.Name))
	if err != nil {
		return err
	}
	if class {
		if err := p.put(p.key('v', p.rules.SimpleName(n.ID)), nil); err != nil {
			return err
		}
	}
	return p.put(p.key(col, n.Name, n.ID), appendKeyString(nil, n.FilePath))
}

func (p *rebuildExpress) captureNodes(ctx context.Context) error {
	return p.b.RebuildView().visit(ctx, []byte{familyNode}, func(_ *pebble.Snapshot, r row) error {
		n, err := checkedRebuildLightNode(r, p.label)
		if err != nil {
			return err
		}
		candidate, method, err := p.nodeRoles(n)
		if err != nil || !candidate && !method {
			return err
		}
		if err := p.input(r.key, r.value); err != nil {
			return err
		}
		if err := p.put(p.key('f', n.ID), nil); err != nil {
			return err
		}
		if candidate {
			if err := p.saveCandidate(n, 'a'); err != nil {
				return err
			}
		}
		if !method {
			return nil
		}
		if err := p.put(p.key('k', n.ID), keyStrings(nil, n.Name, n.FilePath)); err != nil {
			return err
		}
		return p.members(ctx, n.ID, func(r row, _ string) error {
			raw, err := p.inputKey(r.key)
			if err != nil || raw == nil {
				return err
			}
			return p.claimKey(append(p.column('u'), raw...), r.key)
		})
	})
}

func (p *rebuildExpress) members(ctx context.Context, id string, yield func(row, string) error) error {
	return p.visit(ctx, guardOutgoing(id, graph.EdgeMemberOf), func(r row) error {
		e, err := guardLightEdge(r)
		if err != nil {
			return err
		}
		class, ok := p.rules.Owner(e)
		if !ok {
			return nil
		}
		return yield(r, class)
	})
}

func (p *rebuildExpress) record(ctx context.Context, receipts iter.Seq2[ParseFacts, error]) error {
	var order uint64
	for facts, err := range receipts {
		if err != nil {
			return err
		}
		file := pathkey.Normalize(facts.File.FilePath)
		for _, n := range facts.Nodes {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := p.recordNode(n, file, facts.EffectiveLanguage); err != nil {
				return err
			}
		}
		for _, e := range facts.Edges {
			if err := ctx.Err(); err != nil {
				return err
			}
			switch e.Kind {
			case graph.EdgeCalls, graph.EdgeMemberOf:
				if err := p.recordEdge(e, facts.EffectiveLanguage != "" && e.FilePath == file, order); err != nil {
					return err
				}
			}
			order++
		}
	}
	return nil
}

func (p *rebuildExpress) recordNode(raw *graph.Node, file, language string) error {
	if language == "" || raw.FilePath != file {
		return nil
	}
	caller := p.key('n', raw.ID)
	state, err := rebuildScratchState(p.b.pending, caller, "express caller proof")
	if err != nil {
		return err
	}
	candidate, method, err := p.nodeRoles(raw)
	if err != nil {
		return err
	}
	potential, err := p.present(p.key('f', raw.ID))
	if err != nil || state == 0 && !candidate && !method && !potential {
		return err
	}
	if state != 0 {
		live, err := p.storeReceiptNode(raw.ID)
		if err != nil || live == nil {
			return err
		}
		dir := p.rules.Directory(file)
		if err := p.put(p.key('N', raw.ID), appendKeyString(nil, dir)); err != nil {
			return err
		}
		if err := noteRebuildScratchState(p.put, caller, state, rebuildScratchProofBit(dir == p.rules.Directory(live.FilePath))); err != nil {
			return err
		}
	} else {
		// Candidate replay uses the receipt's role and path. The live row
		// supplies only existence, not any of its node payload.
		present, err := p.present(pendingNodeKey(raw.ID))
		if err != nil || !present {
			return err
		}
	}
	if !candidate && !method && !potential {
		return nil
	}
	// First and later receipts for the same known ID may contradict each
	// other even if one copy exactly matches the graph.
	key := p.key('d', raw.ID)
	prior, err := p.get(key)
	if err != nil {
		return err
	}
	var role byte
	if candidate {
		role |= 1
	}
	if method {
		role |= 2
	}
	shape := []byte{role}
	if role != 0 {
		shape = keyStrings(shape, raw.Name, file)
	}
	if bytes.Equal(prior, shape) {
		return nil
	}
	if prior != nil {
		if err := p.put(p.key('D', raw.ID), nil); err != nil {
			return err
		}
		if prior[0]&1 != 0 {
			oldName, _, err := readKeyString(prior[1:])
			if err != nil {
				return err
			}
			if err := p.b.pending.Delete(p.key('A', oldName, raw.ID), pebble.NoSync); err != nil {
				return err
			}
		}
		if prior[0]&2 != 0 && !method {
			if err := p.b.pending.Delete(p.key('K', raw.ID), pebble.NoSync); err != nil {
				return err
			}
		}
	}
	if err := p.put(key, shape); err != nil {
		return err
	}
	if candidate {
		if err := p.saveCandidate(raw, 'A'); err != nil {
			return err
		}
	}
	if method {
		return p.put(p.key('K', raw.ID), keyStrings(nil, raw.Name, file))
	}
	return nil
}

func (p *rebuildExpress) recordEdge(e *graph.Edge, admitted bool, order uint64) error {
	if e.Kind == graph.EdgeMemberOf {
		return p.recordOwnerEdge(e, admitted, order, nil, p.rules.Owner, 'l',
			func(err error) error { return errors.Join(err, invalid("express receipt live edge missing")) },
			func(key []byte) (*graph.Edge, error) {
				return pendingGet(p.b.pending, key, func(val []byte) (*graph.Edge, error) {
					return guardLightEdge(row{key, val})
				})
			})
	}
	raw := pendingEdgeKey(e)
	key := append(p.column('r'), raw...)
	claim, err := p.marked(key)
	if err != nil || len(claim) <= 1 || claim[0] != rebuildCapabilityWanted {
		return err
	}
	old := claim[1:]
	owned, err := p.get(append(p.column('h'), old...))
	if err != nil || !bytes.Equal(owned, raw) {
		return err
	}
	val, err := p.get(old)
	if err != nil || val == nil {
		return errors.Join(err, invalid("express receipt live edge missing"))
	}
	live, err := p.decodeKeyedEdge(old, val)
	if err != nil {
		return err
	}
	a, matched := p.query(e)
	b, ok := p.query(live)
	if !matched || !ok || a != b {
		if err := p.put(append(p.column('j'), old...), []byte{rebuildScratchProofBit(matched)}); err != nil {
			return err
		}
		admitted = false
	}
	// SQLite admits the first duplicate edge key. Once compared, later
	// receipts cannot silently replace its query or gain a second order slot.
	if admitted {
		if err := p.put(binary.BigEndian.AppendUint64(p.column('z'), order), raw); err != nil {
			return err
		}
	}
	return noteRebuildScratchPayload(p.put, key, claim, rebuildScratchProofBit(admitted))
}

func (p *rebuildExpress) owner(ctx context.Context, id string) (string, bool, bool, error) {
	var owners rebuildMemberOwners
	err := p.members(ctx, id, func(r row, candidate string) error {
		relevant, err := p.present(p.key('v', candidate))
		if err != nil {
			return err
		}
		if relevant {
			if err := p.input(r.key, r.value); err != nil {
				return err
			}
		}
		owners.observe(candidate)
		raw, err := p.inputKey(r.key)
		if err != nil {
			return err
		}
		return owners.prove(&p.rebuildCallMoves, raw, r.key, candidate)
	})
	return owners.elect(&p.rebuildCallMoves, id, 'l', err)
}

func decodeExpressMethod(v []byte) (name, file string, err error) {
	name, rest, err := readKeyString(v)
	if err != nil {
		return "", "", err
	}
	file, rest, err = readKeyString(rest)
	if err != nil || len(rest) != 0 {
		return "", "", invalid("express method payload")
	}
	return name, file, nil
}

func (p *rebuildExpress) joinMembers(ctx context.Context) error {
	if err := p.visit(ctx, p.column('k'), func(r row) error {
		id, _, err := readKeyString(r.key[len(p.prefix)+1:])
		if err != nil {
			return err
		}
		name, file, err := decodeExpressMethod(r.value)
		if err != nil {
			return err
		}
		raw, err := p.get(p.key('K', id))
		if err != nil {
			return err
		}
		class, exact, uncertain, err := p.owner(ctx, id)
		if err != nil {
			return err
		}
		if uncertain {
			if err := p.markOwners(ctx, id, 'x', r.value); err != nil {
				return err
			}
			return p.markOwners(ctx, id, 'X', raw)
		}
		if class == "" {
			return nil
		}
		if err := p.put(p.key('b', class, name, id), appendKeyString(nil, file)); err != nil {
			return err
		}
		if raw == nil {
			return nil
		}
		name, file, err = decodeExpressMethod(raw)
		if err != nil {
			return err
		}
		col := byte('B')
		if !exact {
			col = 'X'
		}
		return p.put(p.key(col, class, name, id), appendKeyString(nil, file))
	}); err != nil {
		return err
	}
	// An admitted raw method that had no live role can enter an election.
	return p.visit(ctx, p.column('K'), func(r row) error {
		id, _, err := readKeyString(r.key[len(p.prefix)+1:])
		if err != nil {
			return err
		}
		live, err := p.present(p.key('k', id))
		if err != nil || live {
			return err
		}
		return p.markOwners(ctx, id, 'X', r.value)
	})
}

func (p *rebuildExpress) markOwners(ctx context.Context, id string, col byte, method []byte) error {
	if method == nil {
		return nil
	}
	name, file, err := decodeExpressMethod(method)
	if err != nil {
		return err
	}
	return p.members(ctx, id, func(r row, class string) error {
		relevant, err := p.present(p.key('v', class))
		if err != nil || !relevant {
			return err
		}
		if err := p.input(r.key, r.value); err != nil {
			return err
		}
		return p.put(p.key(col, class, name, id), appendKeyString(nil, file))
	})
}

// Express tracks the four native convention branches without retaining the
// candidate lists. IDs are unique across the full and suffix-stripped names.
func (p *rebuildExpress) convention(ctx context.Context, q rebuildExpressQuery, fromDir string, col byte) (string, string, bool, error) {
	type choice struct {
		id, file string
		count    int
	}
	var all, sameDir, preferred, preferredSameDir choice
	add := func(c *choice, id, file string) {
		if c.count == 0 {
			c.id, c.file = id, file
		}
		c.count = min(2, c.count+1)
	}
	name, suffix := q.convention()
	for _, name := range p.rules.Names(name, suffix) {
		prefix := p.key(col, name)
		if err := p.visit(ctx, prefix, func(r row) error {
			id, rest, err := readKeyString(r.key[len(prefix):])
			if err != nil || id == "" || len(rest) != 0 {
				return invalid("express convention candidate")
			}
			file, rest, err := readKeyString(r.value)
			if err != nil || len(rest) != 0 {
				return invalid("express convention file")
			}
			local := fromDir != "" && p.rules.Directory(file) == fromDir
			add(&all, id, file)
			if local {
				add(&sameDir, id, file)
			}
			if p.rules.Preferred(file, q.class) {
				add(&preferred, id, file)
				if local {
					add(&preferredSameDir, id, file)
				}
			}
			return nil
		}); err != nil {
			return "", "", false, err
		}
	}
	switch preferred.count {
	case 1:
		return preferred.id, preferred.file, false, nil
	case 2:
		if preferredSameDir.count == 1 {
			return preferredSameDir.id, preferredSameDir.file, true, nil
		}
	default:
		if all.count == 1 {
			return all.id, all.file, false, nil
		}
		if sameDir.count == 1 {
			return sameDir.id, sameDir.file, true, nil
		}
	}
	return "", "", false, nil
}

type rebuildExpressDecision struct {
	target, classID, classFile string
	local                      bool
}

// Native NodesByKinds reads the union in ascending ID order. In a repeated
// method bucket the first ID in the chosen class file wins, not a singleton test.
func (p *rebuildExpress) method(ctx context.Context, class, name, classID, classFile string, col, possible byte) (string, bool, bool, error) {
	var first, inFile string
	var count int
	visit := func(col byte, visit func(id, file string)) error {
		prefix := p.key(col, class, name)
		return p.visit(ctx, prefix, func(r row) error {
			id, rest, err := readKeyString(r.key[len(prefix):])
			if err != nil || id == "" || len(rest) != 0 {
				return invalid("express method candidate")
			}
			file, rest, err := readKeyString(r.value)
			if err != nil || len(rest) != 0 {
				return invalid("express method file")
			}
			visit(id, file)
			return nil
		})
	}
	if err := visit(col, func(id, file string) {
		if first == "" {
			first = id
		}
		if classID != "" && inFile == "" && file == classFile {
			inFile = id
		}
		count = min(2, count+1)
	}); err != nil {
		return "", false, false, err
	}
	var uncertain, hasPossible bool
	if err := visit(possible, func(id, file string) {
		hasPossible = true
		// A possible row only matters if it changes the singleton or the
		// first matching ID. Adding several losing rows cannot change either.
		uncertain = uncertain || count == 0 || count == 1 && inFile == "" ||
			classID != "" && file == classFile && (inFile == "" || id < inFile)
	}); err != nil {
		return "", false, false, err
	}
	if count == 1 {
		// If an unknown owner could add a row, the class-file match is
		// the witness that a singleton would still win as a repeated pick.
		return first, hasPossible && inFile != "", uncertain, nil
	}
	return inFile, count > 1, uncertain, nil
}

func (p *rebuildExpress) elect(ctx context.Context, q rebuildExpressQuery, fromDir string, proved bool) (rebuildExpressDecision, bool, error) {
	byName, byMethod, possible := byte('a'), byte('b'), byte('x')
	if proved {
		byName, byMethod, possible = 'A', 'B', 'X'
	}
	id, file, local, err := p.convention(ctx, q, fromDir, byName)
	if err != nil || q.class == "" {
		return rebuildExpressDecision{target: id, local: local}, false, err
	}
	class := q.class
	if id != "" {
		class = p.rules.SimpleName(id)
	}
	target, repeated, uncertain, err := p.method(ctx, class, q.method, id, file, byMethod, possible)
	decision := rebuildExpressDecision{target: target}
	if target != "" && (repeated || class != q.class) {
		decision.classID, decision.classFile, decision.local = id, file, local
	}
	return decision, uncertain, err
}

func (p *rebuildExpress) callerDirs(id string) (string, string, error) {
	caller, err := p.storeReceiptNode(id)
	if err != nil || caller == nil {
		return "", "", err
	}
	live := p.rules.Directory(caller.FilePath)
	raw, err := p.get(p.key('N', id))
	if err != nil || raw == nil {
		return live, live, err
	}
	dir, rest, err := readKeyString(raw)
	if err != nil || len(rest) != 0 {
		return "", "", invalid("express receipt directory")
	}
	return live, dir, nil
}

func (p *rebuildExpress) plan(ctx context.Context) error {
	var refused rebuildScratchRefusals
	prefix := p.column('h')
	if err := p.visit(ctx, prefix, func(r row) error {
		old := r.key[len(prefix):]
		e, err := p.edge(old)
		if err != nil || e == nil {
			return errors.Join(err, invalid("express owned Call missing"))
		}
		var claim []byte
		if len(r.value) != 0 {
			claim, err = p.marked(append(p.column('r'), r.value...))
			if err != nil {
				return err
			}
		}
		differentQuery, err := rebuildScratchState(p.b.pending, append(p.column('j'), old...), "express first receipt query")
		if err != nil {
			return err
		}
		if differentQuery == rebuildCapabilityProved {
			refused.add(graph.EdgeIdentityFor(e), bytes.Clone(old))
			return nil
		}
		q, _ := p.query(e)
		liveDir, rawDir, err := p.callerDirs(e.From)
		if err != nil {
			return err
		}
		live, liveUncertain, err := p.elect(ctx, q, liveDir, false)
		if err != nil {
			return err
		}
		proved, rawUncertain, err := p.elect(ctx, q, rawDir, true)
		if err != nil {
			return err
		}
		if !liveUncertain && !rawUncertain && live.target == "" && proved.target == "" {
			return nil
		}
		// A first non-handler receipt can only agree on no move.
		exact := differentQuery == 0 && live == proved && !liveUncertain && !rawUncertain
		if live.local || proved.local {
			key := pendingNodeKey(e.From)
			if _, err := pendingGet(p.b.pending, key, func(val []byte) (bool, error) {
				return true, p.input(key, val)
			}); err != nil {
				return err
			}
			state, err := rebuildScratchState(p.b.pending, p.key('n', e.From), "express caller proof")
			if err != nil {
				return err
			}
			exact = exact && state == rebuildScratchExact
		}
		if !exact || len(claim) <= 1 || claim[0] != rebuildScratchExact || !bytes.Equal(claim[1:], old) {
			refused.add(graph.EdgeIdentityFor(e), bytes.Clone(old))
			return nil
		}
		if !p.rules.Rebind(e, live.target) {
			return invalid("express elected target did not rebind")
		}
		p.stats.Moves++
		return p.stage(old, e)
	}); err != nil {
		return err
	}
	return refused.err()
}

func (p *rebuildExpress) order(ctx context.Context) error {
	return p.visit(ctx, p.column('z'), func(r row) error {
		_, err := p.orderCallReceipt(r.value)
		return err
	})
}
