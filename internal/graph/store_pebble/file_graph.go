package store_pebble

import (
	"bytes"
	"context"
	"encoding/binary"
	"path/filepath"
	"slices"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zeebo/blake3"
	"github.com/zzet/gortex/internal/graph"
)

const fileMetaRows byte = 3

func fileMetaKey(repo, path string) []byte {
	return keyStrings([]byte{familyFileMeta, fileMetaRows}, repo, path)
}

func encodeFileMeta(row graph.FileMetaRow) ([]byte, error) {
	if row.FilePath == "" || row.Size < 0 || row.NodeCount < 0 {
		return nil, invalid("file metadata shape")
	}
	b := appendMetaString([]byte{1}, row.ContentHash)
	b = binary.AppendVarint(b, int64(row.Size))
	b = binary.AppendVarint(b, int64(row.NodeCount))
	return appendMetaString(b, row.Errors), nil
}

func decodeFileMeta(path string, raw []byte) (graph.FileMetaRow, error) {
	if len(raw) == 0 || raw[0] != 1 || path == "" {
		return graph.FileMetaRow{}, invalid("file metadata format")
	}
	d := recordCursor{buf: raw[1:]}
	m := graph.FileMetaRow{
		FilePath: path, ContentHash: d.str(), Size: d.integer(), NodeCount: d.integer(), Errors: d.str(),
	}
	if m.Size < 0 || m.NodeCount < 0 {
		return graph.FileMetaRow{}, invalid("file metadata shape")
	}
	return m, d.finish()
}

func checkFileMetaRow(r row) error {
	_, tail, err := readKeyString(r.key[2:])
	if err != nil {
		return err
	}
	path, tail, err := readKeyString(tail)
	if err != nil || len(tail) != 0 {
		return invalid("file metadata key")
	}
	_, err = decodeFileMeta(path, r.value)
	return err
}

// AddParsedFile takes one cached extraction. Raw edges enter private scratch,
// not the file-graph stage. The caller supplies files in manifest order.
func (b *BuildStore) AddParsedFile(repo string, f ParseFacts) error {
	return b.addParsedFile(repo, f, nil)
}

// AddParsedFileSelected replays a qualified receipt. Keep must be the node
// selection from ReduceRebuildSharedClaims. Edges, sidecars and the original
// per-file extraction count are not filtered.
func (b *BuildStore) AddParsedFileSelected(repo string, f ParseFacts, keep func(string) (bool, error)) error {
	return b.addParsedFile(repo, f, keep)
}

func (b *BuildStore) addParsedFile(repo string, f ParseFacts, keep func(string) (bool, error)) error {
	return b.withWrites(func() error {
		if f.File.NodeCount != len(f.Nodes) {
			return invalid("file metadata does not count its extracted nodes")
		}
		key := fileMetaKey(repo, f.File.FilePath)
		raw, err := pendingGet(b.pending, key, copyValue)
		if err != nil {
			return err
		}
		if raw != nil {
			return invalid("repeated parse facts for one file")
		}
		raw, err = encodeFileMeta(f.File)
		if err != nil {
			return err
		}
		for _, n := range f.Nodes {
			if keep != nil {
				selected, err := keep(n.ID)
				if err != nil {
					return err
				}
				if !selected {
					continue
				}
			}
			if err := b.writeNode(n); err != nil {
				return err
			}
		}
		for _, e := range f.Edges {
			if err := b.addEdge(e); err != nil {
				return err
			}
			if err := b.recordExternalReceipt(f, e); err != nil {
				return err
			}
		}
		if err := b.writeParsedSidecars(repo, f); err != nil {
			return err
		}
		return b.pending.Set(key, raw, pebble.NoSync)
	})
}

// PrepareFileGraph seals declarations and file metadata against the exact
// manifest witness. The worker calls this before running any resolver pass.
// The daemon owns the stage's private directory lease and reception.
func (b *BuildStore) PrepareFileGraph(ctx context.Context, dir string, manifest Witness, incarnation string) (StageTables, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.first != nil {
		return StageTables{}, b.first
	}
	if !filepath.IsAbs(dir) || incarnation == "" || manifest.Profile != b.profile || manifest.Stage != StageManifest ||
		len(manifest.Families) != 1 || manifest.Families[0].Family != familyManifest {
		return StageTables{}, invalid("file graph needs its manifest witness")
	}
	raw, err := encodeWitness(manifest)
	if err != nil {
		return StageTables{}, err
	}
	format := b.db.FormatMajorVersion().MaxTableFormat()
	gold := sortedRuns{dir: dir, name: "file-graph", options: b.options, format: format, limit: goldBufferBytes}
	refs := sortedRuns{dir: dir, name: "file-refs", options: b.options, format: format, limit: goldBufferBytes}
	seal := &goldSeal{refs: refs, owner: b.keptOwner, edgeOwner: b.rebuildEdgeSourceOwner}
	if err := b.writeRows(ctx, &gold, StageFileGraph); err != nil {
		return StageTables{}, err
	}
	var files uint64
	if err := pendingVisit(ctx, b.pending, []byte{familyFileMeta, fileMetaRows}, func(r row) error {
		files++
		return gold.add(bytes.Clone(r.key), bytes.Clone(r.value))
	}); err != nil {
		return StageTables{}, err
	}
	if files != manifest.Families[0].Count {
		return StageTables{}, invalid("not every admitted file has parsed metadata")
	}
	w := manifest
	w.Stage = StageFileGraph
	w.Families = slices.Clone(w.Families)
	stream := goldWitness{digest: blake3.New()}
	paths, err := writeGoldTables(ctx, nil, &gold, goldFileCap-1, func(r row) error {
		if err := seal.visit(r); err != nil {
			return err
		}
		stream.add(r)
		return nil
	}, seal.finish)
	if err != nil {
		return StageTables{}, err
	}
	stream.flush()
	w.Families = append(w.Families, stream.families...)
	slices.SortFunc(w.Families, func(a, b FamilyWitness) int { return int(a.Family) - int(b.Family) })
	for i, path := range paths {
		paths[i] = filepath.Base(path)
	}
	return StageTables{Dir: dir, Files: paths, Incarnation: incarnation, Base: blake3.Sum256(raw), Witness: w}, nil
}

// FileMetadata reads the exact source receipt once declarations are ready.
func (s *Snapshot) FileMetadata(repo, path string) (graph.FileMetaRow, error) {
	if err := s.RequireStage(StageFileGraph); err != nil {
		return graph.FileMetaRow{}, err
	}
	return point(s, fileMetaKey(repo, path), func(raw []byte) (graph.FileMetaRow, error) {
		return decodeFileMeta(path, raw)
	})
}
