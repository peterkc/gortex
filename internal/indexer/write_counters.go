package indexer

import (
	"github.com/zzet/gortex/internal/storage"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// These are process-interval activity counters, not exclusive per-edit costs.
// Overlapping patches/background writers contribute to each enclosing interval;
// nested phases must not be summed with their parent totals.
func writeCounterFields(prefix string, d storage.VFSIOSplit) []zap.Field {
	fields := []zap.Field{
		zap.Int64(prefix+"wal_bytes", d.WALBytes()),
		zap.Int64(prefix+"main_bytes", d.WriterMainWriteBytes+d.OtherMainWriteBytes),
		zap.Bool(prefix+"wal_bytes_reset", d.WALReset),
		zap.Int64(prefix+"rows", d.Rows.Total()), zap.Int64(prefix+"txns", d.Transactions),
		zap.Int64(prefix+"node_rows", d.Rows.Nodes), zap.Int64(prefix+"edge_rows", d.Rows.Edges),
		zap.Int64(prefix+"symbol_fts_rows", d.Rows.SymbolFTS), zap.Int64(prefix+"content_fts_rows", d.Rows.ContentFTS),
	}
	// Suppress small rebind noise; piggyback on the existing phase/patch line.
	r := d.Reindex
	if r.Updated+r.Deleted+r.Inserted >= 100 {
		reason := "none"
		if r.Generic > 0 {
			reason = "not_uniform_namespace_conversion"
		}
		if r.ReceiptFallback > 0 {
			if r.Generic > 0 {
				reason = "not_uniform_namespace_conversion,receipt_reverse_conversion"
			} else {
				reason = "receipt_reverse_conversion"
			}
		}
		fields = append(fields, zap.Int64(prefix+"reindex_updated", r.Updated), zap.Int64(prefix+"reindex_deleted", r.Deleted), zap.Int64(prefix+"reindex_inserted", r.Inserted), zap.Bool(prefix+"reindex_fast_path", r.FastPath > 0 && r.Generic == 0 && r.ReceiptFallback == 0), zap.String(prefix+"reindex_fallback", reason))
	}
	return fields
}

type patchWriteLog struct {
	split       storage.VFSIOSplit
	unavailable bool
}

func (m patchWriteLog) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	if m.unavailable {
		enc.AddBool("write_counters_unavailable", true)
		return nil
	}
	for _, f := range writeCounterFields("", m.split) {
		f.AddTo(enc)
	}
	return nil
}

// writeCounterMark samples an installed source; callers check presence separately.
func (idx *Indexer) writeCounterMark() storage.VFSIOMark {
	return idx.writeCounters()
}

func patchWriteLogSince(sample func() storage.VFSIOMark, start storage.VFSIOMark) patchWriteLog {
	if sample == nil {
		return patchWriteLog{unavailable: true}
	}
	return patchWriteLog{split: sample().Since(start)}
}

func (mi *MultiIndexer) writeCounterSource() func() storage.VFSIOMark {
	if mi.writeCounters != nil {
		return mi.writeCounters
	}
	if source, ok := mi.graph.(interface{ WriteMark() storage.VFSIOMark }); ok {
		return source.WriteMark
	}
	return nil
}
func (idx *Indexer) startReconcilePhase(logger *zap.Logger, repo, phase string, fields ...zap.Field) *reconcilePhaseTiming {
	return startReconcilePhaseWithWrites(idx.writeCounters, logger, repo, phase, fields...)
}
func (mi *MultiIndexer) startReconcilePhase(logger *zap.Logger, repo, phase string, fields ...zap.Field) *reconcilePhaseTiming {
	return startReconcilePhaseWithWrites(mi.writeCounterSource(), logger, repo, phase, fields...)
}
