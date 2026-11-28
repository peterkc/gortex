package indexer

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"

	"github.com/zeebo/blake3"
	"github.com/zzet/gortex/internal/codeowners"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph/store_pebble"
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
		idx.frozenRebuildContractPolicy.Store(idx.rebuildContractPolicy())
		installedOptions = true
	})
	if installedOwners && installedOptions && !policy.OwnershipUnavailable {
		policy.Codeowners = bytes.Clone(policy.Codeowners)
		idx.frozenRebuildParsePolicy.Store(&policy)
	}
}

// Bump this for a change to detached fact capture that does not change the
// binary layout or the shared native contract extraction policy.
const rebuildContractCaptureVersion = 1

// 3: Go package-scope builtin declarations on the file receipt.
// 4: JS/TS lexical-scope bindings, unproved user receivers and global-alias writes (host delegates included).
// 5: JS/TS lexical/function scopes, scoped types and writes, and additional mutation forms (host delegates included).
// 6: range 21 receiver scope/write fixes.
const rebuildBuiltinReceiverCaptureVersion = 6

// The same language selection controls parser opt-in and receipt invalidation.
func capturesRebuildBuiltinReceiverFacts(lang string) bool {
	switch lang {
	case "go", "javascript", "typescript", "vue", "svelte", "astro", "html":
		return true
	}
	return false
}

// The rebuild worker binds the effective event-bus override once. Both the
// file-node stamp and capture use the same snapshot as every key lookup.
type rebuildContractPolicy struct {
	eventBus []contracts.EventBusBoundary
	suffix   []byte
}

func (idx *Indexer) rebuildContractPolicy() *rebuildContractPolicy {
	boundaries := idx.eventBusBoundaries()
	// A rebuild job's config comes from the frozen JSON request. Bind all
	// settings in the file-node stamp, not just the capture detectors' knobs.
	settings, _ := json.Marshal(struct {
		Settings config.IndexConfig
		EventBus []contracts.EventBusBoundary
	}{contractExtractionSettings(idx.config), boundaries})
	return &rebuildContractPolicy{eventBus: boundaries, suffix: rebuildContractRecipeSuffix(settings)}
}

func rebuildContractRecipeSuffix(settings []byte) []byte {
	var b []byte
	for _, version := range []string{contractExtractionPolicyVersion, contractDependencyInputVersion, contracts.RecordFingerprintVersion} {
		b = binary.AppendUvarint(b, uint64(len(version)))
		b = append(b, version...)
	}
	digest := blake3.Sum256(settings)
	return append(b, digest[:]...)
}

// rebuildExtractorRecipe binds ordinary extraction and contract fact policy.
// Include admissible refinement versions before looking up a receipt, when
// the current bytes may no longer be available for a content probe.
func (idx *Indexer) rebuildExtractorRecipe(base rebuildDigest, input rebuildInput) rebuildDigest {
	ordinary := idx.rebuildBaseExtractorRecipe(base, input)
	domain := "gortex/rebuild/contract-parse\x00"
	captureReceiver := capturesRebuildBuiltinReceiverFacts(input.Language)
	if captureReceiver {
		domain = "gortex/rebuild/contract-parse-receiver\x00"
	}
	b := append([]byte(domain), ordinary[:]...)
	b = append(b, store_pebble.ContractParseFactsFormat)
	if captureReceiver {
		b = binary.AppendUvarint(b, rebuildBuiltinReceiverCaptureVersion)
	}
	b = binary.AppendUvarint(b, rebuildContractCaptureVersion)
	b = binary.AppendUvarint(b, postExtractionPolicyVersion)
	b = binary.AppendUvarint(b, uint64(extractorVersionForLang(input.Language)))
	if parser.ExtensionNeedsContentProbe(input.Path) {
		for _, lang := range possibleRebuildLanguageRefinements(input.Path) {
			if lang != input.Language {
				b = binary.AppendUvarint(b, uint64(len(lang)))
				b = append(b, lang...)
				b = binary.AppendUvarint(b, uint64(extractorVersionForLang(lang)))
			}
		}
	}
	b = binary.AppendUvarint(b, 0) // end refinement versions
	policy := idx.frozenRebuildContractPolicy.Load()
	if policy == nil {
		policy = idx.rebuildContractPolicy()
	}
	return blake3.Sum256(append(b, policy.suffix...))
}

// The ordinary recipe is also the prefix of the contract recipe. Keep the
// last-match-wins CODEOWNERS order and the existing Go Temporal policy.
func (idx *Indexer) rebuildBaseExtractorRecipe(base rebuildDigest, input rebuildInput) rebuildDigest {
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
