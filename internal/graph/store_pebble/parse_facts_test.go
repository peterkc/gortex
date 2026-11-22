package store_pebble

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestParseFactsStoreCodec(t *testing.T) {
	f := ParseFacts{
		Nodes: []*graph.Node{
			{ID: "a.go::Z", Kind: graph.KindFunction, Name: "Z", FilePath: "a.go", Language: "go"},
			{ID: "a.go::C", Kind: graph.KindConstant, Name: "C", FilePath: "a.go", Language: "go"},
		},
		Edges:     []*graph.Edge{{From: "a.go::Z", To: "a.go::C", Kind: graph.EdgeReferences, FilePath: "a.go", Line: 9}},
		File:      graph.FileMetaRow{FilePath: "a.go", Size: 64, NodeCount: 2, ContentHash: "digest", Errors: "[9]"},
		Constants: []graph.ConstantValueRow{{FilePath: "a.go", NodeID: "a.go::C", Value: "1"}},
		Shingles:  map[string][]uint64{"a.go::Z": {19, 13}},
		Symbols:   []graph.SymbolFTSItem{{NodeID: "a.go::Z", Tokens: "z hello"}},
	}
	packed, err := EncodeParseFacts(f)
	require.NoError(t, err)
	read, err := DecodeParseFacts(packed)
	require.NoError(t, err)
	require.Len(t, read.Nodes, 2)
	require.Len(t, read.Edges, 1)
	require.Equal(t, f, read)
	again, err := EncodeParseFacts(read)
	require.NoError(t, err)
	require.Equal(t, packed, again)
	_, err = DecodeParseFacts(packed[:len(packed)-1])
	require.ErrorIs(t, err, errInvalidRecord)
	_, err = DecodeParseFacts(append(bytes.Clone(packed), 0))
	require.ErrorIs(t, err, errInvalidRecord)

	f.Nodes[0].AbsoluteFilePath = "/other/checkout/a.go"
	_, err = EncodeParseFacts(f)
	require.ErrorIs(t, err, errInvalidRecord)
	f.Nodes[0].AbsoluteFilePath = ""
	f.File.NodeCount = 0
	_, err = EncodeParseFacts(f)
	require.ErrorIs(t, err, errInvalidRecord)
}
