package indexer

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"

	"github.com/zeebo/blake3"
	"github.com/zzet/gortex/internal/codeowners"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/pathkey"
)

// rebuildParsePolicy travels with the build request. The child compiles the
// same policy once; it never reloads these mutable files after the manifest.
// It is not persisted in the head or in an entry.
type rebuildParsePolicy struct {
	Codeowners           []byte
	TemporalEnvHelpers   []string
	OwnershipUnavailable bool
}

func freezeRebuildParsePolicy(root string, cfg config.IndexConfig) rebuildParsePolicy {
	policy := rebuildParsePolicy{
		TemporalEnvHelpers: parser.NewExtractionOptions(config.LoadLocalTemporalEnvHelpers(root)).TemporalEnvHelpers(),
	}
	if cfg.Coverage.IsEnabled("ownership") {
		_, _, _ = codeowners.LoadFromRepoWith(root, func(path string) ([]byte, error) {
			raw, err := os.ReadFile(path)
			if err == nil {
				policy.Codeowners = raw
			} else if !errors.Is(err, os.ErrNotExist) {
				policy.OwnershipUnavailable = true
			}
			return raw, err
		})
	}
	return policy
}

// installRebuildParsePolicy runs on a fresh file worker before SetRootPath,
// which would otherwise load extraction options from that checkout itself.
func (idx *Indexer) installRebuildParsePolicy(policy rebuildParsePolicy) {
	var installedOwners, installedOptions bool
	idx.codeownersOnce.Do(func() {
		idx.codeownersRules = codeowners.Parse(policy.Codeowners)
		installedOwners = true
	})
	idx.extractionOptionsOnce.Do(func() {
		opts := parser.NewExtractionOptions(policy.TemporalEnvHelpers)
		idx.extractionOptions.Store(&opts)
		installedOptions = true
	})
	if installedOwners && installedOptions && !policy.OwnershipUnavailable {
		policy.Codeowners = bytes.Clone(policy.Codeowners)
		idx.frozenRebuildParsePolicy.Store(&policy)
	}
}

// rebuildExtractorRecipe binds the settings that apply during extraction.
// CODEOWNERS is last-match-wins; the owner order is preserved. Only the Go
// extractor reads the Temporal list. Clone scoring thresholds apply later.
func (idx *Indexer) rebuildExtractorRecipe(base rebuildDigest, input rebuildInput) rebuildDigest {
	graphPath := pathkey.Normalize(input.Path)
	version := byte(4) // v4 binds effective clone extraction to the receipt.
	if graphPath != input.Path {
		version = 5 // Keep the NFC graph-key variant distinct.
	}
	cloneMode := byte(0)
	if idx.config.Coverage.IsEnabled("clones") {
		cloneMode = 1
	}
	b := append([]byte{'r', version, cloneMode}, base[:]...)
	appendList := func(values []string) {
		b = binary.AppendUvarint(b, uint64(len(values)))
		for _, value := range values {
			b = binary.AppendUvarint(b, uint64(len(value)))
			b = append(b, value...)
		}
	}
	var owners []string
	if idx.config.Coverage.IsEnabled("ownership") {
		owners = codeowners.MatchFile(graphPath, idx.loadCodeownersRules())
	}
	appendList(owners)
	if input.Language == "go" {
		appendList(idx.extractionOptionsValue().TemporalEnvHelpers())
	}
	return blake3.Sum256(b)
}
