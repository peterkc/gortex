package indexer

import (
	"context"
	"slices"
	"strings"
	"sync"
	"unsafe"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graphview"
)

// contractInventoryProjection belongs to one captured job, with its existing
// borrowed selected readers/handles. It is never placed in the actor cache.
// Only the selected file projection is shared across full scans; receipt
// windows, exact input witnesses and work proof are still scanned as before.
// A changed/unknown revision takes the original checked physical read.
type contractInventoryProjection struct {
	mu      sync.Mutex
	entries map[string]*contractInventoryProjectionEntry
	bytes   int64
}

type contractInventoryProjectionEntry struct {
	trusted       bool
	physicalReads int
	core          graph.Reader
	handles       []*store_sqlite.Store
	revisions     []store_sqlite.PayloadInputRevision
	rows          map[string]graph.FileMetaRow
	bytes         int64
}

func newContractInventoryProjection(readers map[string]graph.Reader, handles map[string][]*store_sqlite.Store) *contractInventoryProjection {
	p := &contractInventoryProjection{entries: make(map[string]*contractInventoryProjectionEntry, len(readers))}
	for repo, reader := range readers {
		p.entries[repo] = &contractInventoryProjectionEntry{core: reader, handles: slices.Clone(handles[repo]), trusted: contractInventoryCompositionCovered(reader, handles[repo])}
	}
	return p
}

func contractInventoryRevisions(ctx context.Context, handles []*store_sqlite.Store) ([]store_sqlite.PayloadInputRevision, error) {
	out := make([]store_sqlite.PayloadInputRevision, len(handles))
	for i, handle := range handles {
		var err error
		out[i], err = handle.PayloadInputRevisionContext(ctx)
		if err != nil {
			return nil, err
		}
	}
	return out, ctx.Err()
}

// read preserves physical errors and their original ordering. Nothing is cached
// when a selected mutation overlaps capture, admission exceeds the existing
// compact envelope, or the context has failed. A cache hit is not an input-state
// witness: the caller's scanner must still validate its selected receipt vector.
func (p *contractInventoryProjection) read(ctx context.Context, repo string) (map[string]graph.FileMetaRow, error) {
	return p.readSnapshot(ctx, repo, nil)
}

func (p *contractInventoryProjection) readSnapshot(ctx context.Context, repo string, afterCapture func()) (map[string]graph.FileMetaRow, error) {
	if ctx == nil {
		return nil, graph.ErrContractProjectionIncomplete
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	entry := p.entries[repo]
	if entry == nil || entry.core == nil || len(entry.handles) == 0 {
		return nil, graph.ErrContractInputVector
	}
	before, err := contractInventoryRevisions(ctx, entry.handles)
	if err != nil {
		return nil, err
	}
	if entry.trusted && entry.rows != nil && slices.Equal(before, entry.revisions) {
		return cloneContractInventoryRows(entry.rows), ctx.Err()
	}
	p.bytes -= entry.bytes
	entry.rows, entry.revisions, entry.bytes = nil, nil, 0
	entry.physicalReads++
	rows, err := captureAcceptedContractInventory(ctx, entry.core, entry.handles, repo)
	if err != nil {
		return nil, err
	}
	if afterCapture != nil {
		afterCapture()
	}
	after, err := contractInventoryRevisions(ctx, entry.handles)
	if err != nil {
		return nil, err
	}
	if entry.trusted && slices.Equal(before, after) {
		charge := contractInventoryRowsCharge(rows) + contractDeltaAllocation(int64(cap(after))*int64(unsafe.Sizeof(store_sqlite.PayloadInputRevision{})))
		if charge <= int64(contractFollowupCompactLimit)-p.bytes {
			entry.rows = cloneContractInventoryRows(rows)
			entry.revisions = after
			entry.bytes = charge
			p.bytes += charge
		}
	}
	return rows, ctx.Err()
}

func contractInventoryRowsCharge(rows map[string]graph.FileMetaRow) int64 {
	// Conservative map/entry overhead plus rounded owned strings. Count the
	// FilePath key separately from its value even though they could be shared.
	size := int64(256) + int64(len(rows))*(int64(unsafe.Sizeof(graph.FileMetaRow{}))+int64(unsafe.Sizeof(""))+64)
	for key, row := range rows {
		for _, value := range []string{key, row.FilePath, row.ContentHash, row.Errors} {
			size += contractDeltaAllocation(int64(len(value)))
		}
	}
	return size
}

func cloneContractInventoryRows(rows map[string]graph.FileMetaRow) map[string]graph.FileMetaRow {
	out := make(map[string]graph.FileMetaRow, len(rows))
	for key, row := range rows {
		row.FilePath, row.ContentHash, row.Errors = strings.Clone(row.FilePath), strings.Clone(row.ContentHash), strings.Clone(row.Errors)
		out[strings.Clone(key)] = row
	}
	return out
}

// Opaque/custom wrappers may change a selected answer without advancing these
// physical clocks. Never unwrap one merely because it exposes a borrowed core.
// The actual built-in overlay composition must consist only of the supplied
// physical stores and generation layers; an editor/memory layer falls back.
func contractInventoryCompositionCovered(reader graph.Reader, handles []*store_sqlite.Store) bool {
	covered := func(h *store_sqlite.Store) bool {
		if h == nil {
			return false
		}
		for _, input := range handles {
			if input != nil && input.PayloadInputRevision() == h.PayloadInputRevision() {
				return true
			}
		}
		return false
	}
	var visit func(graph.Reader, int) bool
	visit = func(r graph.Reader, depth int) bool {
		if depth > graphview.MaxGenerationAncestryDepth+graphview.MaxDirtyChainDepth {
			return false
		}
		switch v := r.(type) {
		case *store_sqlite.Store:
			return covered(v)
		case *graph.OverlaidView:
			layer, ok := v.LayerReader().(*graphview.GenerationLayer)
			return ok && layer != nil && covered(layer.ProjectionHandle()) && visit(v.Base(), depth+1)
		default:
			return false
		}
	}
	return visit(reader, 0)
}
