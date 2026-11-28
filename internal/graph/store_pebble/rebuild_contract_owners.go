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
	err := v.visitContractFileNodes(ctx, path, func(n *graph.Node) error {
		if n.Kind == graph.KindFile && n.RepoPrefix == repo && (owner == "" || n.ID < owner) {
			owner = n.ID
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return owner, nil
}

// VisitContractFileTypes streams only type declarations from the exact linked
// file. These declarations can affect native same-file body enrichment.
func (v RebuildView) VisitContractFileTypes(ctx context.Context, file string, yield func(*graph.Node) error) error {
	return v.visitContractFileNodes(ctx, file, func(n *graph.Node) error {
		if n.Kind == graph.KindType || n.Kind == graph.KindInterface {
			return yield(n)
		}
		return nil
	})
}

func (v RebuildView) visitContractFileNodes(ctx context.Context, file string, yield func(*graph.Node) error) error {
	return v.visit(ctx, keyStrings([]byte{familyFile}, file), func(snap *pebble.Snapshot, r row) error {
		id := string(r.value)
		if id == "" || !bytes.Equal(r.key, keyStrings([]byte{familyFile}, file, id)) {
			return invalid("working contract file index identity")
		}
		n, err := pendingGet(snap, pendingNodeKey(id), decodeNodeLight)
		if err != nil {
			return err
		}
		if n == nil || n.ID != id || n.FilePath != file {
			return invalid("working contract file placement")
		}
		return yield(n)
	})
}
