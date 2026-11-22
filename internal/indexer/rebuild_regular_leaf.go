package indexer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"

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
) (*rebuildInput, source.FrozenRootPath, error) {
	if !listed.IsRegular() || (member != rebuildGitTracked && member != rebuildGitUntracked) {
		return nil, source.FrozenRootPath{}, nil
	}
	if root == nil || !validRebuildNamespaceKey(key) || fileBytes <= 0 || raw == nil || *raw < 0 || *raw > rebuildCppRawLimit || emit == nil {
		return nil, source.FrozenRootPath{}, errRebuildLeafAdmission
	}
	obs, err := source.ObserveFrozenLiteralRootPath(ctx, root, key)
	if err != nil {
		return nil, obs, rebuildLeafReadError(key, err)
	}
	size := obs.Info().Size()
	if obs.Path() != key || !obs.Info().Mode().IsRegular() || size < 0 {
		return nil, obs, fmt.Errorf("%w: listed regular file changed: %q", ErrBuildInputChanged, key)
	}
	lang, named := idx.registry.DetectLanguage(key)
	var prefix []byte
	if !named {
		need := min(size, sniffPrefixBytes)
		if need > *raw {
			return nil, obs, fmt.Errorf("%w: classification prefix for %q", errRebuildCppCaptureLimit, key)
		}
		prefix, err = obs.ReadPrefix(ctx, need)
		if err != nil {
			return nil, obs, rebuildLeafReadError(key, err)
		}
		*raw -= int64(len(prefix))
		lang, _ = idx.registry.DetectLanguageContent(key, prefix)
		if lang == "" {
			lang = idx.transforms.languageFor(key)
		}
	}
	maxSize := idx.config.MaxFileSize
	if lang == "" || maxSize > 0 && size > maxSize {
		row := rebuildClassificationSidecarRow(key, lang, size, maxSize, prefix, !named)
		if lang != "" {
			obs = source.FrozenRootPath{} // A parse size skip cannot authorize a resolver body read.
		}
		return nil, obs, emit(row)
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
			return nil, obs, err
		}
		return nil, source.FrozenRootPath{}, emit(row)
	}
	if size > fileBytes {
		return nil, obs, fmt.Errorf("%w: source bytes for %q exceed parser envelope", ErrBuildTransformBounds, key)
	}
	data := prefix
	if named || int64(len(prefix)) != size {
		data, err = readRebuildClassifiedBody(ctx, obs, fileBytes, prefix)
		if err != nil {
			return nil, obs, err
		}
	}
	if !named {
		if err := emit(rebuildClassificationSidecarRow(key, lang, size, maxSize, prefix, true)); err != nil {
			return nil, obs, err
		}
	}
	return &rebuildInput{Path: key, Language: lang, Kind: merkle.RegularFile, Size: size, Content: blake3.Sum256(data)}, obs, nil
}

// freezeRebuildCheckoutLeaf classifies the owner's Git/path-authorized leaf.
// A skip gives regular type evidence only. Links are never parse leaves;
// only root manifest links carry a checked no-follow identity.
func (idx *Indexer) freezeRebuildCheckoutLeaf(ctx context.Context, root *os.Root, key string, mode fs.FileMode,
	member rebuildGitMember, content *contentAdmissionGate, fileBytes int64, raw *int64, emit func(rebuildSidecarRow) error,
) (rebuildCheckoutLeaf, error) {
	if member != rebuildGitTracked && member != rebuildGitUntracked {
		return rebuildCheckoutLeaf{}, nil
	}
	if mode&fs.ModeSymlink != 0 && (key == "go.mod" || key == "package.json") {
		return freezeRebuildRootManifestLink(ctx, root, key, raw)
	}
	if !mode.IsRegular() {
		return rebuildCheckoutLeaf{}, nil
	}
	input, obs, err := idx.freezeRebuildRegularLeaf(ctx, root, key, mode, member, content, fileBytes, raw, emit)
	if err != nil {
		return rebuildCheckoutLeaf{}, err
	}
	if input == nil {
		leaf := rebuildCheckoutLeaf{typeOnly: true}
		if base := path.Base(key); obs.Info() != nil && (base == "go.mod" || base == "go.work") {
			leaf.goConfig = &obs
		}
		return leaf, nil
	}
	return rebuildCheckoutLeaf{input: input, parse: true}, nil
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
