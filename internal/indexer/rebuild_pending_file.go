package indexer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
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
	facts, reused, err := idx.loadRebuildFileWith(ctx, src, store, input, recipe, bounds, store_pebble.DecodeParseFacts,
		func(ctx context.Context, rel string, ext parser.Extractor, _, prepared []byte) (store_pebble.ParseFacts, []byte, error) {
			entry, err := idx.submitPendingRebuildFile(ctx, rel, ext, prepared, policy, parserExe, store.limit)
			if err != nil {
				return store_pebble.ParseFacts{}, nil, err
			}
			if len(entry) > store.limit {
				return store_pebble.ParseFacts{}, nil, fmt.Errorf("%w: worker returned %d bytes, limit %d", errParseEntryTooLarge, len(entry), store.limit)
			}
			facts, err := store_pebble.DecodeParseFacts(entry)
			if err != nil {
				return store_pebble.ParseFacts{}, nil, fmt.Errorf("decode parser worker entry: %w", err)
			}
			if facts.File.FilePath != rel {
				return store_pebble.ParseFacts{}, nil, errors.New("parser worker entry has a different file identity")
			}
			if facts.EffectiveLanguage != ext.Language() {
				return store_pebble.ParseFacts{}, nil, fmt.Errorf("parser worker effective language differs: got %q, want %q", facts.EffectiveLanguage, ext.Language())
			}
			return facts, entry, nil
		})
	if err != nil {
		return store_pebble.ParseFacts{}, false, fmt.Errorf("%w: %s: %w", ErrPendingRebuildFile, input.Path, err)
	}
	return facts, reused, nil
}

func (idx *Indexer) submitPendingRebuildFile(ctx context.Context, rel string, ext parser.Extractor, prepared []byte, policy *rebuildParsePolicy, exe string, entryLimit int) ([]byte, error) {
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
	resp := pool.SubmitFullFacts(rel, ext.Language(), prepared, idx.extractionOptionsValue().TemporalEnvHelpers(), crashpool.RebuildFileOptions{
		IndexConfigJSON: cfg, Codeowners: policy.Codeowners, Extractor: fmt.Sprintf("%T", ext), EntryLimit: entryLimit,
	})
	if resp.Bad() {
		return nil, fmt.Errorf("parser worker: %s", resp.Err)
	}
	if len(resp.RebuildEntry) == 0 {
		return nil, errors.New("parser worker returned no rebuild entry")
	}
	return resp.RebuildEntry, nil
}

// RunRebuildParseWorker is the __parse-worker entry point. It adds strict
// rebuild requests without changing ordinary crashpool extraction.
func RunRebuildParseWorker(r io.Reader, w io.Writer) error {
	return crashpool.RunWorkerWithRebuild(r, w, servePendingRebuildFile)
}

func servePendingRebuildFile(reg *parser.Registry, path string, ext parser.Extractor, prepared []byte, options parser.ExtractionOptions, frozen crashpool.RebuildFileOptions) ([]byte, error) {
	var cfg config.IndexConfig
	if err := json.Unmarshal(frozen.IndexConfigJSON, &cfg); err != nil {
		return nil, fmt.Errorf("decode frozen parse config: %w", err)
	}
	idx := New(graph.New(), reg, cfg, zap.NewNop())
	defer idx.Close()
	idx.installRebuildParsePolicy(rebuildParsePolicy{
		Codeowners: frozen.Codeowners, TemporalEnvHelpers: options.TemporalEnvHelpers(),
	})
	facts, err := idx.extractPreparedRebuildFile(context.Background(), path, ext, prepared)
	if err != nil {
		return nil, err
	}
	return store_pebble.EncodeParseFacts(facts)
}
