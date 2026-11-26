package store_pebble

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/cockroachdb/pebble/v2"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildPlaceholderClaimAbsentSkipsPending(t *testing.T) {
	// A closed pending DB is a tripwire for any iterator open. A census
	// miss must return before touching pending, even with other saved sites.
	db, err := pebble.Open(filepath.Join(t.TempDir(), "pending"), pendingOptions(nil))
	require.NoError(t, err)
	require.NoError(t, db.Close())
	site := placeholderSite("unresolved::present", "f.go", 1)
	p := &RebuildPlaceholderSources{build: &BuildStore{pending: db}, sites: map[string]struct{}{string(site): {}}}
	rp := graph.PlaceholderRepoint{OldFrom: "unresolved::absent", NewFrom: "winner", FilePath: "f.go", Line: 1}
	require.NoError(t, p.claim(t.Context(), rp))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, p.claim(ctx, rp), context.Canceled)
	require.Equal(t, map[string]struct{}{string(site): {}}, p.sites)
	require.Zero(t, p.stats)
}
