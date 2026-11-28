package store_pebble

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
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
	count, err := b.VisitRebuildContractIsolationAffected(ctx, visit)
	require.NoError(t, err)
	require.Equal(t, uint64(4), count)
	require.Len(t, rows, 4)
	require.Equal(t, 4, len(map[HeadDigest]struct{}{rows[0].RecordKey: {}, rows[1].RecordKey: {}, rows[2].RecordKey: {}, rows[3].RecordKey: {}}))

	// A failed delivery must stop the visit without counting the rejected row.
	stop := errors.New("stop accepting omissions")
	calls := 0
	count, err = b.VisitRebuildContractIsolationAffected(ctx, func(RebuildContractBatch) error {
		calls++
		if calls == 2 {
			return stop
		}
		return nil
	})
	require.ErrorIs(t, err, stop)
	require.Equal(t, 2, calls)
	require.Equal(t, uint64(1), count)

	require.NoError(t, b.RestoreRebuildDeclarations(ctx))
	rows = nil
	count, err = b.VisitRebuildContractIsolationAffected(ctx, visit)
	require.NoError(t, err)
	require.Zero(t, count)
	require.Empty(t, rows)
	require.Error(t, b.MarkRebuildContractIsolationAffected(ctx, "", "api.go", contracts.ContractHTTP, "python"))
}

func TestRebuildContractIsolationAffectedRejectsInvalidScopeInsideCut(t *testing.T) {
	for _, tc := range []struct {
		name     string
		kind     contracts.ContractType
		language string
		wantErr  string
	}{
		{"missing extractor", contracts.ContractHTTP, "", "contract isolation scope outside cut or without extractor"},
		{"reads config without an ID", contracts.ContractType(graph.EdgeReadsConfig), "java", "contract isolation omission type"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			b := newBuild(t, FullProfile)
			require.NoError(t, b.IsolateRebuildLinks(ctx))

			err := b.MarkRebuildContractIsolationAffected(ctx, "", "api.go", tc.kind, tc.language)

			require.ErrorIs(t, err, errInvalidRecord)
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}
