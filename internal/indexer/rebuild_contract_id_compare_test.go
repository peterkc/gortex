package indexer

import (
	"errors"
	"iter"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCompareRebuildContractIDs(t *testing.T) {
	ids := func(keys ...string) iter.Seq2[string, error] {
		return func(yield func(string, error) bool) {
			for _, key := range keys {
				if !yield(key, nil) {
					return
				}
			}
		}
	}
	const deferred, retained = "env::DEFERRED", "env::RETAINED"
	private := ids(deferred, retained)
	published := ids(retained)
	for _, tc := range []struct {
		name     string
		expected iter.Seq2[string, error]
		actual   iter.Seq2[string, error]
		wantErr  string
	}{
		{name: "built-empty", expected: ids(), actual: ids()},
		{name: "unbuilt", expected: nil, actual: ids(), wantErr: "missing id stream"},
		{name: "private equality with deferred record", expected: private, actual: ids(deferred, retained)},
		{name: "private loses deferred record", expected: private, actual: published, wantErr: `missing id "env::DEFERRED"`},
		{name: "published equality without deferred record", expected: published, actual: ids(retained)},
		{name: "published extra deferred record", expected: published, actual: private, wantErr: `extra id "env::DEFERRED"`},
		{name: "extra at end", expected: ids(deferred), actual: private, wantErr: `extra id "env::RETAINED"`},
		{name: "missing at end", expected: private, actual: ids(deferred), wantErr: `missing id "env::RETAINED"`},
		{name: "same broken order is not equality", expected: ids(retained, deferred), actual: ids(retained, deferred), wantErr: "out of order"},
		{name: "same duplicate is not equality", expected: ids(retained, retained), actual: ids(retained, retained), wantErr: "repeated"},
		{name: "duplicate only in actual is extra", expected: private, actual: ids(deferred, deferred), wantErr: `extra id "env::DEFERRED"`},
		{name: "empty id is not built-empty", expected: ids(""), actual: ids(""), wantErr: "empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := compareRebuildContractIDs(t.Context(), tc.expected, tc.actual)
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorIs(t, err, errRebuildContractIDs)
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}

func TestCompareRebuildContractIDsReadFailureStopsBothStreams(t *testing.T) {
	readErr := errors.New("saved id reader failed")
	var expectedClosed, actualClosed bool
	expected := func(yield func(string, error) bool) {
		defer func() { expectedClosed = true }()
		if !yield("env::KEY", nil) {
			return
		}
		yield("env::NEXT", nil)
	}
	actual := func(yield func(string, error) bool) {
		defer func() { actualClosed = true }()
		if !yield("env::KEY", nil) {
			return
		}
		yield("", readErr)
	}
	err := compareRebuildContractIDs(t.Context(), expected, actual)
	require.ErrorIs(t, err, errRebuildContractIDs)
	require.ErrorIs(t, err, readErr)
	require.True(t, expectedClosed)
	require.True(t, actualClosed)
}
