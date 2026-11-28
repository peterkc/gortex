package indexer

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"

	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func (r rebuildContractReceiptReader) extractorOf(file string) string {
	return r.census.byPath[r.census.originByGraphPath[file]]
}

func (r rebuildContractReceiptReader) deferredContractLanguages() ([]string, error) {
	if r.census == nil || r.census.byPath == nil || r.census.originByGraphPath == nil || uint64(len(r.census.byPath)) != r.census.admitted {
		return nil, fmt.Errorf("%w: incomplete contract extractor census", graph.ErrContractProjectionStale)
	}
	deferred := make(map[string]struct{})
	for _, id := range r.census.byPath {
		if id == "" {
			return nil, fmt.Errorf("%w: missing effective contract extractor", graph.ErrContractProjectionStale)
		}
		if !rebuildLinkedExtractor(id) {
			deferred[id] = struct{}{}
		}
	}
	isolated, err := r.linkView.RebuildLinksIsolated()
	if err != nil {
		return nil, err
	}
	if isolated {
		for _, fact := range r.job.classifications {
			if rebuildHoldsOutCheckedSkip(fact) {
				deferred[fact.Language] = struct{}{}
			}
		}
	}
	return slices.Sorted(maps.Keys(deferred)), nil
}

func (r rebuildContractReceiptReader) scopedRawContract(c contracts.Contract) contracts.Contract {
	c.RepoPrefix = r.idx.repoPrefix
	if c.WorkspaceID == "" {
		c.WorkspaceID = r.idx.workspaceID
	}
	if c.ProjectID == "" {
		c.ProjectID = r.idx.projectID
	}
	return c
}

// selectRebuildContractRecord proves the registration source, owner and late
// field consumers. The complete record and its election survive a negative
// decision. A checked source without a linked-dependence proof is not an error,
// but may not supply payload. Invalid or stale receipt evidence is an error.
func (r rebuildContractReceiptReader) selectRebuildContractRecord(ctx context.Context, view store_pebble.RebuildView,
	item store_pebble.RebuildContractRecord, owner string,
) (bool, error) {
	c := item.Contract
	if c.Type == contracts.ContractDependency && c.FilePath == "go.mod" {
		return true, nil // checked root dependency replay, before main
	}
	lang := r.extractorOf(c.FilePath)
	if lang == "" {
		return false, fmt.Errorf("%w: missing contract source %q", graph.ErrContractProjectionStale, c.FilePath)
	}
	if item.BodyUnproved {
		return false, nil
	}
	var n *graph.Node
	if owner != "" {
		var err error
		n, err = view.Node(ctx, owner)
		if err != nil {
			return false, err
		}
		if n == nil || n.RepoPrefix != c.RepoPrefix {
			return false, fmt.Errorf("%w: elected contract owner %q changed", graph.ErrContractProjectionStale, owner)
		}
	}
	if rebuildLinkedExtractor(lang) && (n == nil || rebuildLinkedExtractor(r.extractorOf(n.FilePath))) {
		return true, nil // checked source and fully linked owner; late consumers have run
	}
	if n == nil || n.FilePath != c.FilePath {
		// A missing/raw foreign owner or a moved handler may have depended
		// on the deferred link turn. Do not promote its registration owner.
		return false, nil
	}
	base, facts, err := r.forLinkedDeclaration(ctx, n)
	if err != nil {
		return false, err
	}
	if c.Type == contracts.ContractDI {
		// DI reads live Provides/Consumes. Only a checked identical parse
		// edge whose concrete endpoint is already admitted is link-free.
		for _, e := range base.Edges {
			raw, ok := diContractFromEdge(e)
			if !ok || !reflect.DeepEqual(r.scopedRawContract(raw), c) {
				continue
			}
			for _, field := range []string{graph.DIBindingBean, graph.DIBindingUseClass} {
				id, _ := c.Meta[field].(string)
				if id == "" {
					continue
				}
				target, err := view.Node(ctx, id)
				if err != nil || target == nil {
					return false, err
				}
				if _, _, err := r.forLinkedDeclaration(ctx, target); err != nil {
					return false, err
				}
			}
			return true, nil
		}
		return false, nil
	}
	// Native consumes these fields during bare-type, return-type and shape
	// passes. A deferred source needs a positive proof that none of those
	// graph-dependent consumers ran. Pure raw records (for example EnvVar
	// source and parse owner) have no such input. Router rewrites and handler
	// ownership must also be independent; require the exact checked raw row.
	for _, key := range []string{"request_type", "response_type", "response_envelope", "response_expr"} {
		if _, present := c.Meta[key]; present {
			return false, nil
		}
	}
	for _, raw := range facts.Records {
		trail, fallback, _ := contracts.ProviderHandlerHints(raw)
		if trail != "" || fallback != "" || !reflect.DeepEqual(r.scopedRawContract(raw), c) {
			continue
		}
		return true, nil
	}
	return false, nil
}

func rebuildContractRecordOmission(c contracts.Contract) store_pebble.RebuildContractOmission {
	var raw []byte
	for _, part := range []string{c.RepoPrefix, c.FilePath, c.SymbolID, c.ID, string(c.Role)} {
		raw = binary.AppendUvarint(raw, uint64(len(part)))
		raw = append(raw, part...)
	}
	raw = binary.AppendVarint(raw, int64(c.Line))
	return store_pebble.RebuildContractOmission{RepoPrefix: c.RepoPrefix, FilePath: c.FilePath,
		Type: c.Type, ID: c.ID, RecordKey: store_pebble.HeadDigest(sha256.Sum256(raw))}
}

func rebuildShapeRefs(c contracts.Contract) map[string]struct{} {
	refs := make(map[string]struct{})
	remember := func(raw any) {
		if id, _ := raw.(string); strings.Contains(id, "::") {
			refs[id] = struct{}{}
		}
	}
	for _, key := range []string{"request_type", "response_type"} {
		remember(c.Meta[key])
	}
	env, _ := c.Meta["response_envelope"].([]map[string]any)
	for _, row := range env {
		remember(row["type"])
	}
	return refs
}

// An omitted registry record can still prove an independent native shape
// reference when the same checked raw registration already names that type.
func (r rebuildContractReceiptReader) rawRebuildShapeRefs(ctx context.Context, c contracts.Contract) (map[string]struct{}, error) {
	if r.extractorOf(c.FilePath) == "" {
		return nil, nil
	}
	_, facts, err := r.forGraphPath(ctx, c.FilePath)
	if err != nil {
		return nil, err
	}
	for _, raw := range facts.Records {
		if raw.ID == c.ID && raw.FilePath == c.FilePath && raw.SymbolID == c.SymbolID && raw.Role == c.Role && raw.Line == c.Line {
			return rebuildShapeRefs(raw), nil // native first representative
		}
	}
	return nil, nil
}

func rebuildShapeOmission(o store_pebble.RebuildContractOmission, repo, file, id string) store_pebble.RebuildContractOmission {
	o.RepoPrefix, o.FilePath = repo, file
	key := append([]byte("shape\x00"), o.RecordKey[:]...)
	o.RecordKey = store_pebble.HeadDigest(sha256.Sum256(append(key, id...)))
	return o
}

func rebuildShapeCandidate(file string, kind graph.NodeKind, startLine int) bool {
	return (kind == graph.KindType || kind == graph.KindInterface) && startLine > 0 && contracts.ShapeExtractorForPath(file) != nil
}

// heldShapeFile uses held declaration attribution only to scope an omission.
func heldShapeFile(view store_pebble.RebuildView, id string) (file, language string, err error) {
	file, language, kind, startLine, err := view.RebuildHeldDeclaration(id)
	if err != nil || !rebuildShapeCandidate(file, kind, startLine) {
		return "", "", err
	}
	return file, language, nil
}
