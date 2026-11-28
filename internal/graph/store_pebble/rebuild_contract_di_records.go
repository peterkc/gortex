package store_pebble

import (
	"errors"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
)

// AppendDIContracts orders deciding edges by the native repo projection
// (from, to, kind, file, line), not the working edge key's kind-first order.
// Only mapped records are written to the borrowed ordering range.
func (r *RebuildContractRecords) AppendDIContracts(repo string, project func(*graph.Edge) (contracts.Contract, bool)) (err error) {
	if err := r.ready(); err != nil {
		return err
	}
	prefix := rebuildContractRecordKey('d')
	defer func() {
		err = errors.Join(err, r.build.pending.DeleteRange(prefix, prefixEnd(prefix), pebble.NoSync))
	}()
	if err := r.build.RebuildView().VisitDIContractEdges(r.ctx, repo, func(e *graph.Edge) error {
		c, ok := project(e)
		if !ok {
			return nil
		}
		key := appendKeyLine(rebuildContractRecordKey('d', e.From, e.To, string(e.Kind), e.FilePath), e.Line)
		value := contractFactsEncoder{}
		writeFactRecord(&value, c)
		if value.err != nil {
			return value.err
		}
		return r.build.putRebuildScratch(key, value.buf, goldBufferBytes, "contract DI record")
	}); err != nil {
		return err
	}
	return pendingVisit(r.ctx, r.build.pending, prefix, func(s row) error {
		d := recordCursor{buf: s.value}
		c := readFactRecord(&d)
		if err := d.finish(); err != nil {
			return err
		}
		return r.Add(RebuildContractRecord{Contract: c})
	})
}
