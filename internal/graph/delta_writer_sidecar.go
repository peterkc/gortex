package graph

import (
	"errors"
	"fmt"
)

// Side tables of a delta.
//
// A delta's per-file side tables — symbol full-text documents, the files
// inventory, constant values, reference facts, clone shingles, the content
// index, per-file failures and the file-mtime ledger — are keyed by the paths
// the delta covers, and the published generation's file masks hide the layer
// below's rows at those paths. So every side-table WRITE the per-save engine
// makes is forwarded to the generation handle (the sidecar): what it writes
// for a covered path is exactly that path's rows in the published generation.
//
// Reads are answered where the rows live. The reference-fact read composes
// the delta's own facts (for its covered paths) over the view below's. The
// other reads in this file answer from the generation alone. Missing support is
// a checked error, not authoritative empty data. Advisory probes may retain
// conservative fallback behavior; required writes and content scans also record
// refusal so an ignored error cannot publish an incomplete delta.
// DeltaWriterCapabilities lists every capability and its decision; the
// indexer's checklist test fails when the per-save engine asserts one that is
// not on the list.

// sidecarAs returns the sidecar as T when it implements it.
func sidecarAs[T any](dw *DeltaWriter) (T, bool) {
	v, ok := dw.sidecar.(T)
	return v, ok
}

// deltaSidecarError keeps read failures checked without making an advisory
// probe (for example ContentRepoHasRows) poison a supported fallback.
func deltaSidecarError(op string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("graph: delta sidecar %s: %w", op, err)
}

// sidecarWriteResult preserves failures even when a legacy engine ignores its
// returned error. The existing delta owner refuses Unsupported before publish.
func (dw *DeltaWriter) sidecarWriteResult(op string, err error) error {
	err = deltaSidecarError(op, err)
	if err != nil {
		dw.noteUnsupported(err.Error())
	}
	return err
}

// BulkSetFileMtimes implements FileMtimeWriter on the generation.
func (dw *DeltaWriter) BulkSetFileMtimes(repoPrefix string, mtimes map[string]int64) error {
	if w, ok := sidecarAs[FileMtimeWriter](dw); ok {
		return dw.sidecarWriteResult("BulkSetFileMtimes", w.BulkSetFileMtimes(repoPrefix, mtimes))
	}
	if len(mtimes) == 0 {
		return nil
	}
	return dw.sidecarWriteResult("BulkSetFileMtimes", errors.ErrUnsupported)
}

// ReplaceFileMtimes implements FileMtimeReplacer on the generation.
func (dw *DeltaWriter) ReplaceFileMtimes(repoPrefix string, mtimes map[string]int64) error {
	if w, ok := sidecarAs[FileMtimeReplacer](dw); ok {
		return dw.sidecarWriteResult("ReplaceFileMtimes", w.ReplaceFileMtimes(repoPrefix, mtimes))
	}
	if len(mtimes) == 0 {
		return nil
	}
	return dw.sidecarWriteResult("ReplaceFileMtimes", errors.ErrUnsupported)
}

// DeleteFileMtimes implements FileMtimeDeleter on the generation.
func (dw *DeltaWriter) DeleteFileMtimes(repoPrefix string, paths []string) error {
	if w, ok := sidecarAs[FileMtimeDeleter](dw); ok {
		return dw.sidecarWriteResult("DeleteFileMtimes", w.DeleteFileMtimes(repoPrefix, paths))
	}
	if len(paths) == 0 {
		return nil
	}
	return dw.sidecarWriteResult("DeleteFileMtimes", errors.ErrUnsupported)
}

// BatchUpsertSymbolFTS implements SymbolFTSBatchUpserter on the generation.
func (dw *DeltaWriter) BatchUpsertSymbolFTS(items []SymbolFTSItem) error {
	if w, ok := sidecarAs[SymbolFTSBatchUpserter](dw); ok {
		return dw.sidecarWriteResult("BatchUpsertSymbolFTS", w.BatchUpsertSymbolFTS(items))
	}
	if len(items) == 0 {
		return nil
	}
	return dw.sidecarWriteResult("BatchUpsertSymbolFTS", errors.ErrUnsupported)
}

// BatchDeleteSymbolFTS implements SymbolFTSBatchDeleter on the generation.
// A lower-layer file mask does not prove an unknown sidecar has no own rows.
func (dw *DeltaWriter) BatchDeleteSymbolFTS(nodeIDs []string) error {
	if w, ok := sidecarAs[SymbolFTSBatchDeleter](dw); ok {
		return dw.sidecarWriteResult("BatchDeleteSymbolFTS", w.BatchDeleteSymbolFTS(nodeIDs))
	}
	if len(nodeIDs) == 0 {
		return nil
	}
	return dw.sidecarWriteResult("BatchDeleteSymbolFTS", errors.ErrUnsupported)
}

// BulkSetRefFacts implements RefFactsWriter on the generation.
func (dw *DeltaWriter) BulkSetRefFacts(repoPrefix string, facts []RefFact) error {
	if w, ok := sidecarAs[RefFactsWriter](dw); ok {
		return dw.sidecarWriteResult("BulkSetRefFacts", w.BulkSetRefFacts(repoPrefix, facts))
	}
	if len(facts) == 0 {
		return nil
	}
	return dw.sidecarWriteResult("BulkSetRefFacts", errors.ErrUnsupported)
}

// DeleteRefFactsByFiles implements RefFactsWriter on the generation.
func (dw *DeltaWriter) DeleteRefFactsByFiles(repoPrefix string, files []string) error {
	if w, ok := sidecarAs[RefFactsWriter](dw); ok {
		return dw.sidecarWriteResult("DeleteRefFactsByFiles", w.DeleteRefFactsByFiles(repoPrefix, files))
	}
	if len(files) == 0 {
		return nil
	}
	return dw.sidecarWriteResult("DeleteRefFactsByFiles", errors.ErrUnsupported)
}

// LoadRefFactsByFiles implements RefFactsReader: the delta's own facts for
// the paths it covers, the view below's for every other path.
func (dw *DeltaWriter) LoadRefFactsByFiles(repoPrefix string, files []string) ([]RefFact, error) {
	var own, rest []string
	for _, f := range files {
		if dw.layer.HasFile(f) {
			own = append(own, f)
		} else {
			rest = append(rest, f)
		}
	}
	var out []RefFact
	// An empty file filter requests both layers' complete repository facts.
	if len(own) > 0 || len(files) == 0 {
		if r, ok := sidecarAs[RefFactsReader](dw); ok {
			facts, err := r.LoadRefFactsByFiles(repoPrefix, own)
			if err != nil {
				return nil, deltaSidecarError("LoadRefFactsByFiles", err)
			}
			out = append(out, facts...)
		} else if dw.sidecar != nil {
			return nil, deltaSidecarError("LoadRefFactsByFiles (generation)", errors.ErrUnsupported)
		}
		// With no generation sidecar at all, there cannot be own fact rows.
		// An unknown non-nil adapter does not provide that proof.
	}
	if len(rest) > 0 || len(files) == 0 {
		if r, ok := dw.below.(RefFactsReader); ok {
			facts, err := r.LoadRefFactsByFiles(repoPrefix, rest)
			if err != nil {
				return nil, deltaSidecarError("LoadRefFactsByFiles", err)
			}
			for _, fact := range facts {
				if len(files) == 0 && dw.layer.HasFile(fact.FilePath) {
					continue
				}
				out = append(out, fact)
			}
		} else {
			return nil, deltaSidecarError("LoadRefFactsByFiles (below)", errors.ErrUnsupported)
		}
	}
	return out, nil
}

// LoadRefFactsByTargets implements RefFactsReader: the view below's facts at
// uncovered paths plus the delta's own.
func (dw *DeltaWriter) LoadRefFactsByTargets(repoPrefix string, targetIDs []string) (map[string][]RefFact, error) {
	out := make(map[string][]RefFact)
	if len(targetIDs) == 0 {
		return out, nil
	}
	if r, ok := dw.below.(RefFactsReader); ok {
		load := r.LoadRefFactsByTargets
		if dw.baseCache != nil {
			// The view below is an immutable stack: its facts per target are
			// kept per stack (the affected-by planner asks for the changed
			// declarations' targets on every save of a file). Over a dirty
			// chain they are kept for the stack below the chain, and the
			// chain's own facts are added per read.
			kept, overlay := r.LoadRefFactsByTargets, RefFactsChainOverlay(nil)
			if dw.chainLayers > 0 {
				kept = nil
				if splitter, ok := dw.below.(RefFactsChainSplitter); ok {
					if below, chain, ok := splitter.RefFactsSplitAt(dw.chainLayers); ok {
						kept, overlay = below.LoadRefFactsByTargets, chain
					}
				}
			}
			if kept != nil {
				load = func(repo string, ids []string) (map[string][]RefFact, error) {
					facts, err := dw.baseCache.stackRefFactsByTargets(repo, ids, kept)
					if err != nil || overlay == nil {
						return facts, err
					}
					return overlay(repo, ids, facts)
				}
			}
		}
		below, err := load(repoPrefix, targetIDs)
		if err != nil {
			return nil, deltaSidecarError("LoadRefFactsByTargets", err)
		}
		for file, facts := range below {
			if dw.layer.HasFile(file) {
				continue
			}
			out[file] = append(out[file], facts...)
		}
	} else {
		return nil, deltaSidecarError("LoadRefFactsByTargets (below)", errors.ErrUnsupported)
	}
	if r, ok := sidecarAs[RefFactsReader](dw); ok {
		own, err := r.LoadRefFactsByTargets(repoPrefix, targetIDs)
		if err != nil {
			return nil, deltaSidecarError("LoadRefFactsByTargets", err)
		}
		for file, facts := range own {
			out[file] = append(out[file], facts...)
		}
	} else if dw.sidecar != nil {
		return nil, deltaSidecarError("LoadRefFactsByTargets (generation)", errors.ErrUnsupported)
	}
	// A nil sidecar proves the own table absent, as in graph-only deltas.
	return out, nil
}

// SetFileMetas implements FileMetaWriter on the generation.
func (dw *DeltaWriter) SetFileMetas(repoPrefix string, rows []FileMetaRow) error {
	if w, ok := sidecarAs[FileMetaWriter](dw); ok {
		return dw.sidecarWriteResult("SetFileMetas", w.SetFileMetas(repoPrefix, rows))
	}
	if len(rows) == 0 {
		return nil
	}
	return dw.sidecarWriteResult("SetFileMetas", errors.ErrUnsupported)
}

// DeleteFileMetasByFiles implements FileMetaWriter on the generation.
func (dw *DeltaWriter) DeleteFileMetasByFiles(repoPrefix string, files []string) error {
	if w, ok := sidecarAs[FileMetaWriter](dw); ok {
		return dw.sidecarWriteResult("DeleteFileMetasByFiles", w.DeleteFileMetasByFiles(repoPrefix, files))
	}
	if len(files) == 0 {
		return nil
	}
	return dw.sidecarWriteResult("DeleteFileMetasByFiles", errors.ErrUnsupported)
}

// ReplaceFileIndexFailures implements FileIndexFailureWriter on the
// generation.
func (dw *DeltaWriter) ReplaceFileIndexFailures(repoPrefix string, failures []FileIndexFailure) error {
	if w, ok := sidecarAs[FileIndexFailureWriter](dw); ok {
		return dw.sidecarWriteResult("ReplaceFileIndexFailures", w.ReplaceFileIndexFailures(repoPrefix, failures))
	}
	return dw.sidecarWriteResult("ReplaceFileIndexFailures", errors.ErrUnsupported)
}

// ReplaceContentFiles implements ContentFTSBatchReplacer on the generation.
func (dw *DeltaWriter) ReplaceContentFiles(repoPrefix string, files []ContentFTSFileReplacement) error {
	if w, ok := sidecarAs[ContentFTSBatchReplacer](dw); ok {
		return dw.sidecarWriteResult("ReplaceContentFiles", w.ReplaceContentFiles(repoPrefix, files))
	}
	if len(files) == 0 {
		return nil
	}
	return dw.sidecarWriteResult("ReplaceContentFiles", errors.ErrUnsupported)
}

// BulkSetCloneShingles implements CloneShingleWriter on the generation.
func (dw *DeltaWriter) BulkSetCloneShingles(repoPrefix string, rows map[string][]uint64) error {
	if w, ok := sidecarAs[CloneShingleWriter](dw); ok {
		return dw.sidecarWriteResult("BulkSetCloneShingles", w.BulkSetCloneShingles(repoPrefix, rows))
	}
	if len(rows) == 0 {
		return nil
	}
	return dw.sidecarWriteResult("BulkSetCloneShingles", errors.ErrUnsupported)
}

// DeleteCloneShingles implements CloneShingleWriter on the generation.
func (dw *DeltaWriter) DeleteCloneShingles(nodeIDs []string) error {
	if w, ok := sidecarAs[CloneShingleWriter](dw); ok {
		return dw.sidecarWriteResult("DeleteCloneShingles", w.DeleteCloneShingles(nodeIDs))
	}
	if len(nodeIDs) == 0 {
		return nil
	}
	return dw.sidecarWriteResult("DeleteCloneShingles", errors.ErrUnsupported)
}

// WipeContent implements ContentSearcher on the generation.
func (dw *DeltaWriter) WipeContent(repoPrefix string) error {
	if w, ok := sidecarAs[ContentSearcher](dw); ok {
		return dw.sidecarWriteResult("WipeContent", w.WipeContent(repoPrefix))
	}
	return dw.sidecarWriteResult("WipeContent", errors.ErrUnsupported)
}

// WipeContentFile implements ContentSearcher on the generation. A covering
// file mask hides lower rows, not rows an unknown sidecar may already hold.
func (dw *DeltaWriter) WipeContentFile(filePath string) error {
	if w, ok := sidecarAs[ContentSearcher](dw); ok {
		return dw.sidecarWriteResult("WipeContentFile", w.WipeContentFile(filePath))
	}
	return dw.sidecarWriteResult("WipeContentFile", errors.ErrUnsupported)
}

// WipeContentFileInRepo is the repository-scoped WipeContentFile.
func (dw *DeltaWriter) WipeContentFileInRepo(repoPrefix, filePath string) error {
	if w, ok := sidecarAs[interface {
		WipeContentFileInRepo(repoPrefix, filePath string) error
	}](dw); ok {
		return dw.sidecarWriteResult("WipeContentFileInRepo", w.WipeContentFileInRepo(repoPrefix, filePath))
	}
	// The existing legacy fallback is valid only with an actual content writer.
	if w, ok := sidecarAs[ContentSearcher](dw); ok {
		return dw.sidecarWriteResult("WipeContentFileInRepo", w.WipeContentFile(filePath))
	}
	return dw.sidecarWriteResult("WipeContentFileInRepo", errors.ErrUnsupported)
}

// ContentRepoHasRows reports whether the generation holds content rows for
// the repository: the rows a delta's wipes can reach.
func (dw *DeltaWriter) ContentRepoHasRows(repoPrefix string) (bool, error) {
	if r, ok := sidecarAs[interface {
		ContentRepoHasRows(repoPrefix string) (bool, error)
	}](dw); ok {
		present, err := r.ContentRepoHasRows(repoPrefix)
		return present, deltaSidecarError("ContentRepoHasRows", err)
	}
	return false, deltaSidecarError("ContentRepoHasRows", errors.ErrUnsupported)
}

// AppendContent implements ContentSearcher on the generation.
func (dw *DeltaWriter) AppendContent(repoPrefix string, items []ContentFTSItem) error {
	if w, ok := sidecarAs[ContentSearcher](dw); ok {
		return dw.sidecarWriteResult("AppendContent", w.AppendContent(repoPrefix, items))
	}
	if len(items) == 0 {
		return nil
	}
	return dw.sidecarWriteResult("AppendContent", errors.ErrUnsupported)
}

// SearchContent implements ContentSearcher over the generation's own rows. The
// per-save engine writes content; it does not search it.
func (dw *DeltaWriter) SearchContent(query, repoPrefix string, limit int) ([]ContentHit, error) {
	if r, ok := sidecarAs[ContentSearcher](dw); ok {
		hits, err := r.SearchContent(query, repoPrefix, limit)
		return hits, deltaSidecarError("SearchContent", err)
	}
	return nil, deltaSidecarError("SearchContent", errors.ErrUnsupported)
}

// BuildContentIndex implements ContentSearcher on the generation.
func (dw *DeltaWriter) BuildContentIndex() error {
	if w, ok := sidecarAs[ContentSearcher](dw); ok {
		return dw.sidecarWriteResult("BuildContentIndex", w.BuildContentIndex())
	}
	return dw.sidecarWriteResult("BuildContentIndex", errors.ErrUnsupported)
}

// ScanContent implements ContentSearcher over the generation's own rows.
func (dw *DeltaWriter) ScanContent(repoPrefix string, fn func(nodeID, filePath, body string) bool) error {
	if r, ok := sidecarAs[ContentSearcher](dw); ok {
		return dw.sidecarWriteResult("ScanContent", r.ScanContent(repoPrefix, fn))
	}
	return dw.sidecarWriteResult("ScanContent", errors.ErrUnsupported)
}

var (
	_ ContentSearcher         = (*DeltaWriter)(nil)
	_ FileMtimeWriter         = (*DeltaWriter)(nil)
	_ FileMtimeReplacer       = (*DeltaWriter)(nil)
	_ FileMtimeDeleter        = (*DeltaWriter)(nil)
	_ SymbolFTSBatchUpserter  = (*DeltaWriter)(nil)
	_ SymbolFTSBatchDeleter   = (*DeltaWriter)(nil)
	_ RefFactsWriter          = (*DeltaWriter)(nil)
	_ RefFactsReader          = (*DeltaWriter)(nil)
	_ ConstantValueWriter     = (*DeltaWriter)(nil)
	_ FileMetaWriter          = (*DeltaWriter)(nil)
	_ FileIndexFailureWriter  = (*DeltaWriter)(nil)
	_ ContentFTSBatchReplacer = (*DeltaWriter)(nil)
	_ CloneShingleWriter      = (*DeltaWriter)(nil)
)

// RefFactsChainOverlay adds a dirty chain's reference facts for targets to
// the facts the stack below the chain holds for them (below), as the view
// below the delta composes the two.
type RefFactsChainOverlay func(repoPrefix string, targetIDs []string, below map[string][]RefFact) (map[string][]RefFact, error)

// RefFactsChainSplitter is a view below a delta whose reference facts compose
// a stack of generations: it answers for the generations below the top
// chainLayers alone, and composes the top ones over such an answer.
type RefFactsChainSplitter interface {
	RefFactsSplitAt(chainLayers int) (below RefFactsReader, chain RefFactsChainOverlay, ok bool)
}
