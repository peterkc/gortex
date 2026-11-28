package store_pebble

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"iter"
	"strings"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/pathkey"
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
// effects of skipped producers. Candidate reads the edge identity and caller
// language. Maybe's payload predicates may veto but not add a lexical result.
// MayChangeLive distinguishes a terminal removal from an in-place refresh.
// IndexedEdge returns the projected edge and its exact name, qualified-name,
// and file-directory lookup buckets. An empty string is a key; a nil edge skips.
// The callbacks do not mutate the working graph.
type RebuildExternalCallRules struct {
	Candidate        func(*graph.Edge, *graph.Node) (RebuildExternalCallCandidate, bool)
	Maybe            func(e *graph.Edge, caller *graph.Node, receiptLanguage string) []RebuildExternalCallCandidate
	MayChangeLive    func(context.Context, RebuildView, *graph.Edge, *graph.Node) (remove, refresh bool, err error)
	Rebind           func(*graph.Edge, string, string) graph.EdgeReindex
	IndexedEdge      func(*graph.Edge) (edge *graph.Edge, names, quals []string, directory bool)
	IndexedNode      func(*graph.Node) bool
	IndexedCandidate func(context.Context, RebuildView, *graph.Edge, *graph.Node) (*graph.Edge, RebuildExternalCallCandidate, bool, error)
	Extra            func(context.Context, RebuildView, func(*graph.Edge, RebuildExternalCallCandidate) error) error
}

const rebuildExternalCallRowBytes = 16 << 20

const (
	externalCallCertain byte = iota + 1
	externalCallOptional
	externalCallPossible
)

type rebuildExternalCallPass struct {
	b            *BuildStore
	rules        RebuildExternalCallRules
	count        uint64
	indexedCount uint64
}

// RebuildExternalCallSynthesis runs under the worker's exclusive stage
// ownership. The pending index stores receipt keys, ordered votes and mutation
// plans on disk. All retained-effect proof finishes before any graph write.
// The count matches native: stationary package edges and selected rebind
// attempts, including occupied targets but not converging loser deletes.
func (b *BuildStore) RebuildExternalCallSynthesis(ctx context.Context, receipts iter.Seq2[ParseFacts, error], rules RebuildExternalCallRules) (count uint64, err error) {
	defer func() {
		if err != nil {
			count, err = 0, fmt.Errorf("%w: %w", ErrRebuildExternalCallSynthesis, err)
		}
	}()
	if receipts == nil || rules.Candidate == nil || rules.Maybe == nil || rules.Rebind == nil {
		return 0, invalid("external-call synthesis requires receipts and rules")
	}
	if rules.IndexedEdge != nil && (rules.IndexedNode == nil || rules.IndexedCandidate == nil) {
		return 0, invalid("incomplete external-call indexed lookup rules")
	}
	view := b.RebuildView()
	if err := view.mutationError(); err != nil {
		return 0, err
	}
	prefix := rebuildExternalCallColumn(0)[:rebuildExternalCallColumnBytes-1]
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
	if err := p.indexedEntrants(ctx, view); err != nil {
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
	if err := p.checkChunkCallers(ctx); err != nil {
		return 0, err
	}
	if err := p.packages(ctx); err != nil {
		return 0, err
	}
	return p.count, b.withWrites(func() error { return p.apply(ctx) })
}

// Scratch columns: file authority f, first receipt digests r and raw inputs h,
// caller languages a, incumbent node digests i, duplicate receipts j,
// deferred indexed lookup projections k and postings B (name), Q (qualified
// name), D (import directory),
// consumed raw keys c, deferred receipt keys u, site votes g, package votes p,
// missing synthetic callers w, retained packages t, payload refreshes y, and
// the checked mutation
// plan (node n, delete d, move m, custody l).
const rebuildExternalCallColumnBytes = 4

func rebuildExternalCallColumn(column byte) []byte { return []byte{'g', '9', 'x', column} }

// The prior stage's exact node digest lives outside the temporary columns.
// It is private to the build and proves a package on an idempotent rerun.
func rebuildExternalCallPackageProofKey(id string) []byte {
	return keyStrings([]byte{'g', '9', 's'}, id)
}

func (p *rebuildExternalCallPass) put(key, value []byte) error {
	return p.b.putRebuildScratch(key, value, rebuildExternalCallRowBytes, "external-call synthesis")
}

// Lookup buckets need only exact equality. Prefix the byte length to keep
// adjacent names separate without expanding bytes inside the lookup key.
func rebuildExternalCallLookupPrefix(column byte, name string) []byte {
	return append(binary.AppendUvarint(rebuildExternalCallColumn(column), uint64(len(name))), name...)
}

func (p *rebuildExternalCallPass) captureIndexedEdge(raw *graph.Edge) error {
	e, names, quals, directory := p.rules.IndexedEdge(raw)
	if e == nil {
		return nil
	}
	value, err := encodeEdge(e)
	if err != nil {
		return err
	}
	ordinal := p.indexedCount
	if err := p.put(binary.BigEndian.AppendUint64(rebuildExternalCallColumn('k'), ordinal), value); err != nil {
		return err
	}
	p.indexedCount++
	for _, name := range names {
		if err := p.put(binary.BigEndian.AppendUint64(rebuildExternalCallLookupPrefix('B', name), ordinal), nil); err != nil {
			return err
		}
	}
	for _, qual := range quals {
		if err := p.put(binary.BigEndian.AppendUint64(rebuildExternalCallLookupPrefix('Q', qual), ordinal), nil); err != nil {
			return err
		}
	}
	if directory {
		return p.put(binary.BigEndian.AppendUint64(rebuildExternalCallColumn('D'), ordinal), nil)
	}
	return nil
}

func (p *rebuildExternalCallPass) indexedEntrants(ctx context.Context, view RebuildView) (err error) {
	if p.indexedCount == 0 {
		return nil
	}
	var iters [3]*pebble.Iterator
	for i := range iters {
		it, openErr := p.b.pending.NewIterWithContext(ctx, nil)
		if openErr != nil {
			return openErr
		}
		defer func() { err = errors.Join(err, it.Close()) }()
		iters[i] = it
	}
	return view.VisitExternalCallNodes(ctx, p.rules.IndexedNode, func(n *graph.Node) error {
		prefixes := [3][]byte{
			rebuildExternalCallLookupPrefix('B', n.Name),
			rebuildExternalCallLookupPrefix('Q', n.QualName),
		}
		if n.Kind == graph.KindFile {
			prefixes[2] = rebuildExternalCallColumn('D')
		}
		var valid [3]bool
		for i, prefix := range prefixes {
			if prefix != nil {
				iters[i].SetBounds(prefix, prefixEnd(prefix))
				valid[i] = iters[i].First()
			}
		}
		// Merge the matching buckets in receipt order. An edge in multiple
		// buckets dispatches once; duplicate receipt rows have distinct ordinals.
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			var ordinal uint64
			var found bool
			var advance [3]bool
			for i, prefix := range prefixes {
				if prefix == nil {
					continue
				}
				it := iters[i]
				if err := it.Error(); err != nil {
					return err
				}
				if !valid[i] {
					continue
				}
				tail := it.Key()[len(prefix):]
				if len(tail) != 8 {
					return invalid("external-call lookup posting ordinal")
				}
				next := binary.BigEndian.Uint64(tail)
				if !found || next < ordinal {
					ordinal, found, advance = next, true, [3]bool{}
				}
				advance[i] = next == ordinal
			}
			if !found {
				return nil
			}
			e, err := pendingGet(p.b.pending, binary.BigEndian.AppendUint64(rebuildExternalCallColumn('k'), ordinal), decodeEdge)
			if err != nil {
				return err
			}
			if e == nil {
				return invalid("external-call lookup projection missing")
			}
			site, candidate, ok, err := p.rules.IndexedCandidate(ctx, view, e, n)
			if err != nil {
				return err
			}
			if ok {
				if err := p.possible(site, candidate); err != nil {
					return err
				}
			}
			for i, next := range advance {
				if next {
					valid[i] = iters[i].Next()
				}
			}
		}
	})
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

// This is a key filter, not the native terminal parser or stdlib policy.
func rebuildExternalCallTerminal(to string) bool {
	return strings.HasPrefix(to, "dep::") || graph.IsStdlibStub(to) || strings.HasPrefix(to, "external::")
}

// IsRebuildExternalCallRetained reports the extractor languages retained by the link projection.
func IsRebuildExternalCallRetained(lang string) bool {
	return lang == "go" || lang == "javascript" || lang == "typescript"
}

func (p *rebuildExternalCallPass) fileLanguage(file string) (string, bool, error) {
	raw, err := pendingGet(p.b.pending, keyStrings(rebuildExternalCallColumn('f'), pathkey.Normalize(file)), copyValue)
	if err != nil || raw == nil {
		return "", false, err
	}
	return string(raw[1:]), true, nil
}

func (p *rebuildExternalCallPass) receipts(ctx context.Context, receipts iter.Seq2[ParseFacts, error]) error {
	for f, err := range receipts {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		lang := f.EffectiveLanguage
		deferred := lang != "" && !IsRebuildExternalCallRetained(lang)
		file := pathkey.Normalize(f.File.FilePath)
		prev, seen, err := p.fileLanguage(file)
		if err != nil {
			return err
		}
		if seen && prev != lang {
			return invalid("conflicting external-call receipt file languages")
		}
		if !seen {
			if err := p.put(keyStrings(rebuildExternalCallColumn('f'), file), append([]byte{1}, lang...)); err != nil {
				return err
			}
		}
		for _, n := range f.Nodes {
			if n == nil {
				continue
			}
			// Native nodes upsert, unlike converging edge rebinds. The caller
			// classification needs only Language, not the rest of its payload.
			if err := p.put(keyStrings(rebuildExternalCallColumn('a'), n.ID), append([]byte{1}, n.Language...)); err != nil {
				return err
			}
			if strings.HasPrefix(n.ID, "external-call::") {
				digest, err := rebuildExternalCallNodeDigest(projectRebuildReceiptNode(n))
				if err != nil {
					return err
				}
				if err := p.put(keyStrings(rebuildExternalCallColumn('i'), n.ID), digest[:]); err != nil {
					return err
				}
			}
		}
		for _, e := range f.Edges {
			if err := ctx.Err(); err != nil {
				return err
			}
			// The indexed lookup needs Reads and every receipt occurrence,
			// including same-key duplicates.
			if deferred && p.rules.IndexedEdge != nil {
				if err := p.captureIndexedEdge(e); err != nil {
					return err
				}
			}
			if e == nil || !rebuildExternalCallKind(e.Kind) {
				continue
			}
			key := pendingEdgeKey(e)
			receipt := append(rebuildExternalCallColumn('r'), key...)
			first, err := pendingGet(p.b.pending, receipt, copyValue)
			if err != nil {
				return err
			}
			raw, err := encodeEdge(e)
			if err != nil {
				return err
			}
			digest := sha256.Sum256(raw)
			if first != nil {
				if !bytes.Equal(first, digest[:]) {
					if err := p.put(append(rebuildExternalCallColumn('j'), key...), nil); err != nil {
						return err
					}
				}
				// Native rebind keeps the first payload at a duplicate identity.
				continue
			}
			if err := p.put(receipt, digest[:]); err != nil {
				return err
			}
			if deferred {
				// This authority belongs to the extractor, not necessarily the
				// file recorded by its edge or the caller node's language.
				if err := p.put(append(rebuildExternalCallColumn('u'), key...), []byte(lang)); err != nil {
					return err
				}
			}
			if !rebuildExternalCallTerminal(e.To) && (!deferred || !graph.IsUnresolvedTarget(e.To)) {
				continue
			}
			// Terminal inputs may supply the first duplicate's winning payload.
			// Only deferred unresolved receipts can vote via Maybe, including
			// those whose live successor is already non-terminal.
			h := append(rebuildExternalCallColumn('h'), key...)
			value := appendKeyString(nil, lang)
			if len(h)+len(value)+len(raw) <= rebuildExternalCallRowBytes {
				value = append(value, raw...)
			}
			// Keep an oversized-input marker. Only a consulted receipt can
			// refuse; a stationary, non-voting stdlib row cannot.
			if err := p.put(h, value); err != nil {
				return err
			}
		}
	}
	return ctx.Err()
}

func readRebuildExternalCallNode(reader pebble.Reader, id string, decode decodeRow[*graph.Node]) (*graph.Node, error) {
	key := pendingNodeKey(id)
	return pendingGet(reader, key, func(value []byte) (*graph.Node, error) {
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

// caller reads the classification projection without charging unrelated
// metadata. A voting row must subsequently charge the bytes it consulted.
func (p *rebuildExternalCallPass) caller(id string) (*graph.Node, bool, error) {
	key := pendingNodeKey(id)
	var oversized bool
	n, err := pendingGet(p.b.pending, key, func(value []byte) (*graph.Node, error) {
		oversized = len(key)+len(value) > rebuildExternalCallRowBytes
		n, err := decodeNodeLight(value)
		if err == nil && n.ID != id {
			return nil, invalid("external-call node does not match key")
		}
		return n, err
	})
	return n, oversized, err
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
	var stale *RebuildUnknownProvenanceError
	if errors.As(err, &stale) {
		return rebuildEdgeCustody{proof: rebuildProofUntrackedMove}, false, nil
	}
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

// receiptCaller returns the native parse caller's language, if a receipt
// establishes it. An absent receipt is not a claim about a generated caller.
func (p *rebuildExternalCallPass) receiptCaller(id string) (*graph.Node, error) {
	raw, err := pendingGet(p.b.pending, keyStrings(rebuildExternalCallColumn('a'), id), copyValue)
	if err != nil || raw == nil {
		return nil, err
	}
	return &graph.Node{ID: id, Language: string(raw[1:])}, nil
}

func (p *rebuildExternalCallPass) receiptEntrants(ctx context.Context) error {
	if err := pendingVisit(ctx, p.b.pending, rebuildLinkRecorderPrefix, func(r row) error {
		if len(r.value) < 2 || r.value[0] != familyEdge || r.value[1] != edgeOut {
			return nil
		}
		raw, err := decodeLinkRecorder(r.value)
		if err != nil || !rebuildExternalCallKind(raw.Kind) {
			return err
		}
		live, err := p.present(r.key[len(rebuildLinkRecorderPrefix):])
		if err != nil || !live {
			return err
		}
		return p.put(append(rebuildExternalCallColumn('c'), r.value...), r.key[len(rebuildLinkRecorderPrefix):])
	}); err != nil {
		return err
	}
	return p.visit(ctx, 'h', func(r row) error {
		lang, raw, err := readKeyString(r.value)
		if err != nil {
			return err
		}
		key := r.key[rebuildExternalCallColumnBytes:]
		exact, err := p.present(key)
		if err != nil || exact {
			return err
		}
		consumed, err := pendingGet(p.b.pending, append(rebuildExternalCallColumn('c'), key...), copyValue)
		if err != nil {
			return err
		}
		deferred := lang != "" && !IsRebuildExternalCallRetained(lang)
		if consumed != nil {
			id, err := decodeLinkRecorder(consumed)
			if err != nil {
				return err
			}
			if !deferred || strings.HasPrefix(id.To, "external-call::") {
				return nil
			}
		}
		id, err := decodeLinkRecorder(key)
		if err != nil {
			return err
		}
		e := &graph.Edge{From: id.From, To: id.To, Kind: id.Kind, FilePath: id.FilePath, Line: id.Line}
		if len(raw) != 0 {
			e, err = decodeEdge(raw)
			if err != nil {
				return err
			}
			if !bytes.Equal(key, pendingEdgeKey(e)) {
				return invalid("external-call receipt edge does not match key")
			}
		}
		caller, err := p.receiptCaller(e.From)
		if err != nil {
			return err
		}
		if caller == nil {
			caller, err = readRebuildExternalCallNode(p.b.pending, e.From, decodeNodeLight)
			if err != nil {
				return err
			}
		}
		if len(raw) == 0 {
			_, eligible := p.rules.Candidate(e, caller)
			if eligible || len(p.rules.Maybe(e, caller, lang)) != 0 {
				return fmt.Errorf("%w: external-call receipt input", ErrRebuildLinkInputSize)
			}
			return nil
		}
		// A retained receipt can be missing without a proved consuming move.
		// A deferred predecessor can also survive at its original terminal.
		if candidate, ok := p.rules.Candidate(e, caller); ok {
			if err := p.possibleInput(e, candidate, consumed != nil); err != nil {
				return err
			}
		}
		for _, candidate := range p.rules.Maybe(e, caller, lang) {
			if consumed != nil {
				id, err := decodeLinkRecorder(consumed)
				if err != nil {
					return err
				}
				// The exact same native landing is already represented by the
				// live vote. It is not a second deletion obligation.
				if id.From == e.From && id.Kind == e.Kind && id.FilePath == e.FilePath && id.Line == e.Line && id.To == candidate.OldTo {
					continue
				}
			}
			if err := p.possibleInput(e, candidate, consumed != nil); err != nil {
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
			id, err := decodeLinkRecorder(it.Key())
			if err != nil {
				return err
			}
			if !rebuildExternalCallKind(id.Kind) {
				continue
			}
			// receiptEntrants follows raw recorder keys independently, including
			// deferred receipts moved to non-terminal successors.
			if !strings.HasPrefix(id.To, "external-call::") && !rebuildExternalCallTerminal(id.To) && !graph.IsUnresolvedTarget(id.To) {
				continue
			}
			key := it.Key()
			custody, exact, err := p.custody(key)
			if err != nil {
				return err
			}
			rawKey := key
			if custody.raw != nil {
				rawKey = custody.raw
			}
			deferred, err := pendingGet(p.b.pending, append(rebuildExternalCallColumn('u'), rawKey...), copyValue)
			if err != nil {
				return err
			}
			value, err := it.ValueAndErr()
			if err != nil {
				return err
			}
			e, err := decodeEdgeLight(value)
			if err != nil {
				return err
			}
			if !bytes.Equal(key, pendingEdgeKey(e)) {
				return invalid("external-call live edge does not match key")
			}
			lang, _, err := p.fileLanguage(e.FilePath)
			if err != nil {
				return err
			}
			if len(deferred) != 0 {
				lang = string(deferred)
			}
			if err := p.liveEdge(ctx, view, key, rawKey, value, e, lang, exact); err != nil {
				return err
			}
		}
		return errors.Join(ctx.Err(), it.Error())
	})
}

func (p *rebuildExternalCallPass) liveEdge(ctx context.Context, view RebuildView, key, rawKey, value []byte, e *graph.Edge, lang string, exact bool) error {
	caller, callerLarge, err := p.caller(e.From)
	if err != nil {
		return err
	}
	retained, err := p.retained(e, caller)
	if err != nil {
		return err
	}
	if strings.HasPrefix(e.To, "external-call::") {
		p.count++
		if retained {
			if !exact {
				return fmt.Errorf("unproved retained external-call incumbent: %+v", graph.EdgeIdentityFor(e))
			}
			if err := rebuildExternalCallInput(key, value); err != nil {
				return err
			}
			if err := p.put(keyStrings(rebuildExternalCallColumn('t'), e.To), nil); err != nil {
				return err
			}
			if bytes.Equal(rawKey, key) {
				return p.checkDuplicate(rawKey, value, e, "", "")
			}
		}
		return nil
	}
	actual, eligible := p.rules.Candidate(e, caller)
	// Candidate and Maybe's initial lexical check need only light rows.
	// Edge metadata can veto a Maybe result, but cannot add one.
	if eligible || len(p.rules.Maybe(e, caller, lang)) != 0 {
		if err := rebuildExternalCallInput(key, value); err != nil {
			return err
		}
		e, err = decodeEdge(value)
		if err != nil {
			return err
		}
	}
	var alternates []RebuildExternalCallCandidate
	var mayChange, mayRefresh bool
	if eligible && p.rules.MayChangeLive != nil {
		mayChange, mayRefresh, err = p.rules.MayChangeLive(ctx, view, e, caller)
		if err != nil {
			return err
		}
	} else if !eligible {
		alternates = p.rules.Maybe(e, caller, lang)
	}
	if rawCaller, err := p.receiptCaller(e.From); err != nil {
		return err
	} else if rawCaller != nil {
		alternate, rawEligible := p.rules.Candidate(e, rawCaller)
		if eligible != rawEligible || eligible && actual.Language != alternate.Language {
			mayChange = true
			if rawEligible {
				alternates = append(alternates, alternate)
			}
		}
	}
	if eligible || len(alternates) != 0 {
		if callerLarge {
			return fmt.Errorf("%w: external-call caller input", ErrRebuildLinkInputSize)
		}
		if err := rebuildExternalCallInput(key, value); err != nil {
			return err
		}
	}
	// Native resolves callers in mutation chunks. A missing caller with a
	// package ID can be created by an earlier chunk of this same pass.
	if caller == nil && strings.HasPrefix(e.From, "external-call::") && !graph.IsUnresolvedTarget(e.To) {
		if err := p.put(append(rebuildExternalCallColumn('w'), key...), nil); err != nil {
			return err
		}
	}
	if eligible {
		if actual.OldTo != "" && actual.OldTo != e.To {
			return invalid("external-call live terminal changed by candidate classifier")
		}
		state := externalCallCertain
		if !exact || mayChange {
			state = externalCallOptional
		}
		if mayRefresh && retained {
			if err := p.put(append(rebuildExternalCallColumn('y'), key...), nil); err != nil {
				return err
			}
		}
		if err := p.vote(e, actual, state, false); err != nil {
			return err
		}
	}
	for _, alt := range alternates {
		// An identical caller vote is not a second deletion obligation.
		if eligible && alt.Node != nil && alt.Node.ID == actual.Node.ID && alt.Language == actual.Language &&
			(alt.OldTo == "" || alt.OldTo == e.To) {
			continue
		}
		if err := p.possibleInput(e, alt, true); err != nil {
			return err
		}
	}
	return nil
}

func (p *rebuildExternalCallPass) retained(e *graph.Edge, caller *graph.Node) (bool, error) {
	lang, found, err := p.fileLanguage(e.FilePath)
	if err != nil {
		return false, err
	}
	if !found {
		if caller != nil {
			lang = caller.Language
		}
		if IsRebuildExternalCallRetained(lang) {
			return true, nil
		}
		raw, err := p.receiptCaller(e.From)
		if err != nil {
			return false, err
		}
		if raw != nil {
			lang = raw.Language
		}
	}
	return IsRebuildExternalCallRetained(lang), nil
}

// Only the winning payload (or a stationary incumbent) needs duplicate
// proof. Later losers are deleted regardless of their payload.
func (p *rebuildExternalCallPass) checkDuplicate(rawKey, liveRaw []byte, live *graph.Edge, node, lang string) error {
	dup, err := p.present(append(rebuildExternalCallColumn('j'), rawKey...))
	if err != nil || !dup {
		return err
	}
	first, err := pendingGet(p.b.pending, append(rebuildExternalCallColumn('r'), rawKey...), copyValue)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(liveRaw)
	if bytes.Equal(first, digest[:]) {
		return nil
	}
	if node != "" && bytes.Equal(rawKey, pendingEdgeKey(live)) {
		// Rebind overwrites provenance/confidence and removes import hints.
		// Compare the output payload, not those erased raw fields.
		value, err := pendingGet(p.b.pending, append(rebuildExternalCallColumn('h'), rawKey...), copyValue)
		if err != nil {
			return err
		}
		if value != nil {
			_, firstRaw, err := readKeyString(value)
			if err != nil {
				return err
			}
			if len(firstRaw) == 0 {
				return fmt.Errorf("%w: external-call duplicate receipt", ErrRebuildLinkInputSize)
			}
			a, err := decodeEdge(firstRaw)
			if err != nil {
				return err
			}
			b, err := decodeEdge(liveRaw)
			if err != nil {
				return err
			}
			firstMove, liveMove := p.rules.Rebind(a, node, lang), p.rules.Rebind(b, node, lang)
			if firstMove.Edge != nil && liveMove.Edge != nil {
				left, err := encodeEdge(firstMove.Edge)
				if err != nil {
					return err
				}
				right, err := encodeEdge(liveMove.Edge)
				if err != nil {
					return err
				}
				if bytes.Equal(left, right) {
					return nil
				}
			}
		}
	}
	return fmt.Errorf("uncertain retained external-call duplicate payload: %+v", graph.EdgeIdentityFor(live))
}

func (p *rebuildExternalCallPass) possible(e *graph.Edge, c RebuildExternalCallCandidate) error {
	if e == nil || c.OldTo == "" || c.OldTo == e.To {
		return p.possibleInput(e, c, false)
	}
	key := pendingEdgeKey(e)
	exists, err := p.present(key)
	if err != nil {
		return err
	}
	if !exists {
		live, err := pendingGet(p.b.pending, append(rebuildExternalCallColumn('c'), key...), copyValue)
		if err != nil {
			return err
		}
		if live != nil {
			id, err := decodeLinkRecorder(live)
			if err != nil {
				return err
			}
			exists = id.To != c.OldTo && !strings.HasPrefix(id.To, "external-call::")
		}
	}
	return p.possibleInput(e, c, exists)
}

func (p *rebuildExternalCallPass) possibleInput(e *graph.Edge, c RebuildExternalCallCandidate, liveInput bool) error {
	return p.vote(e, c, externalCallPossible, liveInput)
}

// Ordered as native: From, Kind, FilePath, signed Line, package ID, old To.
// The final flag and language do not take part in the native comparison.
func (p *rebuildExternalCallPass) vote(e *graph.Edge, c RebuildExternalCallCandidate, state byte, liveInput bool) error {
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
		if err != nil || exact && !liveInput {
			return err
		}
		key[len(key)-1] = state
		if liveInput {
			return p.put(key, []byte{1})
		}
		seen, err := p.present(key)
		if err != nil || seen {
			return err
		}
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
		voteKey := r.key[rebuildExternalCallColumnBytes:]
		v, err := readRebuildExternalCallVote(voteKey)
		if err != nil {
			return err
		}
		if !bytes.Equal(group, v.group) {
			group = bytes.Clone(v.group)
			winner, first, firstOld = false, true, ""
			caller, _, err := p.caller(v.edge.From)
			if err != nil {
				return err
			}
			retained, err = p.retained(&v.edge, caller)
			if err != nil {
				return err
			}
			incumbent := v.edge
			incumbent.To = v.node
			key := pendingEdgeKey(&incumbent)
			occupied, err = p.present(key)
			if err != nil {
				return err
			}
			if occupied && retained {
				_, exact, err := p.custody(key)
				if err != nil {
					return err
				}
				if !exact {
					return fmt.Errorf("unproved external-call incumbent: %+v", graph.EdgeIdentityFor(&incumbent))
				}
			}
		}
		if retained {
			if v.state == externalCallOptional {
				return fmt.Errorf("uncertain retained external-call terminal: %+v", graph.EdgeIdentityFor(&v.edge))
			}
			if v.state == externalCallPossible {
				if !occupied && (!winner || v.edge.To == firstOld) {
					return fmt.Errorf("uncertain retained external-call winner: %+v", graph.EdgeIdentityFor(&v.edge))
				}
				if len(r.value) != 0 {
					return fmt.Errorf("uncertain retained external-call deletion: %+v", graph.EdgeIdentityFor(&v.edge))
				}
			}
			if first {
				if err := p.put(keyStrings(rebuildExternalCallColumn('t'), v.node), nil); err != nil {
					return err
				}
			}
		}
		if err := p.put(append(keyStrings(rebuildExternalCallColumn('p'), v.node), voteKey...), r.value); err != nil {
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
		return p.planMove(v, old, retained)
	})
}

func (p *rebuildExternalCallPass) planMove(v rebuildExternalCallVote, old []byte, retained bool) error {
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
	if retained {
		refresh, err := p.present(append(rebuildExternalCallColumn('y'), old...))
		if err != nil {
			return err
		}
		if refresh {
			return fmt.Errorf("uncertain retained external-call winner payload: %+v", graph.EdgeIdentityFor(e))
		}
		rawKey := old
		if custody.raw != nil {
			rawKey = custody.raw
		}
		raw, err := encodeEdge(e)
		if err != nil {
			return err
		}
		if err := p.checkDuplicate(rawKey, raw, e, v.node, v.lang); err != nil {
			return err
		}
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

// Native collects all terminal edges before the pass but looks up their
// caller nodes in each mutation chunk. If a possible earlier vote could
// create a previously missing caller, later classification is not frozen.
// A later creator cannot affect this caller; we conservatively include any
// earlier site (even when native might keep it in the same chunk).
func (p *rebuildExternalCallPass) checkChunkCallers(ctx context.Context) error {
	return p.visit(ctx, 'w', func(r row) error {
		e, err := p.edge(r.key[rebuildExternalCallColumnBytes:])
		if err != nil || e == nil {
			return err
		}
		actual, eligible := p.rules.Candidate(e, nil)
		retained, err := p.retained(e, nil)
		if err != nil {
			return err
		}
		if !retained && eligible {
			retained, err = p.present(keyStrings(rebuildExternalCallColumn('t'), actual.Node.ID))
			if err != nil {
				return err
			}
		}
		prefix := keyStrings(rebuildExternalCallColumn('p'), e.From)
		return pendingVisit(ctx, p.b.pending, prefix, func(vote row) error {
			v, err := readRebuildExternalCallVote(vote.key[len(prefix):])
			if err != nil || v.edge.From > e.From {
				return err
			}
			alt, ok := p.rules.Candidate(e, &graph.Node{ID: e.From, Language: v.lang})
			if eligible == ok && (!ok || actual.Language == alt.Language) {
				return nil
			}
			relevant := retained
			if !relevant && ok {
				relevant, err = p.present(keyStrings(rebuildExternalCallColumn('t'), alt.Node.ID))
				if err != nil {
					return err
				}
			}
			if relevant {
				return fmt.Errorf("uncertain retained external-call chunk caller: %q", e.From)
			}
			return nil
		})
	})
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
		node, rest, err := readKeyString(r.key[rebuildExternalCallColumnBytes:])
		if err != nil {
			return err
		}
		if node != id {
			if err := finish(); err != nil {
				return err
			}
			id, lang, certainOrder = node, "", nil
			voted, different, actual = false, false, false
			retained, err = p.present(keyStrings(rebuildExternalCallColumn('t'), id))
			if err != nil {
				return err
			}
			existing, err = p.present(pendingNodeKey(id))
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
	if err := finish(); err != nil {
		return err
	}
	return p.visit(ctx, 't', func(r row) error {
		id, _, err := readKeyString(r.key[rebuildExternalCallColumnBytes:])
		if err != nil {
			return err
		}
		return p.provePackage(id)
	})
}

func rebuildExternalCallNodeDigest(n *graph.Node) ([sha256.Size]byte, error) {
	portable := *n
	portable.RepoPrefix, portable.WorkspaceID, portable.ProjectID, portable.AbsoluteFilePath = "", "", "", ""
	portable.FilePath = pathkey.Normalize(portable.FilePath)
	raw, err := encodeNode(&portable)
	return sha256.Sum256(raw), err
}

func (p *rebuildExternalCallPass) provePackage(id string) error {
	receipt, err := pendingGet(p.b.pending, keyStrings(rebuildExternalCallColumn('i'), id), copyValue)
	if err != nil {
		return err
	}
	n, err := readRebuildExternalCallNode(p.b.pending, id, decodeNode)
	if err != nil {
		return err
	}
	if n == nil && receipt == nil {
		return nil
	}
	if receipt != nil && n != nil {
		digest, err := rebuildExternalCallNodeDigest(n)
		if err != nil {
			return err
		}
		if bytes.Equal(receipt, digest[:]) {
			return nil
		}
	}
	if receipt == nil && n != nil {
		prior, err := pendingGet(p.b.pending, rebuildExternalCallPackageProofKey(id), copyValue)
		if err != nil {
			return err
		}
		if prior != nil {
			raw, err := encodeNode(n)
			if err != nil {
				return err
			}
			digest := sha256.Sum256(raw)
			if bytes.Equal(prior, digest[:]) {
				return nil
			}
		}
	}
	return fmt.Errorf("unproved retained external-call package incumbent: %q", id)
}

func (p *rebuildExternalCallPass) apply(ctx context.Context) error {
	if err := p.visit(ctx, 'n', func(r row) error {
		n, err := decodeNode(r.value)
		if err != nil {
			return err
		}
		if err := p.b.writeNode(n); err != nil {
			return err
		}
		digest := sha256.Sum256(r.value)
		return p.b.pending.Set(rebuildExternalCallPackageProofKey(n.ID), digest[:], pebble.NoSync)
	}); err != nil {
		return err
	}
	if err := p.visit(ctx, 'd', func(r row) error {
		key := r.key[rebuildExternalCallColumnBytes:]
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
		key := r.key[rebuildExternalCallColumnBytes:]
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

// ExternalCallNode returns the bounded light caller projection used by the
// skipped-producer checks. It does not read unrelated node metadata.
func (v RebuildView) ExternalCallNode(ctx context.Context, id string) (*graph.Node, error) {
	return v.externalCallNode(ctx, id, decodeNodeLight)
}

// ExternalCallTarget returns a bounded full row for a skipped producer that
// reads target metadata rather than just caller language.
func (v RebuildView) ExternalCallTarget(ctx context.Context, id string) (*graph.Node, error) {
	return v.externalCallNode(ctx, id, decodeNode)
}

func (v RebuildView) externalCallNode(ctx context.Context, id string, decode decodeRow[*graph.Node]) (*graph.Node, error) {
	var n *graph.Node
	err := v.read(ctx, func(snap *pebble.Snapshot) error {
		var err error
		n, err = readRebuildExternalCallNode(snap, id, decode)
		return err
	})
	return n, err
}

// VisitExternalCallEdges filters by key and skipped-producer role before
// charging the edge row bound. An oversized non-voter never needs a full
// payload decode; the role probe reads only the metadata fields it uses.
// selectRole may read the caller during the callback if the edge role needs it.
func (v RebuildView) VisitExternalCallEdges(ctx context.Context, selectEdge func(graph.EdgeIdentity) bool,
	selectRole func(*graph.Edge, func() (*graph.Node, error)) (bool, error), yield func(*graph.Edge) error,
) error {
	return v.visit(ctx, []byte{familyEdge, edgeOut}, func(snap *pebble.Snapshot, r row) error {
		id, err := decodeLinkRecorder(r.key)
		if err != nil || !selectEdge(id) {
			return err
		}
		sizeErr := rebuildExternalCallInput(r.key, r.value)
		decode := decodeEdge
		if sizeErr != nil {
			decode = func(raw []byte) (*graph.Edge, error) {
				return readEdgeMetadata(raw, readExternalCallRoleMetadata)
			}
		}
		e, err := decode(r.value)
		if err != nil {
			return err
		}
		if !bytes.Equal(r.key, pendingEdgeKey(e)) {
			return invalid("external-call live edge does not match key")
		}
		selected, err := selectRole(e, func() (*graph.Node, error) {
			caller, err := pendingGet(snap, pendingNodeKey(id.From), decodeNodeLight)
			if err == nil && caller != nil && caller.ID != id.From {
				return nil, invalid("external-call caller does not match key")
			}
			return caller, err
		})
		if err != nil || !selected {
			return err
		}
		if sizeErr != nil {
			return sizeErr
		}
		return yield(e)
	})
}

func readExternalCallRoleMetadata(c *recordCursor) map[string]any {
	return readRebuildRoleMetadata(c, func(key string) bool {
		switch key {
		case "godot_connection", "godot_method", "rust_path", "rust_recv", "receiver_type", "rust_recv_expr":
			return true
		default:
			return false
		}
	})
}

// readRebuildRoleMetadata projects scalar role fields before the size check.
func readRebuildRoleMetadata(c *recordCursor, selects func(string) bool) map[string]any {
	if !c.boolean() {
		return nil
	}
	raw := c.bytes()
	if c.err != nil {
		return nil
	}
	meta, err := rebuildRoleMetadata(raw, selects)
	c.err = err
	return meta
}

func rebuildRoleMetadata(raw []byte, selects func(string) bool) (map[string]any, error) {
	if !isFlatMeta(raw) {
		return nil, invalid("metadata revision")
	}
	d := v3MetaScan{metaDecoder: metaDecoder{buf: raw[2:]}}
	n, err := d.readCount()
	if err != nil {
		return nil, err
	}
	var meta map[string]any
	var previous []byte
	for i := 0; i < n; i++ {
		key, err := d.rawString()
		if err != nil {
			return nil, err
		}
		if i > 0 && bytes.Compare(key, previous) <= 0 {
			return nil, invalid("metadata keys not unique and sorted")
		}
		previous = key
		start := d.pos
		if err := d.scanValue(1); err != nil {
			return nil, err
		}
		if !selects(string(key)) {
			continue
		}
		scalar := raw[2+start : 2+d.pos]
		switch scalar[0] {
		case metaTagBool, metaTagString:
		case metaTagJSON:
			// Named strings and bools can use a JSON leaf. A JSON object
			// or array cannot supply one of these role fields.
			leaf := v3MetaScan{metaDecoder: metaDecoder{buf: scalar[1:]}}
			text, err := leaf.rawString()
			if err != nil {
				return nil, err
			}
			text = bytes.TrimSpace(text)
			if len(text) == 0 || text[0] != '"' && text[0] != 't' && text[0] != 'f' {
				continue
			}
		default:
			continue
		}
		value := metaDecoder{buf: scalar}
		x, err := value.readValue(string(key), 1)
		if err != nil {
			return nil, err
		}
		switch x.(type) {
		case bool, string:
			if meta == nil {
				meta = make(map[string]any)
			}
			meta[string(key)] = x
		}
	}
	if d.pos != len(d.buf) {
		return nil, invalid("unbound metadata bytes")
	}
	return meta, nil
}

// VisitExternalCallNodes filters a light node before charging the row bound.
func (v RebuildView) VisitExternalCallNodes(ctx context.Context, selectNode func(*graph.Node) bool, yield func(*graph.Node) error) error {
	return v.visit(ctx, []byte{familyNode}, func(_ *pebble.Snapshot, r row) error {
		n, err := decodeNodeLight(r.value)
		if err != nil {
			return err
		}
		if !bytes.Equal(r.key, pendingNodeKey(n.ID)) {
			return invalid("external-call node does not match key")
		}
		if !selectNode(n) {
			return nil
		}
		if err := rebuildExternalCallInput(r.key, r.value); err != nil {
			return err
		}
		return yield(n)
	})
}
