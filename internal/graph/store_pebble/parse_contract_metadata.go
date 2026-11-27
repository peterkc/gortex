package store_pebble

import (
	"encoding/binary"
	"fmt"
	"math"

	"github.com/zzet/gortex/internal/contracts"
)

// Contract metadata uses the existing native value tags and binary string
// codec, but has no JSON fallback. Its collection counts preserve typed nil;
// the legacy graph metadata format does not distinguish it from empty.
const (
	contractMetaStringMap      byte = 0x14
	contractMetaStringMapSlice byte = 0x15
)

func writeFactMeta(e *contractFactsEncoder, m map[string]any, depth int) {
	writeFactMap(e, m, func(e *contractFactsEncoder, v any) {
		writeFactMetaValue(e, v, depth+1)
	})
}

func readFactMeta(d *recordCursor, depth int) map[string]any {
	return readFactMap(d, func(d *recordCursor) any {
		return readFactMetaValue(d, depth+1)
	})
}

func writeFactMetaValue(e *contractFactsEncoder, value any, depth int) {
	if e.err != nil {
		return
	}
	if depth > metaMaxNativeDepth {
		e.err = invalid("contract metadata nesting")
		return
	}
	switch v := value.(type) {
	case nil, string, bool, int, int64, uint64, float64:
		e.buf, _ = appendMetaValue(e.buf, v, depth)
	case []string:
		e.buf = append(e.buf, metaTagStrSlice)
		writeFactSlice(e, v, writeFactString)
	case []int:
		e.buf = append(e.buf, metaTagIntSlice)
		writeFactSlice(e, v, (*contractFactsEncoder).integer)
	case []int64:
		e.buf = append(e.buf, metaTagI64Slice)
		writeFactSlice(e, v, writeFactInt64)
	case []uint64:
		e.buf = append(e.buf, metaTagU64Slice)
		writeFactSlice(e, v, writeFactUint64)
	case []bool:
		e.buf = append(e.buf, metaTagBools)
		writeFactSlice(e, v, (*contractFactsEncoder).boolean)
	case []float64:
		e.buf = append(e.buf, metaTagF64Slice)
		writeFactSlice(e, v, writeFactFloat)
	case map[string]int:
		e.buf = append(e.buf, metaTagIntMap)
		writeFactMap(e, v, (*contractFactsEncoder).integer)
	case map[string]string:
		e.buf = append(e.buf, contractMetaStringMap)
		writeFactMap(e, v, writeFactString)
	case []map[string]string:
		e.buf = append(e.buf, contractMetaStringMapSlice)
		writeFactSlice(e, v, func(e *contractFactsEncoder, row map[string]string) {
			writeFactMap(e, row, writeFactString)
		})
	case map[string]any:
		e.buf = append(e.buf, metaTagMap)
		writeFactMeta(e, v, depth)
	case []map[string]any:
		e.buf = append(e.buf, metaTagMapSlice)
		writeFactSlice(e, v, func(e *contractFactsEncoder, m map[string]any) {
			writeFactMeta(e, m, depth)
		})
	case []any:
		e.buf = append(e.buf, metaTagAnySlice)
		writeFactSlice(e, v, func(e *contractFactsEncoder, child any) {
			writeFactMetaValue(e, child, depth+1)
		})
	case *contracts.Shape:
		e.buf = append(e.buf, metaTagShape)
		writeFactShape(e, v)
	default:
		e.err = fmt.Errorf("%w: unsupported contract metadata type %T", errInvalidRecord, v)
	}
}

func readFactMetaValue(d *recordCursor, depth int) any {
	if d.err != nil {
		return nil
	}
	if len(d.buf) == 0 || depth > metaMaxNativeDepth {
		d.err = invalid("truncated or deeply nested contract metadata")
		return nil
	}
	tag := d.buf[0]
	d.buf = d.buf[1:]
	switch tag {
	case metaTagNil:
		return nil
	case metaTagString:
		return d.str()
	case metaTagBool:
		return d.boolean()
	case metaTagInt:
		return d.integer()
	case metaTagInt64:
		return readFactInt64(d)
	case metaTagUint64:
		return readFactUint64(d)
	case metaTagFloat64:
		return readFactFloat(d)
	case metaTagStrSlice:
		return readFactSlice(d, (*recordCursor).str)
	case metaTagIntSlice:
		return readFactSlice(d, (*recordCursor).integer)
	case metaTagI64Slice:
		return readFactSlice(d, readFactInt64)
	case metaTagU64Slice:
		return readFactSlice(d, readFactUint64)
	case metaTagBools:
		return readFactSlice(d, (*recordCursor).boolean)
	case metaTagF64Slice:
		return readFactSlice(d, readFactFloat)
	case metaTagIntMap:
		return readFactMap(d, (*recordCursor).integer)
	case contractMetaStringMap:
		return readFactMap(d, (*recordCursor).str)
	case contractMetaStringMapSlice:
		return readFactSlice(d, func(d *recordCursor) map[string]string {
			return readFactMap(d, (*recordCursor).str)
		})
	case metaTagMap:
		return readFactMeta(d, depth)
	case metaTagMapSlice:
		return readFactSlice(d, func(d *recordCursor) map[string]any {
			return readFactMeta(d, depth)
		})
	case metaTagAnySlice:
		return readFactSlice(d, func(d *recordCursor) any {
			return readFactMetaValue(d, depth+1)
		})
	case metaTagShape:
		return readFactShape(d)
	default:
		d.err = invalid("contract metadata tag")
		return nil
	}
}

func writeFactInt64(e *contractFactsEncoder, v int64) {
	e.buf = binary.AppendVarint(e.buf, v)
}

func writeFactUint64(e *contractFactsEncoder, v uint64) {
	e.buf = binary.AppendUvarint(e.buf, v)
}

func writeFactFloat(e *contractFactsEncoder, v float64) {
	e.buf = binary.LittleEndian.AppendUint64(e.buf, math.Float64bits(v))
}

func readFactInt64(d *recordCursor) int64 {
	v, n := binary.Varint(d.buf)
	if n <= 0 {
		d.err = invalid("contract metadata int64")
		return 0
	}
	d.buf = d.buf[n:]
	return v
}

func readFactUint64(d *recordCursor) uint64 {
	v, n := binary.Uvarint(d.buf)
	if n <= 0 {
		d.err = invalid("contract metadata uint64")
		return 0
	}
	d.buf = d.buf[n:]
	return v
}

func readFactFloat(d *recordCursor) float64 {
	if len(d.buf) < 8 {
		d.err = invalid("truncated contract metadata float")
		return 0
	}
	v := math.Float64frombits(binary.LittleEndian.Uint64(d.buf))
	d.buf = d.buf[8:]
	return v
}
