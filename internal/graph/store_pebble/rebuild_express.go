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
	return p.b.RebuildView().visit(ctx, []byte{familyEdge, edgeOut}, func(_ *pebble.Snapshot, r row) error {
		kind, err := rebuildAttributionEdgeKind(r.key)
		if err != nil || kind != graph.EdgeCalls {
			return err
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
		if raw, err := p.inputKey(r.key); err != nil {
			return err
		} else if raw != nil {
			if err := p.claim(raw, r.key); err != nil {
				return err
			}
		}
		name, suffix := q.convention()
		for _, name := range p.rules.Names(name, suffix) {
			if err := p.put(p.key('q', name), nil); err != nil {
				return err
			}
		}
		if q.class != "" {
			if err := p.put(p.key('w', q.method), nil); err != nil {
				return err
			}
		}
		p.stats.Stubs++
		if err := p.put(p.key('n', e.From), []byte{rebuildCapabilityWanted}); err != nil {
			return err
		}
		return p.put(append(p.column('h'), r.key...), nil)
	})
}

func expressMethodKind(kind graph.NodeKind) bool {
	return kind == graph.KindMethod || kind == graph.KindFunction
}

func (p *rebuildExpress) nodeRoles(n *graph.Node, candidate, method byte) error {
	if p.rules.Candidate(n) {
		needed, err := p.present(p.key('q', n.Name))
		if err != nil {
			return err
		}
		if needed {
			if err := p.put(p.key(candidate, n.Name, n.ID), appendKeyString(nil, n.FilePath)); err != nil {
				return err
			}
		}
	}
	if !expressMethodKind(n.Kind) {
		return nil
	}
	needed, err := p.present(p.key('w', n.Name))
	if err != nil || !needed {
		return err
	}
	return p.put(p.key(method, n.ID), keyStrings(nil, n.Name, n.FilePath))
}

func (p *rebuildExpress) captureNodes(ctx context.Context) error {
	return p.b.RebuildView().visit(ctx, []byte{familyNode}, func(_ *pebble.Snapshot, r row) error {
		n, err := checkedRebuildLightNode(r, p.label)
		if err != nil {
			return err
		}
		if err := p.nodeRoles(n, 'a', 'k'); err != nil {
			return err
		}
		if !expressMethodKind(n.Kind) {
			return nil
		}
		needed, err := p.present(p.key('k', n.ID))
		if err != nil || !needed {
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
			if e.Kind == graph.EdgeCalls || e.Kind == graph.EdgeMemberOf {
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
	caller := p.key('n', raw.ID)
	state, err := rebuildScratchState(p.b.pending, caller, "express caller proof")
	if err != nil {
		return err
	}
	candidate, err := p.present(p.key('q', raw.Name))
	if err != nil {
		return err
	}
	method, err := p.present(p.key('w', raw.Name))
	if err != nil || state == 0 && !candidate && !method {
		return err
	}
	live, err := p.storeReceiptNode(raw.ID)
	if err != nil {
		return err
	}
	exact := language != "" && raw.Language != "" && sameRebuildLanguageMarker(raw, live, file)
	if state != 0 {
		if err := noteRebuildScratchState(p.put, caller, state, rebuildScratchProofBit(exact)); err != nil {
			return err
		}
	}
	if !exact {
		return nil
	}
	return p.nodeRoles(raw, 'A', 'K')
}

func (p *rebuildExpress) recordEdge(e *graph.Edge, exact bool, order uint64) error {
	return p.recordOwnerEdge(e, exact, order, func(live *graph.Edge) bool {
		a, matched := p.query(e)
		b, ok := p.query(live)
		return matched && ok && a == b
	}, p.rules.Owner, 'l', func(err error) error {
		return errors.Join(err, invalid("express receipt live edge missing"))
	})
}

func (p *rebuildExpress) owner(ctx context.Context, id string) (string, bool, bool, error) {
	var owners rebuildMemberOwners
	err := p.members(ctx, id, func(r row, candidate string) error {
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
			return p.markOwners(ctx, id, name, raw)
		}
		if class == "" {
			return nil
		}
		if err := p.put(p.key('b', class, name, id), appendKeyString(nil, file)); err != nil {
			return err
		}
		if raw == nil || !exact {
			return nil
		}
		name, file, err = decodeExpressMethod(raw)
		if err != nil {
			return err
		}
		return p.put(p.key('B', class, name, id), appendKeyString(nil, file))
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
		return p.markOwners(ctx, id, "", r.value)
	})
}

func (p *rebuildExpress) markOwners(ctx context.Context, id, liveName string, raw []byte) error {
	var rawName string
	if raw != nil {
		var err error
		rawName, _, err = decodeExpressMethod(raw)
		if err != nil {
			return err
		}
	}
	return p.members(ctx, id, func(_ row, class string) error {
		for _, name := range [...]string{liveName, rawName} {
			if class != "" && name != "" {
				if err := p.put(p.key('x', class, name), nil); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// Express convention only needs the three singleton tests the native tiers
// consult. Candidate IDs are unique across the full and suffix-stripped names.
func (p *rebuildExpress) convention(ctx context.Context, q rebuildExpressQuery, fromDir string, col byte) (string, string, error) {
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
			return "", "", err
		}
	}
	choices := [...]choice{sameDir, all}
	if preferred.count != 0 {
		choices = [...]choice{preferred, preferredSameDir}
	}
	for _, c := range choices {
		if c.count == 1 {
			return c.id, c.file, nil
		}
	}
	return "", "", nil
}

// Native NodesByKinds reads the union in ascending ID order. In a repeated
// method bucket the first ID in the chosen class file wins, not a singleton test.
func (p *rebuildExpress) method(ctx context.Context, class, method, classID, classFile string, col byte) (string, bool, error) {
	uncertain, err := p.present(p.key('x', class, method))
	if err != nil || uncertain {
		return "", uncertain, err
	}
	prefix := p.key(col, class, method)
	var first, inFile string
	var count int
	if err := p.visit(ctx, prefix, func(r row) error {
		id, rest, err := readKeyString(r.key[len(prefix):])
		if err != nil || id == "" || len(rest) != 0 {
			return invalid("express method candidate")
		}
		file, rest, err := readKeyString(r.value)
		if err != nil || len(rest) != 0 {
			return invalid("express method file")
		}
		if first == "" {
			first = id
		}
		if classID != "" && inFile == "" && file == classFile {
			inFile = id
		}
		count = min(2, count+1)
		return nil
	}); err != nil {
		return "", false, err
	}
	if count == 1 {
		return first, false, nil
	}
	return inFile, false, nil
}

func (p *rebuildExpress) elect(ctx context.Context, q rebuildExpressQuery, fromDir string, proved bool) (string, bool, error) {
	byName, byMethod := byte('a'), byte('b')
	if proved {
		byName, byMethod = 'A', 'B'
	}
	id, file, err := p.convention(ctx, q, fromDir, byName)
	if err != nil || q.class == "" {
		return id, false, err
	}
	class := q.class
	if id != "" {
		class = p.rules.SimpleName(id)
	}
	return p.method(ctx, class, q.method, id, file, byMethod)
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
		q, _ := p.query(e)
		caller, err := p.storeReceiptNode(e.From)
		if err != nil {
			return err
		}
		var fromDir string
		if caller != nil {
			fromDir = p.rules.Directory(caller.FilePath)
		}
		target, uncertain, err := p.elect(ctx, q, fromDir, false)
		if err != nil {
			return err
		}
		if uncertain {
			refused.add(graph.EdgeIdentityFor(e), bytes.Clone(old))
			return nil
		}
		if target == "" {
			return nil
		}
		proved, uncertain, err := p.elect(ctx, q, fromDir, true)
		if err != nil {
			return err
		}
		exact := target == proved && !uncertain
		if caller != nil && fromDir != "" {
			state, err := rebuildScratchState(p.b.pending, p.key('n', e.From), "express caller proof")
			if err != nil {
				return err
			}
			exact = exact && state == rebuildScratchExact
		}
		raw, err := p.inputKey(old)
		if err != nil {
			return err
		}
		var claim []byte
		if raw != nil {
			claim, err = p.marked(append(p.column('r'), raw...))
			if err != nil {
				return err
			}
		}
		if !exact || len(claim) <= 1 || claim[0] != rebuildScratchExact || !bytes.Equal(claim[1:], old) {
			refused.add(graph.EdgeIdentityFor(e), bytes.Clone(old))
			return nil
		}
		if !p.rules.Rebind(e, target) {
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
