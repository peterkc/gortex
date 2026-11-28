// Package store_pebble owns the key and value formats for the Pebble graph store.
package store_pebble

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"

	"github.com/zzet/gortex/internal/graph"
)

var errInvalidRecord = errors.New("invalid Pebble graph record")

func invalid(reason string) error {
	return fmt.Errorf("%w: %s", errInvalidRecord, reason)
}

const (
	nodeFormat byte = 0x31
	edgeFormat byte = 0x32
)

// Keep field order fixed. New layouts require a new format byte.
func encodeNode(n *graph.Node) ([]byte, error) {
	return encodeNodeRecord(n, appendMetadata)
}

func encodeNodeRecord(n *graph.Node, appendMeta func([]byte, map[string]any) ([]byte, error)) ([]byte, error) {
	b := []byte{nodeFormat}
	for _, s := range []string{n.ID, string(n.Kind), n.Name, n.QualName, n.FilePath} {
		b = appendMetaString(b, s)
	}
	for _, v := range []int{n.StartLine, n.EndLine, n.StartColumn, n.EndColumn} {
		b = binary.AppendVarint(b, int64(v))
	}
	for _, s := range []string{n.Language, n.RepoPrefix, n.WorkspaceID, n.ProjectID, n.AbsoluteFilePath, n.Origin} {
		b = appendMetaString(b, s)
	}
	b = appendBool(b, n.Stub)
	var stamp []byte
	if !n.FetchedAt.IsZero() {
		var err error
		stamp, err = n.FetchedAt.MarshalBinary()
		if err != nil {
			return nil, err
		}
	}
	b = appendBytes(b, stamp)
	return appendMeta(b, n.Meta)
}

func encodeEdge(e *graph.Edge) ([]byte, error) {
	return encodeEdgeRecord(e, appendMetadata)
}

func encodeEdgeRecord(e *graph.Edge, appendMeta func([]byte, map[string]any) ([]byte, error)) ([]byte, error) {
	if math.IsNaN(e.Confidence) || math.IsInf(e.Confidence, 0) {
		return nil, invalid("non-finite edge confidence")
	}
	b := []byte{edgeFormat}
	for _, s := range []string{e.From, e.To, string(e.Kind), e.FilePath} {
		b = appendMetaString(b, s)
	}
	b = binary.AppendVarint(b, int64(e.Line))
	b = binary.LittleEndian.AppendUint64(b, math.Float64bits(e.Confidence))
	for _, s := range []string{e.ConfidenceLabel, e.Origin, e.Tier} {
		b = appendMetaString(b, s)
	}
	b = appendBool(b, e.CrossRepo)
	for _, s := range []string{e.Context, e.ReturnUsage, e.Via, e.Alias} {
		b = appendMetaString(b, s)
	}
	b = appendBool(b, e.NameOnly)
	b = appendBool(b, e.Provisional)
	return appendMeta(b, e.Meta)
}

func appendBytes(b, v []byte) []byte {
	return append(binary.AppendUvarint(b, uint64(len(v))), v...)
}

func appendBool(b []byte, v bool) []byte {
	if v {
		return append(b, 1)
	}
	return append(b, 0)
}

func appendMetadata(b []byte, m map[string]any) ([]byte, error) {
	b = appendBool(b, m != nil)
	if m == nil {
		return b, nil
	}
	raw, ok := encodeMetaFast(m)
	if !ok {
		return nil, invalid("unsupported metadata value")
	}
	return appendBytes(b, raw), nil
}

func decodeNode(b []byte) (*graph.Node, error) { return readNode(b, true) }

func decodeNodeLight(b []byte) (*graph.Node, error) { return readNode(b, false) }

func readNode(b []byte, metadata bool) (*graph.Node, error) {
	if len(b) == 0 || b[0] != nodeFormat {
		return nil, invalid("node format")
	}
	d := recordCursor{buf: b[1:]}
	n := &graph.Node{ID: d.str(), Kind: graph.NodeKind(d.str()), Name: d.str(), QualName: d.str(), FilePath: d.str()}
	n.StartLine = d.integer()
	n.EndLine = d.integer()
	n.StartColumn = d.integer()
	n.EndColumn = d.integer()
	n.Language = d.str()
	n.RepoPrefix = d.str()
	n.WorkspaceID = d.str()
	n.ProjectID = d.str()
	n.AbsoluteFilePath = d.str()
	n.Origin = d.str()
	n.Stub = d.boolean()
	if stamp := d.bytes(); len(stamp) > 0 {
		if err := n.FetchedAt.UnmarshalBinary(stamp); err != nil {
			return nil, invalid("node timestamp")
		}
	}
	if metadata {
		n.Meta = d.metadata()
	} else if d.boolean() {
		_ = d.bytes()
	}
	if err := d.finish(); err != nil {
		return nil, err
	}
	return n, nil
}

func decodeEdge(b []byte) (*graph.Edge, error) { return readEdge(b, true) }

func decodeEdgeLight(b []byte) (*graph.Edge, error) { return readEdge(b, false) }

func readEdge(b []byte, metadata bool) (*graph.Edge, error) {
	if len(b) == 0 || b[0] != edgeFormat {
		return nil, invalid("edge format")
	}
	d := recordCursor{buf: b[1:]}
	e := &graph.Edge{From: d.str(), To: d.str(), Kind: graph.EdgeKind(d.str()), FilePath: d.str()}
	e.Line = d.integer()
	e.Confidence = d.float()
	e.ConfidenceLabel = d.str()
	e.Origin = d.str()
	e.Tier = d.str()
	e.CrossRepo = d.boolean()
	e.Context = d.str()
	e.ReturnUsage = d.str()
	e.Via = d.str()
	e.Alias = d.str()
	e.NameOnly = d.boolean()
	e.Provisional = d.boolean()
	if metadata {
		e.Meta = d.metadata()
	} else if d.boolean() {
		_ = d.bytes()
	}
	if err := d.finish(); err != nil {
		return nil, err
	}
	return e, nil
}

// Latch the first parse error so field decoding stays in wire order.
type recordCursor struct {
	buf []byte
	err error
}

func (d *recordCursor) str() string { return string(d.bytes()) }

func (d *recordCursor) bytes() []byte {
	if d.err != nil {
		return nil
	}
	length, width := binary.Uvarint(d.buf)
	if width <= 0 || length > uint64(len(d.buf)-width) {
		d.err = invalid("truncated or overflowing length")
		return nil
	}
	end := width + int(length)
	b := d.buf[width:end]
	d.buf = d.buf[end:]
	return b
}

func (d *recordCursor) integer() int {
	if d.err != nil {
		return 0
	}
	v, width := binary.Varint(d.buf)
	if width <= 0 || int64(int(v)) != v {
		d.err = invalid("truncated or overflowing integer")
		return 0
	}
	d.buf = d.buf[width:]
	return int(v)
}

func (d *recordCursor) boolean() bool {
	if d.err != nil {
		return false
	}
	if len(d.buf) == 0 || d.buf[0] > 1 {
		d.err = invalid("truncated or invalid boolean")
		return false
	}
	v := d.buf[0] == 1
	d.buf = d.buf[1:]
	return v
}

func (d *recordCursor) float() float64 {
	if d.err != nil {
		return 0
	}
	if len(d.buf) < 8 {
		d.err = invalid("truncated edge confidence")
		return 0
	}
	v := math.Float64frombits(binary.LittleEndian.Uint64(d.buf))
	d.buf = d.buf[8:]
	if math.IsNaN(v) || math.IsInf(v, 0) {
		d.err = invalid("non-finite edge confidence")
	}
	return v
}

func (d *recordCursor) metadata() map[string]any {
	if !d.boolean() {
		return nil
	}
	raw := d.bytes()
	if d.err != nil {
		return nil
	}
	if len(raw) >= 2 && raw[0] == metaFlatMagic0 && raw[1] == contractRowMetaVersion {
		meta := recordCursor{buf: raw[2:]}
		m := readFactMeta(&meta, 0)
		d.err = meta.finish()
		return m
	}
	m, err := decodeMetaFast(raw)
	if err != nil {
		d.err = fmt.Errorf("%w: metadata: %w", errInvalidRecord, err)
	}
	return m
}

func (d *recordCursor) finish() error {
	if d.err != nil {
		return d.err
	}
	if len(d.buf) != 0 {
		return invalid("trailing bytes")
	}
	return nil
}
