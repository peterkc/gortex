package indexer

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

type closingEvidenceGraph struct {
	graph.Store
	closed bool
}

func (g *closingEvidenceGraph) Close() error { g.closed = true; return nil }

func TestContractEvidenceRefusesMissingOpener(t *testing.T) {
	parent := t.TempDir()
	c := &contractConvergence{ctx: t.Context(), req: ContractFollowupRequest{ScratchParent: parent}, report: &ContractFollowupReport{}}
	evidence, scratch, dir, err := c.openEvidence()
	require.EqualError(t, err, "contract followup: evidence opener unavailable")
	require.Nil(t, evidence)
	require.Nil(t, scratch)
	require.Empty(t, dir)
	entries, readErr := os.ReadDir(parent)
	require.NoError(t, readErr)
	require.Empty(t, entries)
}

func TestContractEvidenceRefusesStoreWithoutEvidenceCapabilities(t *testing.T) {
	for _, closeable := range []bool{false, true} {
		t.Run(map[bool]string{false: "memory_graph", true: "closeable_graph"}[closeable], func(t *testing.T) {
			parent := t.TempDir()
			candidate := &closingEvidenceGraph{Store: graph.New()}
			result := candidate.Store
			if closeable {
				result = candidate
			}
			c := &contractConvergence{ctx: t.Context(), req: ContractFollowupRequest{ScratchParent: parent, OpenEvidence: func(string) (graph.Store, error) { return result, nil }}, report: &ContractFollowupReport{}}
			evidence, scratch, dir, err := c.openEvidence()
			require.EqualError(t, err, "contract followup: evidence store lacks required capabilities")
			require.Nil(t, evidence)
			require.Nil(t, scratch)
			require.Empty(t, dir)
			require.Equal(t, closeable, candidate.closed)
			entries, readErr := os.ReadDir(parent)
			require.NoError(t, readErr)
			require.Empty(t, entries)
		})
	}
}
