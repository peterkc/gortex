package resolver

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildPlatformGoCatalogMatchesNative(t *testing.T) {
	const rev = graph.PlatformCatalogRevision
	for category, names := range map[string]map[string]struct{}{"": goBuiltinFuncs, "type": goBuiltinTypes, "const": goBuiltinConsts} {
		for name := range names {
			parts := []string{"go"}
			if category != "" {
				parts = append(parts, category)
			}
			id := graph.StubID("", graph.StubKindBuiltin, append(parts, name)...)
			entry, ok := graph.LookupPlatformBuiltin(rev, id)
			require.True(t, ok, id)
			require.Equal(t, graph.PlatformBuiltin{Language: "go", Category: category, Name: name}, entry)
			require.Equal(t, category == "", graph.IsGoBuiltinFunction(name), id)
		}
	}
}
