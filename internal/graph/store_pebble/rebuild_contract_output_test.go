package store_pebble

import (
	"errors"
	"iter"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildContractOutputFreezesRows(t *testing.T) {
	b, err := OpenBuild(t.TempDir(), FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	ctx := t.Context()
	n := &graph.Node{ID: "http::GET::/x", Kind: graph.KindContract, Language: "contract", FilePath: "a.go",
		Meta: map[string]any{"contract_meta": map[string]any{"status_codes": []int{200, 202}}}}
	e := &graph.Edge{From: "a.go::Read", To: n.ID, Kind: graph.EdgeProvides, FilePath: "a.go", Line: 4,
		Meta: map[string]any{"contract_owner_meta": map[string]any{"path_param_names": []string{"id"}}}}
	shape := contracts.ExtractShape("a.go", []byte("type Input struct {\n ID int\n}\n"), 1, 3)
	require.NotNil(t, shape)
	o := RebuildContractOmission{FilePath: "a.go", Type: contracts.ContractEnv, ID: "env::OMITTED", RecordKey: HeadDigest{1}}
	want := []RebuildContractBatch{{Nodes: []*graph.Node{n}}, {Edges: []*graph.Edge{e}},
		{Shapes: map[string]*contracts.Shape{"a.go::Input": shape}}, {Omitted: []RebuildContractOmission{o}}}
	stop := errors.New("stop after freeze")
	require.NoError(t, b.WithRebuildContractRecords(ctx, func(r *RebuildContractRecords) error {
		var escaped iter.Seq2[RebuildContractBatch, error]
		collect := func(add func(RebuildContractBatch) error) error {
			return add(RebuildContractBatch{Nodes: []*graph.Node{n}, Edges: []*graph.Edge{e}, Shapes: want[2].Shapes, Omitted: []RebuildContractOmission{o}})
		}
		require.ErrorIs(t, r.WithContractOutput(collect, func(read iter.Seq2[RebuildContractBatch, error], coverage ContractCoverage) error {
			escaped = read
			require.Equal(t, ContractCoverage{OmittedRecords: 1}, coverage)
			for range 2 {
				var got []RebuildContractBatch
				for row, err := range read {
					require.NoError(t, err)
					got = append(got, row)
				}
				require.Equal(t, want, got)
				got[0].Nodes[0].Meta["changed"] = true
			}
			return stop
		}), stop)
		for batch, err := range escaped {
			require.Error(t, err)
			require.Zero(t, batch)
		}
		return r.WithContractOutput(collect, func(read iter.Seq2[RebuildContractBatch, error], coverage ContractCoverage) error {
			require.Equal(t, ContractCoverage{OmittedRecords: 1}, coverage)
			var got []RebuildContractBatch
			for row, err := range read {
				require.NoError(t, err)
				got = append(got, row)
			}
			require.Equal(t, want, got)
			return nil
		})
	}))
}
