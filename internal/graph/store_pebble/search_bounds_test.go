package store_pebble

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSearchDocumentBoundsReuse(t *testing.T) {
	prefixIDs := map[string]uint32{"a": 1, "ab": 256, "ac": 65536, "z": 2}
	var calls int
	lookup := func(prefix string) (uint32, error) {
		calls++
		return prefixIDs[prefix], nil
	}
	docs := []searchDoc{
		{Rowid: 1, Length: 4, NodePresent: true, Terms: []searchTerm{{"ab", 1}, {"ac", 2}, {"z", 1}}},
		{Rowid: 2, Length: 3, Terms: []searchTerm{{"ab", 2}, {"z", 1}}},
	}
	var scratch [][]byte
	const avgdl = 3.5
	for _, doc := range docs {
		raw, err := searchDocBounds(doc, avgdl, lookup)
		require.NoError(t, err)
		scratch = append(scratch, raw)
	}
	require.Equal(t, 7, calls, "resolve each prefix once per document, not once per posting")
	require.Len(t, scratch[0], 1+8*4)
	require.Len(t, scratch[1], 1+8*3)

	// The very same scratch document may participate in another term block.
	for range 2 {
		var refs []searchPosting
		maxima := make(map[uint32]float32)
		for i, raw := range scratch {
			present, err := mergeSearchDocBounds(raw, maxima)
			require.NoError(t, err)
			refs = append(refs, searchPosting{Rowid: int64(i + 1), NodePresent: present})
		}
		require.Len(t, maxima, 4)
		require.Equal(t, searchUpper32(searchFactor(3, 4, avgdl)), maxima[1])
		require.Equal(t, searchUpper32(searchFactor(2, 3, avgdl)), maxima[256])
		ac := float64(searchUpper32(searchFactor(2, 4, avgdl)))
		require.Equal(t, float32(ac), maxima[65536])
		block := searchPackBlock(refs, maxima)
		require.NotEmpty(t, block)
		for _, threshold := range []float64{ac, math.Nextafter(ac, math.Inf(1))} {
			var seen []searchPosting
			skip, err := searchReadBlock(block, searchBlockQuery{
				Phrases: []string{"ac"}, PrefixIDs: []uint32{65536}, IDF: []float64{1}, CanSkip: true, Threshold: threshold,
			}, func(ref searchPosting) error { seen = append(seen, ref); return nil })
			require.NoError(t, err)
			require.Equal(t, threshold > ac, skip)
			if skip {
				require.Empty(t, seen)
			} else {
				require.Equal(t, []searchPosting{{Rowid: 1, NodePresent: true}, {Rowid: 2}}, seen)
			}
		}
	}
	require.Equal(t, 7, calls)

	// A Unicode term without an ASCII prefix needs presence, not a bound.
	raw, err := searchDocBounds(searchDoc{Length: 1, NodePresent: true, Terms: []searchTerm{{"λ", 1}}}, avgdl, lookup)
	require.NoError(t, err)
	maxima := make(map[uint32]float32)
	present, err := mergeSearchDocBounds(raw, maxima)
	require.NoError(t, err)
	require.True(t, present)
	require.Empty(t, maxima)
	_, err = mergeSearchDocBounds(nil, maxima)
	require.ErrorIs(t, err, errInvalidRecord)
	_, err = mergeSearchDocBounds([]byte{1, 2}, maxima)
	require.ErrorIs(t, err, errInvalidRecord)
}
