package indexer

import (
	"context"
	"crypto/sha256"
	"encoding/binary"

	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// visitHeldOutContractOmissions inventories checked raw observations only.
// It must not resolve endpoint questions, elect registry/DI/config winners,
// run native late transforms or expose these receipts to any link reader.
func (r rebuildContractReceiptReader) visitHeldOutContractOmissions(ctx context.Context, yield func(store_pebble.RebuildContractBatch) error) error {
	if !r.isolate {
		return nil
	}
	scope := contracts.SpringConfigScope{RepoPrefix: r.idx.repoPrefix, WorkspaceID: r.idx.workspaceID}
	for _, input := range r.job.inputs {
		held, err := r.holdsOut(input.Path)
		if err != nil {
			return err
		}
		if !held {
			continue
		}
		base, facts, err := r.loadRaw(ctx, input)
		if err != nil {
			return err
		}
		seen := make(map[store_pebble.RebuildContractOmission]struct{})
		add := func(o store_pebble.RebuildContractOmission) error {
			if _, ok := seen[o]; ok {
				return nil
			}
			seen[o] = struct{}{}
			return yield(store_pebble.RebuildContractBatch{Omitted: []store_pebble.RebuildContractOmission{o}})
		}
		for _, raw := range facts.Records {
			o := rebuildContractRecordOmission(r.scopedRawContract(raw))
			if local, _ := contracts.RouterPrefixFactUse(raw, facts.Router); local {
				o.ID = "" // the final route ID needs an excluded reducer
			}
			if err := add(o); err != nil {
				return err
			}
		}
		for i, q := range facts.Questions {
			o := rebuildContractRecordOmission(r.scopedRawContract(q.Record))
			o.ID = "" // the final ID (if any) needs endpoint lookup
			key := append([]byte("held endpoint question\x00"), o.RecordKey[:]...)
			o.RecordKey = store_pebble.HeadDigest(sha256.Sum256(binary.AppendUvarint(key, uint64(i))))
			if err := add(o); err != nil {
				return err
			}
		}
		for _, e := range base.Edges {
			if c, ok := diContractFromEdge(e); ok {
				if err := add(rebuildContractRecordOmission(r.scopedRawContract(c))); err != nil {
					return err
				}
			}
		}
		for _, n := range facts.ConfigKeys {
			// The key identity is known without electing a config-file winner.
			c := contracts.Contract{ID: scope.KeyID(n.Name), Type: contracts.ContractType(graph.EdgeReadsConfig), FilePath: n.FilePath, Line: n.StartLine}
			if err := add(rebuildContractRecordOmission(r.scopedRawContract(c))); err != nil {
				return err
			}
		}
		for _, n := range base.Nodes {
			switch n.Kind {
			case graph.KindField, graph.KindType, graph.KindInterface, graph.KindMethod:
			default:
				continue
			}
			for _, raw := range contracts.SpringConfigKeysOf(n) {
				if _, wildcard := contracts.SpringConfigWildcardPrefix(raw); wildcard {
					if err := add(store_pebble.RebuildSpringConfigHeldOutOmission(scope.RepoPrefix, n.FilePath)); err != nil {
						return err
					}
					continue
				}
				id := scope.KeyID(raw)
				if err := add(store_pebble.RebuildContractEdgeOmission(scope.RepoPrefix, contracts.ContractType(graph.EdgeReadsConfig), id, contracts.SpringConfigReadEdge(n, id, raw))); err != nil {
					return err
				}
			}
		}
	}
	return ctx.Err()
}
