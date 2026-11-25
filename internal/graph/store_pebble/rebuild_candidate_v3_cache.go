package store_pebble

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"math"
	"slices"
	"strings"
	"unsafe"

	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
)

// A row is borrowed read-only. Structural fingerprints cover decoded values
// even when codec normalization is not byte-stable.
type v3CachedRow struct {
	node                                 *graph.Node
	rawBytes, orderBytes, canonicalBytes int
	ordinal                              uint64
	err                                  error // deferred H98 record/decode error; never cache admission
}

type v3Piece struct {
	rows      []v3CachedRow
	next      string
	charge    int64
	encoded   int
	unbounded bool
}

// The charge is for freshly decoded, acyclic values, not arbitrary Go maps
// (e.g. a large map with almost all its entries deleted) or borrowed substrings.
// Source: installed Go 1.27.3 arm64, internal/runtime/gc/sizeclasses.go,
// runtime/msize.go. Include the 8-byte allocation header even for unscanned
// objects above the 512-byte large-object header threshold. Round small objects
// conservatively by size class, large ones by the 8 KiB runtime page.
func v3Alloc(size int64) int64 {
	if size <= 0 {
		return 0
	}
	if size > math.MaxInt64-8200 {
		return math.MaxInt64
	}
	if size > 512 {
		size += 8
	}
	if size <= 256 {
		return (size + 15) &^ 15
	}
	if size <= 32768 {
		// Every class above 256 is <= 1.25x the preceding one.
		return size + size/4 + 16
	}
	return (size + 8191) &^ 8191
}

func v3Add(a, b int64) int64 {
	if a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}

func v3Array(count int, elem int64) int64 {
	if int64(count) > math.MaxInt64/elem {
		return math.MaxInt64
	}
	return v3Alloc(int64(count) * elem)
}

// Go 1.27.3 internal/runtime/maps/{map,table,group}.go: a small map has one
// 8-slot Swiss group. With more entries, one table has 2^k slots, >= 8*n/7,
// up to 1024. Compiler reflectdata/map.go: inline 16-byte string keys, 16-byte
// interface or 8-byte int elements, and an 8-byte control word per eight slots.
// Map header = 48B, table = 32B, directory of one pointer. There are no deleted
// slots here; decodeMetaFast creates fresh maps, encoding/json inserts or
// replaces keys, normalizeMetaJSONNumbers replaces values.
// Above 896 elements, however, hash-skewed table splits can repeatedly double
// the directory. No small, deterministic linear bound follows from map length.
// Treat such values as unchargeable for decoded retention, never as a semantic
// refusal. Pre-decode allocation of them is a separate H98 decoder issue.
const v3SingleTableEntries = 896

func v3MapStorage(n int, elem int64) int64 {
	if n > v3SingleTableEntries {
		return math.MaxInt64
	}
	charge := v3Alloc(48)
	if n == 0 {
		return charge
	}
	group := 8 + 8*(16+elem)
	if n <= 8 {
		return charge + v3Alloc(group)
	}
	slots := int64(16)
	for slots*7/8 < int64(n) {
		slots *= 2
	}
	return charge + v3Alloc(32) + v3Alloc(8) + v3Alloc(slots/8*group)
}

func v3MetaMapCharge(m map[string]any) int64 {
	if m == nil {
		return 0
	}
	charge := v3MapStorage(len(m), 16)
	if charge == math.MaxInt64 {
		return charge
	}
	for key, value := range m {
		charge = v3Add(charge, v3Alloc(int64(len(key))))
		charge = v3Add(charge, v3MetaValueCharge(value))
	}
	return charge
}

// Every closed native tag (metadata.go) and every result of JSON number/array
// normalization is covered. Interface slots themselves live in their enclosing
// allocation. Scalar, string and slice boxes are charged separately even when
// runtime/iface.go can share a static zero/small-int object. Shape is a pointer
// with no box. Container storage includes capacity, not just length.
func v3MetaValueCharge(value any) int64 {
	switch v := value.(type) {
	case nil:
		return 0
	case bool, int, int64, uint64, float64:
		return v3Alloc(8)
	case string:
		return v3Add(v3Alloc(16), v3Alloc(int64(len(v))))
	case []int:
		return v3Add(v3Alloc(24), v3Array(cap(v), 8))
	case []int64:
		return v3Add(v3Alloc(24), v3Array(cap(v), 8))
	case []uint64:
		return v3Add(v3Alloc(24), v3Array(cap(v), 8))
	case []float64:
		return v3Add(v3Alloc(24), v3Array(cap(v), 8))
	case []bool:
		return v3Add(v3Alloc(24), v3Array(cap(v), 1))
	case []string:
		charge := v3Add(v3Alloc(24), v3Array(cap(v), 16))
		for _, s := range v {
			charge = v3Add(charge, v3Alloc(int64(len(s))))
		}
		return charge
	case map[string]int:
		if v == nil {
			return 0
		}
		charge := v3MapStorage(len(v), 8)
		for key := range v {
			charge = v3Add(charge, v3Alloc(int64(len(key))))
		}
		return charge
	case map[string]any:
		return v3MetaMapCharge(v)
	case []map[string]any:
		charge := v3Add(v3Alloc(24), v3Array(cap(v), 8))
		for _, m := range v {
			charge = v3Add(charge, v3MetaMapCharge(m))
		}
		return charge
	case []any:
		charge := v3Add(v3Alloc(24), v3Array(cap(v), 16))
		for _, item := range v {
			charge = v3Add(charge, v3MetaValueCharge(item))
		}
		return charge
	case *contracts.Shape:
		if v == nil {
			return 0
		}
		charge := v3Alloc(int64(unsafe.Sizeof(*v)))
		charge = v3Add(charge, v3Alloc(int64(len(v.Kind))))
		charge = v3Add(charge, v3Array(cap(v.Fields), int64(unsafe.Sizeof(contracts.ShapeField{}))))
		for _, field := range v.Fields {
			for _, s := range [...]string{field.Name, field.Type, field.JSONTag, field.Comment} {
				charge = v3Add(charge, v3Alloc(int64(len(s))))
			}
		}
		charge = v3Add(charge, v3Array(cap(v.Notes), 16))
		for _, s := range v.Notes {
			charge = v3Add(charge, v3Alloc(int64(len(s))))
		}
		return charge
	default:
		return math.MaxInt64 // not a decoded closed tag; do not retain
	}
}

func v3NodeCharge(n *graph.Node) int64 {
	if n == nil {
		return 0
	}
	charge := v3Alloc(int64(unsafe.Sizeof(*n)))
	for _, s := range [...]string{n.ID, string(n.Kind), n.Name, n.QualName, n.FilePath, n.Language, n.RepoPrefix, n.WorkspaceID, n.ProjectID, n.AbsoluteFilePath, n.Origin} {
		charge = v3Add(charge, v3Alloc(int64(len(s))))
	}
	if !n.FetchedAt.IsZero() {
		// time.UnmarshalBinary may create a Location and single-element zone
		// and zoneTrans arrays through FixedZone. Shared UTC/Local are global.
		charge = v3Add(charge, 256)
	}
	return v3Add(charge, v3MetaMapCharge(n.Meta))
}

// The fingerprint deliberately does not encodeNode again. A final H98 order
// value can decode to a value that is not byte-stable (or not encodable) after
// JSON numeric normalization. Hash the delivered type and contents directly.
type v3Fingerprinter struct {
	h    hash.Hash
	word [8]byte
}

func (f *v3Fingerprinter) uint(n uint64) {
	binary.LittleEndian.PutUint64(f.word[:], n)
	_, _ = f.h.Write(f.word[:])
}

func (f *v3Fingerprinter) boolean(b bool) {
	if b {
		f.uint(1)
	} else {
		f.uint(0)
	}
}

func (f *v3Fingerprinter) str(s string) {
	f.uint(uint64(len(s)))
	_, _ = f.h.Write([]byte(s))
}

func (f *v3Fingerprinter) slice(length, capacity int, nilSlice bool) {
	f.uint(uint64(length))
	f.uint(uint64(capacity))
	f.boolean(nilSlice)
}

func (f *v3Fingerprinter) meta(m map[string]any, depth int) error {
	f.boolean(m == nil)
	f.uint(uint64(len(m)))
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		f.str(key)
		if err := f.value(m[key], depth+1); err != nil {
			return err
		}
	}
	return nil
}

func (f *v3Fingerprinter) value(value any, depth int) error {
	// Valid H98 native nesting <=63 plus encoding/json nesting <=10000.
	// A visitor that inserts a cycle must fail rather than recurse forever.
	if depth > 10100 {
		return errors.New("v3 cache fingerprint: excessive decoded nesting")
	}
	switch v := value.(type) {
	case nil:
		f.uint(metaTagNil)
	case bool:
		f.uint(metaTagBool)
		f.boolean(v)
	case string:
		f.uint(metaTagString)
		f.str(v)
	case int:
		f.uint(metaTagInt)
		f.uint(uint64(v))
	case int64:
		f.uint(metaTagInt64)
		f.uint(uint64(v))
	case uint64:
		f.uint(metaTagUint64)
		f.uint(v)
	case float64:
		f.uint(metaTagFloat64)
		f.uint(math.Float64bits(v))
	case []int:
		f.uint(metaTagIntSlice)
		f.slice(len(v), cap(v), v == nil)
		for _, n := range v {
			f.uint(uint64(n))
		}
	case []int64:
		f.uint(metaTagI64Slice)
		f.slice(len(v), cap(v), v == nil)
		for _, n := range v {
			f.uint(uint64(n))
		}
	case []uint64:
		f.uint(metaTagU64Slice)
		f.slice(len(v), cap(v), v == nil)
		for _, n := range v {
			f.uint(n)
		}
	case []float64:
		f.uint(metaTagF64Slice)
		f.slice(len(v), cap(v), v == nil)
		for _, n := range v {
			f.uint(math.Float64bits(n))
		}
	case []bool:
		f.uint(metaTagBools)
		f.slice(len(v), cap(v), v == nil)
		for _, b := range v {
			f.boolean(b)
		}
	case []string:
		f.uint(metaTagStrSlice)
		f.slice(len(v), cap(v), v == nil)
		for _, s := range v {
			f.str(s)
		}
	case map[string]int:
		f.uint(metaTagIntMap)
		f.boolean(v == nil)
		f.uint(uint64(len(v)))
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			f.str(k)
			f.uint(uint64(v[k]))
		}
	case map[string]any:
		f.uint(metaTagMap)
		return f.meta(v, depth)
	case []map[string]any:
		f.uint(metaTagMapSlice)
		f.slice(len(v), cap(v), v == nil)
		for _, m := range v {
			if err := f.meta(m, depth); err != nil {
				return err
			}
		}
	case []any:
		f.uint(metaTagAnySlice)
		f.slice(len(v), cap(v), v == nil)
		for _, item := range v {
			if err := f.value(item, depth+1); err != nil {
				return err
			}
		}
	case *contracts.Shape:
		f.uint(metaTagShape)
		f.boolean(v == nil)
		if v == nil {
			return nil
		}
		f.str(v.Kind)
		f.slice(len(v.Fields), cap(v.Fields), v.Fields == nil)
		for _, field := range v.Fields {
			f.str(field.Name)
			f.str(field.Type)
			f.str(field.JSONTag)
			f.str(field.Comment)
			f.boolean(field.Required)
			f.boolean(field.Repeated)
		}
		f.slice(len(v.Notes), cap(v.Notes), v.Notes == nil)
		for _, note := range v.Notes {
			f.str(note)
		}
	default:
		return fmt.Errorf("v3 cache fingerprint: unexpected decoded Meta type %T", value)
	}
	return nil
}

func v3RowFingerprint(row v3CachedRow) ([32]byte, error) {
	f := v3Fingerprinter{h: sha256.New()}
	var digest [32]byte
	f.uint(uint64(row.rawBytes))
	f.uint(uint64(row.orderBytes))
	f.uint(uint64(row.canonicalBytes))
	f.uint(row.ordinal)
	f.boolean(row.err != nil)
	if row.err != nil {
		f.str(row.err.Error())
	}
	n := row.node
	f.boolean(n != nil)
	if n != nil {
		for _, s := range [...]string{n.ID, string(n.Kind), n.Name, n.QualName, n.FilePath, n.Language, n.RepoPrefix, n.WorkspaceID, n.ProjectID, n.AbsoluteFilePath, n.Origin} {
			f.str(s)
		}
		for _, i := range [...]int{n.StartLine, n.EndLine, n.StartColumn, n.EndColumn} {
			f.uint(uint64(i))
		}
		f.boolean(n.Stub)
		f.uint(uint64(n.FetchedAt.Unix()))
		f.uint(uint64(n.FetchedAt.Nanosecond()))
		zone, off := n.FetchedAt.Zone()
		f.str(zone)
		f.uint(uint64(off))
		f.str(n.FetchedAt.Location().String())
		if err := f.meta(n.Meta, 0); err != nil {
			return digest, err
		}
	}
	copy(digest[:], f.h.Sum(digest[:0]))
	return digest, nil
}

// Preflight the encoder-produced raw record without constructing decoded maps,
// slices or strings. If it fails the reader must isolate the row and let the
// normal H98 codec report the error at its original stage. This check is about
// allocator exposure, not candidate admission.
func v3UnboundedRecord(raw []byte) (bool, error) {
	if len(raw) == 0 || raw[0] != nodeFormat {
		return true, invalid("node format")
	}
	d := recordCursor{buf: raw[1:]}
	for range 5 {
		_ = d.bytes()
	}
	for range 4 {
		_ = d.integer()
	}
	for range 6 {
		_ = d.bytes()
	}
	_ = d.boolean()
	_ = d.bytes() // timestamp has a fixed-size decoder, no variable container
	var meta []byte
	if d.boolean() {
		meta = d.bytes()
		if len(meta) == 0 {
			return true, invalid("metadata revision")
		}
	}
	if err := d.finish(); err != nil {
		return true, err
	}
	if meta == nil {
		return false, nil
	}
	if !isFlatMeta(meta) {
		return true, invalid("metadata revision")
	}
	scan := v3MetaScan{metaDecoder: metaDecoder{buf: meta[2:]}}
	if err := scan.scanMap(0); err != nil {
		return true, err
	}
	if scan.pos != len(scan.buf) {
		return true, invalid("unbound metadata bytes")
	}
	return scan.unbounded, nil
}

type v3MetaScan struct {
	metaDecoder
	unbounded bool
}

func (d *v3MetaScan) rawString() ([]byte, error) {
	n, err := d.uvarint()
	if err != nil {
		return nil, err
	}
	if n > uint64(len(d.buf)-d.pos) {
		return nil, errMetaTruncated
	}
	return d.readBytes(int(n))
}

func (d *v3MetaScan) scanMap(depth int) error {
	n, err := d.readCount()
	if err != nil {
		return err
	}
	d.unbounded = d.unbounded || n > v3SingleTableEntries
	var previous []byte
	for i := 0; i < n; i++ {
		key, err := d.rawString()
		if err != nil {
			return err
		}
		if i > 0 && bytes.Compare(key, previous) <= 0 {
			return invalid("metadata keys not unique and sorted")
		}
		previous = key
		if err := d.scanValue(depth + 1); err != nil {
			return err
		}
	}
	return nil
}

func (d *v3MetaScan) scanValue(depth int) error {
	tag, err := d.readByte()
	if err != nil {
		return err
	}
	if depth > metaMaxNativeDepth && tag != metaTagJSON {
		return invalid("metadata nesting exceeds native depth")
	}
	switch tag {
	case metaTagNil:
		return nil
	case metaTagString:
		_, err = d.rawString()
		return err
	case metaTagBool:
		b, err := d.readByte()
		if err != nil {
			return err
		}
		if b > 1 {
			return invalid("metadata boolean flag")
		}
		return nil
	case metaTagInt, metaTagInt64:
		_, err = d.varint()
		return err
	case metaTagUint64:
		_, err = d.uvarint()
		return err
	case metaTagFloat64:
		_, err = d.readBytes(8)
		return err
	case metaTagMap:
		return d.scanMap(depth + 1)
	case metaTagJSON, metaTagShape:
		raw, err := d.rawString()
		if err != nil {
			return err
		}
		large, err := v3LargeJSONObject(raw)
		d.unbounded = d.unbounded || large
		return err
	case metaTagStrSlice, metaTagMapSlice, metaTagAnySlice, metaTagIntSlice, metaTagI64Slice, metaTagU64Slice, metaTagF64Slice, metaTagBools, metaTagIntMap:
		n, err := d.readCount()
		if err != nil {
			return err
		}
		if tag == metaTagIntMap {
			d.unbounded = d.unbounded || n > v3SingleTableEntries
		}
		var previous []byte
		for i := 0; i < n; i++ {
			switch tag {
			case metaTagStrSlice:
				_, err = d.rawString()
			case metaTagMapSlice:
				err = d.scanMap(depth + 1)
			case metaTagAnySlice:
				err = d.scanValue(depth + 1)
			case metaTagIntSlice, metaTagI64Slice:
				_, err = d.varint()
			case metaTagU64Slice:
				_, err = d.uvarint()
			case metaTagF64Slice:
				_, err = d.readBytes(8)
			case metaTagBools:
				var b byte
				b, err = d.readByte()
				if err == nil && b > 1 {
					return invalid("metadata boolean flag")
				}
			case metaTagIntMap:
				var key []byte
				key, err = d.rawString()
				if err == nil {
					if i > 0 && bytes.Compare(key, previous) <= 0 {
						return invalid("metadata keys not unique and sorted")
					}
					previous = key
					_, err = d.varint()
				}
			}
			if err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("store_pebble: unknown meta value tag 0x%02x", tag)
	}
}

// json.Valid uses encoding/json's bounded nesting scanner (10000 in Go 1.27.3;
// JSONv2 is enabled by default in internal/buildcfg/exp.go).
// Given valid JSON, every unquoted colon starts one object member at its current
// depth, and no array value contains an unquoted colon. Count members without
// allocating any JSON maps or copying strings. Typed Shape also uses JSON.
func v3LargeJSONObject(raw []byte) (bool, error) {
	if !json.Valid(raw) {
		return true, invalid("metadata JSON")
	}
	var counts [1001]uint16
	depth := 0
	quoted := false
	for i := 0; i < len(raw); i++ {
		b := raw[i]
		if quoted {
			if b == '\\' {
				i++
			} else if b == '"' {
				quoted = false
			}
			continue
		}
		switch b {
		case '"':
			quoted = true
		case '{', '[':
			depth++
			// An unstable row may require encodeNode between the two decodes.
			// JSONv2/arshal_default.go starts growing a seen-pointers map
			// past depth 1000. It deletes slots, so arity alone cannot bound it.
			if depth > 1000 {
				return true, nil
			}
			counts[depth] = 0
		case '}', ']':
			depth--
		case ':':
			counts[depth]++
			if counts[depth] > v3SingleTableEntries {
				return true, nil
			}
		}
	}
	return false, nil
}

func v3Fingerprint(n *graph.Node) ([32]byte, error) { return v3RowFingerprint(v3CachedRow{node: n}) }

const (
	v3CachePieceBytes = 64 << 10
	v3Expansion       = 512
	v3DecoderScratch  = 256 << 10
)

// Source accounting for a structurally preflighted record (Go 1.27.3 arm64):
//   - codec.go: the Node and timestamp are fixed; each string has its own
//     length in the input. A minimum node record uses 19 bytes before framing.
//   - metadata.go: nil consumes a tag; bool, int, int64, uint64 and float64
//     consume a tag plus >=1 byte and have at most an 8B scalar box. Strings
//     have their own length, copy and 16B box. Int/I64/U64/F64 slices each
//     have 8B elements, bool slices 1B, string/any/map slices 16/16/8B,
//     all with a 24B box; count and per-element input are validated by
//     preflight before make. Native maps have a count and each entry has
//     at least a key-length byte and a value-tag byte (or signed varint).
//     The worst small map is one nonempty entry: <=394B table+header for
//     at least three bytes; larger one-table maps cost less per entry.
//   - JSON: default JSONv2 v1 compatibility (v2_stream.go:38-90) buffers
//     the input and decodes it via v2/arshal_default.go. Generic objects
//     grow one Swiss table and arrays use growslice; object syntax needs
//     >=4 bytes per member, arrays >=1 per element. Token/name stacks in
//     jsontext/state.go grow with syntax depth. v2/intern.go uses a fixed
//     4KiB string cache, copying strings (up to 256 bytes per intern slot).
//     v2 jsonopts default allows duplicate names, so namespace tracking is
//     not needed; native map values are fresh. Shape fields are fixed-size
//     structs with four strings and two bools, plus a slice of notes. No
//     unmarshaler is defined. JSON number normalization replaces each
//     box and can allocate one extra string/map slice per array; it does
//     not add elements. Original v1 uses the same output types and bounds.
//   - runtime/slice.go grows capacities geometrically; map/table.go grows
//     by powers of two within one table. Rounded capacities, boxes and
//     copies are charged conservatively (v3Alloc). Fingerprint scratch is
//     a hash and sorted string headers per map entry, plus one string copy.
//
// The 512x reserve deliberately exceeds the native/JSON/Shape decoded object
// charges, also accounting for one H98 unstable-row second decode and the
// intervening encoding/sorting buffers. Fixed scratch covers small JSON
// decoder/options/string-cache allocations. It is not a process heap limit:
// GC lag, goroutine stacks, allocator arenas, shared runtime caches, Pebble
// iterator internals and exceptional singletons are outside this counter.
//
// Reservation is additional to the two retained generations. See preflight:
// maps that could split a Swiss table and malformed input are isolated by the
// reader and not covered by the normal decoder envelope. These, and oversize
// singleton rows, keep H98's original transient decoder exposure. The number
// is a charge for live objects/scratch, not a guarantee about GC lag or RSS.
func v3ReserveBytes(encoded int) int64 {
	if encoded < 0 || int64(encoded) > (math.MaxInt64-v3DecoderScratch)/v3Expansion {
		return math.MaxInt64
	}
	return int64(encoded)*v3Expansion + v3DecoderScratch
}

type v3CacheEntry struct {
	piece           *v3Piece
	key             string
	nextKey         string
	fingerprints    [][32]byte
	id              uint64
	charge          int64
	encoded, rowCap int
	unbounded       bool
	prev, next      *v3CacheEntry
	gen             int
}

type v3CacheGeneration struct {
	head   *v3CacheEntry
	charge int64
}

func (g *v3CacheGeneration) insert(e *v3CacheEntry, gen int) {
	e.gen = gen
	e.prev = nil
	e.next = g.head
	if g.head != nil {
		g.head.prev = e
	}
	g.head = e
	g.charge += e.charge
}

func (g *v3CacheGeneration) remove(e *v3CacheEntry) {
	if e.prev == nil {
		g.head = e.next
	} else {
		e.prev.next = e.next
	}
	if e.next != nil {
		e.next.prev = e.prev
	}
	e.prev, e.next = nil, nil
	g.charge -= e.charge
}

// Serial only. Two capped generations share the budget remaining after the
// fixed state and piece lookup index. Collisions in a bounded index stream
// through the same loader; there is no history of unloaded or evicted IDs.
const rebuildCandidateCacheBytes = 512 << 20

type v3Cache struct {
	index      []*v3CacheEntry
	pieceCount uint64
	indexBytes int64
	gens       [2]v3CacheGeneration
	current    int
	half       int64
	borrowed   *v3CacheEntry // unretained result of the last get, checked on release
	closed     bool
}

func newV3Cache(budget int64, pieceCount uint64) *v3Cache {
	c := &v3Cache{pieceCount: pieceCount}
	available := max(0, budget-v3Alloc(int64(unsafe.Sizeof(*c))))
	// Leave room for entries even if the file would require a very large
	// dense index. On normal files use one pointer per piece.
	const pointerBytes = int64(unsafe.Sizeof((*v3CacheEntry)(nil)))
	slots := min(pieceCount, uint64(available/pointerBytes))
	if slots != pieceCount || v3Array(int(slots), pointerBytes) > available/2 {
		slots = min(pieceCount, uint64(available/(4*pointerBytes)))
		for slots != 0 && v3Array(int(slots), pointerBytes) > available/2 {
			slots /= 2
		}
	}
	c.indexBytes = v3Array(int(slots), pointerBytes)
	c.index = make([]*v3CacheEntry, int(slots))
	c.half = (available - c.indexBytes) / 2
	return c
}

func (c *v3Cache) slot(id uint64) *v3CacheEntry {
	if len(c.index) == 0 {
		return nil
	}
	return c.index[id%uint64(len(c.index))]
}

func v3PieceEntry(key string, id uint64, piece *v3Piece) (*v3CacheEntry, bool, error) {
	// Own exact-size backing for these keys, even if a reader passed a slice of
	// a larger string. Account all storage reachable from the retained piece.
	piece.next = strings.Clone(piece.next)
	e := &v3CacheEntry{piece: piece, key: strings.Clone(key), nextKey: piece.next, id: id, encoded: piece.encoded, unbounded: piece.unbounded, rowCap: cap(piece.rows), fingerprints: make([][32]byte, len(piece.rows))}
	charge := v3Alloc(int64(unsafe.Sizeof(*e)))
	charge = v3Add(charge, v3Alloc(int64(unsafe.Sizeof(*piece))))
	charge = v3Add(charge, v3Alloc(int64(len(key))))
	charge = v3Add(charge, v3Alloc(int64(len(piece.next))))
	charge = v3Add(charge, v3Array(cap(piece.rows), int64(unsafe.Sizeof(v3CachedRow{}))))
	charge = v3Add(charge, v3Array(cap(e.fingerprints), 32))
	deferred := false
	for i, row := range piece.rows {
		var err error
		e.fingerprints[i], err = v3RowFingerprint(row)
		if err != nil {
			return nil, false, err
		}
		charge = v3Add(charge, v3NodeCharge(row.node))
		deferred = deferred || row.err != nil
	}
	if deferred {
		charge = math.MaxInt64
	} // Original error object can own arbitrary detail; never retain.
	e.charge, piece.charge = charge, charge
	return e, deferred, nil
}

func (e *v3CacheEntry) verify() error {
	p := e.piece
	if p.encoded != e.encoded || p.next != e.nextKey || p.unbounded != e.unbounded || p.charge != e.charge || len(p.rows) != len(e.fingerprints) || cap(p.rows) != e.rowCap {
		return fmt.Errorf("v3 cache piece %d mutated", e.id)
	}
	for i, row := range p.rows {
		digest, err := v3RowFingerprint(row)
		if err != nil {
			return fmt.Errorf("v3 cache piece %d row %d: %w", e.id, i, err)
		}
		if digest != e.fingerprints[i] {
			return fmt.Errorf("v3 cache piece %d row %d mutated", e.id, i)
		}
	}
	return nil
}

func (c *v3Cache) releaseBorrowed() error {
	if c.borrowed == nil {
		return nil
	}
	e := c.borrowed
	c.borrowed = nil
	return e.verify()
}

func (c *v3Cache) drop(gen int) error {
	var err error
	g := &c.gens[gen]
	for e := g.head; e != nil; e = g.head {
		err = errors.Join(err, e.verify())
		g.remove(e)
		c.index[e.id%uint64(len(c.index))] = nil
	}
	return err
}

func (c *v3Cache) get(key string, pieceID uint64, load func(reserve func(encoded int, unbounded bool) error) (*v3Piece, error)) (*v3Piece, error) {
	if c.closed || pieceID >= c.pieceCount {
		return nil, errors.New("v3 cache closed or piece ID out of range")
	}
	if err := c.releaseBorrowed(); err != nil {
		return nil, err
	}
	entry := c.slot(pieceID)
	if entry != nil && entry.id == pieceID {
		if key != entry.key {
			return nil, fmt.Errorf("v3 cache piece %d has different key", pieceID)
		}
		// Preserve a hot old-generation entry when the current generation has
		// room, without duplicating it or evicting anything on a hit.
		if entry.gen != c.current && entry.charge <= c.half-c.gens[c.current].charge {
			c.gens[entry.gen].remove(entry)
			c.gens[c.current].insert(entry, c.current)
		}
		return entry.piece, nil
	}
	var reserved int64
	var pieceBytes int
	var unbounded, called bool
	var reserveErr error
	piece, err := load(func(encoded int, exceptional bool) error {
		if called || encoded <= 0 {
			reserveErr = errors.New("v3 cache load must reserve exactly once for positive encoded length")
			return reserveErr
		}
		called = true
		reserved = v3ReserveBytes(encoded)
		pieceBytes, unbounded = encoded, exceptional
		if reserved == math.MaxInt64 {
			reserveErr = errors.New("v3 cache reserve overflow")
			return reserveErr
		}

		return nil
	})
	if err != nil {
		return nil, err
	}
	if reserveErr != nil {
		return nil, reserveErr
	}
	if !called || piece == nil || piece.encoded != pieceBytes || piece.unbounded != unbounded {
		return nil, errors.New("v3 cache load did not match its reservation")
	}
	rowBytes := pieceBytes - len(piece.next)
	if rowBytes <= 0 {
		return nil, errors.New("v3 cache piece has no encoded rows")
	}
	if (unbounded || rowBytes > v3CachePieceBytes) && len(piece.rows) != 1 {
		return nil, errors.New("v3 cache exceptional piece is not a singleton")
	}
	entry, deferred, err := v3PieceEntry(key, pieceID, piece)
	if err != nil {
		return nil, err
	}
	if !unbounded && !deferred && entry.charge > reserved {
		return nil, fmt.Errorf("v3 cache piece %d exceeded its decode reserve: %d > %d", pieceID, entry.charge, reserved)
	}
	if len(c.index) == 0 || c.slot(pieceID) != nil || unbounded || deferred || rowBytes > v3CachePieceBytes || entry.charge > c.half {
		// No additional candidate size refusal. Same decoded reader, one
		// borrowed piece, verified even though it never enters a generation.
		c.borrowed = entry
		return piece, nil
	}
	if entry.charge > c.half-c.gens[c.current].charge {
		old := 1 - c.current
		if err := c.drop(old); err != nil {
			return nil, err
		}
		c.current = old
	}
	c.gens[c.current].insert(entry, c.current)
	c.index[pieceID%uint64(len(c.index))] = entry
	return piece, nil
}

func (c *v3Cache) close() error {
	if c.closed {
		return nil
	}
	c.closed = true
	err := errors.Join(c.releaseBorrowed(), c.drop(0), c.drop(1))
	c.index = nil
	c.indexBytes = 0
	return err
}
