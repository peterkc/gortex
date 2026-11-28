package store_pebble

import (
	"context"
	"crypto/sha256"
	"encoding/binary"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/pathkey"
)

// MarkRebuildContractIsolationAffected records one omission for a supported
// file/type and held effective extractor. Early and late readers share this
// private set; repeat observations of a scope cannot inflate the count.
func (b *BuildStore) MarkRebuildContractIsolationAffected(ctx context.Context, repo, file string, kind contracts.ContractType, language string) error {
	return b.withWrites(func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !b.rebuildLinksIsolated || language == "" {
			return invalid("contract isolation scope outside cut or without extractor")
		}
		file = pathkey.Normalize(file)
		raw := []byte("contract isolation affected\x00")
		for _, part := range []string{repo, file, string(kind), language} {
			raw = binary.AppendUvarint(raw, uint64(len(part)))
			raw = append(raw, part...)
		}
		o := RebuildContractOmission{RepoPrefix: repo, FilePath: file, Type: kind, RecordKey: HeadDigest(sha256.Sum256(raw))}
		key, err := contractOmissionKey(o)
		if err != nil {
			return err
		}
		if !contractOmissionIDMatchesType(o) {
			return invalid("contract isolation omission type")
		}
		return b.pending.Set(rebuildHoldoutKey('o', key), []byte{contractOmissionFormat}, pebble.NoSync)
	})
}

// VisitRebuildContractIsolationAffected feeds the detached contract output
// before its freeze. Restore clears these scratch rows with the held graph.
func (b *BuildStore) VisitRebuildContractIsolationAffected(ctx context.Context, yield func(RebuildContractBatch) error) error {
	prefix := rebuildHoldoutKey('o', nil)
	return b.RebuildView().visit(ctx, prefix, func(_ *pebble.Snapshot, r row) error {
		o, err := decodeContractOmission(r.key[len(prefix):], r.value)
		if err != nil {
			return err
		}
		return yield(RebuildContractBatch{Omitted: []RebuildContractOmission{o}})
	})
}
