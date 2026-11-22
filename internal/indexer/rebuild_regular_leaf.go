package indexer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/zeebo/blake3"
	"github.com/zzet/gortex/internal/indexer/merkle"
	"github.com/zzet/gortex/internal/indexer/source"
)

var errRebuildLeafAdmission = errors.New("unproved rebuild file admission")

// freezeRebuildRegularLeaf runs after Git and path policy have admitted the
// listed name. It never follows a link or opens a body to classify a skip.
// Stub facts and the returned parse input remain tentative until sidecar seal.
func (idx *Indexer) freezeRebuildRegularLeaf(ctx context.Context, root *os.Root, key string, listed fs.FileMode,
	member rebuildGitMember, content *contentAdmissionGate, fileBytes int64, raw *int64, emit func(rebuildSidecarRow) error,
) (*rebuildInput, error) {
	if !listed.IsRegular() || (member != rebuildGitTracked && member != rebuildGitUntracked) {
		return nil, nil
	}
	if root == nil || !validRebuildNamespaceKey(key) || fileBytes <= 0 || raw == nil || *raw < 0 || *raw > rebuildCppRawLimit || emit == nil {
		return nil, errRebuildLeafAdmission
	}
	obs, err := source.ObserveFrozenLiteralRootPath(ctx, root, key)
	if err != nil {
		return nil, rebuildLeafReadError(key, err)
	}
	size := obs.Info().Size()
	if obs.Path() != key || !obs.Info().Mode().IsRegular() || size < 0 {
		return nil, fmt.Errorf("%w: listed regular file changed: %q", ErrBuildInputChanged, key)
	}
	lang, named := idx.registry.DetectLanguage(key)
	var prefix []byte
	if !named {
		need := min(size, sniffPrefixBytes)
		if need > *raw {
			return nil, fmt.Errorf("%w: classification prefix for %q", errRebuildCppCaptureLimit, key)
		}
		prefix, err = obs.ReadPrefix(ctx, need)
		if err != nil {
			return nil, rebuildLeafReadError(key, err)
		}
		*raw -= int64(len(prefix))
		lang, _ = idx.registry.DetectLanguageContent(key, prefix)
		if lang == "" {
			lang = idx.transforms.languageFor(key)
		}
	}
	maxSize := idx.config.MaxFileSize
	if lang == "" || maxSize > 0 && size > maxSize {
		return nil, emit(rebuildClassificationSidecarRow(key, lang, size, maxSize, prefix, !named))
	}
	// Native applies the asset tracking gate before the content gate.
	reason := ""
	if idx.config.SkipUntrackedAssets && member == rebuildGitUntracked && content != nil && content.classes[lang] != "" {
		reason = skipReasonUntrackedAsset
	} else {
		reason, _ = content.skip(lang, size)
	}
	if reason != "" {
		row, err := rebuildContentSkipSidecarRow(skippedFile{relPath: key, lang: lang, size: size, reason: reason}, maxSize, prefix, !named)
		if err != nil {
			return nil, err
		}
		return nil, emit(row)
	}
	if size > fileBytes {
		return nil, fmt.Errorf("%w: source bytes for %q exceed parser envelope", ErrBuildTransformBounds, key)
	}
	data := prefix
	if named || int64(len(prefix)) != size {
		data, err = readRebuildClassifiedBody(ctx, obs, fileBytes, prefix)
		if err != nil {
			return nil, err
		}
	}
	if !named {
		if err := emit(rebuildClassificationSidecarRow(key, lang, size, maxSize, prefix, true)); err != nil {
			return nil, err
		}
	}
	return &rebuildInput{Path: key, Language: lang, Kind: merkle.RegularFile, Size: size, Content: blake3.Sum256(data)}, nil
}

func readRebuildClassifiedBody(ctx context.Context, obs source.FrozenRootPath, limit int64, prefix []byte) ([]byte, error) {
	data, err := obs.Read(ctx, limit)
	if err != nil {
		return nil, rebuildLeafReadError(obs.Path(), err)
	}
	if !bytes.HasPrefix(data, prefix) {
		return nil, fmt.Errorf("%w: classification prefix changed: %q", ErrBuildInputChanged, obs.Path())
	}
	return data, nil
}

func rebuildLeafReadError(key string, err error) error {
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, source.ErrFrozenRootChanged) || errors.Is(err, source.ErrFrozenRootPath) {
		return fmt.Errorf("%w: %q: %w", ErrBuildInputChanged, key, err)
	}
	return fmt.Errorf("%w: %q: %w", errRebuildLeafAdmission, key, err)
}
