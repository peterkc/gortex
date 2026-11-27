package indexer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zeebo/blake3"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/indexer/source"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
	"github.com/zzet/gortex/internal/parser/tsitter/golang"
	"github.com/zzet/gortex/internal/parser/tsitter/php"
	"github.com/zzet/gortex/internal/pathkey"
	"go.uber.org/zap"
)

// loadRebuildFile loads one immutable entry before touching the checkout. A
// matching entry remains usable even if an edit replaced the frozen bytes.
// Only a missing entry permits extraction; corruption refuses the build.
func (idx *Indexer) loadRebuildFile(ctx context.Context, src source.ContentSource, store rebuildParseStore, input rebuildInput, recipe rebuildDigest, bounds rebuildTransformBounds) (facts store_pebble.ParseFacts, reused bool, err error) {
	return idx.loadRebuildFileWith(ctx, src, store, input, recipe, bounds, store_pebble.DecodeParseFacts,
		func(ctx context.Context, rel string, ext parser.Extractor, _, prepared []byte) (store_pebble.ParseFacts, []byte, error) {
			facts, err := idx.extractPreparedRebuildFile(ctx, rel, ext, prepared)
			return facts, nil, err
		})
}

// The candidate key is separate from v2. C1b-2 owns the active recipe and
// pending-worker transport, including the rest of the contract policy binding.
func rebuildContractCandidateRecipe(recipe rebuildDigest) rebuildDigest {
	return blake3.Sum256(append([]byte("rebuild-contract-candidate/4\x00"), recipe[:]...))
}

// loadRebuildFileWithContractFacts is the inactive candidate receipt path.
func (idx *Indexer) loadRebuildFileWithContractFacts(ctx context.Context, src source.ContentSource, store rebuildParseStore, input rebuildInput, recipe rebuildDigest, bounds rebuildTransformBounds) (store_pebble.ParseFacts, *contracts.FileFacts, bool, error) {
	var facts *contracts.FileFacts
	decode := func(payload []byte) (base store_pebble.ParseFacts, err error) {
		base, facts, _, err = store_pebble.DecodeContractParseFacts(payload)
		return base, err
	}
	base, reused, err := idx.loadRebuildFileWith(ctx, src, store, input, rebuildContractCandidateRecipe(recipe), bounds, decode,
		func(ctx context.Context, rel string, ext parser.Extractor, raw, prepared []byte) (store_pebble.ParseFacts, []byte, error) {
			base, captured, err := idx.extractPreparedRebuildFileWithContractFacts(ctx, rel, ext, raw, prepared)
			if err != nil {
				return store_pebble.ParseFacts{}, nil, err
			}
			payload, err := store_pebble.EncodeContractParseFacts(base, captured)
			facts = captured
			return base, payload, err
		})
	if err != nil {
		return store_pebble.ParseFacts{}, nil, false, err
	}
	return base, facts, reused, nil
}

// All receipt paths share the cache and the work around extraction. Only a
// cache miss calls extract, after verifying the accepted bytes and transforming once.
func (idx *Indexer) loadRebuildFileWith(ctx context.Context, src source.ContentSource, store rebuildParseStore, input rebuildInput, recipe rebuildDigest, bounds rebuildTransformBounds,
	decode func([]byte) (store_pebble.ParseFacts, error),
	extract func(context.Context, string, parser.Extractor, []byte, []byte) (store_pebble.ParseFacts, []byte, error),
) (facts store_pebble.ParseFacts, reused bool, err error) {
	if err := ctx.Err(); err != nil {
		return facts, false, err
	}
	ext, _ := idx.registry.GetByLanguage(input.Language)
	if ext == nil {
		return facts, false, fmt.Errorf("no extractor for %s (%s)", input.Path, input.Language)
	}
	key := input.parseKey(idx.rebuildExtractorRecipe(recipe, input))
	payload, err := store.read(key)
	if err == nil {
		cached, err := decode(payload)
		if err != nil {
			return facts, false, fmt.Errorf("%w: %s: %w", errParseEntryCorrupt, input.Path, err)
		}
		if cached.File.FilePath != input.Path {
			return facts, false, fmt.Errorf("%w: %s: file identity differs", errParseEntryCorrupt, input.Path)
		}
		effective, _ := idx.registry.GetByLanguage(cached.EffectiveLanguage)
		if cached.EffectiveLanguage == "" || effective == nil ||
			(cached.EffectiveLanguage != input.Language && !possibleRebuildLanguageRefinement(input.Path, cached.EffectiveLanguage)) {
			return facts, false, fmt.Errorf("%w: %s: invalid effective parse language %q for admitted language %q",
				errParseEntryCorrupt, input.Path, cached.EffectiveLanguage, input.Language)
		}
		return cached, true, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return facts, false, err
	}
	if bounds.FileBytes > 0 && input.Size > int64(bounds.FileBytes) {
		return facts, false, fmt.Errorf("%w: %s has %d bytes, limit %d", ErrBuildTransformBounds, input.Path, input.Size, bounds.FileBytes)
	}
	raw, err := readFrozenRebuildInput(ctx, src, input)
	if err != nil {
		return facts, false, err
	}
	rel := input.Path
	// Match the native pre-transform probe, without consulting live source.
	// Use the portable path so a checkout directory cannot affect detection.
	if parser.ExtensionNeedsContentProbe(rel) {
		if lang, ok := idx.registry.DetectLanguageContent(rel, raw); ok && lang != input.Language {
			if !possibleRebuildLanguageRefinement(rel, lang) {
				return facts, false, fmt.Errorf("%s: unexplained parse language change from %q to %q", rel, input.Language, lang)
			}
			ext, _ = idx.registry.GetByLanguage(lang)
		}
	}
	prepared, spilled, err := idx.transforms.runRebuild(ctx, pathkey.Normalize(rel), raw, bounds)
	if err != nil {
		return facts, false, err
	}
	if bounds.FileBytes > 0 && len(prepared) > bounds.FileBytes {
		return facts, false, fmt.Errorf("%w: %s produced %d bytes, limit %d", ErrBuildTransformBounds, rel, len(prepared), bounds.FileBytes)
	}
	if spilled != 0 {
		idx.logger.Debug("build command transform spilled", zap.String("file", rel), zap.Int64("spill_bytes", spilled))
	}
	facts, payload, err = extract(ctx, rel, ext, raw, prepared)
	if err != nil {
		return store_pebble.ParseFacts{}, false, err
	}
	if payload == nil {
		payload, err = store_pebble.EncodeParseFacts(facts)
		if err != nil {
			return facts, false, fmt.Errorf("encode parse facts for %s: %w", input.Path, err)
		}
	}
	if err := ctx.Err(); err != nil {
		return facts, false, err
	}
	return facts, false, store.write(key, payload)
}

// possibleRebuildLanguageRefinement checks the range of the pinned native
// content probe. It does not pick a language; the digest-bound receipt records
// which result the probe selected. A registered but impossible ID is corrupt.
func possibleRebuildLanguageRefinement(path, lang string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".h":
		return lang == "cpp" || lang == "objc"
	case ".inc":
		return lang == "c"
	case ".m":
		return lang == "objc" || lang == "matlab" || lang == "mathematica"
	case ".json":
		return lang == "liquid_json"
	case ".xml":
		return lang == "mybatis" || lang == "spring"
	default:
		return false
	}
}

// extractPreparedRebuildFile runs only inside the build worker or the strict
// parse worker. The caller already selected the extractor from frozen input.
func (idx *Indexer) extractPreparedRebuildFile(ctx context.Context, rel string, ext parser.Extractor, prepared []byte) (store_pebble.ParseFacts, error) {
	return idx.extractPreparedRebuildFileInto(ctx, rel, ext, prepared, prepared, nil)
}

// extractPreparedRebuildFileWithContractFacts is the in-memory receipt seam.
// C1b will persist these facts alongside ordinary parse facts. This uses the
// same parse turn as the legacy writer. Only the two named transformed facts
// need a raw parse here; neither needs a later source read or parser call.
func (idx *Indexer) extractPreparedRebuildFileWithContractFacts(ctx context.Context, rel string, ext parser.Extractor, raw, prepared []byte) (store_pebble.ParseFacts, *contracts.FileFacts, error) {
	var facts *contracts.FileFacts
	out, err := idx.extractPreparedRebuildFileInto(ctx, rel, ext, raw, prepared, &facts)
	return out, facts, err
}

func (idx *Indexer) extractPreparedRebuildFileInto(ctx context.Context, rel string, ext parser.Extractor, raw, prepared []byte, contractFacts **contracts.FileFacts) (store_pebble.ParseFacts, error) {
	if contractFacts != nil {
		if retained, ok := ext.(interface{ WithRetainedTree() parser.Extractor }); ok {
			ext = retained.WithRetainedTree()
		}
	}
	graphPath := pathkey.Normalize(rel)
	lang := ext.Language()
	admission := newNativeParseExtractionAdmission(0, nil, idx.nativeParseAdmission.Load())
	result, skipped, err := idx.extractFileCtxWithRawLease(ctx, admission, nil, nil, nil,
		filepath.Join(idx.rootPath, filepath.FromSlash(rel)), graphPath, lang, ext, prepared)
	if result != nil {
		defer result.ReleaseTree()
	}
	if err != nil {
		return store_pebble.ParseFacts{}, fmt.Errorf("extract %s: %w", rel, err)
	}
	if result == nil {
		return store_pebble.ParseFacts{}, fmt.Errorf("extract %s: no parse facts", rel)
	}
	if !skipped {
		if !extractionDispositionFor(result).omitSecondarySourceScans() {
			idx.applyCoverageDomains(graphPath, lang, prepared, result)
		}
		stampExtractionGraphFingerprint(result)
		idx.stampContractDependencyInputs(graphPath, lang, prepared, result)
	}
	// ParseFacts v2 stores the literal receipt origin in File.FilePath and
	// derives every constant's FilePath from it on decode. Their node IDs,
	// like other graph facts, were minted with the NFC key by the extractor.
	meta, _ := idx.prepareFileMeta(rel, prepared, result)
	constants, _ := idx.prepareConstValues(result)
	if graphPath != rel {
		for i := range constants {
			if constants[i].FilePath == graphPath {
				constants[i].FilePath = rel
			}
		}
	}
	facts := store_pebble.ParseFacts{
		Nodes: result.Nodes, Edges: result.Edges, File: meta, Constants: constants, EffectiveLanguage: lang,
	}
	if contractFacts != nil {
		captured, err := idx.captureRebuildContractFacts(ctx, graphPath, lang, raw, prepared, result, skipped)
		if err != nil {
			return store_pebble.ParseFacts{}, err
		}
		*contractFacts = captured
	}
	for _, n := range facts.Nodes {
		if n == nil {
			continue
		}
		if shingles, ok := n.Meta[cloneShinglesMetaKey].([]uint64); ok {
			if facts.Shingles == nil {
				facts.Shingles = make(map[string][]uint64)
			}
			facts.Shingles[n.ID] = shingles
		}
		if idx.shouldIndexForSearch(n) {
			facts.Symbols = append(facts.Symbols, graph.SymbolFTSItem{NodeID: n.ID, Tokens: ftsTokensFor(n, "")})
		}
	}
	return facts, nil
}

// captureRebuildContractFacts is source-local. Contract records see prepared
// source and raw file-scope edges; native lazy shape, mount and configuration
// readers see accepted raw source and the prepared declaration coordinates.
func (idx *Indexer) captureRebuildContractFacts(ctx context.Context, graphPath, lang string, raw, prepared []byte, result *parser.ExtractionResult, skipped bool) (*contracts.FileFacts, error) {
	c := &contracts.FileFacts{Shapes: make(map[string]*contracts.Shape), Handlers: make(map[string]contracts.HandlerFacts)}
	var fileID string
	for _, node := range result.Nodes {
		if node.Kind == graph.KindFile {
			fileID = node.ID
			break
		}
	}
	_, byLanguage := idx.buildPerFileContractExtractors()
	c.ScanSkipped = skipped || extractionDispositionFor(result).omitSecondarySourceScans() || len(prepared) > maxContractSourceBytes
	if !c.ScanSkipped && fileID != "" {
		exts := byLanguage[lang]
		tree := result.Tree
		if result.ContractTree != nil {
			tree = result.ContractTree
		}
		if err := contracts.CheckFileFactTree(graphPath, prepared, tree, exts); err != nil {
			return nil, err
		}
		var edges []*graph.Edge
		for _, edge := range result.Edges {
			if edge.From == fileID {
				edges = append(edges, edge)
			}
		}
		capture := &contracts.EndpointFactCapture{}
		c.Records = idx.collectContractRecordsForFile(graphPath, prepared, result.Nodes, edges, exts, tree, capture)
		c.Questions = capture.Questions
	}
	var callables bool
	for _, node := range result.Nodes {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		switch node.Kind {
		case graph.KindType, graph.KindInterface:
			c.Shapes[node.ID] = contracts.ExtractShape(node.FilePath, raw, node.StartLine, node.EndLine)
		case graph.KindFunction, graph.KindMethod:
			callables = true
		}
	}
	if err := idx.captureRebuildContractBodies(ctx, graphPath, lang, raw, prepared, result, callables, c); err != nil {
		return nil, err
	}
	if contracts.RouterFactsRequired(graphPath, lang, c.Records, c.Questions) {
		facts := contracts.RouterFileFacts(graphPath, raw).Clone()
		c.Router = &facts
	}
	c.ConfigKeys = contracts.SpringFileFacts(graphPath, raw)
	return c, nil
}

// captureRebuildContractBodies does only the two named raw-tree operations.
// Other lazy handler facts already use raw text and never need another tree.
func (idx *Indexer) captureRebuildContractBodies(ctx context.Context, path, lang string, raw, prepared []byte, result *parser.ExtractionResult, callables bool, facts *contracts.FileFacts) error {
	handlerLang := detectLangFromPath(path)
	var grammar *sitter.Language
	var tree *parser.ParseTree
	switch lang {
	case "go":
		if callables && handlerLang == "go" && len(raw) != 0 {
			grammar, tree = golang.GetLanguage(), result.Tree
		}
	case "php":
		for _, c := range facts.Records {
			if key, _ := contracts.LaravelHandlerClassKey(c); key != "" {
				grammar, tree = php.GetLanguage(), result.ContractTree
				break
			}
		}
	}
	rawTreeDeclined := false
	if grammar != nil && !bytes.Equal(raw, prepared) {
		start := time.Now()
		t, err := parser.ParseFileContext(ctx, raw, grammar)
		idx.logger.Debug("build contract raw parse", zap.String("language", lang), zap.Int("bytes", len(raw)), zap.Duration("elapsed", time.Since(start)), zap.Error(err))
		switch {
		case err == nil:
			tree = parser.NewParseTree(t, raw, lang)
			defer tree.Release()
		case errors.Is(err, parser.ErrUTF16Source), errors.Is(err, parser.ErrBinarySource):
			// Native treats these deterministic input refusals as a nil tree.
			rawTreeDeclined = true
			tree = nil
		default:
			return fmt.Errorf("%w: %s: raw %s: %w", contracts.ErrContractFactTree, path, lang, err)
		}
	}
	if grammar != nil && !rawTreeDeclined && (tree == nil || tree.Tree() == nil || tree.Lang() != lang || !bytes.Equal(tree.Source(), raw)) {
		return fmt.Errorf("%w: %s: %s handler facts", contracts.ErrContractFactTree, path, lang)
	}
	if lang == "php" && grammar != nil {
		resolve := languages.PHPClassNameResolverWithTree(tree)
		facts.LaravelHandlerClasses = make(map[string]string)
		for _, c := range facts.Records {
			if key, class := contracts.LaravelHandlerClassKey(c); key != "" {
				if resolve != nil {
					class = resolve(class, c.Line)
				}
				facts.LaravelHandlerClasses[key] = class
			}
		}
	}
	if !callables {
		return nil
	}
	// PHP uses its tree only for class qualification, not provider facts.
	if lang != "go" {
		tree = nil
	}
	var err error
	facts.Handlers, err = contracts.CaptureFileHandlerFacts(ctx, result.Nodes, splitLines(raw), handlerLang, tree)
	return err
}
