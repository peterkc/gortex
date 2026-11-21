package store_pebble

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/graph"
)

// BuildStore holds the private legacy mutation surface before a first publish.
// It is not a storage choice for a running daemon. Only the build owner may
// publish, after all mutation calls have returned.
type BuildStore struct {
	pending       *pebble.DB
	db            *pebble.DB
	options       *pebble.Options
	scratchParent string
	profile       Profile
	mu            sync.Mutex
	published     bool
	first         error
	readErr       error
	resolveMu     sync.Mutex
}

var _ graph.Store = (*BuildStore)(nil)

// OpenBuild opens a new private head. It refuses an existing head.
func OpenBuild(path string, profile Profile) (*BuildStore, error) {
	if profile == "" {
		profile = DefaultProfile
	}
	if profile != DefaultProfile && profile != FullProfile {
		return nil, invalid("unknown index.profile")
	}
	if path == "" {
		return nil, invalid("empty private head path")
	}
	options := headOptions()
	options.ErrorIfExists = true
	db, err := pebble.Open(path, options)
	if err != nil {
		return nil, err
	}
	pending, err := openPending(path)
	if err != nil {
		return nil, errors.Join(err, db.Close())
	}
	return &BuildStore{pending: pending, db: db, options: options, scratchParent: path, profile: profile}, nil
}

func (b *BuildStore) Close() error {
	return errors.Join(b.pending.Close(), os.RemoveAll(filepath.Join(b.scratchParent, "pending")), b.db.Close())
}

// OpenSnapshot returns a new read-only request over the published bytes.
func (b *BuildStore) OpenSnapshot() (*Snapshot, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.published {
		return nil, ErrUnpublishedBuild
	}
	return &Snapshot{snap: b.db.NewSnapshot(), profile: b.profile}, nil
}

func nodeIndexKeys(n *graph.Node) [][]byte {
	keys := [][]byte{
		keyStrings([]byte{familyFile}, n.RepoPrefix, n.FilePath, n.ID),
		keyStrings([]byte{familyName, nameExact}, n.Name, n.ID),
		keyStrings([]byte{familyRepo}, n.RepoPrefix, n.ID),
		keyStrings([]byte{familyNodeKind}, string(n.Kind), n.ID),
	}
	keys = append(keys,
		keyStrings([]byte{familyName, nameLanguage}, n.Language, n.ID),
		keyStrings([]byte{familyName, nameRepoLanguage}, n.RepoPrefix, n.Language, n.ID),
	)
	if !graph.IsContentNode(n) {
		keys = append(keys, keyStrings([]byte{familyName, nameNonContent}, n.RepoPrefix, n.ID))
	}
	if n.QualName != "" {
		keys = append(keys, keyStrings([]byte{familyName, nameQualified}, n.QualName, n.ID))
	}
	for _, gram := range nameGrams(strings.ToLower(n.Name)) {
		keys = append(keys, keyStrings([]byte{familyName, nameGram}, gram, n.ID))
	}
	return keys
}

func edgeIndexRows(e *graph.Edge, id [16]byte, repo string, outKey, value []byte) []row {
	kind := string(e.Kind)
	return []row{
		{append(keyStrings([]byte{familyEdge, edgeIn}, e.To, kind), id[:]...), value},
		{append(keyStrings([]byte{familyEdge, edgeInRepo}, e.To, repo), id[:]...), value},
		{append(appendKeyLine(keyStrings([]byte{familyEdge, edgeOutKind}, e.From, kind), e.Line), id[:]...), value},
		{append(appendKeyLine(keyStrings([]byte{familyEdge, edgeKind}, kind, e.From), e.Line), id[:]...), outKey},
	}
}

func (b *BuildStore) Publish(ctx context.Context) (err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	// An unsupported legacy mutation cannot be mistaken for a complete build.
	if b.first != nil {
		return b.first
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	dir, err := os.MkdirTemp(b.scratchParent, "gold-")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, os.RemoveAll(dir)) }()
	format := b.db.FormatMajorVersion().MaxTableFormat()
	gold := sortedRuns{dir: dir, name: "graph", options: b.options, format: format, limit: goldBufferBytes}
	refs := sortedRuns{dir: dir, name: "refs", options: b.options, format: format, limit: goldBufferBytes}
	seal := &goldSeal{refs: refs, owner: b.keptOwner}
	if err := b.writeRows(ctx, &gold); err != nil {
		return err
	}
	if err := b.writeSymbols(ctx, &gold); err != nil {
		return err
	}
	if err := pendingVisit(ctx, b.pending, []byte{familyFileMeta, shingleRows}, func(r row) error {
		return gold.add(bytes.Clone(r.key), bytes.Clone(r.value))
	}); err != nil {
		return err
	}
	if err := publishGold(ctx, b.db, &gold, seal); err != nil {
		return err
	}
	b.published = true
	return nil
}

func (b *BuildStore) keptOwner(id string) (string, error) {
	n, err := pendingGet(b.pending, pendingNodeKey(id), decodeNodeLight)
	if n == nil || err != nil || !b.profile.keepsNode(n.Kind) {
		return "", err
	}
	return n.RepoPrefix, nil
}

func (b *BuildStore) writeRows(ctx context.Context, gold *sortedRuns) error {
	// Counters include per-node adjacency totals, so they must spill too.
	// Reset just the derived counter keys. Legacy logical rows stay intact.
	prefix := []byte{familyCount}
	if err := b.pending.DeleteRange(prefix, prefixEnd(prefix), pebble.NoSync); err != nil {
		return err
	}
	addCounts := func(keys ...[]byte) error {
		for _, key := range keys {
			if err := addPendingCount(b.pending, key, 1); err != nil {
				return err
			}
		}
		return nil
	}
	if err := pendingVisit(ctx, b.pending, []byte{familyNode}, func(r row) error {
		n, err := decodeNode(r.value)
		if err != nil {
			return err
		}
		if !b.profile.keepsNode(n.Kind) {
			return nil
		}
		if err := gold.add(bytes.Clone(r.key), bytes.Clone(r.value)); err != nil {
			return err
		}
		for _, key := range nodeIndexKeys(n) {
			if err := gold.add(key, []byte(n.ID)); err != nil {
				return err
			}
		}
		repo, kind := n.RepoPrefix, string(n.Kind)
		if err := addPendingCount(b.pending, countKey("repo node bytes", repo), len(r.value)); err != nil {
			return err
		}
		if err := addCounts(countKey("nodes"), countKey("repo", repo), countKey("node kind", kind), countKey("repo kind", repo, kind)); err != nil {
			return err
		}
		if n.Language != "" {
			return addCounts(countKey("language", n.Language), countKey("repo language", repo, n.Language))
		}
		return nil
	}); err != nil {
		return err
	}
	if err := pendingVisit(ctx, b.pending, []byte{familyEdge, edgeOut}, func(r row) error {
		e, err := decodeEdge(r.value)
		if err != nil {
			return err
		}
		kind := string(e.Kind)
		if err := addCounts(countKey("out total", e.From), countKey("in total", e.To), countKey("out kind", e.From, kind), countKey("in kind", e.To, kind)); err != nil {
			return err
		}
		if !b.profile.keepsEdge(e.Kind) {
			return nil
		}
		repo, err := b.keptOwner(e.From)
		if err != nil {
			return err
		}
		id := edgeIdentity(e)
		key, value := outEdgeKey(e, id), bytes.Clone(r.value)
		if err := gold.add(key, value); err != nil {
			return err
		}
		for _, pair := range edgeIndexRows(e, id, repo, key, value) {
			if err := gold.add(pair.key, pair.value); err != nil {
				return err
			}
		}
		if err := addPendingCount(b.pending, countKey("repo edge bytes", repo), len(r.value)); err != nil {
			return err
		}
		return addCounts(countKey("edges"), countKey("repo edges", repo), countKey("edge kind", kind))
	}); err != nil {
		return err
	}
	revisions, err := pendingGet(b.pending, revisionKey, decodeCount)
	if err != nil {
		return err
	}
	if err := gold.add(countKey("revisions"), encodeCount(revisions)); err != nil {
		return err
	}
	return pendingVisit(ctx, b.pending, prefix, func(r row) error {
		return gold.add(bytes.Clone(r.key), bytes.Clone(r.value))
	})
}
