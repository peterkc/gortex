package store_pebble

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/graph"
)

// ErrRebuildView marks a failed working-store read. The underlying error is
// preserved, including cancellation, corruption, mutation and visitor errors.
var ErrRebuildView = errors.New("pebble rebuild working read failed")

// ErrRebuildLinkInputSize marks an input row the link frontier cannot page.
var ErrRebuildLinkInputSize = errors.New("pebble rebuild link input exceeds page bound")

const rebuildLinkPendingRows = 2048

// RebuildView reads a worker's private mutable rows, not a published graph.
// Obtain a view from BuildStore.RebuildView. Each call uses one snapshot;
// ordinary views see successful intervening writes. WithCallerPageCache
// keeps caller rows until ResetCallerPage. This is safe in the main frontier,
// where the only page write is the apply after reduce.
// Visits are tentative until the call returns nil. Callbacks may read or write
// the working store. The owner must join all calls before closing the store.
type RebuildView struct {
	build      *BuildStore
	callerPage *rebuildCallerPageCache
}

// RebuildView returns the typed read surface without changing graph.Store reads.
func (b *BuildStore) RebuildView() RebuildView { return RebuildView{build: b} }

// SelectRebuildLinkFacts limits a detached base receipt to the private F cut.
func (v RebuildView) SelectRebuildLinkFacts(ctx context.Context, f ParseFacts) (ParseFacts, error) {
	return v.build.SelectRebuildLinkFacts(ctx, f)
}

// Node returns the saved node by exact ID, or (nil, nil) for a genuine miss.
func (v RebuildView) Node(ctx context.Context, id string) (*graph.Node, error) {
	var n *graph.Node
	err := v.read(ctx, func(snap *pebble.Snapshot) error {
		key := pendingNodeKey(id)
		var node *graph.Node
		var err error
		if v.callerPage != nil {
			node, err = v.callerPage.read(snap, id, key, false, nil)
		} else {
			node, err = pendingGet(snap, key, decodeNode)
		}
		if err != nil {
			return err
		}
		if node != nil && node.ID != id {
			return invalid("working node ID does not match key")
		}
		n = node
		return nil
	})
	if err != nil {
		return nil, err
	}
	return n, nil
}

// VisitNodes streams all saved nodes in ID order, including rows not kept by
// the publication profile. It can feed StartRebuildLink's source directly via
// a closure that binds ctx: the ordered file builder receives one node at a time.
func (v RebuildView) VisitNodes(ctx context.Context, yield func(*graph.Node) error) error {
	return v.visit(ctx, []byte{familyNode}, func(_ *pebble.Snapshot, r row) error {
		n, err := decodeNode(r.value)
		if err != nil {
			return err
		}
		if !bytes.Equal(r.key, pendingNodeKey(n.ID)) {
			return invalid("working node ID does not match key")
		}
		return yield(n)
	})
}

// VisitFileIdentities streams the live file census without decoding metadata.
// It includes skip nodes; callers decide which of them can affect a result.
func (v RebuildView) VisitFileIdentities(ctx context.Context, yield func(path, language, repo, workspace string) error) error {
	return v.visit(ctx, []byte{familyNode}, func(_ *pebble.Snapshot, r row) error {
		n, err := decodeNodeLight(r.value)
		if err != nil {
			return err
		}
		if !bytes.Equal(r.key, pendingNodeKey(n.ID)) {
			return invalid("working node ID does not match key")
		}
		if n.Kind != graph.KindFile {
			return nil
		}
		path := n.FilePath
		if path == "" {
			path = n.ID
		}
		return yield(path, n.Language, n.RepoPrefix, n.WorkspaceID)
	})
}

const (
	rebuildDirectImportReadRows  = 16 << 10
	rebuildDirectImportReadBytes = 16 << 20
)

// VisitFileImportTargets streams direct imports from every saved node in file.
// Source ownership comes from the exact file index, not the edge's recording
// site. Resolved target placements come from the same snapshot. Unresolved,
// external, missing and pathless targets have an empty targetFile.
// The caller must discard tentative results on any error.
func (v RebuildView) VisitFileImportTargets(ctx context.Context, file string, yield func(targetID, targetFile string) error) error {
	if file == "" || yield == nil {
		return fmt.Errorf("%w: file imports require a file and visitor", ErrRebuildView)
	}
	if len(file) > rebuildDirectImportReadBytes {
		return fmt.Errorf("%w: file import path", ErrRebuildLinkInputSize)
	}
	return v.read(ctx, func(snap *pebble.Snapshot) error {
		rows, remaining := 0, rebuildDirectImportReadBytes
		chargeBytes := func(size int) error {
			if size > remaining {
				return fmt.Errorf("%w: file import read bytes", ErrRebuildLinkInputSize)
			}
			remaining -= size
			return nil
		}
		chargeRow := func(key, value []byte) error {
			if rows == rebuildDirectImportReadRows {
				return fmt.Errorf("%w: file import read rows", ErrRebuildLinkInputSize)
			}
			rows++
			return chargeBytes(len(key) + len(value))
		}
		readNode := func(id string) (*graph.Node, error) {
			key := pendingNodeKey(id)
			if err := chargeRow(key, nil); err != nil {
				return nil, err
			}
			var n *graph.Node
			var err error
			if v.callerPage != nil {
				n, err = v.callerPage.read(snap, id, key, true, chargeBytes)
			} else {
				n, err = pendingGet(snap, key, func(raw []byte) (*graph.Node, error) {
					if err := chargeBytes(len(raw)); err != nil {
						return nil, err
					}
					return decodeNodeLight(raw)
				})
			}
			if err == nil && n != nil && n.ID != id {
				return nil, invalid("working import node ID does not match key")
			}
			return n, err
		}
		return pendingVisit(ctx, snap, keyStrings([]byte{familyFile}, file), func(r row) error {
			if err := chargeRow(r.key, r.value); err != nil {
				return err
			}
			id := string(r.value)
			if id == "" || !bytes.Equal(r.key, keyStrings([]byte{familyFile}, file, id)) {
				return invalid("working import file index identity does not match key")
			}
			n, err := readNode(id)
			if err != nil {
				return err
			}
			if n == nil || n.FilePath != file {
				return invalid("working import source does not own indexed file")
			}
			return pendingVisit(ctx, snap, guardOutgoing(id, graph.EdgeImports), func(r row) error {
				if err := chargeRow(r.key, r.value); err != nil {
					return err
				}
				e, err := guardLightEdge(r)
				if err != nil {
					return err
				}
				targetFile := ""
				if !graph.IsUnresolvedTarget(e.To) && !strings.HasPrefix(e.To, "external::") {
					target, err := readNode(e.To)
					if err != nil {
						return err
					}
					if target != nil {
						targetFile = target.FilePath
					}
				}
				return yield(e.To, targetFile)
			})
		})
	})
}

// VisitEdges streams all saved edges in (from, kind, to, file, line) order.
// Origin is mutable data, not part of the pending identity. This storage
// order is not the native order for first-wins reducers or the P3 comparator.
func (v RebuildView) VisitEdges(ctx context.Context, yield func(*graph.Edge) error) error {
	return v.visit(ctx, []byte{familyEdge, edgeOut}, func(_ *pebble.Snapshot, r row) error {
		e, err := decodeEdge(r.value)
		if err != nil {
			return err
		}
		if !bytes.Equal(r.key, pendingEdgeKey(e)) {
			return invalid("working edge identity does not match key")
		}
		return yield(e)
	})
}

// VisitDIContractEdges streams live Provides and Consumes rows whose source
// is in repo. The edge-kind index is built at seal, not in the working store.
func (v RebuildView) VisitDIContractEdges(ctx context.Context, repo string, yield func(*graph.Edge) error) error {
	return v.visit(ctx, []byte{familyEdge, edgeOut}, func(snap *pebble.Snapshot, r row) error {
		kind, err := rebuildAttributionEdgeKind(r.key)
		if err != nil || (kind != graph.EdgeProvides && kind != graph.EdgeConsumes) {
			return err
		}
		e, err := decodeEdge(r.value)
		if err != nil {
			return err
		}
		if !bytes.Equal(r.key, pendingEdgeKey(e)) {
			return invalid("working di contract edge identity")
		}
		n, err := pendingGet(snap, pendingNodeKey(e.From), decodeNodeLight)
		if err != nil || n == nil {
			return err
		}
		if n.ID != e.From {
			return invalid("working di contract edge source identity")
		}
		if n.RepoPrefix != repo {
			return nil
		}
		return yield(e)
	})
}

// VisitLinkPendingPages streams the cold resolver frontier from one snapshot
// in complete edge-key order. The caller borrows the page until it returns;
// edge edits cannot insert an additional input into this scan. It must discard
// the private build if a later page or read fails.
//
// The bounds charge complete encoded keys and values and the decoded edge
// count, not RSS. Unlike SQLite's pager, an oversized single row is refused.
func (v RebuildView) VisitLinkPendingPages(ctx context.Context, maxRows, maxBytes int, yield func([]*graph.Edge) error) error {
	if maxRows < 1 || maxRows > rebuildLinkPendingRows || maxBytes < 1 || maxBytes > 16<<20 || yield == nil {
		return fmt.Errorf("%w: invalid pending edge page limits or visitor", ErrRebuildView)
	}
	var page []*graph.Edge
	var size int
	drain := func() error {
		if err := yield(page); err != nil {
			return err
		}
		clear(page)
		page = page[:0]
		size = 0
		return ctx.Err()
	}
	prefix := []byte{familyEdge, edgeOut}
	if err := v.visit(ctx, prefix, func(_ *pebble.Snapshot, r row) error {
		// Filter on the stored target before decoding its metadata.
		key := r.key[len(prefix):]
		for range 2 { // from, kind
			_, rest, err := readKeyString(key)
			if err != nil {
				return err
			}
			key = rest
		}
		to, _, err := readKeyString(key)
		if err != nil || !linkPendingTarget(to) {
			return err
		}
		rowBytes := len(r.key) + len(r.value)
		if rowBytes > maxBytes {
			return fmt.Errorf("%w: pending edge needs %d encoded bytes, limit %d", ErrRebuildLinkInputSize, rowBytes, maxBytes)
		}
		if len(page) == maxRows || rowBytes > maxBytes-size {
			if err := drain(); err != nil {
				return err
			}
		}
		e, err := decodeEdge(r.value)
		if err != nil {
			return err
		}
		if !bytes.Equal(r.key, pendingEdgeKey(e)) {
			return invalid("working edge identity does not match key")
		}
		page = append(page, e)
		size += rowBytes
		return nil
	}); err != nil {
		return err
	}
	if len(page) == 0 {
		return nil
	}
	if err := errors.Join(drain(), v.mutationError()); err != nil {
		return fmt.Errorf("%w: %w", ErrRebuildView, err)
	}
	return nil
}

// The pinned SQLite frontier uses binary prefix ranges for bare placeholders
// and ASCII-insensitive LIKE for qualified ones. LIKE stops at embedded NUL;
// the binary ranges do not. Fn-value placeholders belong to a different pass.
func linkPendingTarget(id string) bool {
	if strings.HasPrefix(id, graph.FnValuePlaceholderMarker) {
		return false
	}
	text, _, _ := strings.Cut(id, "\x00")
	if strings.Contains(text, "::") {
		text = strings.Map(func(r rune) rune {
			if 'A' <= r && r <= 'Z' {
				return r + 'a' - 'A'
			}
			return r
		}, text)
	}
	return (strings.HasPrefix(id, graph.UnresolvedMarker) || strings.Contains(text, "::"+graph.UnresolvedMarker)) &&
		!strings.Contains(text, "::"+graph.FnValuePlaceholderMarker)
}

// ApplyLinkReindexes replaces targets or kinds for a checked resolver page
// and records the original key of each installed move. The reducer supplies
// unique, complete old keys in frontier order; duplicate destination keys
// keep the first claimant. Unlike the legacy mutation adapter this must not
// split the page at 256 rows.
// On error the caller must abandon the private build.
func (b *BuildStore) ApplyLinkReindexes(ctx context.Context, moves []graph.EdgeReindex) error {
	return b.applyRebuildLinkReindexes(ctx, moves, "")
}

func (b *BuildStore) applyRebuildLinkReindexes(ctx context.Context, moves []graph.EdgeReindex, refreshKind graph.EdgeKind) error {
	err := b.withWrites(func() error {
		if len(moves) > rebuildLinkPendingRows {
			return invalid("link edge move page exceeds row bound")
		}
		var prior []byte
		for _, move := range moves {
			if err := ctx.Err(); err != nil {
				return err
			}
			if move.Edge == nil || move.OldTo == "" || move.Edge.To == "" || move.RefreshIdentity != (refreshKind != "") {
				return invalid("link edge move must replace an exact target or kind")
			}
			if move.RefreshIdentity {
				if move.Edge.Kind != refreshKind || move.OldKind != "" || move.OldFrom == "" ||
					move.OldFilePath != move.Edge.FilePath || move.OldLine != move.Edge.Line {
					return invalid("dataflow lift must keep kind and recording site")
				}
				switch refreshKind {
				case graph.EdgeArgOf:
					if move.OldFrom != move.Edge.From || move.Edge.To == move.OldTo {
						return invalid("argument lift must refresh only the target")
					}
				case graph.EdgeReturnsTo:
					if move.Edge.From == "" || move.OldFrom == move.Edge.From || move.Edge.To != move.OldTo {
						return invalid("return lift must refresh only the source")
					}
				default:
					return invalid("unexpected dataflow lift kind")
				}
			} else if move.OldFrom != "" {
				return invalid("link edge move must replace an exact target or kind")
			}
			old := *move.Edge
			old.To = move.OldTo
			if move.RefreshIdentity {
				old.From = move.OldFrom
			}
			if move.OldKind != "" {
				old.Kind = move.OldKind
			}
			key := pendingEdgeKey(&old)
			if bytes.Compare(prior, key) >= 0 {
				return invalid("link edge moves are not in complete old-key order")
			}
			saved, err := b.pendingEdge(key)
			if err != nil {
				return err
			}
			if saved == nil || !bytes.Equal(key, pendingEdgeKey(saved)) {
				return invalid("link move has no matching saved old edge")
			}
			prior = key
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := b.reindexChunkWithProof(moves, refreshKind == "", refreshKind == graph.EdgeReturnsTo); err != nil {
			return err
		}
		return ctx.Err()
	})
	if err != nil {
		return fmt.Errorf("%w: apply resolver page: %w", ErrRebuildLink, err)
	}
	return nil
}

// VisitNameCandidates streams one exact-name AllRepos lookup for the native
// extern route, ordered by repository, language, ID. An empty language list
// includes all languages; the caller must restore (repository, ID) order if
// using that native wildcard scope. An empty name matches nothing.
func (v RebuildView) VisitNameCandidates(ctx context.Context, name string, languages []string, yield func(*graph.Node) error) error {
	return v.visitCandidates(ctx, nameRebuildScoped, name, languages, yield)
}

// VisitQualNameCandidates streams an exact, nonempty qualified name in ID order.
func (v RebuildView) VisitQualNameCandidates(ctx context.Context, qualName string, yield func(*graph.Node) error) error {
	return v.visitCandidates(ctx, nameQualified, qualName, nil, yield)
}

func (v RebuildView) visitCandidates(ctx context.Context, column byte, key string, languages []string, yield func(*graph.Node) error) error {
	if key == "" {
		return v.read(ctx, func(*pebble.Snapshot) error { return nil })
	}
	return v.visitCandidatePrefix(ctx, column, keyStrings([]byte{familyName, column}, key), languages, yield)
}

func (v RebuildView) visitCandidatePrefix(ctx context.Context, column byte, prefix []byte, languages []string, yield func(*graph.Node) error) error {
	return v.visit(ctx, prefix, func(snap *pebble.Snapshot, r row) error {
		_, _, id, _, err := readRebuildCandidateIndex(column, r.key, r.value)
		if err != nil {
			return err
		}
		n, err := pendingGet(snap, pendingNodeKey(id), decodeNode)
		if err != nil {
			return err
		}
		if n == nil || n.ID != id || !bytes.Equal(r.key, rebuildCandidateKey(column, n)) {
			return invalid("stale working candidate index entry")
		}
		if len(languages) == 0 || slices.Contains(languages, n.Language) {
			return yield(n)
		}
		return nil
	})
}

func (v RebuildView) visit(ctx context.Context, prefix []byte, visit func(*pebble.Snapshot, row) error) error {
	return v.read(ctx, func(snap *pebble.Snapshot) error {
		return pendingVisit(ctx, snap, prefix, func(r row) error {
			if err := v.mutationError(); err != nil {
				return err
			}
			if err := visit(snap, r); err != nil {
				return err
			}
			return ctx.Err()
		})
	})
}

func (v RebuildView) mutationError() error {
	v.build.mu.Lock()
	defer v.build.mu.Unlock()
	return v.build.first
}

func (v RebuildView) read(ctx context.Context, read func(*pebble.Snapshot) error) (err error) {
	defer func() {
		if err != nil {
			err = fmt.Errorf("%w: %w", ErrRebuildView, err)
		}
	}()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := v.mutationError(); err != nil {
		return err
	}
	snap := v.build.pending.NewSnapshot()
	defer func() { err = errors.Join(err, snap.Close()) }()
	if err := read(snap); err != nil {
		return err
	}
	return errors.Join(ctx.Err(), v.mutationError())
}

// The scoped-name column is scratch-only. The qualified-name column uses the
// existing exact qualified-name layout. Private entries store ID and the
// optional canonical candidate encoded length; serving indexes are unchanged.
const nameRebuildScoped byte = 'r'

func rebuildCandidateKey(column byte, n *graph.Node) []byte {
	if n == nil {
		return nil
	}
	if column == nameQualified {
		if n.QualName == "" {
			return nil
		}
		return keyStrings([]byte{familyName, column}, n.QualName, n.ID)
	}
	if n.Name == "" {
		return nil
	}
	return keyStrings([]byte{familyName, column}, n.Name, n.RepoPrefix, n.Language, n.ID)
}

// Key fields are the exact scope and ID used by candidate order. ID-only
// values are unsized, not invalid: scalar selection still checks the node.
func readRebuildCandidateIndex(column byte, key, value []byte) (repo, lang, id string, size uint64, err error) {
	if len(key) < 2 || key[0] != familyName || key[1] != column {
		return "", "", "", 0, invalid("stale working candidate index entry")
	}
	_, rest, err := readKeyString(key[2:])
	if err != nil {
		return "", "", "", 0, err
	}
	if column == nameRebuildScoped {
		repo, rest, err = readKeyString(rest)
		if err != nil {
			return "", "", "", 0, err
		}
		lang, rest, err = readKeyString(rest)
		if err != nil {
			return "", "", "", 0, err
		}
	}
	id, rest, err = readKeyString(rest)
	if err != nil {
		return "", "", "", 0, err
	}
	if len(rest) != 0 || len(value) < len(id) || string(value[:len(id)]) != id {
		return "", "", "", 0, invalid("stale working candidate index entry")
	}
	if len(value) == len(id) {
		return repo, lang, id, 0, nil
	}
	if len(value) != len(id)+8 {
		return "", "", "", 0, invalid("stale working candidate index entry")
	}
	size = binary.BigEndian.Uint64(value[len(id):])
	return repo, lang, id, size, nil
}

func updateRebuildCandidates(batch rebuildNodeBatch, prev, next *graph.Node, size uint64) error {
	var value []byte
	if next != nil {
		value = []byte(next.ID)
		if size != 0 {
			value = binary.BigEndian.AppendUint64(value, size)
		}
	}
	for _, column := range [...]byte{nameRebuildScoped, nameQualified} {
		oldKey, newKey := rebuildCandidateKey(column, prev), rebuildCandidateKey(column, next)
		if oldKey != nil && !bytes.Equal(oldKey, newKey) {
			if err := batch.Delete(oldKey, nil); err != nil {
				return err
			}
		}
		// Even an unchanged key must receive the new payload length.
		if newKey != nil {
			if err := batch.Set(newKey, value, nil); err != nil {
				return err
			}
		}
	}
	return nil
}
