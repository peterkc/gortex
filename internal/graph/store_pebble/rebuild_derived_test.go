package store_pebble

import (
	"bytes"
	"encoding/gob"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRebuildDeferredDerivedWitness(t *testing.T) {
	for _, gitPolicy := range []bool{false, true} {
		for _, coverage := range []*RelationCoverage{nil, {}, {DeferredLanguages: []string{"python"}}} {
			prior := StageTables{Incarnation: testHeadIncarnation, Witness: Witness{
				Stage: StageRelations, Profile: FullProfile, Input: HeadDigest{1}, Recipe: HeadDigest{2},
				FrozenInputs: HeadDigest{3}, UsesGitIgnorePolicy: gitPolicy, Relations: coverage,
			}}
			derived, err := PrepareDeferredDerived(t.TempDir(), prior)
			require.NoError(t, err)
			w := derived.Witness
			require.Equal(t, prior.Witness.Version(), w.Version())
			raw, err := encodeWitness(w)
			require.NoError(t, err)
			require.Equal(t, witnessD64Format, raw[0])
			got, err := decodeWitness(raw)
			require.NoError(t, err)
			require.Equal(t, w, got)
			var transport bytes.Buffer
			require.NoError(t, gob.NewEncoder(&transport).Encode(w))
			var received Witness
			require.NoError(t, gob.NewDecoder(&transport).Decode(&received))
			require.Equal(t, w, received)

			_, err = decodeWitness(raw[:len(raw)-1])
			require.ErrorIs(t, err, errInvalidRecord)
			bad := w
			bad.Unsupported = slices.Clone(w.Unsupported)
			bad.Unsupported[0].Reason = "complete"
			_, err = encodeWitness(bad)
			require.ErrorIs(t, err, errInvalidRecord)
			bad = w
			bad.Stage = StageRelations
			_, err = encodeWitness(bad)
			require.ErrorIs(t, err, errInvalidRecord)
		}
	}
}
