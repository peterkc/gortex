package store_pebble

import (
	"context"
	"encoding/binary"
	"errors"
	"path/filepath"

	"github.com/zeebo/blake3"
)

const familyManifest byte = 'H'

// ManifestFile is one admitted input. Kind is the Merkle tag for the bytes
// hashed (regular content 'f' or unresolved symlink text 'l').
type ManifestFile struct {
	Path, Language     string
	Size               int64
	Kind               byte
	Content, Extractor HeadDigest
}

func encodeManifestFile(f ManifestFile) ([]byte, error) {
	if f.Path == "" || f.Language == "" || f.Size < 0 || (f.Kind != 'f' && f.Kind != 'l') {
		return nil, invalid("manifest file identity")
	}
	b := []byte{1, f.Kind} // format tag; presence means admitted
	b = appendMetaString(b, f.Language)
	b = binary.AppendVarint(b, f.Size)
	b = append(b, f.Content[:]...)
	return append(b, f.Extractor[:]...), nil
}

func decodeManifestFile(path string, b []byte) (ManifestFile, error) {
	if len(b) < 2 || b[0] != 1 {
		return ManifestFile{}, invalid("manifest file format")
	}
	d := recordCursor{buf: b[2:]}
	f := ManifestFile{Path: path, Kind: b[1], Language: d.str(), Size: int64(d.integer())}
	if d.err != nil || len(d.buf) != 64 || f.Language == "" || f.Size < 0 || (f.Kind != 'f' && f.Kind != 'l') {
		return ManifestFile{}, invalid("manifest file identity")
	}
	f.Content, f.Extractor = HeadDigest(d.buf[:32]), HeadDigest(d.buf[32:])
	return f, nil
}

// PrepareManifest writes only immutable worker files in the owned stage
// directory. The daemon supplies the stage's witness at ingest.
func PrepareManifest(ctx context.Context, dir string, files []ManifestFile, w Witness, incarnation string) (StageTables, error) {
	if w.Stage != StageManifest || len(w.Families) != 0 || incarnation == "" {
		return StageTables{}, invalid("unexpected manifest stage")
	}
	if _, err := encodeWitness(w); err != nil {
		return StageTables{}, err
	}
	options := headOptions()
	runs := sortedRuns{dir: dir, name: "manifest", options: options, format: options.FormatMajorVersion.MaxTableFormat(), limit: goldBufferBytes}
	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return StageTables{}, err
		}
		value, err := encodeManifestFile(f)
		if err != nil {
			return StageTables{}, err
		}
		if err := runs.add(keyStrings([]byte{familyManifest}, f.Path), value); err != nil {
			return StageTables{}, err
		}
	}
	h := blake3.New()
	var count uint64
	paths, err := writeGoldTables(ctx, nil, &runs, goldFileCap-1, func(r row) error {
		writeWitnessRow(h, r.key, r.value)
		count++
		return nil
	}, func(ctx context.Context) error { return ctx.Err() })
	if err != nil {
		return StageTables{}, err
	}
	w.Families = []FamilyWitness{{Family: familyManifest, Count: count, Digest: HeadDigest(h.Sum(nil))}}
	for i, path := range paths {
		paths[i] = filepath.Base(path)
	}
	return StageTables{Dir: dir, Files: paths, Incarnation: incarnation, Witness: w}, nil
}

// ManifestFile reads one admitted file identity from the request snapshot.
func (s *Snapshot) ManifestFile(path string) (ManifestFile, error) {
	if err := s.RequireStage(StageManifest); err != nil {
		return ManifestFile{}, err
	}
	return point(s, keyStrings([]byte{familyManifest}, path), func(raw []byte) (ManifestFile, error) {
		return decodeManifestFile(path, raw)
	})
}

func (s *Snapshot) ManifestFiles(ctx context.Context, yield func(ManifestFile) error) error {
	if err := s.RequireStage(StageManifest); err != nil {
		return err
	}
	return scan(ctx, s, []byte{familyManifest}, func(key, raw []byte) (bool, error) {
		path, rest, err := readKeyString(key[1:])
		if err != nil || len(rest) != 0 {
			return false, errors.Join(invalid("manifest file key"), err)
		}
		f, err := decodeManifestFile(path, raw)
		if err != nil {
			return false, err
		}
		return true, yield(f)
	})
}
