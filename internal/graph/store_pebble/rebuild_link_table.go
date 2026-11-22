package store_pebble

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"slices"

	"github.com/cockroachdb/pebble/v2/sstable"
	"github.com/cockroachdb/pebble/v2/vfs"
	"github.com/zzet/gortex/internal/graph"
)

const (
	// LinkPageRequests is the maximum number of positions in a single page.
	LinkPageRequests   = 4096
	linkPartitionLimit = 2
	linkResultRows     = 1 << 18
	linkResultBytes    = 128 << 20
	linkRecordBytes    = 16 << 20
	linkResultKeyBytes = 12 // global request position (4), then source ordinal (8)
)

// LinkKeyKind selects the exact name or qualified name index.
type LinkKeyKind byte

const (
	LinkName LinkKeyKind = iota + 1
	LinkQualName
)

// LinkRequest is one position in a resolver page. RepoPrefix is exact (even
// when empty) unless AllRepos is set. An empty Languages list permits any
// language. Neither caller restriction changes the owner of the join key.
type LinkRequest struct {
	Kind       LinkKeyKind
	Key        string
	RepoPrefix string
	Languages  []string
	AllRepos   bool
}

func (r LinkRequest) match(n *graph.Node) bool {
	key := n.Name
	if r.Kind == LinkQualName {
		key = n.QualName
	}
	return key == r.Key && (r.AllRepos || n.RepoPrefix == r.RepoPrefix) &&
		(len(r.Languages) == 0 || slices.Contains(r.Languages, n.Language))
}

func linkPrefix(kind LinkKeyKind, key string) []byte {
	return appendKeyString([]byte{byte(kind)}, key)
}

func linkPartition(prefix []byte, partitions int) int {
	h := fnv.New64a()
	_, _ = h.Write(prefix)
	return int(h.Sum64() % uint64(partitions))
}

func linkPartitionPath(dir string, part int) string {
	return filepath.Join(dir, fmt.Sprintf("partition-%d", part), "candidates.sst")
}

func linkReplyPath(partition string, id uint64) string {
	return filepath.Join(filepath.Dir(partition), fmt.Sprintf("result-%d.sst", id))
}

func newLinkTable(path string) (*sstable.Writer, error) {
	return newTable(path, sstable.WriterOptions{
		Compression: sstable.SnappyCompression, BlockSize: 32 << 10,
		TableFormat: sstable.TableFormatPebblev4,
	})
}

func openLinkTable(ctx context.Context, path string) (*sstable.Reader, error) {
	file, err := vfs.Default.Open(path)
	if err != nil {
		return nil, err
	}
	readable, err := sstable.NewSimpleReadable(file)
	if err != nil {
		return nil, errors.Join(err, file.Close())
	}
	table, err := sstable.NewReader(ctx, readable, sstable.ReaderOptions{})
	if err != nil {
		return nil, errors.Join(err, readable.Close())
	}
	return table, nil
}

// The caller streams facts in snapshot order. The ordinal is a tie-breaker,
// not part of the hash key. Sorting runs therefore preserves the order within
// each complete join key, including duplicate candidate rows.
func buildLinkTables(ctx context.Context, dir string, partitions int, source func(func(*graph.Node) error) error) error {
	runsDir := filepath.Join(dir, "runs")
	if err := os.Mkdir(runsDir, 0700); err != nil {
		return err
	}
	runs := sortedRuns{
		dir: runsDir, name: "candidates", limit: goldBufferBytes,
		options: headOptions(), format: sstable.TableFormatPebblev4,
	}
	var ordinal uint64
	if err := source(func(n *graph.Node) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if n == nil || n.ID == "" {
			return errors.New("candidate has no node identity")
		}
		ordinal++
		if n.Name == "" && n.QualName == "" {
			return nil
		}
		value, err := encodeNode(n)
		if err != nil {
			return fmt.Errorf("encode candidate %q: %w", n.ID, err)
		}
		for i, key := range [...]string{n.Name, n.QualName} {
			if key == "" {
				continue
			}
			prefix := linkPrefix(LinkKeyKind(i)+LinkName, key)
			if len(prefix)+8+len(value) > linkRecordBytes {
				return fmt.Errorf("candidate record exceeds %d bytes", linkRecordBytes)
			}
			if err := runs.add(binary.BigEndian.AppendUint64(prefix, ordinal), value); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if err := writeLinkPartitions(ctx, dir, partitions, &runs); err != nil {
		return err
	}
	return os.RemoveAll(runsDir)
}

func writeLinkPartitions(ctx context.Context, dir string, partitions int, runs *sortedRuns) (err error) {
	var writers [linkPartitionLimit]*sstable.Writer
	for i := range partitions {
		path := linkPartitionPath(dir, i)
		if err := os.Mkdir(filepath.Dir(path), 0700); err != nil {
			return err
		}
		w, e := newLinkTable(path)
		if e != nil {
			return e
		}
		defer func() { err = errors.Join(err, w.Close()) }()
		writers[i] = w
	}
	return runs.merge(ctx, func(r row) error {
		part := linkPartition(r.key[:len(r.key)-8], partitions)
		return writers[part].Set(r.key, r.value)
	})
}

// LinkPageStats counts actual work across all partitions of a successful page.
// ResultBytes counts serialized keys and values. SpillBytes is on-disk size,
// counted only for result SSTs written before the page finished selecting.
type LinkPageStats struct {
	ScannedRows int
	ResultRows  int
	ResultBytes int
	SpillRows   int
	SpillBytes  int
}

type linkResultWriter struct {
	path        string
	limit       int
	buffered    []row
	bufferBytes int
	writer      *sstable.Writer
	stats       LinkPageStats
}

func (w *linkResultWriter) add(key, value []byte) error {
	size := len(key) + len(value)
	if size > linkRecordBytes || w.stats.ResultRows == linkResultRows || w.stats.ResultBytes+size > linkResultBytes {
		return errors.New("candidate result exceeds link record or page bound")
	}
	w.stats.ResultRows++
	w.stats.ResultBytes += size
	// Count the retained slice headers as well as the cloned payload.
	if w.writer == nil && w.bufferBytes+size+48 <= w.limit {
		w.buffered = append(w.buffered, row{bytes.Clone(key), bytes.Clone(value)})
		w.bufferBytes += size + 48
		return nil
	}
	if w.writer == nil {
		if err := w.flush(); err != nil {
			return err
		}
	}
	return w.writer.Set(key, value)
}

func (w *linkResultWriter) flush() error {
	writer, err := newLinkTable(w.path)
	if err != nil {
		return err
	}
	w.writer = writer
	for _, r := range w.buffered {
		if err := writer.Set(r.key, r.value); err != nil {
			return err
		}
	}
	w.buffered = nil
	return nil
}

func selectLinkPartition(ctx context.Context, it sstable.Iterator, path string, limit int, requests []linkPositionedRequest) (stats LinkPageStats, err error) {
	out := linkResultWriter{path: path, limit: limit}
	defer func() {
		if out.writer != nil {
			err = errors.Join(err, out.writer.Close())
		}
	}()
	for _, req := range requests {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		prefix := linkPrefix(req.Kind, req.Key)
		for kv := it.SeekGE(prefix, 0); kv != nil && bytes.HasPrefix(kv.K.UserKey, prefix); kv = it.Next() {
			if err := ctx.Err(); err != nil {
				return stats, err
			}
			if len(kv.K.UserKey) != len(prefix)+8 {
				return stats, errors.New("invalid candidate ordinal")
			}
			raw, _, err := kv.Value(nil)
			if err != nil {
				return stats, err
			}
			if len(kv.K.UserKey)+len(raw) > linkRecordBytes {
				return stats, errors.New("oversized candidate record")
			}
			n, err := decodeNodeLight(raw)
			if err != nil {
				return stats, err
			}
			out.stats.ScannedRows++
			if !req.match(n) {
				continue
			}
			var key [linkResultKeyBytes]byte
			binary.BigEndian.PutUint32(key[:], uint32(req.position))
			copy(key[4:], kv.K.UserKey[len(prefix):])
			if err := out.add(key[:], raw); err != nil {
				return stats, err
			}
		}
		if err := it.Error(); err != nil {
			return stats, err
		}
	}
	if out.stats.ResultRows == 0 {
		return out.stats, nil
	}
	spilled := out.writer != nil
	if !spilled {
		if err := out.flush(); err != nil {
			return stats, err
		}
	}
	writer := out.writer
	out.writer = nil
	if err := writer.Close(); err != nil {
		return stats, err
	}
	if spilled {
		info, err := os.Stat(path)
		if err != nil {
			return stats, err
		}
		out.stats.SpillRows = out.stats.ResultRows
		out.stats.SpillBytes = int(info.Size())
	}
	return out.stats, nil
}
