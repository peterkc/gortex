package indexer

import (
	"maps"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRebuildLinkInputHoldout(t *testing.T) {
	byPath := map[string]string{
		"main.go": "go", "screen.jsx": "javascript", "app.tsx": "typescript",
		"host.ts": "html", "app.vue": "vue", "test_example.py": "python",
	}
	census := &rebuildExtractorCensus{admitted: uint64(len(byPath)), byPath: maps.Clone(byPath)}
	for _, tc := range []struct {
		path    string
		holdOut bool
	}{
		{path: "main.go"},
		{path: "screen.jsx"},
		{path: "app.tsx"},
		{path: "host.ts", holdOut: true},
		{path: "app.vue", holdOut: true},
		{path: "test_example.py", holdOut: true},
	} {
		t.Run(tc.path, func(t *testing.T) {
			heldOut, err := census.holdsOutLinkInput(tc.path)
			require.NoError(t, err)
			require.Equal(t, tc.holdOut, heldOut)
		})
	}
	require.Equal(t, byPath, census.byPath, "the full census survives link selection")
	_, err := census.holdsOutLinkInput("missing.go")
	require.ErrorIs(t, err, errRebuildCallerAttribution)
	require.ErrorContains(t, err, "missing.go")
	_, err = (&rebuildExtractorCensus{admitted: 2, byPath: map[string]string{"main.go": "go"}}).holdsOutLinkInput("main.go")
	require.ErrorIs(t, err, errRebuildCallerAttribution)
}
