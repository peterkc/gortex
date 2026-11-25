package resolver

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/storage"
)

func TestWriteCounterSourcePhaseLogs(t *testing.T) {
	for _, source := range []string{"nil-graph", "memory-graph", "supplied-zero", "supplied-vfs", "supplied-wal-fallback"} {
		t.Run(source, func(t *testing.T) {
			core, logs := observer.New(zap.InfoLevel)
			logger := zap.New(core)
			r := &Resolver{}
			if source != "nil-graph" {
				r = New(graph.New())
			}
			mark := storage.VFSIOMark{}
			calls := 0
			present := source != "nil-graph" && source != "memory-graph"
			if present {
				r.SetWriteCounterSource(func() storage.VFSIOMark { calls++; return mark })
			}
			if source == "supplied-vfs" {
				mark.Available = true
			}
			if source == "supplied-wal-fallback" {
				mark.FallbackWAL = storage.WALWriteMark{Valid: true, MxFrame: 5, PageSize: 4096}
			}
			finish := r.startIncrementalPhase(logger, "test")
			if source != "supplied-zero" {
				mark.Rows.Nodes = 3
				mark.Transactions = 2
				mark.WriteBytes[2][1] = 123
				mark.FallbackWAL.MxFrame += 3
			}
			finish()
			require.Len(t, logs.All(), 2)
			keys := []string{"phase_wal_bytes", "phase_main_bytes", "phase_rows", "phase_txns", "phase_wal_bytes_reset"}
			for i, entry := range logs.All() {
				fields := entry.ContextMap()
				if !present {
					require.Equal(t, true, fields["phase_write_counters_unavailable"])
					markers := 0
					for _, field := range entry.Context {
						if field.Key == "phase_write_counters_unavailable" {
							markers++
						}
					}
					require.Equal(t, 1, markers)
					for _, key := range keys {
						require.NotContains(t, fields, key)
					}
					continue
				}
				require.NotContains(t, fields, "phase_write_counters_unavailable")
				for _, key := range keys {
					require.Contains(t, fields, key)
				}
				rows, txns, wal := int64(0), int64(0), int64(0)
				if i == 1 && source != "supplied-zero" {
					rows, txns, wal = 3, 2, 123
					if source == "supplied-wal-fallback" {
						wal = 3 * (4096 + 24)
					}
				}
				require.Equal(t, rows, fields["phase_rows"])
				require.Equal(t, txns, fields["phase_txns"])
				require.Equal(t, wal, fields["phase_wal_bytes"])
			}
			if present {
				require.Equal(t, 2, calls)
			} else {
				require.Zero(t, calls)
			}
		})
	}
}

func TestWriteCounterNativeSourceAndRetargetToUnavailable(t *testing.T) {
	for _, fallback := range []bool{false, true} {
		t.Run(map[bool]string{false: "native", true: "native-wal-fallback"}[fallback], func(t *testing.T) {
			store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "counter.sqlite"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, store.Close()) })
			r := New(store)
			if fallback {
				r.SetWriteCounterSource(func() storage.VFSIOMark {
					m := store.WriteMark()
					m.Available = false
					m.FallbackWAL = store.WALWriteMark()
					return m
				})
			}
			core, logs := observer.New(zap.InfoLevel)
			logger := zap.New(core)
			finish := r.startIncrementalPhase(logger, "native")
			store.AddBatch([]*graph.Node{{ID: "counted", Name: "Counted", Kind: graph.KindFunction}}, nil)
			finish()
			require.Len(t, logs.All(), 2)
			for _, entry := range logs.All() {
				require.NotContains(t, entry.ContextMap(), "phase_write_counters_unavailable")
			}
			require.Equal(t, int64(0), logs.All()[0].ContextMap()["phase_rows"])
			fields := logs.All()[1].ContextMap()
			require.GreaterOrEqual(t, fields["phase_rows"].(int64), int64(1))
			require.Greater(t, fields["phase_txns"].(int64), int64(0))
			require.Greater(t, fields["phase_wal_bytes"].(int64), int64(0))
			r.SetWriteCounterSource(nil)
			r.SetGraph(graph.New())
			r.startIncrementalPhase(logger, "unavailable")()
			for _, entry := range logs.All()[2:] {
				require.Equal(t, true, entry.ContextMap()["phase_write_counters_unavailable"])
				require.NotContains(t, entry.ContextMap(), "phase_rows")
			}
		})
	}
}
