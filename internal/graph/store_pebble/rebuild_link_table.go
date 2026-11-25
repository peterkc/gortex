package store_pebble

import (
	"context"
	"errors"
	"slices"

	"github.com/cockroachdb/pebble/v2/sstable"
	"github.com/cockroachdb/pebble/v2/vfs"
	"github.com/zzet/gortex/internal/graph"
)

const (
	// LinkPageRequests is the maximum number of positions in a single page.
	LinkPageRequests   = 4096
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
