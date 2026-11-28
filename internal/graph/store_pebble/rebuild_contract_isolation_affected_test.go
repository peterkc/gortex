package store_pebble

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/contracts"
)

func TestRebuildContractIsolationAffectedDedupesScopes(t *testing.T) {
	ctx := t.Context()
	b := newBuild(t, FullProfile)
	require.NoError(t, b.IsolateRebuildLinks(ctx))
	mark := func(file string, kind contracts.ContractType, language string) {
		require.NoError(t, b.MarkRebuildContractIsolationAffected(ctx, "", file, kind, language))
	}
	mark("api.go", contracts.ContractHTTP, "python")
	mark("api.go", contracts.ContractHTTP, "python") // early and late readers
	mark("api.go", contracts.ContractHTTP, "ruby")
	mark("api.go", contracts.ContractGRPC, "python")
	mark("other.go", contracts.ContractHTTP, "python")
	var rows []RebuildContractOmission
	visit := func(batch RebuildContractBatch) error {
		require.Empty(t, batch.Nodes)
		require.Empty(t, batch.Edges)
		require.Empty(t, batch.Shapes)
		require.Len(t, batch.Omitted, 1)
		o := batch.Omitted[0]
		require.Empty(t, o.ID)
		require.NotZero(t, o.RecordKey)
		rows = append(rows, o)
		return nil
	}
	require.NoError(t, b.VisitRebuildContractIsolationAffected(ctx, visit))
	require.Len(t, rows, 4)
	require.Equal(t, 4, len(map[HeadDigest]struct{}{rows[0].RecordKey: {}, rows[1].RecordKey: {}, rows[2].RecordKey: {}, rows[3].RecordKey: {}}))
	require.NoError(t, b.RestoreRebuildDeclarations(ctx))
	rows = nil
	require.NoError(t, b.VisitRebuildContractIsolationAffected(ctx, visit))
	require.Empty(t, rows)
	require.Error(t, b.MarkRebuildContractIsolationAffected(ctx, "", "api.go", contracts.ContractHTTP, "python"))
}
