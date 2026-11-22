package indexer

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"strings"

	"github.com/zeebo/blake3"
	"github.com/zzet/gortex/internal/indexer/merkle"
	"github.com/zzet/gortex/internal/indexer/source"
)

// ErrBuildInputChanged means a frozen input must be re-enumerated, not parsed
// for the version that named its old content.
var ErrBuildInputChanged = errors.New("build input changed after enumeration")

type rebuildDigest [32]byte

// rebuildInput describes the bytes and role of one admitted path. Its content
// digest excludes the path and kind; the input root and parse key include both.
type rebuildInput struct {
	Path     string
	Language string
	Kind     merkle.FileKind
	Size     int64
	Content  rebuildDigest
}

func readRebuildInput(ctx context.Context, src source.ContentSource, rel, language string, limit int64) (rebuildInput, []byte, error) {
	if err := ctx.Err(); err != nil {
		return rebuildInput{}, nil, err
	}
	// Keep the enumerator's exact spelling, including non-UTF-8 Git paths.
	if rel == "." || path.Clean(rel) != rel || !filepath.IsLocal(filepath.FromSlash(rel)) ||
		filepath.ToSlash(rel) != rel || strings.IndexByte(rel, 0) >= 0 || language == "" {
		return rebuildInput{}, nil, fmt.Errorf("invalid build input path or language: %q (%q)", rel, language)
	}
	meta, err := src.Stat(rel)
	if err != nil {
		return rebuildInput{}, nil, err
	}
	kind := merkle.RegularFile
	var data []byte
	if meta.Symlink {
		kind = merkle.Symlink
		data = []byte(meta.SymlinkTarget)
		if int64(len(data)) > limit {
			return rebuildInput{}, nil, fmt.Errorf("%s: %w", rel, source.ErrRegularFileTooLarge)
		}
	} else {
		data, _, err = source.ReadRegularFile(ctx, src, rel, limit)
		if err != nil {
			return rebuildInput{}, nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return rebuildInput{}, nil, err
	}
	return rebuildInput{Path: rel, Language: language, Kind: kind, Size: int64(len(data)), Content: blake3.Sum256(data)}, data, nil
}

// readFrozenRebuildInput returns exactly the bytes its caller may extract. Use
// the frozen size as the read bound so a later oversized edit cannot grow it.
func readFrozenRebuildInput(ctx context.Context, src source.ContentSource, frozen rebuildInput) ([]byte, error) {
	got, data, err := readRebuildInput(ctx, src, frozen.Path, frozen.Language, frozen.Size)
	if errors.Is(err, source.ErrNotInSource) || errors.Is(err, source.ErrNotRegularFile) || errors.Is(err, source.ErrRegularFileTooLarge) {
		return nil, fmt.Errorf("%w: %s: %w", ErrBuildInputChanged, frozen.Path, err)
	}
	if err != nil {
		return nil, err
	}
	if got != frozen {
		return nil, fmt.Errorf("%w: %s", ErrBuildInputChanged, frozen.Path)
	}
	return data, nil
}

func rebuildInputRoot(inputs []rebuildInput) (string, error) {
	leaves := make(map[string]merkle.FileNode, len(inputs))
	for _, in := range inputs {
		if _, duplicate := leaves[in.Path]; duplicate {
			return "", fmt.Errorf("duplicate build input path: %q", in.Path)
		}
		leaves[in.Path] = merkle.FileNode{Hash: hex.EncodeToString(in.Content[:]), Salt: in.Language, Kind: in.Kind}
	}
	return merkle.FromKnownFiles(leaves).Root, nil
}

func (in rebuildInput) parseKey(extractorRecipe rebuildDigest) rebuildDigest {
	b := append([]byte{'p', 1, byte(in.Kind)}, extractorRecipe[:]...)
	b = append(b, in.Content[:]...)
	b = binary.AppendUvarint(b, uint64(len(in.Path)))
	b = append(b, in.Path...)
	b = append(b, in.Language...)
	return blake3.Sum256(b)
}
