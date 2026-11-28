package store_pebble

import (
	"crypto/sha256"
	"errors"
	"strings"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
)

func rebuildSpringConfigKey(column byte, fields ...string) []byte {
	return keyStrings(append(rebuildContractRecordKey('g'), column), fields...)
}

// VisitSpringConfig replays the cold native Spring config pass from checked,
// value-redacted per-file facts and linked reader nodes. Each callback borrows
// one proposed row; it must not write the working graph during the scan.
func (r *RebuildContractRecords) VisitSpringConfig(scope contracts.SpringConfigScope,
	keysAt func(string) ([]*graph.Node, error), yield func(RebuildContractBatch) error,
) (err error) {
	if err := r.ready(); err != nil || scope.RepoRoot == "" {
		return err
	}
	prefix := rebuildContractRecordKey('g')
	defer func() {
		err = errors.Join(err, r.build.pending.DeleteRange(prefix, prefixEnd(prefix), pebble.NoSync))
	}()
	put := func(key []byte) error {
		return r.build.putRebuildScratch(key, []byte{1}, goldBufferBytes, "Spring config replay")
	}
	matches := func(repo, workspace string) bool {
		return repo == scope.RepoPrefix && (scope.WorkspaceID == "" || scope.WorkspaceID == workspace)
	}
	view := r.build.RebuildView()
	var projected bool
	if err := view.VisitFileIdentities(r.ctx, func(path, language, repo, workspace string) error {
		if !matches(repo, workspace) {
			return nil
		}
		lower := strings.ToLower(path)
		if !strings.EqualFold(language, "java") && !strings.HasSuffix(lower, ".yml") &&
			!strings.HasSuffix(lower, ".yaml") && !strings.HasSuffix(lower, ".properties") {
			return nil
		}
		projected = true
		if _, ok := contracts.SpringConfigProfile(path); !ok {
			return nil
		}
		return put(rebuildSpringConfigKey('p', path))
	}); err != nil || !projected {
		return err
	}
	state := func(key []byte) (byte, error) {
		return rebuildScratchState(r.build.pending, key, "invalid Spring config replay key")
	}
	paths := rebuildSpringConfigKey('p')
	if err := pendingVisit(r.ctx, r.build.pending, paths, func(s row) error {
		path, rest, err := readKeyString(s.key[len(paths):])
		if err != nil || len(rest) != 0 {
			return invalid("invalid Spring config file key")
		}
		keys, err := keysAt(path)
		if err != nil {
			return err
		}
		for _, saved := range keys {
			n := *saved
			n.ID = scope.KeyID(n.Name)
			n.RepoPrefix = scope.RepoPrefix
			n.WorkspaceID = scope.WorkspaceID
			// Equal relaxed spellings have the same parent prefix. Separate
			// prefix keys preserve immediate-child election and target order.
			if dot := strings.LastIndexByte(n.Name, '.'); dot >= 0 {
				if err := put(rebuildSpringConfigKey('x', scope.KeyID(n.Name[:dot]), n.ID)); err != nil {
					return err
				}
			}
			key := rebuildSpringConfigKey('k', n.ID)
			seen, err := state(key)
			if err != nil {
				return err
			}
			if seen != 0 {
				continue
			}
			if err := put(key); err != nil {
				return err
			}
			old, err := view.Node(r.ctx, n.ID)
			if err != nil {
				return err
			}
			if old == nil {
				if err := yield(RebuildContractBatch{Nodes: []*graph.Node{&n}}); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		return err
	}
	return view.VisitNodes(r.ctx, func(reader *graph.Node) error {
		if !matches(reader.RepoPrefix, reader.WorkspaceID) {
			return nil
		}
		switch reader.Kind {
		case graph.KindField, graph.KindType, graph.KindInterface, graph.KindMethod:
		default:
			return nil
		}
		emit := func(id, raw string) error {
			seenKey := rebuildSpringConfigKey('s', reader.ID, id)
			seen, err := state(seenKey)
			if err != nil || seen != 0 {
				return err
			}
			if err := put(seenKey); err != nil {
				return err
			}
			e := contracts.SpringConfigReadEdge(reader, id, raw)
			edgeKey := pendingEdgeKey(e)
			old, err := r.build.pendingEdge(edgeKey)
			if err != nil || old != nil {
				return err
			}
			present, err := state(rebuildSpringConfigKey('k', id))
			if err != nil {
				return err
			}
			if present == 0 {
				target, err := view.Node(r.ctx, id)
				if err != nil {
					return err
				}
				if target == nil {
					// Native emits this direct edge even when its target is absent.
					// Record the physical edge identity, not its raw annotation spelling.
					omission := RebuildContractOmission{
						RepoPrefix: reader.RepoPrefix, FilePath: reader.FilePath, ID: id,
						Type: contracts.ContractType(graph.EdgeReadsConfig), RecordKey: HeadDigest(sha256.Sum256(edgeKey)),
					}
					return yield(RebuildContractBatch{Omitted: []RebuildContractOmission{omission}})
				}
			}
			return yield(RebuildContractBatch{Edges: []*graph.Edge{e}})
		}
		for _, raw := range contracts.SpringConfigKeysOf(reader) {
			if !strings.HasSuffix(raw, "*") {
				if err := emit(scope.KeyID(raw), raw); err != nil {
					return err
				}
				continue
			}
			prefix := strings.TrimSuffix(strings.TrimSuffix(raw, "*"), ".")
			bucket := rebuildSpringConfigKey('x', scope.KeyID(prefix))
			if err := pendingVisit(r.ctx, r.build.pending, bucket, func(s row) error {
				id, rest, err := readKeyString(s.key[len(bucket):])
				if err != nil || len(rest) != 0 {
					return invalid("invalid Spring config target key")
				}
				return emit(id, raw)
			}); err != nil {
				return err
			}
		}
		return nil
	})
}
