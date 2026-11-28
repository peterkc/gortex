package store_pebble

import (
	"bytes"
	"context"
	"fmt"
	"path"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/graph"
)

// AddRebuildManifestArtifacts is the native-ordered writer for checked root
// module and workspace facts. Only a writer-owned synthetic file can replace
// a held file at the same path. Its exact replacement and incident edges are
// private proof, not permission for another pass to use a held declaration.
func (b *BuildStore) AddRebuildManifestArtifacts(ctx context.Context, nodes []*graph.Node, edges []*graph.Edge) error {
	return b.withWrites(func() error {
		for _, n := range nodes {
			if err := ctx.Err(); err != nil {
				return err
			}
			if !b.rebuildLinksIsolated {
				if err := b.writeNode(n); err != nil {
					return err
				}
				continue
			}
			held, err := b.rebuildHeldNodeCapture(n.ID)
			if err != nil {
				return err
			}
			if held != nil {
				if n.Kind != graph.KindFile || n.ID != n.FilePath || held.Kind != graph.KindFile || held.FilePath != n.FilePath || !rebuildManifestWriterFile(n.FilePath) {
					return fmt.Errorf("%w: manifest writer collides with held node %q", ErrRebuildLinkIsolation, n.ID)
				}
				owned, err := b.rebuildManifestSupersession(n.ID)
				if err != nil {
					return err
				}
				live, err := b.pendingNode(n.ID)
				if err != nil {
					return err
				}
				if live != nil && !owned {
					return fmt.Errorf("%w: manifest writer found unrelated node %q", ErrRebuildLinkIsolation, n.ID)
				}
				if err := b.writeNode(n); err != nil {
					return err
				}
				key := pendingNodeKey(n.ID)
				raw, err := pendingGet(b.pending, key, copyValue)
				if err != nil {
					return err
				}
				if raw == nil {
					return invalid("manifest writer node was not saved")
				}
				if err := b.pending.Set(rebuildHoldoutKey('s', key), raw, pebble.NoSync); err != nil {
					return err
				}
			} else if err := b.writeNode(n); err != nil {
				return err
			}
		}
		for _, e := range edges {
			if err := ctx.Err(); err != nil {
				return err
			}
			if !b.rebuildLinksIsolated {
				if err := b.addEdge(e); err != nil {
					return err
				}
				continue
			}
			incident := false
			for _, id := range []string{e.From, e.To} {
				held, err := b.rebuildHeldNodeCapture(id)
				if err != nil {
					return err
				}
				if held == nil {
					continue
				}
				owned, err := b.rebuildManifestSupersession(id)
				if err != nil {
					return err
				}
				if !owned || (e.Kind != graph.EdgeDependsOnModule && e.Kind != graph.EdgePackageWorkspaceMember) {
					return fmt.Errorf("%w: manifest writer edge uses held node %q", ErrRebuildLinkIsolation, id)
				}
				incident = true
			}
			if incident {
				key := pendingEdgeKey(e)
				live, err := b.pendingEdge(key)
				if err != nil {
					return err
				}
				if live != nil {
					owned, err := b.rebuildManifestEdgeMatches(key)
					if err != nil {
						return err
					}
					if !owned {
						return fmt.Errorf("%w: manifest writer found unrelated incident edge", ErrRebuildLinkIsolation)
					}
				}
			}
			if err := b.addEdge(e); err != nil {
				return err
			}
			if incident {
				key := pendingEdgeKey(e)
				raw, err := pendingGet(b.pending, key, copyValue)
				if err != nil {
					return err
				}
				if raw == nil {
					return invalid("manifest writer edge was not saved")
				}
				if err := b.pending.Set(rebuildHoldoutKey('w', key), raw, pebble.NoSync); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

func rebuildManifestWriterFile(file string) bool {
	if path.Base(file) == "package.json" {
		return true
	}
	// Only parsed or checked-skip paths can be held. The native module
	// writer also emits go.mod and yarn.lock, which have no extractor.
	switch file {
	case "package-lock.json", "pnpm-lock.yaml":
		return true
	}
	return false
}

func (b *BuildStore) rebuildHeldNodeCapture(id string) (*graph.Node, error) {
	return pendingGet(b.pending, rebuildHoldoutKey('n', pendingNodeKey(id)), func(raw []byte) (*graph.Node, error) {
		d := recordCursor{buf: raw}
		// Isolation already used the receipt to select the raw edges to
		// restore. The writer replaces the captured node by ID.
		_ = d.bytes()
		if d.err != nil {
			return nil, d.err
		}
		n, err := decodeNodeLight(d.buf)
		if err != nil {
			return nil, err
		}
		if n.ID != id {
			return nil, invalid("held manifest capture identity")
		}
		return n, nil
	})
}

func (b *BuildStore) rebuildManifestSupersession(id string) (bool, error) {
	key := pendingNodeKey(id)
	want, err := pendingGet(b.pending, rebuildHoldoutKey('s', key), copyValue)
	if err != nil || want == nil {
		return false, err
	}
	match, err := pendingGet(b.pending, key, func(raw []byte) (bool, error) { return bytes.Equal(raw, want), nil })
	if err == nil && !match {
		err = fmt.Errorf("%w: manifest replacement differs for %q", ErrRebuildLinkIsolation, id)
	}
	return match, err
}

func (b *BuildStore) rebuildManifestEdgeMatches(key []byte) (bool, error) {
	want, err := pendingGet(b.pending, rebuildHoldoutKey('w', key), copyValue)
	if err != nil || want == nil {
		return false, err
	}
	match, err := pendingGet(b.pending, key, func(raw []byte) (bool, error) { return bytes.Equal(raw, want), nil })
	if err != nil || !match {
		return false, err
	}
	e, err := decodeEdge(want)
	if err != nil {
		return false, err
	}
	reverse, err := pendingGet(b.pending, pendingReverseKey(e, key), copyValue)
	if err != nil || !bytes.Equal(reverse, key) {
		return false, err
	}
	custody, err := readRebuildEdgeCustody(b.pending, key)
	return err == nil && custody.raw == nil && custody.proof == 0, err
}
