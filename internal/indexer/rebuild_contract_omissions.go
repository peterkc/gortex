package indexer

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"maps"
	"slices"
	"strings"

	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/pathkey"
)

// checkedSpringConfigSizeSkip binds this exact graph path and repository to
// the owner's validated literal classification, not to a KindFile stub.
func (r rebuildContractReceiptReader) checkedSpringConfigSizeSkip(repo, file string) bool {
	if r.idx == nil || r.census == nil || repo != r.idx.repoPrefix || !r.sizeSkippedContractSource(file) {
		return false
	}
	_, profile := contracts.SpringConfigProfile(file)
	return profile
}

// visitHeldOutContractOmissions inventories checked raw observations only.
// It must not resolve endpoint questions, elect registry/DI/config winners,
// run native late transforms or expose these receipts to any link reader.
func (r rebuildContractReceiptReader) visitHeldOutContractOmissions(ctx context.Context,
	hasSelectedShapeRef func(string) (bool, error), yield func(store_pebble.RebuildContractBatch) error,
) error {
	isolated, err := r.linkView.RebuildLinksIsolated()
	if err != nil || !isolated {
		return err
	}
	scope := contracts.SpringConfigScope{RepoPrefix: r.idx.repoPrefix, WorkspaceID: r.idx.workspaceID}
	for _, path := range slices.Sorted(maps.Keys(r.job.classifications)) {
		fact := r.job.classifications[path]
		file := pathkey.Normalize(path)
		if rebuildHoldsOutCheckedSkip(fact) && r.checkedSpringConfigSizeSkip(scope.RepoPrefix, file) {
			if err := yield(store_pebble.RebuildContractBatch{Omitted: []store_pebble.RebuildContractOmission{
				store_pebble.RebuildSpringConfigUnknownOmission(scope.RepoPrefix, file),
			}}); err != nil {
				return err
			}
		}
	}
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
		addRecord := func(c contracts.Contract, o store_pebble.RebuildContractOmission) error {
			if err := add(o); err != nil {
				return err
			}
			refs := rebuildShapeRefs(c)
			for _, key := range [...]string{"request_type", "response_type"} {
				name, _ := c.Meta[key].(string)
				if name == "" || strings.Contains(name, "::") {
					continue
				}
				id, err := r.heldRecordBareShapeID(ctx, scope.RepoPrefix, name)
				if err != nil {
					return err
				}
				if strings.Contains(id, "::") {
					refs[id] = struct{}{}
				}
			}
			for _, id := range slices.Sorted(maps.Keys(refs)) {
				file, err := r.heldRecordShapeFile(ctx, id, hasSelectedShapeRef)
				if err != nil {
					return err
				}
				if file != "" {
					if err := add(rebuildShapeOmission(o, scope.RepoPrefix, file, id)); err != nil {
						return err
					}
				}
			}
			return nil
		}
		for _, raw := range facts.Records {
			o := rebuildContractRecordOmission(r.scopedRawContract(raw))
			if local, _ := contracts.RouterPrefixFactUse(raw, facts.Router); local {
				o.ID = "" // the final route ID needs an excluded reducer
			}
			if err := addRecord(raw, o); err != nil {
				return err
			}
		}
		for i, q := range facts.Questions {
			o := rebuildContractRecordOmission(r.scopedRawContract(q.Record))
			o.ID = "" // the final ID (if any) needs endpoint lookup
			key := append([]byte("held endpoint question\x00"), o.RecordKey[:]...)
			o.RecordKey = store_pebble.HeadDigest(sha256.Sum256(binary.AppendUvarint(key, uint64(i))))
			if err := addRecord(q.Record, o); err != nil {
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
			if !contracts.SpringReaderKind(n.Kind) {
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

// heldRecordBareShapeID attributes a possible omitted shape without upgrading
// a record or handing a held candidate to the linking pass. An ambiguous
// deciding tier cannot name a shape. Count non-shape-capable KindTypes too.
func (r rebuildContractReceiptReader) heldRecordBareShapeID(ctx context.Context, repo, name string) (string, error) {
	type tier struct {
		id    string
		count int
	}
	var same, fallback tier
	observe := func(id string) error {
		vote := &fallback
		if repo != "" && strings.HasPrefix(id, repo+"/") {
			vote = &same
		}
		vote.id, vote.count = id, min(vote.count+1, 2)
		return nil
	}
	if err := r.linkView.VisitNameCandidates(ctx, name, nil, func(n *graph.Node) error {
		if n.Kind == graph.KindType {
			return observe(n.ID)
		}
		return nil
	}); err != nil {
		return "", err
	}
	if err := r.linkView.VisitRebuildHeldNameIDs(ctx, name, graph.KindType, observe); err != nil {
		return "", err
	}
	if same.count == 0 {
		same = fallback
	}
	if same.count != 1 {
		return "", nil
	}
	return same.id, nil
}

func (r rebuildContractReceiptReader) heldRecordShapeFile(ctx context.Context, id string, hasSelectedShapeRef func(string) (bool, error)) (string, error) {
	if file, _, err := heldShapeFile(r.linkView, id); err != nil || file != "" {
		return file, err
	}
	selected, err := hasSelectedShapeRef(id)
	if err != nil || selected {
		return "", err // a selected record already supplies this shape lookup
	}
	n, err := r.linkView.Node(ctx, id)
	if err != nil || n == nil || !rebuildShapeCandidate(n.FilePath, n.Kind, n.StartLine) {
		return "", err
	}
	return n.FilePath, nil
}
