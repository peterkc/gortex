package store_pebble

import (
	"crypto/sha256"
	"errors"

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
// The isolated worker projects only linked-language files; today Spring files are held out.
func (r *RebuildContractRecords) VisitSpringConfig(scope contracts.SpringConfigScope,
	keysAt func(string) (keys []*graph.Node, skipped bool, err error), yield func(RebuildContractBatch) error,
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
	languages, extensions := contracts.SpringConfigFileProjection()
	if err := view.VisitFileIdentities(r.ctx, func(path, language, repo, workspace string) error {
		if !matches(repo, workspace) {
			return nil
		}
		if !graph.ProjectionMatches(path, language, languages, extensions) {
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
	var skippedConfig bool
	unknown := func(file string) error {
		key := rebuildSpringConfigKey('u', file)
		seen, err := state(key)
		if err != nil || seen != 0 {
			return err
		}
		if err := put(key); err != nil {
			return err
		}
		return yield(RebuildContractBatch{Omitted: []RebuildContractOmission{RebuildSpringConfigUnknownOmission(scope.RepoPrefix, file)}})
	}
	paths := rebuildSpringConfigKey('p')
	if err := pendingVisit(r.ctx, r.build.pending, paths, func(s row) error {
		path, rest, err := readKeyString(s.key[len(paths):])
		if err != nil || len(rest) != 0 {
			return invalid("invalid Spring config file key")
		}
		keys, skipped, err := keysAt(path)
		if err != nil {
			return err
		}
		if skipped {
			skippedConfig = true
			return unknown(path)
		}
		for _, saved := range keys {
			n := *saved
			scope.ScopeKeyNode(&n)
			// Equal relaxed spellings have the same parent prefix. Separate
			// prefix keys preserve immediate-child election and target order.
			if parent, ok := contracts.SpringConfigParentPrefix(n.Name); ok {
				if err := put(rebuildSpringConfigKey('x', scope.KeyID(parent), n.ID)); err != nil {
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
	return view.visit(r.ctx, []byte{familyNode}, func(_ *pebble.Snapshot, nodeRow row) error {
		reader, err := checkedRebuildLightNode(nodeRow, "Spring config")
		if err != nil || !matches(reader.RepoPrefix, reader.WorkspaceID) {
			return err
		}
		if !contracts.SpringReaderKind(reader.Kind) {
			return nil
		}
		reader, err = decodeNode(nodeRow.value)
		if err != nil {
			return err
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
					omission := RebuildContractEdgeOmission(reader.RepoPrefix, contracts.ContractType(graph.EdgeReadsConfig), id, e)
					return yield(RebuildContractBatch{Omitted: []RebuildContractOmission{omission}})
				}
			}
			return yield(RebuildContractBatch{Edges: []*graph.Edge{e}})
		}
		for _, raw := range contracts.SpringConfigKeysOf(reader) {
			prefix, wildcard := contracts.SpringConfigWildcardPrefix(raw)
			if !wildcard {
				if err := emit(scope.KeyID(raw), raw); err != nil {
					return err
				}
				continue
			}
			if skippedConfig {
				if err := unknown(reader.FilePath); err != nil {
					return err
				}
			}
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

// RebuildContractEdgeOmission scopes an omitted direct native edge to its
// recording file and exact physical identity, independent of its payload.
func RebuildContractEdgeOmission(repo string, kind contracts.ContractType, id string, e *graph.Edge) RebuildContractOmission {
	return RebuildContractOmission{RepoPrefix: repo, FilePath: e.FilePath, Type: kind, ID: id,
		RecordKey: HeadDigest(sha256.Sum256(pendingEdgeKey(e)))}
}

// RebuildSpringConfigUnknownOmission covers a skipped config source or a
// wildcard reader whose target universe depends on such a source.
func RebuildSpringConfigUnknownOmission(repo, file string) RebuildContractOmission {
	return RebuildContractOmission{RepoPrefix: repo, FilePath: file, Type: contracts.ContractType(graph.EdgeReadsConfig),
		RecordKey: HeadDigest(sha256.Sum256(keyStrings([]byte("spring config unknown"), repo, file)))}
}

// RebuildSpringConfigHeldOutOmission covers an F-held reader whose wildcard
// targets cannot be established without excluded link inputs.
func RebuildSpringConfigHeldOutOmission(repo, file string) RebuildContractOmission {
	return RebuildContractOmission{RepoPrefix: repo, FilePath: file, Type: contracts.ContractType(graph.EdgeReadsConfig),
		RecordKey: HeadDigest(sha256.Sum256(keyStrings([]byte("spring config held out"), repo, file)))}
}
