package store_pebble

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/graph"
)

// The scratch index holds compact keys, not graph objects. Pebble flushes its
// bounded memtables to sorted runs. It keeps the last publish's logical rows
// (including groups omitted by the profile) plus pending mutations. It is
// never a serving reader source and needs neither a WAL nor crash recovery.
func openPending(dir string, onBackgroundError func(error)) (*pebble.DB, error) {
	path := filepath.Join(dir, "pending")
	if err := os.Mkdir(path, 0o700); err != nil {
		return nil, err
	}
	db, err := pebble.Open(path, pendingOptions(onBackgroundError))
	if err != nil {
		return nil, errors.Join(err, os.RemoveAll(path))
	}
	return db, nil
}

func pendingOptions(onBackgroundError func(error)) *pebble.Options {
	options := &pebble.Options{
		CacheSize:  buildBlockCacheBytes,
		DisableWAL: true, ErrorIfExists: true, MemTableSize: 4 << 20,
		MemTableStopWritesThreshold: 2,
	}
	if onBackgroundError != nil {
		options.AddEventListener(pebble.EventListener{BackgroundError: onBackgroundError})
	}
	return options
}

func pendingGet[T any](db pebble.Reader, key []byte, decode decodeRow[T]) (v T, err error) {
	raw, closer, err := db.Get(key)
	if errors.Is(err, pebble.ErrNotFound) {
		return v, nil
	}
	if err != nil {
		return v, err
	}
	defer func() { err = errors.Join(err, closer.Close()) }()
	return decode(raw)
}

func pendingVisit(ctx context.Context, db interface {
	NewIterWithContext(context.Context, *pebble.IterOptions) (*pebble.Iterator, error)
}, prefix []byte, visit func(row) error) (err error) {
	it, err := db.NewIterWithContext(ctx, &pebble.IterOptions{LowerBound: prefix, UpperBound: prefixEnd(prefix)})
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, it.Close()) }()
	for ok := it.First(); ok; ok = it.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		v, err := it.ValueAndErr()
		if err != nil {
			return err
		}
		if err := visit(row{it.Key(), v}); err != nil {
			return err
		}
	}
	return errors.Join(ctx.Err(), it.Error())
}

func pendingNodeKey(id string) []byte { return keyStrings([]byte{familyNode}, id) }
func pendingEdgeKey(e *graph.Edge) []byte {
	// Origin is mutable provenance, not a part of the legacy mutation key.
	return appendKeyLine(keyStrings([]byte{familyEdge, edgeOut}, e.From, string(e.Kind), e.To, e.FilePath), e.Line)
}
func pendingReverseKey(e *graph.Edge, primary []byte) []byte {
	return append(appendKeyString([]byte{familyEdge, edgeIn}, e.To), primary...)
}

func (b *BuildStore) pendingNode(id string) (*graph.Node, error) {
	return pendingGet(b.pending, pendingNodeKey(id), decodeNode)
}
func (b *BuildStore) pendingEdge(key []byte) (*graph.Edge, error) {
	return pendingGet(b.pending, key, decodeEdge)
}

func (b *BuildStore) writeNode(n *graph.Node) error {
	if n == nil {
		return nil
	}
	if graph.IsProxyID(n.ID) {
		return invalid("proxy node cannot be published")
	}
	prev, err := b.pendingNode(n.ID)
	if err != nil {
		return err
	}
	if prev != nil {
		prevSynthetic, _ := prev.Meta["synthetic"].(bool)
		nextSynthetic, _ := n.Meta["synthetic"].(bool)
		if prev.Kind == n.Kind && (n.Kind == graph.KindString || (prevSynthetic && nextSynthetic)) && prev.FilePath != "" && n.FilePath != "" && prev.FilePath < n.FilePath {
			return nil
		}
		if n.RepoPrefix == "" && prev.RepoPrefix != "" {
			copy := *n
			copy.RepoPrefix = prev.RepoPrefix
			n = &copy
		}
	}
	value, err := encodeNode(n)
	if err != nil {
		return err
	}
	batch := b.pending.NewBatch()
	defer batch.Close()
	if prev != nil {
		if prev.FilePath != n.FilePath {
			if err := batch.Delete(keyStrings([]byte{familyFile}, prev.FilePath, n.ID), nil); err != nil {
				return err
			}
		}
		if prev.RepoPrefix != n.RepoPrefix {
			if err := batch.Delete(keyStrings([]byte{familyRepo}, prev.RepoPrefix, n.ID), nil); err != nil {
				return err
			}
		}
	}
	if err := batch.Set(keyStrings([]byte{familyFile}, n.FilePath, n.ID), []byte(n.ID), nil); err != nil {
		return err
	}
	if err := batch.Set(keyStrings([]byte{familyRepo}, n.RepoPrefix, n.ID), []byte(n.ID), nil); err != nil {
		return err
	}
	if err := batch.Set(pendingNodeKey(n.ID), value, nil); err != nil {
		return err
	}
	var candidateSize uint64
	if n.Name != "" || n.QualName != "" {
		candidateSize = rebuildCandidateEncodedSize(value)
	}
	if err := updateRebuildCandidates(batch, prev, n, candidateSize); err != nil {
		return err
	}
	if err := batch.Commit(pebble.NoSync); err != nil {
		return err
	}
	b.v3NodeWrites++
	return nil
}

func (b *BuildStore) putEdge(e *graph.Edge) error {
	return b.writePendingEdge(e, rebuildEdgeCustody{}, false)
}

func (b *BuildStore) putEdgeWithCustody(e *graph.Edge, custody rebuildEdgeCustody) error {
	return b.writePendingEdge(e, custody, false)
}

// clearRecorder lets a payload replacement invalidate the old key's
// recorder when the incoming row has no lineage. Same-key stamps leave it.
func (b *BuildStore) writePendingEdge(e *graph.Edge, custody rebuildEdgeCustody, clearRecorder bool) error {
	if graph.IsProxyID(e.From) || graph.IsProxyID(e.To) {
		return invalid("proxy edge cannot be published")
	}
	key := pendingEdgeKey(e)
	raw, err := encodeEdge(e)
	if err != nil {
		return err
	}
	recorder, err := custody.encode(raw)
	if err != nil {
		return err
	}
	batch := b.pending.NewBatch()
	defer batch.Close()
	if err := batch.Set(key, raw, nil); err != nil {
		return err
	}
	if err := batch.Set(pendingReverseKey(e, key), key, nil); err != nil {
		return err
	}
	if recorder != nil {
		if err := batch.Set(rebuildLinkRecorderKey(key), recorder, nil); err != nil {
			return err
		}
	} else if clearRecorder {
		if err := batch.Delete(rebuildLinkRecorderKey(key), nil); err != nil {
			return err
		}
	}
	return batch.Commit(pebble.NoSync)
}

func (b *BuildStore) deleteEdge(key []byte, edge *graph.Edge) error {
	// Central deletes also invalidate lineage for any later reuse of this key.
	recorderKey := rebuildLinkRecorderKey(key)
	recorded, err := pendingGet(b.pending, recorderKey, func([]byte) (bool, error) {
		return true, nil
	})
	if err != nil {
		return err
	}
	batch := b.pending.NewBatch()
	defer batch.Close()
	if err := batch.Delete(key, nil); err != nil {
		return err
	}
	if err := batch.Delete(pendingReverseKey(edge, key), nil); err != nil {
		return err
	}
	if recorded {
		if err := batch.Delete(recorderKey, nil); err != nil {
			return err
		}
	}
	return batch.Commit(pebble.NoSync)
}

func (b *BuildStore) evict(prefix []byte) (nodes, edges int, err error) {
	// Snapshot iterators over the private index can coexist with writes. Only
	// one record is decoded at a time. The reverse scan is opened after the
	// forward deletes, so self-loops and edges touching two victims count once.
	ctx := context.Background()
	err = pendingVisit(ctx, b.pending, prefix, func(r row) error {
		n, err := b.pendingNode(string(r.value))
		if err != nil {
			return err
		}
		if n == nil {
			return invalid("missing node in private eviction index")
		}
		out := keyStrings([]byte{familyEdge, edgeOut}, n.ID)
		if err := pendingVisit(ctx, b.pending, out, func(r row) error {
			e, err := decodeEdge(r.value)
			if err != nil {
				return err
			}
			if err := b.deleteEdge(r.key, e); err != nil {
				return err
			}
			edges++
			return nil
		}); err != nil {
			return err
		}
		in := keyStrings([]byte{familyEdge, edgeIn}, n.ID)
		if err := pendingVisit(ctx, b.pending, in, func(r row) error {
			e, err := b.pendingEdge(r.value)
			if err != nil {
				return err
			}
			if e == nil {
				return invalid("missing edge in private reverse index")
			}
			if err := b.deleteEdge(r.value, e); err != nil {
				return err
			}
			edges++
			return nil
		}); err != nil {
			return err
		}
		batch := b.pending.NewBatch()
		defer batch.Close()
		for _, k := range [][]byte{pendingNodeKey(n.ID), keyStrings([]byte{familyFile}, n.FilePath, n.ID), keyStrings([]byte{familyRepo}, n.RepoPrefix, n.ID), pendingSymbolKey(n.ID)} {
			if err := batch.Delete(k, nil); err != nil {
				return err
			}
		}
		if err := updateRebuildCandidates(batch, n, nil, 0); err != nil {
			return err
		}
		if err := batch.Commit(pebble.NoSync); err != nil {
			return err
		}
		b.v3NodeWrites++
		nodes++
		return nil
	})
	return nodes, edges, err
}

func addPendingCount(db *pebble.DB, key []byte, increment int) error {
	count, err := pendingGet(db, key, decodeCount)
	if err != nil {
		return err
	}
	return db.Set(key, encodeCount(count+increment), pebble.NoSync)
}

func pendingSymbolKey(id string) []byte  { return keyStrings([]byte{familySymbol}, id) }
func copyValue(v []byte) ([]byte, error) { return bytes.Clone(v), nil }
