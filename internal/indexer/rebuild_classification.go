package indexer

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/zeebo/blake3"
	"github.com/zzet/gortex/internal/indexer/merkle"
)

// The owner attests the language selected from checked capture. A zero
// PrefixDigest means a known-name size skip without a read. BLAKE3(empty)
// instead attests an actual empty prefix read, even for a no-language result.
type rebuildClassificationFact struct {
	Size         int64         `json:"size"`
	MaxFileSize  int64         `json:"max_file_size"`
	Language     string        `json:"language,omitempty"`
	PrefixBytes  int64         `json:"prefix_bytes,omitempty"`
	PrefixDigest rebuildDigest `json:"prefix_digest,omitzero"`
}

var rebuildEmptyPrefixDigest = rebuildDigest(blake3.Sum256(nil))

// The caller owns Git/path approval, the checked read and its raw-byte
// charge. This helper saves only the outcome and digest, never the prefix.
func rebuildClassificationSidecarRow(rel, language string, size, maxFileSize int64, prefix []byte, sniffed bool) rebuildSidecarRow {
	fact := rebuildClassificationFact{
		Size: size, MaxFileSize: max(0, maxFileSize), Language: language, PrefixBytes: int64(len(prefix)),
	}
	if sniffed {
		fact.PrefixDigest = blake3.Sum256(prefix)
	}
	return rebuildSidecarRow{Kind: rebuildClassificationRow, Path: []byte(rel), Classification: &fact}
}

func (f rebuildClassificationFact) sizeSkipped() bool {
	return f.Language != "" && f.MaxFileSize > 0 && f.Size > f.MaxFileSize
}

func (r rebuildSidecarRow) checkClassification() error {
	f := r.Classification
	if f == nil || r.Present || r.Digest != (rebuildDigest{}) || len(r.Dirs) != 0 || r.Config != nil || r.NPM != nil ||
		!validRebuildNamespaceKey(string(r.Path)) {
		return fmt.Errorf("%w: invalid classification row", errRebuildInputSidecar)
	}
	if f.Size < 0 || f.MaxFileSize < 0 || !utf8.ValidString(f.Language) || f.Language != strings.TrimSpace(f.Language) ||
		strings.ContainsFunc(f.Language, unicode.IsControl) {
		return fmt.Errorf("%w: invalid classification size, limit or language for %q", errRebuildInputSidecar, r.Path)
	}
	if f.PrefixDigest == (rebuildDigest{}) {
		if f.PrefixBytes != 0 || !f.sizeSkipped() {
			return fmt.Errorf("%w: missing classification prefix proof for %q", errRebuildInputSidecar, r.Path)
		}
	} else if f.PrefixBytes != min(f.Size, sniffPrefixBytes) || (f.PrefixBytes == 0) != (f.PrefixDigest == rebuildEmptyPrefixDigest) {
		return fmt.Errorf("%w: invalid classification prefix length or digest for %q", errRebuildInputSidecar, r.Path)
	}
	return nil
}

func (idx *Indexer) checkRebuildClassification(key string, f rebuildClassificationFact) error {
	if f.MaxFileSize != max(0, idx.config.MaxFileSize) {
		return fmt.Errorf("%w: classification size limit differs from frozen config for %q", errRebuildInputSidecar, key)
	}
	named, known := idx.registry.DetectLanguage(key)
	// This is a filename-only shape check, not a repeat of the prefix probe.
	if f.PrefixDigest == (rebuildDigest{}) {
		if !known || named != f.Language {
			return fmt.Errorf("%w: unexplained no-prefix classification for %q", errRebuildInputSidecar, key)
		}
		return nil
	}
	if known {
		return fmt.Errorf("%w: classification prefix on a known name %q", errRebuildInputSidecar, key)
	}
	if f.Language != idx.transforms.languageFor(key) {
		if _, ok := idx.registry.GetByLanguage(f.Language); !ok || f.PrefixBytes == 0 {
			return fmt.Errorf("%w: invalid language for classification prefix on %q", errRebuildInputSidecar, key)
		}
	}
	return nil
}

func (f rebuildClassificationFact) matchesParseInput(input rebuildInput) bool {
	// A short prefix is the whole input. Compare its digest when possible;
	// a longer input's body digest cannot prove the prefix without a read.
	return !f.sizeSkipped() && f.Language != "" && input.Kind == merkle.RegularFile &&
		f.Language == input.Language && f.Size == input.Size &&
		(f.PrefixBytes != f.Size || f.PrefixDigest == input.Content)
}
