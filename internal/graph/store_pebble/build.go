package store_pebble

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
	pending          *pebble.DB
	db               *pebble.DB
	options          *pebble.Options
	scratchParent    string
	profile          Profile
	mu               sync.Mutex
	published        bool
	first            error
	readErr          error
	cleanupErr       error
	resolveMu        sync.Mutex
	v3NodeWrites     uint64 // protected by mu; edge moves do not change it
	contractOverlay  bool   // protected by mu; never present at a stage offer
	contractsStarted bool
	contractsSealed  HeadDigest

	rebuildLinksIsolated     bool              // protected by mu
	rebuildIsolationCoverage *RelationCoverage // checked census; protected by mu
}

var _ graph.Store = (*BuildStore)(nil)

// OpenBuild opens a new private head. It refuses an existing head.
func OpenBuild(path string, profile Profile) (*BuildStore, error) {
	return openBuild(path, profile, nil)
}

// OpenWorkerBuild is only for the isolated build worker process. On a Pebble
// background error it exits instead of leaving a caller waiting for a flush.
// The owner must reap the child and remove its lease.
func OpenWorkerBuild(path string, profile Profile) (*BuildStore, error) {
	var once sync.Once
	return openBuild(path, profile, func(err error) {
		once.Do(func() {
			_, _ = fmt.Fprintf(os.Stderr, "rebuild build worker: Pebble scratch background error: %q\n", err.Error())
			os.Exit(1)
		})
	})
}

func openBuild(path string, profile Profile, onBackgroundError func(error)) (*BuildStore, error) {
	if profile == "" {
		profile = DefaultProfile
	}
	if profile != DefaultProfile && profile != FullProfile {
		return nil, invalid("unknown index.profile")
	}
	if path == "" {
		return nil, invalid("empty private head path")
	}
	pendingPath := filepath.Join(path, "pending")
	if _, err := os.Lstat(pendingPath); !os.IsNotExist(err) {
		if err != nil {
			return nil, err
		}
		return nil, &os.PathError{Op: "open", Path: pendingPath, Err: os.ErrExist}
	}
	options := headOptions()
	options.ErrorIfExists = true
	if onBackgroundError != nil {
		options.AddEventListener(pebble.EventListener{BackgroundError: onBackgroundError})
	}
	db, err := pebble.Open(path, options)
	if err != nil {
		return nil, err
	}
	pending, err := openPending(path, onBackgroundError)
	if err != nil {
		return nil, errors.Join(err, db.Close())
	}
	return &BuildStore{pending: pending, db: db, options: options, scratchParent: path, profile: profile}, nil
}

func (b *BuildStore) Close() error {
	return errors.Join(b.cleanupErr, b.pending.Close(), os.RemoveAll(filepath.Join(b.scratchParent, "pending")), b.db.Close())
}

// OpenSnapshot returns a new read-only request over the published bytes.
func (b *BuildStore) OpenSnapshot() (*Snapshot, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.published {
		return nil, ErrUnpublishedBuild
	}
	return &Snapshot{snap: pebbleSnapshotReader{b.db.NewSnapshot()}, profile: b.profile}, nil
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

func edgeIndexRows(e *graph.Edge, id [16]byte, owner sourceOwner, outKey, value []byte) []row {
	kind := string(e.Kind)
	rows := []row{
		{append(keyStrings([]byte{familyEdge, edgeIn}, e.To, kind), id[:]...), value},
		{append(appendKeyLine(keyStrings([]byte{familyEdge, edgeOutKind}, e.From, kind), e.Line), id[:]...), value},
		{append(appendKeyLine(keyStrings([]byte{familyEdge, edgeKind}, kind, e.From), e.Line), id[:]...), outKey},
	}
	if owner.present {
		rows = append(rows, row{append(keyStrings([]byte{familyEdge, edgeInRepo}, e.To, owner.repo), id[:]...), value})
	}
	return rows
}

func (b *BuildStore) Publish(ctx context.Context) (err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.contractOverlay {
		return ErrRebuildContractOverlay
	}
	// An unsupported legacy mutation cannot be mistaken for a complete build.
	if b.first != nil {
		return b.first
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	dir, err := os.MkdirTemp(filepath.Join(b.scratchParent, "pending"), "gold-")
	if err != nil {
		return err
	}
	defer func() {
		if cleanupErr := os.RemoveAll(dir); cleanupErr != nil {
			if err == nil {
				// Publication is already visible. Close reports the cleanup failure.
				b.cleanupErr = errors.Join(b.cleanupErr, cleanupErr)
			} else {
				err = errors.Join(err, cleanupErr)
			}
		}
	}()
	format := b.db.FormatMajorVersion().MaxTableFormat()
	gold := sortedRuns{dir: dir, name: "graph", options: b.options, format: format, limit: goldBufferBytes}
	refs := sortedRuns{dir: dir, name: "refs", options: b.options, format: format, limit: goldBufferBytes}
	seal := &goldSeal{refs: refs, owner: b.keptOwner, edgeOwner: b.rebuildEdgeSourceOwner}
	if err := b.writeRows(ctx, &gold, StageSearch); err != nil {
		return err
	}
	if err := b.writeSymbols(ctx, &gold, exactFromNodes); err != nil {
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

type sourceOwner struct {
	repo         string
	present, off bool
}

func (b *BuildStore) keptOwner(id string) (sourceOwner, error) {
	n, err := pendingGet(b.pending, pendingNodeKey(id), decodeNodeLight)
	if n == nil || err != nil {
		return sourceOwner{}, err
	}
	kept := b.profile.keepsNode(n.Kind)
	return sourceOwner{repo: n.RepoPrefix, present: kept, off: !kept}, nil
}

// Main reconciles placeholder sources; the return lift uses the selected Call.
// Both can name a Go builtin function or a type conversion.
func rebuildGoBuiltinSource(e *graph.Edge) (graph.PlatformBuiltin, bool) {
	entry, ok := graph.LookupPlatformBuiltin(graph.PlatformCatalogRevision, e.From)
	return entry, ok && entry.Language == "go" && (entry.Category == "" || entry.Category == "type") &&
		(graph.PlaceholderSourceKind(e.Kind) || e.Kind == graph.EdgeReturnsTo)
}

// The node-free source takes its scope from the recorded Go file, not the
// builtin ID. Producer custody ties that scope to a same-site move.
func (b *BuildStore) provedBuiltinSourceOwner(e *graph.Edge) (sourceOwner, error) {
	entry, ok := rebuildGoBuiltinSource(e)
	if !ok || e.FilePath == "" || e.Line <= 0 {
		return sourceOwner{}, invalid("invalid builtin source role")
	}
	c, err := readRebuildEdgeCustody(b.pending, pendingEdgeKey(e))
	if err != nil {
		return sourceOwner{}, err
	}
	if c.proof != rebuildProofGoBuiltinSource {
		return sourceOwner{}, invalid("unproved builtin source repoint")
	}
	file, err := pendingGet(b.pending, pendingNodeKey(e.FilePath), decodeNodeLight)
	if err != nil {
		return sourceOwner{}, err
	}
	if file == nil || file.ID != e.FilePath || file.FilePath != e.FilePath || file.Kind != graph.KindFile ||
		file.Language != "go" || entry.ID(file.RepoPrefix) != e.From {
		return sourceOwner{}, invalid("builtin source recording scope differs")
	}
	return sourceOwner{repo: file.RepoPrefix, present: true}, nil
}

func (b *BuildStore) rebuildEdgeSourceOwner(e *graph.Edge) (sourceOwner, error) {
	if graph.IsBuiltinStub(e.From) {
		return b.provedBuiltinSourceOwner(e)
	}
	return b.keptOwner(e.From)
}

// projectedEdgeOwner distinguishes an omitted declaration from an absent
// endpoint. Only the former removes the edge and its adjacency counts.
func (b *BuildStore) projectedEdgeOwner(e *graph.Edge) (sourceOwner, bool, error) {
	owner, err := b.rebuildEdgeSourceOwner(e)
	if err != nil || owner.off {
		return owner, false, err
	}
	if b.profile == FullProfile {
		return owner, true, nil
	}
	target, err := b.keptOwner(e.To)
	return owner, !target.off, err
}

func (b *BuildStore) writeRows(ctx context.Context, gold *sortedRuns, stage Stage) error {
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
		if n.Kind == graph.KindBuiltin {
			return invalid("builtin node cannot be published")
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
	if stage == StageFileGraph {
		return pendingVisit(ctx, b.pending, prefix, func(r row) error {
			return gold.add(bytes.Clone(r.key), bytes.Clone(r.value))
		})
	}
	if err := pendingVisit(ctx, b.pending, []byte{familyEdge, edgeOut}, func(r row) error {
		e, err := decodeEdge(r.value)
		if err != nil {
			return err
		}
		var owner sourceOwner
		if b.profile != FullProfile {
			var keep bool
			owner, keep, err = b.projectedEdgeOwner(e)
			if err != nil || !keep {
				return err
			}
		}
		kind := string(e.Kind)
		if err := addCounts(countKey("out total", e.From), countKey("in total", e.To), countKey("out kind", e.From, kind), countKey("in kind", e.To, kind)); err != nil {
			return err
		}
		if !b.profile.keepsEdge(e.Kind) {
			return nil
		}
		if b.profile == FullProfile {
			owner, err = b.rebuildEdgeSourceOwner(e)
			if err != nil {
				return err
			}
		}
		repo := owner.repo
		if !owner.present {
			repo = "" // Keep existing accounting for valid node-free sources.
		}
		id := edgeIdentity(e)
		key, value := outEdgeKey(e, id), bytes.Clone(r.value)
		if err := gold.add(key, value); err != nil {
			return err
		}
		for _, pair := range edgeIndexRows(e, id, owner, key, value) {
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
	if stage == StageRelations || stage == StageContracts {
		if err := pendingVisit(ctx, b.pending, []byte{familyIsolation}, func(r row) error {
			return gold.add(bytes.Clone(r.key), bytes.Clone(r.value))
		}); err != nil {
			return err
		}
	}
	if stage == StageContracts {
		if err := b.writeContractShapes(ctx, gold); err != nil {
			return err
		}
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
