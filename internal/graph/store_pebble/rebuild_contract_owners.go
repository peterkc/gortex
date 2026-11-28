package store_pebble

import (
	"bytes"
	"context"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/graph"
)

// ContractFileOwner chooses the minimum genuine file node ID at the exact
// repository and path. An absent owner is not inferred from the graph path.
func (v RebuildView) ContractFileOwner(ctx context.Context, repo, path string) (string, error) {
	if path == "" {
		return "", ctx.Err()
	}
	var owner string
	err := v.visit(ctx, keyStrings([]byte{familyFile}, path), func(snap *pebble.Snapshot, r row) error {
		id := string(r.value)
		if id == "" || !bytes.Equal(r.key, keyStrings([]byte{familyFile}, path, id)) {
			return invalid("working contract file index identity")
		}
		n, err := pendingGet(snap, pendingNodeKey(id), decodeNodeLight)
		if err != nil {
			return err
		}
		if n == nil || n.ID != id || n.FilePath != path {
			return invalid("working contract file placement")
		}
		if n.Kind == graph.KindFile && n.RepoPrefix == repo && (owner == "" || id < owner) {
			owner = id
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return owner, nil
}
