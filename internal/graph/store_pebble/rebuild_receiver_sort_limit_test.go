package store_pebble

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReceiverSortRunLimitCause(t *testing.T) {
	// A full path list is sufficient: flush must refuse before opening a
	// file. The receiver can distinguish this from disk and merge failures.
	runs := sortedRuns{name: "claims", rows: []row{{key: []byte("claim")}}, paths: make([]string, goldFileCap)}
	err := runs.flush()
	var limit *sortedRunLimitError
	require.ErrorAs(t, err, &limit)
	require.EqualError(t, err, "pebble claims exceeds 1024 sorted runs")
}
