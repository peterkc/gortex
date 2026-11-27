package store_pebble

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"

	"github.com/cockroachdb/pebble/v2"
	"github.com/cockroachdb/pebble/v2/sstable"
	"github.com/zzet/gortex/internal/graph"
)

var (
	ErrRebuildReceiverMutation       = errors.New("pebble rebuild receiver mutation failed")
	ErrReceiverMutationNonconvergent = errors.New("receiver mutation did not converge within its round bound")
	ErrReceiverMutationInput         = errors.New("receiver mutation input outside the exact projected domain")
)

const receiverRunRowBytes = 64 << 10

// ReceiverMutationRules supplies the native indexer's owner and call rules.
// RoundLimit is zero in production. A lower value plants a refusal on progress.
type ReceiverMutationRules struct {
	OwnerKey        func(id, receiver string) string
	BareCallName    func(target string) string
	IsStdlibMutator func(name string) bool
	RoundLimit      uint64
}

type ReceiverMutationStats struct {
	Facts, Rounds, RoundBound, Emitted, SpillBytes uint64
	Matches                                        uint64 // peak call/field matches per sweep, before masking or deduplication
}

// VisitReceiverMutations yields collapsed indirect capability rows in native
// first-claim site order, breaking within-site field ties by field ID. It does not change the working graph. The caller must keep
// the view stable and write only to a tentative consumer, discarding it on
// any error (including a failed cleanup after the last callback). The full
// capability reducer must give a direct row priority at a shared identity.
func (v RebuildView) VisitReceiverMutations(ctx context.Context, scratchParent string, spillAt int,
	rules ReceiverMutationRules, yield func(*graph.Edge) error,
) (ReceiverMutationStats, error) {
	var emit func(*graph.Edge, string) error
	if yield != nil {
		emit = func(e *graph.Edge, _ string) error { return yield(e) }
	}
	return v.visitReceiverMutations(ctx, scratchParent, spillAt, rules, nil, emit)
}

func (v RebuildView) visitReceiverMutations(ctx context.Context, scratchParent string, spillAt int,
	rules ReceiverMutationRules, masks func(*graph.Edge) (bool, error), yield func(*graph.Edge, string) error,
) (stats ReceiverMutationStats, err error) {
	defer func() {
		if err != nil {
			err = wrapReceiverMutationError(err, stats.Matches)
			stats = ReceiverMutationStats{}
		}
	}()
	if v.build == nil || scratchParent == "" || spillAt < 1 || spillAt > goldBufferBytes ||
		rules.OwnerKey == nil || rules.BareCallName == nil || rules.IsStdlibMutator == nil || yield == nil {
		return stats, errors.New("invalid receiver mutation inputs")
	}
	if err := ctx.Err(); err != nil {
		return stats, err
	}
	dir, err := os.MkdirTemp(scratchParent, "receiver-")
	if err != nil {
		return stats, err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(dir)) }()
	pass := receiverPass{dir: dir, limit: spillAt, options: headOptions(), stats: &stats}
	owners, err := pass.owners(ctx, v, rules)
	if err != nil {
		return stats, err
	}
	calls, facts, err := pass.inputs(ctx, v, rules, owners)
	if err != nil {
		return stats, err
	}
	bound := stats.RoundBound
	if rules.RoundLimit != 0 {
		bound = min(bound, rules.RoundLimit)
	}
	// Both native ordered updates and these synchronous rounds start with the
	// same seeds and apply positive rules only. Each added fact belongs to
	// every closed superset of the seeds. A round with no new fact is closed,
	// so both schedules reach the same least fixed point, even through cycles.
	for stats.Rounds < bound {
		stats.Rounds++
		next, count, err := pass.round(ctx, facts, calls)
		if err != nil {
			return stats, err
		}
		if err := facts.remove(); err != nil {
			return stats, err
		}
		facts = next
		if count == stats.Facts {
			err = pass.emit(ctx, facts, calls, masks, yield)
			return stats, err
		}
		if count >= stats.RoundBound {
			return stats, ErrReceiverMutationNonconvergent
		}
		stats.Facts = count
	}
	return stats, ErrReceiverMutationNonconvergent
}

func wrapReceiverMutationError(err error, matches uint64) error {
	if _, ok := errors.AsType[*sortedRunLimitError](err); ok {
		err = fmt.Errorf("%w: receiver_matches=%d: %w", ErrReceiverMutationInput, matches, err)
	}
	return fmt.Errorf("%w: %w", ErrRebuildReceiverMutation, err)
}

type receiverPass struct {
	dir     string
	limit   int
	options *pebble.Options
	stats   *ReceiverMutationStats
}

func (p receiverPass) runs(name string) *sortedRuns {
	return &sortedRuns{dir: p.dir, name: name, options: p.options, format: sstable.TableFormatPebblev4, limit: p.limit}
}

func addReceiverRow(runs *sortedRuns, key, value []byte) error {
	if len(key)+len(value) > receiverRunRowBytes {
		return fmt.Errorf("%w: projected SST row exceeds %d bytes", ErrReceiverMutationInput, receiverRunRowBytes)
	}
	return runs.add(key, value)
}

// Number the proposals so sortedRuns can still reject accidental duplicate
// keys. Drop this suffix during reduction, not before the external sort.
func addReceiverProposal(runs *sortedRuns, key, value []byte, ordinal uint64) error {
	return addReceiverRow(runs, binary.BigEndian.AppendUint64(key, ordinal), value)
}

func (p receiverPass) merge(ctx context.Context, runs *sortedRuns, visit func(row) error) error {
	// Paths present before the final drain were written due to the spill limit.
	for _, path := range runs.paths {
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		if uint64(info.Size()) > math.MaxUint64-p.stats.SpillBytes {
			return fmt.Errorf("%w: spill byte counter overflow", ErrReceiverMutationInput)
		}
		p.stats.SpillBytes += uint64(info.Size())
	}
	// sortedRuns pins one index (and at most one data block) per cursor.
	// Charge the whole uncompressed index, including lower partitions.
	if err := runs.flush(); err != nil {
		return err
	}
	var indexes uint64
	for _, path := range runs.paths {
		t, err := openGoldTable(ctx, path, runs.options)
		if err != nil {
			return err
		}
		props, err := t.ReadPropertiesBlock(ctx, nil)
		if err := errors.Join(err, t.Close()); err != nil {
			return err
		}
		if props.IndexSize > receiverIndexBytes-indexes {
			return fmt.Errorf("%w: combined sorted run indexes exceed %d bytes", ErrReceiverMutationInput, receiverIndexBytes)
		}
		indexes += props.IndexSize
	}
	if err := runs.merge(ctx, visit); err != nil {
		return err
	}
	for _, path := range runs.paths {
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	return ctx.Err()
}

// The row-block writer buffers its index partitions until Close. Split the
// merged stream by charged bytes so neither the writer nor a lookup keeps
// indexes for an arbitrarily large compacted table open.
const receiverTableBytes = 16 << 20
const receiverIndexBytes = 128 << 20

type receiverPart struct {
	path string
	last []byte
}

type receiverTable []receiverPart

func (t receiverTable) remove() error {
	for _, part := range t {
		if err := os.Remove(part.path); err != nil {
			return err
		}
	}
	return nil
}

func (p receiverPass) table(ctx context.Context, runs *sortedRuns, ordinal bool) (parts receiverTable, count uint64, err error) {
	var w *sstable.Writer
	closePart := func() error {
		if w == nil {
			return nil
		}
		old := w
		w = nil
		return old.Close()
	}
	defer func() { err = errors.Join(err, closePart()) }()
	var last []byte
	var charged int
	err = p.merge(ctx, runs, func(r row) error {
		key := r.key
		if ordinal {
			key = key[:len(key)-8]
			if bytes.Equal(last, key) {
				return nil
			}
		}
		size := len(key) + len(r.value) + 48
		if w == nil || size > receiverTableBytes-charged {
			if err := closePart(); err != nil {
				return err
			}
			path := filepath.Join(p.dir, fmt.Sprintf("%s.part-%d.sst", runs.name, len(parts)))
			writer, err := newLinkTable(path)
			if err != nil {
				return err
			}
			w = writer
			parts = append(parts, receiverPart{path: path})
			charged = 0
		}
		count++
		charged += size
		part := &parts[len(parts)-1]
		part.last = append(part.last[:0], key...)
		last = part.last
		return w.Set(key, r.value)
	})
	return parts, count, err
}

type receiverCursor struct {
	parts receiverTable
	part  int
	table *sstable.Reader
	iter  sstable.Iterator
}

func withReceiverTable(t receiverTable, visit func(*receiverCursor) error) (err error) {
	c := receiverCursor{parts: t, part: -1}
	defer func() { err = errors.Join(err, c.close()) }()
	return visit(&c)
}

func (c *receiverCursor) close() error {
	if c.table == nil {
		return nil
	}
	var err error
	if c.iter != nil {
		err = c.iter.Close()
	}
	err = errors.Join(err, c.table.Close())
	c.table, c.iter, c.part = nil, nil, -1
	return err
}

func (c *receiverCursor) open(ctx context.Context, part int) error {
	if part == c.part {
		return nil
	}
	if err := c.close(); err != nil {
		return err
	}
	t, err := openLinkTable(ctx, c.parts[part].path)
	if err != nil {
		return err
	}
	c.table = t
	c.part = part
	c.iter, err = t.NewIter(sstable.NoTransforms, nil, nil, sstable.TableBlobContext{})
	return err
}

// visit may stop at the first match; callbacks must not reenter this cursor.
func (c *receiverCursor) visit(ctx context.Context, prefix []byte, yield func(row) (bool, error)) error {
	part := sort.Search(len(c.parts), func(i int) bool { return bytes.Compare(c.parts[i].last, prefix) >= 0 })
	for ; part < len(c.parts); part++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := c.open(ctx, part); err != nil {
			return err
		}
		i := c.iter
		for kv := i.SeekGE(prefix, 0); kv != nil; kv = i.Next() {
			if !bytes.HasPrefix(kv.K.UserKey, prefix) {
				return i.Error()
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			value, _, err := kv.Value(nil)
			if err != nil {
				return err
			}
			more, err := yield(row{kv.K.UserKey, value})
			if err != nil || !more {
				return errors.Join(err, i.Error())
			}
		}
		if err := i.Error(); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func (c *receiverCursor) all(ctx context.Context, prefix []byte, visit func(row) error) error {
	return c.visit(ctx, prefix, func(r row) (bool, error) { return true, visit(r) })
}

// Generic RebuildView visitors do not cap rows before decode. Apply the
// existing link-record byte limit at the raw visits AND exact endpoint probes.
func checkReceiverInput(key, value []byte) error {
	if len(key) > linkRecordBytes || len(value) > linkRecordBytes-len(key) {
		return fmt.Errorf("%w: saved row exceeds %d bytes", ErrReceiverMutationInput, linkRecordBytes)
	}
	return nil
}

func receiverInputNode(snap *pebble.Snapshot, id string) (*graph.Node, error) {
	key := pendingNodeKey(id)
	return pendingGet(snap, key, func(raw []byte) (*graph.Node, error) {
		if err := checkReceiverInput(key, raw); err != nil {
			return nil, err
		}
		n, err := decodeNode(raw)
		if err == nil && n.ID != id {
			return nil, invalid("receiver endpoint does not match key")
		}
		return n, err
	})
}

func receiverNodeOwner(n *graph.Node, rules ReceiverMutationRules) string {
	if n == nil {
		return ""
	}
	receiver, _ := n.Meta["receiver"].(string)
	return rules.OwnerKey(n.ID, receiver)
}

func (p receiverPass) owners(ctx context.Context, v RebuildView, rules ReceiverMutationRules) (receiverTable, error) {
	runs := p.runs("owners")
	if err := v.visit(ctx, []byte{familyNode}, func(_ *pebble.Snapshot, r row) error {
		if err := checkReceiverInput(r.key, r.value); err != nil {
			return err
		}
		n, err := decodeNode(r.value)
		if err != nil {
			return err
		}
		if !bytes.Equal(r.key, pendingNodeKey(n.ID)) {
			return invalid("receiver node does not match key")
		}
		if n.Kind != graph.KindMethod && n.Kind != graph.KindField {
			return nil
		}
		owner := receiverNodeOwner(n, rules)
		if owner == "" {
			return nil
		}
		prefix := appendKeyString(nil, owner)
		if n.Kind == graph.KindMethod {
			return addReceiverRow(runs, appendKeyString(append(prefix, 'm'), n.ID), nil)
		}
		if n.Name == "" {
			return nil
		}
		return addReceiverRow(runs, keyStrings(append(prefix, 'f'), n.Name, n.ID), nil)
	}); err != nil {
		return nil, err
	}
	path, _, err := p.table(ctx, runs, false)
	if err != nil {
		return nil, err
	}
	bound := uint64(1)
	var owner, lastField string
	var haveOwner, haveField bool
	var methods, fields uint64
	charge := func() error {
		if fields != 0 && methods > (math.MaxUint64-bound)/fields {
			return fmt.Errorf("%w: round bound overflow", ErrReceiverMutationNonconvergent)
		}
		bound += methods * fields
		return nil
	}
	err = withReceiverTable(path, func(i *receiverCursor) error {
		return i.all(ctx, nil, func(r row) error {
			key, rest, err := readKeyString(r.key)
			if err != nil || len(rest) == 0 {
				return errors.Join(err, invalid("invalid receiver owner key"))
			}
			if !haveOwner || owner != key {
				if err := charge(); err != nil {
					return err
				}
				owner, haveOwner, haveField, methods, fields = key, true, false, 0, 0
			}
			if rest[0] == 'm' {
				methods++
				return nil
			}
			field, _, err := readKeyString(rest[1:])
			if err != nil {
				return err
			}
			if !haveField || field != lastField {
				fields++
				lastField, haveField = field, true
			}
			return nil
		})
	})
	if err := errors.Join(err, charge()); err != nil {
		return nil, err
	}
	p.stats.RoundBound = bound
	return path, nil
}

func receiverFieldID(ctx context.Context, i *receiverCursor, owner, name string) (id string, err error) {
	prefix := appendKeyString(append(appendKeyString(nil, owner), 'f'), name)
	err = i.visit(ctx, prefix, func(r row) (bool, error) {
		var err error
		id, _, err = readKeyString(r.key[len(prefix):])
		return false, err
	})
	return id, err
}

type rebuildReceiverCall struct {
	from, to, file, via, fieldID string
	line                         int
	seed                         bool
}

func (c rebuildReceiverCall) row() ([]byte, []byte) {
	// This is receiverCallLess through the callee. The remaining field and
	// self tie-breakers cannot differ at one complete saved call identity.
	key := appendKeyString(appendKeyLine(keyStrings(nil, c.from, c.file), c.line), c.to)
	value := keyStrings(nil, c.fieldID, c.via)
	return key, appendBool(value, c.seed)
}

func decodeReceiverCall(r row) (c rebuildReceiverCall, err error) {
	c.from, r.key, err = readKeyString(r.key)
	if err == nil {
		c.file, r.key, err = readKeyString(r.key)
	}
	if err != nil || len(r.key) < 8 {
		return c, errors.Join(err, invalid("receiver call site"))
	}
	line := int64(binary.BigEndian.Uint64(r.key) ^ (1 << 63))
	c.line = int(line)
	c.to, r.key, err = readKeyString(r.key[8:])
	if err != nil || len(r.key) != 0 || int64(c.line) != line {
		return c, errors.Join(err, invalid("receiver call target"))
	}
	c.fieldID, r.value, err = readKeyString(r.value)
	if err == nil {
		c.via, r.value, err = readKeyString(r.value)
	}
	if err != nil || len(r.value) != 1 || r.value[0] > 1 {
		return c, errors.Join(err, invalid("receiver call value"))
	}
	c.seed = r.value[0] == 1
	return c, nil
}

func (p receiverPass) inputs(ctx context.Context, v RebuildView, rules ReceiverMutationRules, owners receiverTable) (receiverTable, receiverTable, error) {
	calls, seeds := p.runs("calls"), p.runs("facts-seeds")
	var ordinal uint64
	err := withReceiverTable(owners, func(fields *receiverCursor) error {
		return v.visit(ctx, []byte{familyEdge, edgeOut}, func(snap *pebble.Snapshot, r row) error {
			if err := checkReceiverInput(r.key, r.value); err != nil {
				return err
			}
			e, err := decodeEdge(r.value)
			if err != nil {
				return err
			}
			if !bytes.Equal(r.key, pendingEdgeKey(e)) {
				return invalid("receiver input edge does not match key")
			}
			field, _ := e.Meta["recv_field"].(string)
			self, _ := e.Meta["recv_self"].(bool)
			if e.Kind != graph.EdgeWrites && (e.Kind != graph.EdgeCalls || (field == "" && !self)) {
				return nil
			}
			from, err := receiverInputNode(snap, e.From)
			if err != nil || from == nil || from.Kind != graph.KindMethod {
				return err
			}
			owner := receiverNodeOwner(from, rules)
			if owner == "" {
				return nil
			}
			to, err := receiverInputNode(snap, e.To)
			if err != nil {
				return err
			}
			if e.Kind == graph.EdgeWrites {
				if to == nil || to.Kind != graph.KindField || to.Name == "" || owner != receiverNodeOwner(to, rules) {
					return nil
				}
				id, err := receiverFieldID(ctx, fields, owner, to.Name)
				if err != nil || id == "" {
					return errors.Join(err, invalid("receiver write field missing from owner census"))
				}
				ordinal++
				return addReceiverProposal(seeds, keyStrings(nil, e.From, id), nil, ordinal)
			}
			c := rebuildReceiverCall{from: e.From, to: e.To, file: e.FilePath, line: e.Line}
			if to != nil && to.Kind == graph.KindMethod {
				c.via = to.Name
			}
			if c.via == "" {
				c.via = rules.BareCallName(e.To)
			}
			if field != "" { // Native field precedence even when recv_self is set.
				c.fieldID, err = receiverFieldID(ctx, fields, owner, field)
				if err != nil || c.fieldID == "" {
					return err
				}
				c.seed = rules.IsStdlibMutator(c.via)
			} else if to == nil || to.Kind != graph.KindMethod || owner != receiverNodeOwner(to, rules) {
				return nil
			}
			key, value := c.row()
			return addReceiverRow(calls, key, value)
		})
	})
	if err != nil {
		return nil, nil, err
	}
	if err := owners.remove(); err != nil {
		return nil, nil, err
	}
	callPath, _, err := p.table(ctx, calls, false)
	if err != nil {
		return nil, nil, err
	}
	factPath, count, err := p.table(ctx, seeds, true)
	p.stats.Facts = count
	return callPath, factPath, err
}

func (p receiverPass) visitMatches(ctx context.Context, facts, calls receiverTable, visit func(rebuildReceiverCall, string, uint64) error) error {
	var count uint64
	match := func(call rebuildReceiverCall, fieldID string, site uint64) error {
		count++
		return visit(call, fieldID, site)
	}
	defer func() { p.stats.Matches = max(p.stats.Matches, count) }()
	return withReceiverTable(facts, func(f *receiverCursor) error {
		return withReceiverTable(calls, func(c *receiverCursor) error {
			var site uint64
			return c.all(ctx, nil, func(r row) error {
				call, err := decodeReceiverCall(r)
				if err != nil {
					return err
				}
				site++
				prefix := appendKeyString(nil, call.to)
				if call.fieldID != "" {
					if !call.seed {
						var found bool
						if err := f.visit(ctx, prefix, func(row) (bool, error) {
							found = true
							return false, nil
						}); err != nil || !found {
							return err
						}
					}
					return match(call, call.fieldID, site)
				}
				return f.all(ctx, prefix, func(r row) error {
					id, rest, err := readKeyString(r.key[len(prefix):])
					if err != nil || len(rest) != 0 || id == "" {
						return errors.Join(err, invalid("receiver mutation fact"))
					}
					return match(call, id, site)
				})
			})
		})
	})
}

func (p receiverPass) round(ctx context.Context, facts, calls receiverTable) (receiverTable, uint64, error) {
	runs := p.runs(fmt.Sprintf("facts-%d", p.stats.Rounds%2))
	var ordinal uint64
	add := func(key []byte) error {
		ordinal++
		return addReceiverProposal(runs, key, nil, ordinal)
	}
	if err := withReceiverTable(facts, func(i *receiverCursor) error {
		return i.all(ctx, nil, func(r row) error { return add(bytes.Clone(r.key)) })
	}); err != nil {
		return nil, 0, err
	}
	if err := p.visitMatches(ctx, facts, calls, func(c rebuildReceiverCall, fieldID string, _ uint64) error {
		return add(keyStrings(nil, c.from, fieldID))
	}); err != nil {
		return nil, 0, err
	}
	return p.table(ctx, runs, true)
}

func (p receiverPass) emit(ctx context.Context, facts, calls receiverTable,
	masks func(*graph.Edge) (bool, error), yield func(*graph.Edge, string) error,
) error {
	claims, identities := p.runs("claims"), p.runs("identities")
	if err := p.visitMatches(ctx, facts, calls, func(c rebuildReceiverCall, fieldID string, site uint64) error {
		e := graph.Edge{From: c.from, To: fieldID, Kind: graph.EdgeAccessesField, FilePath: c.file, Line: c.line,
			Origin: graph.OriginASTInferred, Meta: map[string]any{"access": "write", "indirect": true, "via": c.via}}
		if c.via == "" {
			// An empty via competes for the direct pair before site collapse.
			if masks == nil {
				return fmt.Errorf("%w: empty via requires the direct capability reducer", ErrReceiverMutationInput)
			}
			masked, err := masks(&e)
			if err != nil || masked {
				return err
			}
		}
		value, err := encodeEdge(&e)
		if err != nil {
			return err
		}
		// Use the projected scanner's exact claim key. The site suffix
		// keeps the first call in native order even when runs overlap.
		if masks != nil {
			value = append(appendKeyString(nil, c.to), value...)
		}
		return addReceiverProposal(claims, appendKeyString(nil, c.from+"\x00"+fieldID+"\x00"+c.via), value, site)
	}); err != nil {
		return err
	}
	decode := func(value []byte) (string, *graph.Edge, error) {
		var callee string
		if masks != nil {
			var err error
			callee, value, err = readKeyString(value)
			if err != nil {
				return "", nil, err
			}
		}
		e, err := decodeEdge(value)
		return callee, e, err
	}
	var last []byte
	if err := p.merge(ctx, claims, func(r row) error {
		key := r.key[:len(r.key)-8]
		if bytes.Equal(last, key) {
			return nil
		}
		last = append(last[:0], key...)
		_, e, err := decode(r.value)
		if err != nil {
			return err
		}
		return addReceiverRow(identities, append(pendingEdgeKey(e), r.key[len(key):]...), bytes.Clone(r.value))
	}); err != nil {
		return err
	}
	ordered := p.runs("emission")
	last = last[:0]
	var firstSite []byte
	var winner row
	var bestVia string
	drain := func() error {
		if winner.key == nil {
			return nil
		}
		return addReceiverRow(ordered, winner.key, winner.value)
	}
	if err := p.merge(ctx, identities, func(r row) error {
		_, e, err := decode(r.value)
		if err != nil {
			return err
		}
		key := r.key[:len(r.key)-8]
		if !bytes.Equal(last, key) {
			if err := drain(); err != nil {
				return err
			}
			last = append(last[:0], key...)
			firstSite = append(firstSite[:0], r.key[len(key):]...)
			winner = row{}
		}
		// Native collapse replaces the payload with the least via without
		// moving the identity from its first position in the output.
		via, _ := e.Meta["via"].(string)
		if winner.key == nil || via < bestVia {
			winner = row{appendKeyString(bytes.Clone(firstSite), e.To), bytes.Clone(r.value)}
			bestVia = via
		}
		return nil
	}); err != nil {
		return err
	}
	if err := drain(); err != nil {
		return err
	}
	return p.merge(ctx, ordered, func(r row) error {
		callee, e, err := decode(r.value)
		if err != nil {
			return err
		}
		if masks != nil {
			masked, err := masks(e)
			if err != nil || masked {
				return err
			}
		}
		if err := yield(e, callee); err != nil {
			return err
		}
		p.stats.Emitted++
		return nil
	})
}
