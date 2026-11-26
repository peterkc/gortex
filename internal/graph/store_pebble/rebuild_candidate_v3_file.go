package store_pebble

// Logical linker limits are independent of the physical ordered file keys
// and its additional lookup families.
import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/cockroachdb/pebble/v2"
	"github.com/cockroachdb/pebble/v2/sstable"
	"github.com/zzet/gortex/internal/graph"
)

const v3PieceBytes = 64 << 10
const v3Header = 17 // canonical length (8), flags (1), dense piece ID (8)
const (
	v3Stable byte = 1 << iota
	v3Sized
	v3Unbounded
)

// Simulates sortedRuns' logical limits without writing any order tables.
// The final drain counts too; the first row itself may exceed spill.
type v3LogicalRuns struct{ rows, bytes, runs int }

func (r *v3LogicalRuns) add(size, spill int) error {
	if r.rows != 0 && r.bytes+size+48 > spill {
		if r.runs == goldFileCap {
			return fmt.Errorf("pebble order exceeds %d sorted runs", goldFileCap)
		}
		r.runs++
		r.rows, r.bytes = 0, 0
	}
	r.rows++
	r.bytes += size + 48
	return nil
}
func (r *v3LogicalRuns) finish() error {
	if r.rows != 0 && r.runs == goldFileCap {
		return fmt.Errorf("pebble order exceeds %d sorted runs", goldFileCap)
	}
	return nil
}

type v3Summary struct {
	rows, childBytes, orderBytes, maxChild, maxOrder, first int
	unsized                                                 bool
}

func (s *v3Summary) add(child, order int, sized bool) {
	s.rows++
	s.childBytes += child
	s.orderBytes += order
	s.maxChild = max(s.maxChild, child)
	s.maxOrder = max(s.maxOrder, order)
	s.unsized = s.unsized || !sized
}
func (s v3Summary) value() []byte {
	b := make([]byte, 0, 49)
	for _, n := range [...]int{s.rows, s.childBytes, s.orderBytes, s.maxChild, s.maxOrder, s.first} {
		b = binary.BigEndian.AppendUint64(b, uint64(n))
	}
	return appendBool(b, s.unsized)
}
func v3ReadSummary(b []byte) (v3Summary, error) {
	if len(b) != 49 || b[48] > 1 {
		return v3Summary{}, invalid("v3 candidate summary")
	}
	s := v3Summary{unsized: b[48] == 1}
	dst := [...]*int{&s.rows, &s.childBytes, &s.orderBytes, &s.maxChild, &s.maxOrder, &s.first}
	for i, p := range dst {
		*p = int(binary.BigEndian.Uint64(b[i*8:]))
		if *p < 0 {
			return v3Summary{}, invalid("v3 candidate summary size")
		}
	}
	return s, nil
}
func v3RunKey(key []byte) ([]byte, error) {
	if len(key) == 0 {
		return nil, invalid("v3 candidate key")
	}
	fields := 1
	switch key[0] {
	case 'A':
		fields = 3
	case 'B':
		fields = 2
	case 'Q':
	default:
		return nil, invalid("v3 candidate family")
	}
	rest := key[1:]
	for range fields {
		_, next, err := readKeyString(rest)
		if err != nil {
			return nil, err
		}
		rest = next
	}
	return key[:len(key)-len(rest)], nil
}
func v3LogicalOrderKeyBytes(key, prefix []byte) int {
	// order record includes original position and ordinal (12). A's final
	// language and ID strings survive, B and Q retain just ID.
	extra := len(key) - len(prefix) - 8
	if key[0] == 'A' {
		rest := prefix[1:]
		for range 2 {
			_, rest, _ = readKeyString(rest)
		}
		extra += len(rest)
	}
	return 12 + extra
}

func buildV3File(ctx context.Context, dir string, view RebuildView, source func(func(*graph.Node) error) error) (pieces uint64, chargeErrors bool, err error) {
	runsDir := filepath.Join(dir, "v3-runs")
	if err = os.Mkdir(runsDir, 0700); err != nil {
		return 0, false, err
	}
	// Each A/B/Q physical row is <=3 times its corresponding H98 build row:
	// every added escaped tuple byte comes from the same raw value, and the
	// 17-byte header fits in the old row overhead. A+B duplicate a name row.
	// Six times the H98 run threshold therefore admits every old logical run;
	// no new physical run-cap refusal can precede the logical cap.
	runs := sortedRuns{dir: runsDir, name: "v3", limit: 6 * goldBufferBytes, options: headOptions(), format: sstable.TableFormatPebblev4}
	var logical v3LogicalRuns
	var ordinal uint64
	var snap *pebble.Snapshot
	if view.build != nil {
		snap = view.build.pending.NewSnapshot()
		defer func() { err = errors.Join(err, snap.Close()) }()
	}
	err = source(func(n *graph.Node) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if n == nil || n.ID == "" {
			return errors.New("candidate has no node identity")
		}
		ordinal++
		if n.Name == "" && n.QualName == "" {
			return nil
		}
		raw, err := encodeNode(n)
		if err != nil {
			return fmt.Errorf("encode candidate %q: %w", n.ID, err)
		}
		// First enforce the original logical build checks, in emission order.
		for i, k := range [...]string{n.Name, n.QualName} {
			if k == "" {
				continue
			}
			old := len(linkPrefix(LinkKeyKind(i)+LinkName, k)) + 8 + len(raw)
			if old > linkRecordBytes {
				return fmt.Errorf("candidate record exceeds %d bytes", linkRecordBytes)
			}
			if err := logical.add(old, goldBufferBytes); err != nil {
				return fmt.Errorf("pebble candidates exceeds %d sorted runs", goldFileCap)
			}
		}
		large, preErr := v3UnboundedRecord(raw)
		// Preserve H98's deferred coordinator codec errors. The builder
		// derives lengths only, never refuses a row on this second roundtrip.
		var canonical []byte
		if !large && preErr == nil {
			decoded, decodeErr := decodeNode(raw)
			if decodeErr == nil {
				canonical, _ = encodeNode(decoded)
			}
		}
		if len(canonical) > 0 && !bytes.Equal(raw, canonical) {
			exception, e := v3UnboundedRecord(canonical)
			large = large || exception || e != nil
		}
		flags := byte(0)
		if bytes.Equal(raw, canonical) {
			flags |= v3Stable
		}
		if large || preErr != nil {
			flags |= v3Unbounded
		}
		for i, k := range [...]string{n.Name, n.QualName} {
			if k == "" {
				continue
			}
			sized := len(canonical) != 0 && len(canonical) == len(raw)
			if snap != nil {
				column := nameRebuildScoped
				if i != 0 {
					column = nameQualified
				}
				key := rebuildCandidateKey(column, n)
				// Missing or malformed entries are unsized here; the scope
				// scan below flags malformed entries even in other languages.
				nbytes, e := pendingGet(snap, key, func(value []byte) (uint64, error) {
					_, _, _, nbytes, _ := readRebuildCandidateIndex(column, key, value)
					return nbytes, nil
				})
				if e != nil {
					return e
				}
				sized = sized && nbytes == uint64(len(canonical))
			}
			val := make([]byte, v3Header, len(raw)+v3Header)
			binary.BigEndian.PutUint64(val, uint64(len(canonical)))
			val[8] = flags
			if sized {
				val[8] |= v3Sized
			}
			val = append(val, raw...)
			var keys [][]byte
			if i == 0 {
				keys = [][]byte{keyStrings([]byte{'A'}, k, n.RepoPrefix, n.Language, n.ID), keyStrings([]byte{'B'}, k, n.RepoPrefix, n.ID)}
			} else {
				keys = [][]byte{keyStrings([]byte{'Q'}, k, n.ID)}
			}
			for _, key := range keys {
				if err := runs.add(binary.BigEndian.AppendUint64(key, ordinal), val); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return 0, false, err
	}
	if err = logical.finish(); err != nil {
		return 0, false, fmt.Errorf("pebble candidates exceeds %d sorted runs", goldFileCap)
	}
	// Summaries have the same lexical order as the run prefixes. A temporary
	// SST allows writing S after Q without a repository-sized in-memory map.
	table, err := newLinkTable(filepath.Join(dir, "v3.sst"))
	if err != nil {
		return 0, false, err
	}
	defer func() { err = errors.Join(err, table.Close()) }()
	summaryPath := filepath.Join(runsDir, "summaries.sst")
	summaries, err := newLinkTable(summaryPath)
	if err != nil {
		return 0, false, err
	}
	var current []byte
	var sum v3Summary
	var pieceBytes int
	flush := func() error {
		if current == nil {
			return nil
		}
		return summaries.Set(append([]byte{'S'}, current...), sum.value())
	}
	err = runs.merge(ctx, func(r row) error {
		prefix, e := v3RunKey(r.key)
		if e != nil {
			return e
		}
		if !bytes.Equal(prefix, current) {
			if e := flush(); e != nil {
				return e
			}
			current = bytes.Clone(prefix)
			sum = v3Summary{first: int(pieces)}
			pieceBytes = 0
		}
		size := len(r.key) + len(r.value)
		if pieceBytes == 0 || pieceBytes+size > v3PieceBytes || r.value[8]&v3Unbounded != 0 {
			pieces++
			pieceBytes = 0
		}
		pieceBytes += size
		if r.value[8]&v3Unbounded != 0 {
			pieceBytes = v3PieceBytes + 1
		} // a forced singleton ends this piece
		value := bytes.Clone(r.value) // SST iterator values are borrowed; never modify its block
		binary.BigEndian.PutUint64(value[9:], pieces-1)
		rawLen := len(r.value) - v3Header
		canon := int(binary.BigEndian.Uint64(r.value))
		sum.add(rawLen+linkResultKeyBytes, canon+v3LogicalOrderKeyBytes(r.key, prefix), r.value[8]&v3Sized != 0)
		return table.Set(r.key, value)
	})
	if err == nil {
		err = flush()
	}
	if err = errors.Join(err, summaries.Close()); err != nil {
		return 0, false, err
	}
	reader, err := openLinkTable(ctx, summaryPath)
	if err != nil {
		return 0, false, err
	}
	err = func() (err error) {
		defer func() { err = errors.Join(err, reader.Close()) }()
		it, e := reader.NewIter(sstable.NoTransforms, nil, nil, sstable.TableBlobContext{})
		if e != nil {
			return e
		}
		defer func() { err = errors.Join(err, it.Close()) }()
		for kv := it.First(); kv != nil; kv = it.Next() {
			if e := ctx.Err(); e != nil {
				return e
			}
			raw, _, e := kv.Value(nil)
			if e != nil {
				return e
			}
			if e = table.Set(kv.K.UserKey, raw); e != nil {
				return e
			}
		}
		return it.Error()
	}()
	if err != nil {
		return 0, false, err
	}
	if snap != nil {
		chargeErrors, err = v3WriteChargeErrors(ctx, snap, table)
		if err != nil {
			return 0, false, err
		}
	}
	return pieces, chargeErrors, os.RemoveAll(runsDir)
}

// H98 reads the whole name+repo index prefix before filtering language. An
// invalid entry or an index row not represented by a node can invalidate a
// request's charge even when that row is absent from its candidate summary.
// Write only affected scopes, in key order, after the ordinary summaries.
func v3WriteChargeErrors(ctx context.Context, snap *pebble.Snapshot, table *sstable.Writer) (found bool, err error) {
	for _, column := range []byte{nameQualified, nameRebuildScoped} {
		var last []byte
		if err := pendingVisit(ctx, snap, []byte{familyName, column}, func(r row) error {
			name, rest, err := readKeyString(r.key[2:])
			if err != nil {
				return nil // no valid request prefix can reach this key
			}
			scope := keyStrings([]byte{'S', 'Z', column}, name)
			if column == nameRebuildScoped {
				repo, _, err := readKeyString(rest)
				if err != nil {
					return nil // no valid scoped request can reach this key
				}
				scope = appendKeyString(scope, repo)
			}
			if bytes.Equal(last, scope) {
				return nil // one flag per affected scope
			}
			_, _, id, _, entryErr := readRebuildCandidateIndex(column, r.key, r.value)
			if entryErr == nil {
				matches, err := pendingGet(snap, pendingNodeKey(id), func(raw []byte) (bool, error) {
					n, err := decodeNodeLight(raw)
					return err == nil && n.ID == id && bytes.Equal(r.key, rebuildCandidateKey(column, n)), nil
				})
				if err != nil {
					return err
				}
				if matches {
					return nil
				}
			}
			last = scope
			found = true
			return table.Set(scope, (v3Summary{unsized: true}).value())
		}); err != nil {
			return found, err
		}
	}
	return found, nil
}

func v3Prefixes(req LinkRequest) [][]byte {
	if req.Kind == LinkQualName {
		return [][]byte{keyStrings([]byte{'Q'}, req.Key)}
	}
	if len(req.Languages) == 0 {
		return [][]byte{keyStrings([]byte{'B'}, req.Key, req.RepoPrefix)}
	}
	langs := slices.Clone(req.Languages)
	slices.Sort(langs)
	langs = slices.Compact(langs)
	prefixes := make([][]byte, 0, len(langs))
	for _, lang := range langs {
		prefixes = append(prefixes, keyStrings([]byte{'A'}, req.Key, req.RepoPrefix, lang))
	}
	return prefixes
}

func (v *v3Reader) summary(prefix []byte) (sum v3Summary, err error) {
	skey := string(prefix)
	if sum, ok := v.summaries[skey]; ok {
		return sum, nil
	}
	defer func() {
		if err == nil && len(skey) < 4096 && len(v.summaries) < v3SingleTableEntries {
			if v.summaries == nil {
				v.summaries = map[string]v3Summary{}
			}
			v.summaries[skey] = sum
		}
	}()
	it, err := v.table.NewIter(sstable.NoTransforms, nil, nil, sstable.TableBlobContext{})
	if err != nil {
		return sum, err
	}
	defer func() { err = errors.Join(err, it.Close()) }()
	key := append([]byte{'S'}, prefix...)
	kv := it.SeekGE(key, 0)
	if kv == nil || !bytes.Equal(kv.K.UserKey, key) {
		return sum, it.Error()
	}
	raw, _, err := kv.Value(nil)
	if err != nil {
		return sum, err
	}
	return v3ReadSummary(raw)
}

func (v *v3Reader) loadPiece(ctx context.Context, prefix, start string, id uint64, reserve func(int, bool) error) (p *v3Piece, err error) {
	it, err := v.table.NewIter(sstable.NoTransforms, nil, nil, sstable.TableBlobContext{})
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, it.Close()) }()
	var encoded []row
	size := 0
	p = &v3Piece{}
	for kv := it.SeekGE([]byte(start), 0); kv != nil && bytes.HasPrefix(kv.K.UserKey, []byte(prefix)); kv = it.Next() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		raw, _, err := kv.Value(nil)
		if err != nil {
			return nil, err
		}
		if len(raw) < v3Header {
			return nil, invalid("v3 candidate row")
		}
		if binary.BigEndian.Uint64(raw[9:]) != id {
			p.next = string(kv.K.UserKey)
			break
		}
		encoded = append(encoded, row{bytes.Clone(kv.K.UserKey), bytes.Clone(raw)})
		size += len(kv.K.UserKey) + len(raw)
	}
	if err := it.Error(); err != nil {
		return nil, err
	}
	if len(encoded) == 0 {
		return nil, invalid("missing v3 candidate piece")
	}
	p.encoded = size + len(p.next)
	p.unbounded = encoded[0].value[8]&v3Unbounded != 0
	if p.unbounded && len(encoded) != 1 {
		return nil, invalid("v3 exceptional candidate piece must be singleton")
	}
	if len(encoded) > 1 && size > v3PieceBytes {
		return nil, invalid("v3 candidate piece bound")
	}
	if err := reserve(p.encoded, p.unbounded); err != nil {
		return nil, err
	}
	p.rows = make([]v3CachedRow, 0, len(encoded))
	for _, r := range encoded {
		raw := r.value[v3Header:]
		n, decodeErr := decodeNode(raw)
		canonical := raw
		if decodeErr == nil && r.value[8]&v3Stable == 0 {
			canonical, decodeErr = encodeNode(n)
			if decodeErr == nil {
				n, decodeErr = decodeNode(canonical)
			}
		}
		canonLen := int(binary.BigEndian.Uint64(r.value))
		if decodeErr != nil {
			n, err = decodeNodeLight(raw)
			if err != nil {
				return nil, err
			}
		} else if canonLen == 0 {
			canonLen = len(canonical)
		} else if len(canonical) != canonLen {
			return nil, invalid("v3 candidate canonical length")
		}
		p.rows = append(p.rows, v3CachedRow{node: n, err: decodeErr, rawBytes: len(raw), canonicalBytes: canonLen, orderBytes: canonLen + v3LogicalOrderKeyBytes(r.key, []byte(prefix)), ordinal: binary.BigEndian.Uint64(r.key[len(r.key)-8:])})
	}
	return p, nil
}

// Requests with narrower qualified filters have no H98 admission summary.
// The file still preserves global ID order; their matching rows are checked
// individually.
func (v *v3Reader) rawRows(ctx context.Context, req LinkRequest, fn func(key, raw []byte, canon int) error) (err error) {
	it, err := v.table.NewIter(sstable.NoTransforms, nil, nil, sstable.TableBlobContext{})
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, it.Close()) }()
	for _, prefix := range v3Prefixes(req) {
		for kv := it.SeekGE(prefix, 0); kv != nil && bytes.HasPrefix(kv.K.UserKey, prefix); kv = it.Next() {
			if err := ctx.Err(); err != nil {
				return err
			}
			value, _, err := kv.Value(nil)
			if err != nil {
				return err
			}
			if len(value) < v3Header {
				return invalid("v3 candidate row")
			}
			raw := value[v3Header:]
			n, err := decodeNodeLight(raw)
			if err != nil {
				return err
			}
			if !req.match(n) {
				continue
			}
			if err := fn(kv.K.UserKey, raw, int(binary.BigEndian.Uint64(value))); err != nil {
				return err
			}
		}
		if err := it.Error(); err != nil {
			return err
		}
	}
	return nil
}
func (v *v3Reader) walk(ctx context.Context, req LinkRequest, fn func(v3CachedRow) error) (err error) {
	defer func() { err = errors.Join(err, v.cache.releaseBorrowed()) }()
	for _, prefix := range v3Prefixes(req) {
		sum, err := v.summary(prefix)
		if err != nil {
			return err
		}
		if sum.rows == 0 {
			continue
		}
		start := string(prefix)
		id := uint64(sum.first)
		for start != "" {
			if err := ctx.Err(); err != nil {
				return err
			}
			p, err := v.cache.get(start, id, func(reserve func(int, bool) error) (*v3Piece, error) {
				return v.loadPiece(ctx, string(prefix), start, id, reserve)
			})
			if err != nil {
				return err
			}
			for _, r := range p.rows {
				if r.node == nil {
					return invalid("v3 candidate identity")
				}
				if !req.match(r.node) {
					if req.Kind == LinkQualName && (!req.AllRepos || len(req.Languages) != 0) {
						continue
					}
					return invalid("v3 candidate does not match request")
				}
				if err := fn(r); err != nil {
					return err
				}
			}
			start = p.next
			id++
		}
	}
	return nil
}

func (v *v3Reader) requestSummary(ctx context.Context, req LinkRequest) (sum v3Summary, err error) {
	if req.Kind == LinkQualName && (!req.AllRepos || len(req.Languages) != 0) {
		err = v.rawRows(ctx, req, func(key, raw []byte, canon int) error {
			prefix, e := v3RunKey(key)
			if e != nil {
				return e
			}
			sum.add(len(raw)+12, canon+v3LogicalOrderKeyBytes(key, prefix), canon != 0 && canon == len(raw))
			return nil
		})
		return sum, err
	}
	for _, prefix := range v3Prefixes(req) {
		s, err := v.summary(prefix)
		if err != nil {
			return sum, err
		}
		sum.rows += s.rows
		sum.childBytes += s.childBytes
		sum.orderBytes += s.orderBytes
		sum.maxChild = max(sum.maxChild, s.maxChild)
		sum.maxOrder = max(sum.maxOrder, s.maxOrder)
		sum.unsized = sum.unsized || s.unsized
	}
	return sum, nil
}

func openV3Reader(ctx context.Context, dir string, view RebuildView, budget int64, source func(func(*graph.Node) error) error) (*v3Reader, error) {
	pieces, chargeErrors, err := buildV3File(ctx, dir, view, source)
	if err != nil {
		return nil, err
	}
	table, err := openLinkTable(ctx, filepath.Join(dir, "v3.sst"))
	if err != nil {
		return nil, err
	}
	v := &v3Reader{table: table, cache: newV3Cache(budget, pieces), buildView: view.build, chargeErrors: chargeErrors}
	if view.build != nil {
		view.build.mu.Lock()
		v.buildEpoch = view.build.v3NodeWrites
		view.build.mu.Unlock()
	}
	return v, nil
}
