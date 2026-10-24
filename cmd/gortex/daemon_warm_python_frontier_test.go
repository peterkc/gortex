package main

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/indexer"
)

func TestWarmupPythonConsumersStayOutsideDerivedPlan(t *testing.T) {
	for _, full := range []bool{false, true} {
		frontier := newWarmupDeltaFrontier()
		frontier.record(&indexer.IndexResult{
			RepoPrefix: "provider", FullRetrack: full,
			DerivedInvalidation:   indexer.DerivedInvalidationPlan{Files: []string{"provider/src/PIL/image.py"}},
			CrossRepoResolveFiles: []string{"consumer/app.py"},
		})
		plans, files, exact := frontier.snapshot(1, false)
		require.Contains(t, files, "consumer/app.py")
		require.Equal(t, !full, exact)
		for _, plan := range plans {
			require.NotContains(t, plan.Files, "consumer/app.py")
		}
	}
}
