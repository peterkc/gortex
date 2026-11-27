package languages

import (
	"maps"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

func TestGoExtractorBuiltinReceiverFacts(t *testing.T) {
	src := []byte(`package sample
func parameter(len func([]int) int, xs []int) int {
	return len(xs)
}
func intrinsic(xs []int) int {
	return len(xs) + len(xs[:1])
}
func ambiguous(xs []int) int {
	{
		len := 0
		_ = len
	}
	return len(xs)
}
func ordinary(xs []int) int {
	return intrinsic(xs)
}
`)
	ext := NewGoExtractor()
	native, err := ext.Extract("sample.go", src)
	require.NoError(t, err)
	defer native.ReleaseTree()
	captured, err := ext.ExtractWithOptions("sample.go", src, parser.ExtractionOptions{CaptureBuiltinReceiverFacts: true})
	require.NoError(t, err)
	defer captured.ReleaseTree()

	wantForm := map[string]string{
		"sample.go::parameter": "shadowed",
		"sample.go::intrinsic": "unbound",
		"sample.go::ambiguous": "unknown",
	}
	baseCalls := edgesOfKind(native.Edges, graph.EdgeCalls)
	calls := edgesOfKind(captured.Edges, graph.EdgeCalls)
	require.Len(t, baseCalls, 5)
	require.Len(t, calls, len(baseCalls))
	hits := map[string]int{}
	for i, base := range baseCalls {
		want := *base
		if form := wantForm[base.From]; form != "" {
			hits[base.From]++
			require.Equal(t, "unresolved::len", base.To)
			want.Meta = maps.Clone(base.Meta)
			want.SetBuiltinReceiverHint(graph.BuiltinReceiverHint{Language: "go", Form: form, Name: "len"})
			assert.NotEqual(t, base.Meta, calls[i].Meta, "hint present only when opted in: %s", base.From)
		}
		assert.Equal(t, &want, calls[i])
	}
	assert.Equal(t, map[string]int{"sample.go::parameter": 1, "sample.go::intrinsic": 2, "sample.go::ambiguous": 1}, hits)
}
