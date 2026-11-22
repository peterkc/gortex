package store_pebble

import (
	"encoding/binary"
	"fmt"
	"maps"
	"slices"

	"github.com/zzet/gortex/internal/graph"
)

const (
	parseFactsFormat             byte = 1
	parseFactsWithLanguageFormat byte = 2
)

// ParseFacts is the portable part of a file extraction. Content declarations
// and unresolved scope facts live in Nodes, Edges and their typed metadata.
// The writer preserves extractor order for later first-wins reductions.
type ParseFacts struct {
	Nodes     []*graph.Node
	Edges     []*graph.Edge
	File      graph.FileMetaRow
	Constants []graph.ConstantValueRow
	Shingles  map[string][]uint64
	Symbols   []graph.SymbolFTSItem
	// EffectiveLanguage is the B2 extractor ID chosen before transforms.
	// Legacy entries have no language authority and leave this empty.
	EffectiveLanguage string
}

// EncodeParseFacts reuses the graph store's node, edge and metadata codecs.
// The parse-store envelope binds these bytes to the input key.
func EncodeParseFacts(f ParseFacts) ([]byte, error) {
	if f.File.FilePath == "" || f.File.Size < 0 || f.File.NodeCount != len(f.Nodes) {
		return nil, invalid("parse file metadata")
	}
	b := []byte{parseFactsFormat}
	b = appendMetaString(b, f.File.FilePath)
	b = appendMetaString(b, f.File.ContentHash)
	b = binary.AppendVarint(b, int64(f.File.Size))
	b = appendMetaString(b, f.File.Errors)
	b = binary.AppendVarint(b, int64(len(f.Nodes)))
	for _, n := range f.Nodes {
		if err := checkParseNode(n); err != nil {
			return nil, err
		}
		row, err := encodeNode(n)
		if err != nil {
			return nil, err
		}
		b = appendBytes(b, row)
	}
	b = binary.AppendVarint(b, int64(len(f.Edges)))
	for _, e := range f.Edges {
		if err := checkParseEdge(e); err != nil {
			return nil, err
		}
		row, err := encodeEdge(e)
		if err != nil {
			return nil, err
		}
		b = appendBytes(b, row)
	}
	b = binary.AppendVarint(b, int64(len(f.Constants)))
	for _, c := range f.Constants {
		if c.NodeID == "" || c.FilePath != f.File.FilePath {
			return nil, invalid("parse constant outside file")
		}
		b = appendMetaString(b, c.NodeID)
		b = appendMetaString(b, c.Value)
	}
	b = binary.AppendVarint(b, int64(len(f.Shingles)))
	for _, id := range slices.Sorted(maps.Keys(f.Shingles)) {
		if id == "" {
			return nil, invalid("parse shingle without a node")
		}
		b = appendMetaString(b, id)
		values := f.Shingles[id]
		b = binary.AppendVarint(b, int64(len(values)))
		for _, v := range values {
			b = binary.LittleEndian.AppendUint64(b, v)
		}
	}
	b = binary.AppendVarint(b, int64(len(f.Symbols)))
	for _, item := range f.Symbols {
		if item.NodeID == "" {
			return nil, invalid("parse symbol without a node")
		}
		b = appendMetaString(b, item.NodeID)
		b = appendMetaString(b, item.Tokens)
	}
	if f.EffectiveLanguage != "" {
		b[0] = parseFactsWithLanguageFormat
		b = appendMetaString(b, f.EffectiveLanguage)
	}
	return b, nil
}

func DecodeParseFacts(raw []byte) (ParseFacts, error) {
	if len(raw) == 0 || (raw[0] != parseFactsFormat && raw[0] != parseFactsWithLanguageFormat) {
		return ParseFacts{}, invalid("parse facts format")
	}
	d := recordCursor{buf: raw[1:]}
	f := ParseFacts{File: graph.FileMetaRow{FilePath: d.str(), ContentHash: d.str(), Size: d.integer(), Errors: d.str()}}
	if f.File.FilePath == "" || f.File.Size < 0 {
		return ParseFacts{}, invalid("parse file metadata")
	}
	for range parseFactCount(&d, 1) {
		n, err := decodeNode(d.bytes())
		if err != nil {
			return ParseFacts{}, err
		}
		if err = checkParseNode(n); err != nil {
			return ParseFacts{}, err
		}
		f.Nodes = append(f.Nodes, n)
	}
	f.File.NodeCount = len(f.Nodes)
	for range parseFactCount(&d, 1) {
		e, err := decodeEdge(d.bytes())
		if err != nil {
			return ParseFacts{}, err
		}
		if err = checkParseEdge(e); err != nil {
			return ParseFacts{}, err
		}
		f.Edges = append(f.Edges, e)
	}
	for range parseFactCount(&d, 2) {
		id, value := d.str(), d.str()
		if id == "" {
			return ParseFacts{}, invalid("parse constant without a node")
		}
		f.Constants = append(f.Constants, graph.ConstantValueRow{NodeID: id, FilePath: f.File.FilePath, Value: value})
	}
	last := ""
	for range parseFactCount(&d, 2) {
		id := d.str()
		if id <= last {
			return ParseFacts{}, invalid("parse shingle node order")
		}
		last = id
		var values []uint64
		for range parseFactCount(&d, 8) {
			values = append(values, binary.LittleEndian.Uint64(d.buf))
			d.buf = d.buf[8:]
		}
		if f.Shingles == nil {
			f.Shingles = make(map[string][]uint64)
		}
		f.Shingles[id] = values
	}
	for range parseFactCount(&d, 2) {
		id, tokens := d.str(), d.str()
		if id == "" {
			return ParseFacts{}, invalid("parse symbol without a node")
		}
		f.Symbols = append(f.Symbols, graph.SymbolFTSItem{NodeID: id, Tokens: tokens})
	}
	if raw[0] == parseFactsWithLanguageFormat {
		f.EffectiveLanguage = d.str()
		if f.EffectiveLanguage == "" {
			return ParseFacts{}, invalid("missing effective parse language")
		}
	}
	if err := d.finish(); err != nil {
		return ParseFacts{}, fmt.Errorf("parse facts: %w", err)
	}
	return f, nil
}

func parseFactCount(d *recordCursor, minWidth int) int {
	n := d.integer()
	if d.err != nil {
		return 0
	}
	if n < 0 || n > len(d.buf)/minWidth {
		d.err = invalid("parse fact count")
		return 0
	}
	return n
}

func checkParseNode(n *graph.Node) error {
	if n == nil || n.ID == "" || graph.IsProxyID(n.ID) || n.AbsoluteFilePath != "" ||
		n.RepoPrefix != "" || n.WorkspaceID != "" || n.ProjectID != "" {
		return invalid("nonportable parse node")
	}
	return nil
}

func checkParseEdge(e *graph.Edge) error {
	if e == nil || e.From == "" || e.To == "" || graph.IsProxyID(e.From) || graph.IsProxyID(e.To) || e.CrossRepo {
		return invalid("nonportable parse edge")
	}
	return nil
}
