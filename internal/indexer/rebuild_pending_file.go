package indexer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"

	"github.com/zeebo/blake3"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/indexer/merkle"
	"github.com/zzet/gortex/internal/indexer/source"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/crashpool"
	"go.uber.org/zap"
)

// ErrPendingRebuildFile leaves the edited file pending. The wrapped cause
// distinguishes a superseded input, worker failure, and unavailable policy.
var ErrPendingRebuildFile = errors.New("pending file declarations unavailable")

// loadPendingRebuildFile uses a fresh, unprefixed indexer initialized with the
// build's frozen config and parse policy. The caller pins the current Gortex
// executable and owns the file envelope and transform scratch. No parser
// starts for a cached file, and no worker failure enables native extraction.
func (idx *Indexer) loadPendingRebuildFile(ctx context.Context, src source.ContentSource, store rebuildParseStore, input rebuildInput, recipe rebuildDigest, bounds rebuildTransformBounds, parserExe string) (store_pebble.ParseFacts, bool, error) {
	policy := idx.frozenRebuildParsePolicy.Load()
	if policy == nil || idx.repoPrefix != "" || recipe == (rebuildDigest{}) {
		return store_pebble.ParseFacts{}, false, fmt.Errorf("%w: %s: frozen parse policy unavailable", ErrPendingRebuildFile, input.Path)
	}
	if idx.transforms == nil || bounds.FileBytes <= 0 {
		return store_pebble.ParseFacts{}, false, fmt.Errorf("%w: %s: %w: transform policy unavailable", ErrPendingRebuildFile, input.Path, ErrBuildTransformBounds)
	}
	facts, _, reused, err := idx.loadRebuildFileWith(ctx, src, store, input, recipe, bounds,
		func(ctx context.Context, key rebuildDigest, _ string, ext parser.Extractor, raw, prepared []byte) ([]byte, error) {
			return idx.submitPendingRebuildFile(ctx, input, recipe, key, ext, raw, prepared, policy, parserExe, store.limit)
		})
	if err != nil {
		return store_pebble.ParseFacts{}, false, fmt.Errorf("%w: %s: %w", ErrPendingRebuildFile, input.Path, err)
	}
	return facts, reused, nil
}

func (idx *Indexer) submitPendingRebuildFile(ctx context.Context, input rebuildInput, recipe, key rebuildDigest, ext parser.Extractor, raw, prepared []byte, policy *rebuildParsePolicy, exe string, entryLimit int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(exe) {
		return nil, errors.New("parser worker executable must be absolute")
	}
	// Use the same frozen config encoding as the build request. In particular,
	// JSON preserves an explicitly disabled coverage flag separately from nil.
	cfg, err := json.Marshal(idx.config)
	if err != nil {
		return nil, fmt.Errorf("encode frozen parse config: %w", err)
	}
	pool, err := idx.newParsePoolFromExecutable(exe, 1)
	if err != nil {
		return nil, fmt.Errorf("start parser worker: %w", err)
	}
	defer pool.Close()
	resp := pool.SubmitFullFacts(input.Path, ext.Language(), prepared, idx.extractionOptionsValue().TemporalEnvHelpers(), crashpool.RebuildFileOptions{
		IndexConfigJSON:  cfg,
		Codeowners:       policy.Codeowners,
		Extractor:        fmt.Sprintf("%T", ext),
		EntryLimit:       entryLimit,
		AdmittedLanguage: input.Language,
		Kind:             byte(input.Kind),
		Size:             input.Size,
		Content:          input.Content,
		BaseRecipe:       recipe,
		Raw:              raw,
	})
	if resp.Bad() {
		return nil, fmt.Errorf("parser worker: %s", resp.Err)
	}
	return checkPendingRebuildReply(resp.RebuildHeader, resp.RebuildEntry, key, input.Path, ext.Language(), prepared)
}

func checkPendingRebuildReply(header, entry []byte, key rebuildDigest, path, language string, prepared []byte) ([]byte, error) {
	if len(entry) == 0 {
		return nil, fmt.Errorf("%w: parser worker returned no rebuild entry", errParseEntryCorrupt)
	}
	if len(header) != parseEntryHeader {
		return nil, fmt.Errorf("%w: parser worker returned a missing or incomplete entry header", errParseEntryCorrupt)
	}
	if err := checkRebuildParseEntryHeader(key, header, entry); err != nil {
		return nil, err
	}
	facts, _, _, err := store_pebble.DecodeContractParseFacts(entry)
	if err != nil {
		return nil, fmt.Errorf("%w: decode parser worker entry: %w", errParseEntryCorrupt, err)
	}
	if facts.File.FilePath != path {
		return nil, fmt.Errorf("%w: parser worker entry has a different file identity", errParseEntryCorrupt)
	}
	if facts.EffectiveLanguage != language {
		return nil, fmt.Errorf("%w: parser worker effective language differs: got %q, want %q", errParseEntryCorrupt, facts.EffectiveLanguage, language)
	}
	if facts.File.Size != len(prepared) || facts.File.ContentHash != contentHashForSource(prepared) {
		return nil, fmt.Errorf("%w: parser worker entry does not match the prepared content", errParseEntryCorrupt)
	}
	if err := checkRebuildReceiptBuiltinNodes(path, facts.Nodes); err != nil {
		return nil, err
	}
	return entry, nil
}

// RunRebuildParseWorker is the __parse-worker entry point. It adds strict
// rebuild requests without changing ordinary crashpool extraction.
func RunRebuildParseWorker(r io.Reader, w io.Writer) error {
	return crashpool.RunWorkerWithRebuild(r, w, servePendingRebuildFile)
}

func servePendingRebuildFile(reg *parser.Registry, path string, ext parser.Extractor, prepared []byte, options parser.ExtractionOptions, frozen crashpool.RebuildFileOptions) ([]byte, []byte, error) {
	input := rebuildInput{
		Path:     path,
		Language: frozen.AdmittedLanguage,
		Kind:     merkle.FileKind(frozen.Kind),
		Size:     frozen.Size,
		Content:  frozen.Content,
	}
	if int64(len(frozen.Raw)) != input.Size || blake3.Sum256(frozen.Raw) != input.Content {
		return nil, nil, fmt.Errorf("%w: %s", ErrBuildInputChanged, path)
	}
	// Gob omits an empty byte slice. Restore the present empty source so
	// native raw-source readers distinguish it from unavailable source.
	if frozen.Raw == nil {
		frozen.Raw = []byte{}
	}
	var cfg config.IndexConfig
	if err := json.Unmarshal(frozen.IndexConfigJSON, &cfg); err != nil {
		return nil, nil, fmt.Errorf("decode frozen parse config: %w", err)
	}
	// The worker extracts a receipt from one file; it neither reads nor writes
	// a graph. The parent owns transforms and publication.
	idx := &Indexer{registry: reg, config: cfg, logger: zap.NewNop()}
	defer idx.Close()
	idx.installRebuildParsePolicy(rebuildParsePolicy{
		Codeowners:         frozen.Codeowners,
		TemporalEnvHelpers: options.TemporalEnvHelpers(),
	})
	key := input.parseKey(idx.rebuildExtractorRecipe(frozen.BaseRecipe, input))
	base, facts, err := idx.extractPreparedRebuildFileWithContractFacts(context.Background(), path, ext, frozen.Raw, prepared)
	if err != nil {
		return nil, nil, err
	}
	entry, err := store_pebble.EncodeContractParseFacts(base, facts)
	if err != nil {
		return nil, nil, err
	}
	stamp := makeRebuildParseEntryHeader(key, entry)
	return stamp[:], entry, nil
}
