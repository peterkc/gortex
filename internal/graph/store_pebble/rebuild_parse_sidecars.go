package store_pebble

import (
	"context"
	"encoding/binary"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/graph"
)

const parsedConstantRows byte = 4 // Private worker column; never sealed.

func parsedConstantKey(id string) []byte {
	return keyStrings([]byte{familyFileMeta, parsedConstantRows}, id)
}

// writeParsedSidecars runs under AddParsedFile's write lock. The immutable
// parse receipt keeps original per-file order, duplicate literals, and tokens.
func (b *BuildStore) writeParsedSidecars(repo string, f ParseFacts) error {
	path := f.File.FilePath
	for _, c := range f.Constants {
		if c.FilePath != path || c.NodeID == "" || graph.IsProxyID(c.NodeID) {
			return invalid("parse constant outside file or without a local node ID")
		}
		// Match SQLite's last write by node ID, including empty literals.
		value := appendMetaString(appendMetaString(nil, repo), path)
		value = appendMetaString(value, c.Value)
		if err := b.pending.Set(parsedConstantKey(c.NodeID), value, pebble.NoSync); err != nil {
			return err
		}
	}
	for id, values := range f.Shingles {
		if id == "" || graph.IsProxyID(id) {
			return invalid("parse shingle without a local node ID")
		}
		n, err := b.pendingNode(id)
		if err != nil {
			return err
		}
		if n == nil || n.FilePath != path {
			return invalid("parse shingle outside file")
		}
		raw := make([]byte, 0, len(values)*8)
		for _, value := range values {
			raw = binary.LittleEndian.AppendUint64(raw, value)
		}
		if err := b.updateShingles(id, shingleKey(repo, id), raw); err != nil {
			return err
		}
	}
	return nil
}

// ConstantValue returns the saved literal for an already-selected exact node
// ID, or nil for a miss. Its caller keeps the native candidate scope.
func (v RebuildView) ConstantValue(ctx context.Context, id string) (*graph.ScopedConstantValueRow, error) {
	var result *graph.ScopedConstantValueRow
	err := v.read(ctx, func(snap *pebble.Snapshot) error {
		var err error
		result, err = pendingGet(snap, parsedConstantKey(id), func(raw []byte) (*graph.ScopedConstantValueRow, error) {
			d := recordCursor{buf: raw}
			repo, path, value := d.str(), d.str(), d.str()
			if err := d.finish(); err != nil {
				return nil, err
			}
			if path == "" {
				return nil, invalid("parse constant file identity")
			}
			return &graph.ScopedConstantValueRow{
				ConstantValueRow: graph.ConstantValueRow{NodeID: id, FilePath: path, Value: value},
				RepoPrefix:       repo,
			}, nil
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// VisitCloneShingles streams one exact repo's saved shingle sets (including
// the empty repo prefix). Yields are tentative, as for other RebuildView visits.
func (v RebuildView) VisitCloneShingles(ctx context.Context, repo string, yield func(string, []uint64) error) error {
	prefix := keyStrings([]byte{familyFileMeta, shingleRows}, repo)
	return v.visit(ctx, prefix, func(_ *pebble.Snapshot, r row) error {
		id, tail, err := readKeyString(r.key[len(prefix):])
		if err != nil || id == "" || len(tail) != 0 || len(r.value)%8 != 0 {
			return invalid("invalid clone shingle row")
		}
		values := make([]uint64, len(r.value)/8)
		for i := range values {
			values[i] = binary.LittleEndian.Uint64(r.value[i*8:])
		}
		return yield(id, values)
	})
}
