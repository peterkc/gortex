package indexer

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/storage"
)

func TestWriteCounterSourcePhaseLogs(t *testing.T) {
	for _, actor := range []string{"indexer", "multi-indexer"} {
		for _, source := range []string{"nil-graph", "memory-graph", "supplied-zero", "supplied-vfs", "supplied-wal-fallback"} {
			for _, outcome := range []string{"complete", "error", "aborted"} {
				t.Run(actor+"/"+source+"/"+outcome, func(t *testing.T) {
					core, logs := observer.New(zap.InfoLevel)
					logger := zap.New(core)
					var g graph.Store
					if source != "nil-graph" {
						g = graph.New()
					}
					mark := storage.VFSIOMark{}
					var sample func() storage.VFSIOMark
					calls := 0
					present := source != "nil-graph" && source != "memory-graph"
					if present {
						sample = func() storage.VFSIOMark { calls++; return mark }
					}
					if source == "supplied-vfs" {
						mark.Available = true
					}
					if source == "supplied-wal-fallback" {
						mark.FallbackWAL = storage.WALWriteMark{Valid: true, MxFrame: 5, PageSize: 4096}
					}
					idx := &Indexer{graph: g}
					mi := &MultiIndexer{graph: g}
					if g != nil {
						idx = New(g, nil, config.IndexConfig{}, logger)
						mi = NewMultiIndexer(g, nil, nil, nil, logger)
					}
					idx.writeCounters, mi.writeCounters = sample, sample
					var timer *reconcilePhaseTiming
					if actor == "indexer" {
						timer = idx.startReconcilePhase(logger, "repo", "test")
					} else {
						timer = mi.startReconcilePhase(logger, "repo", "test")
					}
					if source == "supplied-vfs" || source == "supplied-wal-fallback" {
						mark.Rows.Nodes = 3
						mark.Transactions = 2
						mark.WriteBytes[2][1] = 123
					}
					if source == "supplied-wal-fallback" {
						mark.FallbackWAL.MxFrame += 3
					}
					switch outcome {
					case "complete":
						timer.complete(nil)
					case "error":
						timer.complete(errors.New("test failure"))
					case "aborted":
						timer.abort()
					}
					timer.abort() // A deferred abort must not emit or sample twice.
					require.Len(t, logs.All(), 2)
					keys := []string{"phase_wal_bytes", "phase_main_bytes", "phase_wal_bytes_reset", "phase_rows", "phase_txns", "phase_node_rows", "phase_edge_rows", "phase_symbol_fts_rows", "phase_content_fts_rows"}
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
							rows, txns = 3, 2
							wal = 123
							if source == "supplied-wal-fallback" {
								wal = 3 * (4096 + 24)
							}
						}
						require.Equal(t, rows, fields["phase_rows"])
						require.Equal(t, txns, fields["phase_txns"])
						require.Equal(t, wal, fields["phase_wal_bytes"])
					}
					require.Equal(t, outcome, logs.All()[1].ContextMap()["outcome"])
					if present {
						require.Equal(t, 2, calls)
					} else {
						require.Zero(t, calls)
					}
				})
			}
		}
	}
}

func TestWriteCounterNativeSourcePhaseLogs(t *testing.T) {
	for _, actor := range []string{"indexer", "multi-indexer"} {
		for _, fallback := range []bool{false, true} {
			t.Run(actor+map[bool]string{false: "/native", true: "/native-wal-fallback"}[fallback], func(t *testing.T) {
				idx, store := newSQLiteIndexer(t)
				core, logs := observer.New(zap.InfoLevel)
				logger := zap.New(core)
				mi := NewMultiIndexer(store, nil, nil, nil, logger)
				if fallback {
					// Read the real native WAL meter with physical VFS counters unavailable.
					sample := func() storage.VFSIOMark {
						m := store.WriteMark()
						m.Available = false
						m.FallbackWAL = store.WALWriteMark()
						return m
					}
					idx.writeCounters, mi.writeCounters = sample, sample
				}
				var timer *reconcilePhaseTiming
				if actor == "indexer" {
					timer = idx.startReconcilePhase(logger, "repo", "native")
				} else {
					timer = mi.startReconcilePhase(logger, "repo", "native")
				}
				store.AddBatch([]*graph.Node{{ID: "counted", Name: "Counted", Kind: graph.KindFunction}}, nil)
				timer.complete(nil)
				require.Len(t, logs.All(), 2)
				for _, entry := range logs.All() {
					require.NotContains(t, entry.ContextMap(), "phase_write_counters_unavailable")
				}
				require.Equal(t, int64(0), logs.All()[0].ContextMap()["phase_rows"])
				fields := logs.All()[1].ContextMap()
				require.GreaterOrEqual(t, fields["phase_rows"].(int64), int64(1))
				require.Greater(t, fields["phase_txns"].(int64), int64(0))
				require.Greater(t, fields["phase_wal_bytes"].(int64), int64(0))
			})
		}
	}
}

func TestWriteCounterUnavailableAuxiliaryLogs(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	logger := zap.New(core)
	idx := &Indexer{graph: graph.New()}
	start := idx.applyLapSplitMark()
	logger.Info("lap", applyLapSplitFields("apply", start, idx.applyLapSplitMark())...)
	logger.Info("patch", zap.Inline(patchWriteLogSince(nil, storage.VFSIOMark{})))
	legs := newRefFactLegs(nil)
	legs.lap("nodes")
	logger.Info("legs", legs.fields(1)...)
	require.Len(t, logs.All(), 3)
	require.Equal(t, true, logs.All()[0].ContextMap()["apply_write_counters_unavailable"])
	require.NotContains(t, logs.All()[0].ContextMap(), "apply_rows")
	for _, entry := range logs.All()[1:] {
		require.Equal(t, true, entry.ContextMap()["write_counters_unavailable"])
		require.NotContains(t, entry.ContextMap(), "rows")
		require.NotContains(t, entry.ContextMap(), "nodes_rows")
	}
}

func TestWriteCounterStandaloneDerivedUsesParentSource(t *testing.T) {
	_, store := newSQLiteIndexer(t)
	for _, present := range []bool{false, true} {
		idx := New(graph.NewDeltaWriter(graph.New(), nil), nil, config.IndexConfig{}, zap.NewNop())
		idx.repoPrefix = "repo"
		calls := 0
		if present {
			idx.writeCounters = func() storage.VFSIOMark { calls++; return store.WriteMark() }
		}
		report := idx.runStandaloneIncrementalDerivedPasses(DerivedInvalidationPlan{Files: []string{"repo/a.go"}, BodyOnlyFiles: 1})
		require.Equal(t, 1, report.Repos)
		if present {
			require.GreaterOrEqual(t, calls, 2)
			require.NotNil(t, report.PassWrites)
		} else {
			require.Zero(t, calls)
			require.Nil(t, report.PassWrites)
		}
	}
}
