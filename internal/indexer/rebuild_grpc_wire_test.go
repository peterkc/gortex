package indexer

import (
	"bytes"
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func TestRebuildGRPCWireRefusals(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cause error
		want  error
		code  uint8
		count uint64
	}{
		{"stage", store_pebble.ErrRebuildGRPC, store_pebble.ErrRebuildGRPC, 42, 0},
		{"provenance", &store_pebble.RebuildUnknownProvenanceError{Count: 2}, store_pebble.ErrRebuildUnknownProvenance, 36, 2},
		{"input size", store_pebble.ErrRebuildLinkInputSize, store_pebble.ErrRebuildLinkInputSize, 17, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cause := fmt.Errorf("%w: %w", store_pebble.ErrRebuildGRPC, tc.cause)
			r := buildRefusal(cause)
			require.Equal(t, tc.code, r.Refusal)
			require.Equal(t, tc.count, r.UnknownProvenance)
			if tc.count > 0 {
				require.Contains(t, r.Detail, "count=2")
			}
			var reply, acks bytes.Buffer
			require.NoError(t, writeRebuildBuildReply(&reply, r))
			err := receiveRebuildStages(t.Context(), t.TempDir(), rebuildBuildRequest{}, &reply, &acks,
				func(context.Context, store_pebble.StageTables) error {
					t.Fatal("a refusal must not publish")
					return nil
				})
			require.ErrorIs(t, err, ErrRebuildWorker)
			require.ErrorIs(t, err, tc.want)
			if tc.count > 0 {
				require.ErrorContains(t, err, "unknown_provenance=2")
			}
			require.Empty(t, acks.Bytes())
		})
	}
}
