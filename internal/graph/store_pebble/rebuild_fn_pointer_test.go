package store_pebble

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildFnPointerScratchSpill(t *testing.T) {
	for _, threshold := range []int{1, 1 << 20} {
		t.Run(fmt.Sprint(threshold), func(t *testing.T) {
			b := newBuild(t, FullProfile)
			p, err := b.CaptureFnPointerCensus(t.Context(), threshold, func(*graph.Edge) bool { return false })
			require.NoError(t, err)
			defer func() { require.NoError(t, p.Close()) }()
			// No graph rows exist. Any flushed table must include pass scratch.
			for _, level := range b.pending.Metrics().Levels {
				require.Zero(t, level.TablesCount)
			}
			wide := "from"
			if threshold > 1 {
				wide += strings.Repeat("x", 128<<10)
			}
			require.NoError(t, p.Seed(wide, "fn"))
			for i := range 10 {
				e := &graph.Edge{Kind: graph.EdgeReferences, From: "source", To: "target", Line: i}
				require.NoError(t, p.SaveCopy(e, fmt.Sprint(i), wide))
			}
			changed, err := p.Sweep(t.Context())
			require.NoError(t, err)
			require.True(t, changed)
			targets, err := p.Targets(t.Context(), "9", 64)
			require.NoError(t, err)
			require.Equal(t, []string{"fn"}, targets)
			require.Positive(t, p.spill.Flushes)
			require.GreaterOrEqual(t, p.spill.InputBytes, int64(threshold))
			require.Positive(t, p.spill.TableBytes, "threshold must write SST bytes in this isolated scratch")
			var tables int64
			for _, level := range b.pending.Metrics().Levels {
				tables += int64(level.TablesCount)
			}
			require.Positive(t, tables)
			require.NoError(t, p.Close())
			require.NoError(t, pendingVisit(t.Context(), b.pending, fnPointerPrefix, func(r row) error {
				t.Errorf("scratch row survived Close: %x", r.key)
				return nil
			}))
		})
	}
}
