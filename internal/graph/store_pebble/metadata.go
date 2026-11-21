package store_pebble

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/contracts"
)

const (
	metaFlatMagic0     = 0x00
	metaFlatVersion    = 0x01
	metaMaxNativeDepth = 63
)

// Flat value type tags. The byte after a key selects the value layout.
const (
	metaTagNil      = 0x01 // (no payload)
	metaTagString   = 0x02 // uvarint len, len bytes
	metaTagBool     = 0x03 // 1 byte (0 / 1)
	metaTagInt      = 0x04 // zig-zag varint
	metaTagInt64    = 0x05 // zig-zag varint
	metaTagFloat64  = 0x06 // 8 bytes little-endian IEEE-754 bits
	metaTagStrSlice = 0x07 // uvarint count, then count strings
	metaTagMap      = 0x08 // a map body (recursive)
	metaTagMapSlice = 0x09 // uvarint count, then count map bodies
	metaTagAnySlice = 0x0A // uvarint count, then count tagged values
	metaTagShape    = 0x0B // uvarint len, len bytes of JSON-encoded *contracts.Shape
	metaTagUint64   = 0x0C // unsigned varint
	metaTagF64Slice = 0x0D // uvarint count, then count little-endian float64 values
	metaTagJSON     = 0x0E // JSON-compatible extension value
	metaTagIntSlice = 0x0F // uvarint count, then signed varints
	metaTagI64Slice = 0x10 // uvarint count, then signed varints
	metaTagU64Slice = 0x11 // uvarint count, then unsigned varints
	metaTagBools    = 0x12 // uvarint count, then boolean bytes
	metaTagIntMap   = 0x13 // sorted string keys and signed varints
)

var errMetaTruncated = errors.New("store_pebble: truncated meta blob")

// isFlatMeta reports whether b is a flat-codec blob.
func isFlatMeta(b []byte) bool {
	return len(b) >= 2 && b[0] == metaFlatMagic0 && b[1] == metaFlatVersion
}

// encodeMetaFast serialises m with the flat binary codec. It returns ok=false
// when a value cannot be represented, so no data is silently dropped.
func encodeMetaFast(m map[string]any) (b []byte, ok bool) {
	buf := make([]byte, 0, 32+len(m)*24)
	buf = append(buf, metaFlatMagic0, metaFlatVersion)
	buf, ok = appendMetaMap(buf, m, 0)
	if !ok {
		return nil, false
	}
	return buf, true
}

// appendMetaMap writes a map body (count + sorted entries). Sorting the keys
// makes the encoding deterministic.
func appendMetaMap(buf []byte, m map[string]any, depth int) ([]byte, bool) {
	buf = binary.AppendUvarint(buf, uint64(len(m)))
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		buf = appendMetaString(buf, k)
		var ok bool
		buf, ok = appendMetaValue(buf, m[k], depth+1)
		if !ok {
			return buf, false
		}
	}
	return buf, true
}

func appendMetaString(buf []byte, s string) []byte {
	buf = binary.AppendUvarint(buf, uint64(len(s)))
	return append(buf, s...)
}

// appendMetaValue keeps modeled producer types exact and encodes other
// JSON-compatible values as individual leaves, without widening their siblings.
func appendMetaValue(buf []byte, v any, depth int) ([]byte, bool) {
	if depth > metaMaxNativeDepth {
		return appendMetaJSON(buf, v)
	}
	switch vv := v.(type) {
	case nil:
		return append(buf, metaTagNil), true
	case string:
		buf = append(buf, metaTagString)
		return appendMetaString(buf, vv), true
	case bool:
		bb := byte(0)
		if vv {
			bb = 1
		}
		return append(buf, metaTagBool, bb), true
	case int:
		buf = append(buf, metaTagInt)
		return binary.AppendVarint(buf, int64(vv)), true
	case int64:
		buf = append(buf, metaTagInt64)
		return binary.AppendVarint(buf, vv), true
	case uint64:
		buf = append(buf, metaTagUint64)
		return binary.AppendUvarint(buf, vv), true
	case float64:
		buf = append(buf, metaTagFloat64)
		return binary.LittleEndian.AppendUint64(buf, math.Float64bits(vv)), true
	case []int:
		buf = binary.AppendUvarint(append(buf, metaTagIntSlice), uint64(len(vv)))
		for _, value := range vv {
			buf = binary.AppendVarint(buf, int64(value))
		}
		return buf, true
	case []int64:
		buf = binary.AppendUvarint(append(buf, metaTagI64Slice), uint64(len(vv)))
		for _, value := range vv {
			buf = binary.AppendVarint(buf, value)
		}
		return buf, true
	case []uint64:
		buf = binary.AppendUvarint(append(buf, metaTagU64Slice), uint64(len(vv)))
		for _, value := range vv {
			buf = binary.AppendUvarint(buf, value)
		}
		return buf, true
	case []bool:
		buf = binary.AppendUvarint(append(buf, metaTagBools), uint64(len(vv)))
		for _, value := range vv {
			b := byte(0)
			if value {
				b = 1
			}
			buf = append(buf, b)
		}
		return buf, true
	case map[string]int:
		buf = binary.AppendUvarint(append(buf, metaTagIntMap), uint64(len(vv)))
		keys := make([]string, 0, len(vv))
		for key := range vv {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			buf = appendMetaString(buf, key)
			buf = binary.AppendVarint(buf, int64(vv[key]))
		}
		return buf, true
	case []float64:
		buf = append(buf, metaTagF64Slice)
		buf = binary.AppendUvarint(buf, uint64(len(vv)))
		for _, f := range vv {
			buf = binary.LittleEndian.AppendUint64(buf, math.Float64bits(f))
		}
		return buf, true
	case []string:
		buf = append(buf, metaTagStrSlice)
		buf = binary.AppendUvarint(buf, uint64(len(vv)))
		for _, s := range vv {
			buf = appendMetaString(buf, s)
		}
		return buf, true
	case map[string]any:
		buf = append(buf, metaTagMap)
		return appendMetaMap(buf, vv, depth+1)
	case []map[string]any:
		buf = append(buf, metaTagMapSlice)
		buf = binary.AppendUvarint(buf, uint64(len(vv)))
		for _, mm := range vv {
			var ok bool
			buf, ok = appendMetaMap(buf, mm, depth+1)
			if !ok {
				return buf, false
			}
		}
		return buf, true
	case []any:
		buf = append(buf, metaTagAnySlice)
		buf = binary.AppendUvarint(buf, uint64(len(vv)))
		for _, e := range vv {
			var ok bool
			buf, ok = appendMetaValue(buf, e, depth+1)
			if !ok {
				return buf, false
			}
		}
		return buf, true
	case *contracts.Shape:
		js, err := json.Marshal(vv)
		if err != nil {
			return buf, false
		}
		buf = append(buf, metaTagShape)
		return appendMetaString(buf, string(js)), true
	default:
		return appendMetaJSON(buf, v)
	}
}

func appendMetaJSON(buf []byte, value any) ([]byte, bool) {
	raw, err := json.Marshal(value)
	if err != nil {
		return buf, false
	}
	return appendMetaString(append(buf, metaTagJSON), string(raw)), true
}

// decodeMetaFast reverses encodeMetaFast. A malformed blob returns an error
// (never panics).
func decodeMetaFast(b []byte) (map[string]any, error) {
	if !isFlatMeta(b) {
		return nil, invalid("metadata revision")
	}
	d := &metaDecoder{buf: b[2:]} // skip magic + version
	m, err := d.readMap(0)
	if err != nil {
		return nil, err
	}
	if d.pos != len(d.buf) {
		return nil, invalid("unbound metadata bytes")
	}
	return m, nil
}

// metaDecoder is a bounds-checked cursor over a flat meta blob.
type metaDecoder struct {
	buf []byte
	pos int
}

func (d *metaDecoder) uvarint() (uint64, error) {
	v, n := binary.Uvarint(d.buf[d.pos:])
	if n <= 0 {
		return 0, errMetaTruncated
	}
	d.pos += n
	return v, nil
}

func (d *metaDecoder) varint() (int64, error) {
	v, n := binary.Varint(d.buf[d.pos:])
	if n <= 0 {
		return 0, errMetaTruncated
	}
	d.pos += n
	return v, nil
}

func (d *metaDecoder) readByte() (byte, error) {
	if d.pos >= len(d.buf) {
		return 0, errMetaTruncated
	}
	b := d.buf[d.pos]
	d.pos++
	return b, nil
}

func (d *metaDecoder) readBytes(n int) ([]byte, error) {
	if n < 0 || n > len(d.buf)-d.pos {
		return nil, errMetaTruncated
	}
	b := d.buf[d.pos : d.pos+n]
	d.pos += n
	return b, nil
}

func (d *metaDecoder) readString() (string, error) {
	n, err := d.uvarint()
	if err != nil {
		return "", err
	}
	if n > uint64(len(d.buf)-d.pos) {
		return "", errMetaTruncated
	}
	b, err := d.readBytes(int(n))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// readCount reads a uvarint length and caps it to the remaining buffer so a
// corrupt count cannot trigger a giant allocation (every element occupies at
// least one byte).
func (d *metaDecoder) readCount() (int, error) {
	n, err := d.uvarint()
	if err != nil {
		return 0, err
	}
	if n > uint64(len(d.buf)-d.pos) {
		return 0, errMetaTruncated
	}
	return int(n), nil
}

func (d *metaDecoder) readMap(depth int) (map[string]any, error) {
	count, err := d.readCount()
	if err != nil {
		return nil, err
	}
	m := make(map[string]any, count)
	previous := ""
	for i := 0; i < count; i++ {
		key, err := d.readString()
		if err != nil {
			return nil, err
		}
		val, err := d.readValue(key, depth+1)
		if err != nil {
			return nil, err
		}
		if i > 0 && key <= previous {
			return nil, invalid("metadata keys not unique and sorted")
		}
		previous = key
		m[key] = val
	}
	return m, nil
}

func (d *metaDecoder) readValue(key string, depth int) (any, error) {
	tag, err := d.readByte()
	if err != nil {
		return nil, err
	}
	if depth > metaMaxNativeDepth && tag != metaTagJSON {
		return nil, invalid("metadata nesting exceeds native depth")
	}
	switch tag {
	case metaTagNil:
		return nil, nil
	case metaTagString:
		return d.readString()
	case metaTagBool:
		b, err := d.readByte()
		if err != nil {
			return nil, err
		}
		if b > 1 {
			return nil, invalid("metadata boolean flag")
		}
		return b != 0, nil
	case metaTagInt:
		v, err := d.varint()
		if err != nil {
			return nil, err
		}
		return int(v), nil
	case metaTagInt64:
		v, err := d.varint()
		if err != nil {
			return nil, err
		}
		return v, nil
	case metaTagUint64:
		return d.uvarint()
	case metaTagFloat64:
		b, err := d.readBytes(8)
		if err != nil {
			return nil, err
		}
		return math.Float64frombits(binary.LittleEndian.Uint64(b)), nil
	case metaTagIntSlice, metaTagI64Slice, metaTagU64Slice, metaTagBools:
		count, err := d.readCount()
		if err != nil {
			return nil, err
		}
		switch tag {
		case metaTagIntSlice:
			out := make([]int, count)
			for i := range out {
				value, err := d.varint()
				if err != nil {
					return nil, err
				}
				out[i] = int(value)
			}
			return out, nil
		case metaTagI64Slice:
			out := make([]int64, count)
			for i := range out {
				out[i], err = d.varint()
				if err != nil {
					return nil, err
				}
			}
			return out, nil
		case metaTagU64Slice:
			out := make([]uint64, count)
			for i := range out {
				out[i], err = d.uvarint()
				if err != nil {
					return nil, err
				}
			}
			return out, nil
		default:
			out := make([]bool, count)
			for i := range out {
				value, err := d.readByte()
				if err != nil {
					return nil, err
				}
				if value > 1 {
					return nil, invalid("metadata boolean flag")
				}
				out[i] = value != 0
			}
			return out, nil
		}
	case metaTagIntMap:
		count, err := d.readCount()
		if err != nil {
			return nil, err
		}
		out := make(map[string]int, count)
		previous := ""
		for i := 0; i < count; i++ {
			key, err := d.readString()
			if err != nil {
				return nil, err
			}
			if i > 0 && key <= previous {
				return nil, invalid("metadata keys not unique and sorted")
			}
			value, err := d.varint()
			if err != nil {
				return nil, err
			}
			out[key] = int(value)
			previous = key
		}
		return out, nil
	case metaTagF64Slice:
		count, err := d.readCount()
		if err != nil {
			return nil, err
		}
		out := make([]float64, count)
		for i := range out {
			b, err := d.readBytes(8)
			if err != nil {
				return nil, err
			}
			out[i] = math.Float64frombits(binary.LittleEndian.Uint64(b))
		}
		return out, nil
	case metaTagStrSlice:
		count, err := d.readCount()
		if err != nil {
			return nil, err
		}
		out := make([]string, 0, count)
		for i := 0; i < count; i++ {
			s, err := d.readString()
			if err != nil {
				return nil, err
			}
			out = append(out, s)
		}
		return out, nil
	case metaTagMap:
		return d.readMap(depth + 1)
	case metaTagMapSlice:
		count, err := d.readCount()
		if err != nil {
			return nil, err
		}
		out := make([]map[string]any, 0, count)
		for i := 0; i < count; i++ {
			mm, err := d.readMap(depth + 1)
			if err != nil {
				return nil, err
			}
			out = append(out, mm)
		}
		return out, nil
	case metaTagAnySlice:
		count, err := d.readCount()
		if err != nil {
			return nil, err
		}
		out := make([]any, 0, count)
		for i := 0; i < count; i++ {
			e, err := d.readValue(key, depth+1)
			if err != nil {
				return nil, err
			}
			out = append(out, e)
		}
		return out, nil
	case metaTagJSON:
		text, err := d.readString()
		if err != nil {
			return nil, err
		}
		decoder := json.NewDecoder(strings.NewReader(text))
		decoder.UseNumber()
		var value any
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
			return nil, invalid("metadata JSON has trailing data")
		}
		return normalizeMetaJSONNumbers(key, value), nil
	case metaTagShape:
		s, err := d.readString()
		if err != nil {
			return nil, err
		}
		var sh *contracts.Shape
		if err := json.Unmarshal([]byte(s), &sh); err != nil {
			return nil, err
		}
		return sh, nil
	default:
		return nil, fmt.Errorf("store_pebble: unknown meta value tag 0x%02x", tag)
	}
}

// JSON leaves share the durable numeric conventions. Array coercion by key
// applies only when every element has the target type; integer codes stay intact.
func normalizeMetaJSONNumbers(key string, value any) any {
	switch v := value.(type) {
	case json.Number:
		num, _ := v.Float64()
		switch key {
		case "confidence", "coverage_pct", "score", "similarity", "churn_rate", "rate":
			return num
		case "timestamp", "ts":
			if integer, err := v.Int64(); err == nil {
				return integer
			}
			return int64(num)
		}
		if integer, err := v.Int64(); err == nil {
			return int(integer)
		}
		if num == float64(int64(num)) {
			return int(num)
		}
		return num
	case map[string]any:
		for child, item := range v {
			v[child] = normalizeMetaJSONNumbers(child, item)
		}
	case []any:
		for i, item := range v {
			v[i] = normalizeMetaJSONNumbers(key, item)
		}

		switch key {
		case "path_param_names", "query_params", "status_codes", "notes", "methods", "arg_names", "repos", "usings", "global_usings", "using_static", "scoped_usings", "global_using_static":
			out := make([]string, len(v))
			for i, item := range v {
				text, ok := item.(string)
				if !ok {
					return value
				}
				out[i] = text
			}
			return out
		case "response_envelope", "ef_fluent":
			out := make([]map[string]any, len(v))
			for i, item := range v {
				row, ok := item.(map[string]any)
				if !ok {
					return value
				}
				out[i] = row
			}
			return out
		}
	}
	return value
}
