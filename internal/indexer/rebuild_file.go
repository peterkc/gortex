package indexer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

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

// loadRebuildFile reads a checked contract receipt before touching the checkout.
func (idx *Indexer) loadRebuildFile(ctx context.Context, src source.ContentSource, store rebuildParseStore, input rebuildInput, recipe rebuildDigest, bounds rebuildTransformBounds) (store_pebble.ParseFacts, bool, error) {
	base, _, reused, err := idx.loadRebuildFileWithContractFacts(ctx, src, store, input, recipe, bounds)
	return base, reused, err
}

func (idx *Indexer) loadRebuildFileWithContractFacts(ctx context.Context, src source.ContentSource, store rebuildParseStore, input rebuildInput, recipe rebuildDigest, bounds rebuildTransformBounds) (store_pebble.ParseFacts, *contracts.FileFacts, bool, error) {
	return idx.loadRebuildFileWith(ctx, src, store, input, recipe, bounds,
		func(ctx context.Context, _ rebuildDigest, rel string, ext parser.Extractor, raw, prepared []byte) ([]byte, error) {
			base, facts, err := idx.extractPreparedRebuildFileWithContractFacts(ctx, rel, ext, raw, prepared)
			if err != nil {
				return nil, err
			}
			payload, err := store_pebble.EncodeContractParseFacts(base, facts)
			if err != nil {
				return nil, fmt.Errorf("encode parse facts for %s: %w", rel, err)
			}
			return payload, nil
		})
}

// All receipt paths share the cache and the work around extraction. Only a
// missing current key permits extraction, after checking raw and transforming once.
func (idx *Indexer) loadRebuildFileWith(ctx context.Context, src source.ContentSource, store rebuildParseStore, input rebuildInput, recipe rebuildDigest, bounds rebuildTransformBounds,
	extract func(context.Context, rebuildDigest, string, parser.Extractor, []byte, []byte) ([]byte, error),
) (base store_pebble.ParseFacts, facts *contracts.FileFacts, reused bool, err error) {
	if err := ctx.Err(); err != nil {
		return base, nil, false, err
	}
	ext, _ := idx.registry.GetByLanguage(input.Language)
	if ext == nil {
		return base, nil, false, fmt.Errorf("no extractor for %s (%s)", input.Path, input.Language)
	}
	key := input.parseKey(idx.rebuildExtractorRecipe(recipe, input))
	payload, err := store.read(key)
	if err == nil {
		base, facts, err := idx.decodeRebuildFileReceipt(input, payload)
		return base, facts, err == nil, err
	}
	if !errors.Is(err, os.ErrNotExist) {
		return base, nil, false, err
	}
	if bounds.FileBytes > 0 && input.Size > int64(bounds.FileBytes) {
		return base, nil, false, fmt.Errorf("%w: %s has %d bytes, limit %d", ErrBuildTransformBounds, input.Path, input.Size, bounds.FileBytes)
	}
	raw, err := readFrozenRebuildInput(ctx, src, input)
	if err != nil {
		return base, nil, false, err
	}
	rel := input.Path
	// Match the native pre-transform probe, without consulting live source.
	// Use the portable path so a checkout directory cannot affect detection.
	if parser.ExtensionNeedsContentProbe(rel) {
		if lang, ok := idx.registry.DetectLanguageContent(rel, raw); ok && lang != input.Language {
			if !possibleRebuildLanguageRefinement(rel, lang) {
				return base, nil, false, fmt.Errorf("%s: unexplained parse language change from %q to %q", rel, input.Language, lang)
			}
			ext, _ = idx.registry.GetByLanguage(lang)
		}
	}
	prepared, spilled, err := idx.transforms.runRebuild(ctx, pathkey.Normalize(rel), raw, bounds)
	if err != nil {
		return base, nil, false, err
	}
	if bounds.FileBytes > 0 && len(prepared) > bounds.FileBytes {
		return base, nil, false, fmt.Errorf("%w: %s produced %d bytes, limit %d", ErrBuildTransformBounds, rel, len(prepared), bounds.FileBytes)
	}
	if spilled != 0 {
		idx.logger.Debug("build command transform spilled", zap.String("file", rel), zap.Int64("spill_bytes", spilled))
	}
	payload, err = extract(ctx, key, rel, ext, raw, prepared)
	if err != nil {
		return base, nil, false, err
	}
	if len(payload) > store.limit {
		return base, nil, false, fmt.Errorf("%w: %x", errParseEntryTooLarge, key)
	}
	base, facts, err = idx.decodeRebuildFileReceipt(input, payload)
	if err != nil {
		return base, nil, false, err
	}
	if err := ctx.Err(); err != nil {
		return store_pebble.ParseFacts{}, nil, false, err
	}
	if err := store.write(key, payload); err != nil {
		return store_pebble.ParseFacts{}, nil, false, err
	}
	return base, facts, false, nil
}

func (idx *Indexer) decodeRebuildFileReceipt(input rebuildInput, payload []byte) (store_pebble.ParseFacts, *contracts.FileFacts, error) {
	base, facts, _, err := store_pebble.DecodeContractParseFacts(payload)
	if err != nil {
		return store_pebble.ParseFacts{}, nil, fmt.Errorf("%w: %s: %w", errParseEntryCorrupt, input.Path, err)
	}
	if base.File.FilePath != input.Path {
		return store_pebble.ParseFacts{}, nil, fmt.Errorf("%w: %s: file identity differs", errParseEntryCorrupt, input.Path)
	}
	effective, _ := idx.registry.GetByLanguage(base.EffectiveLanguage)
	if effective == nil ||
		(base.EffectiveLanguage != input.Language && !possibleRebuildLanguageRefinement(input.Path, base.EffectiveLanguage)) {
		return store_pebble.ParseFacts{}, nil, fmt.Errorf("%w: %s: invalid effective parse language %q for admitted language %q",
			errParseEntryCorrupt, input.Path, base.EffectiveLanguage, input.Language)
	}
	if err := checkRebuildReceiptBuiltinNodes(input.Path, base.Nodes); err != nil {
		return store_pebble.ParseFacts{}, nil, err
	}
	return base, facts, nil
}

func checkRebuildReceiptBuiltinNodes(path string, nodes []*graph.Node) error {
	for _, n := range nodes {
		if n.Kind == graph.KindBuiltin {
			return fmt.Errorf("%w: %s: explicit builtin node %q", errParseEntryCorrupt, path, n.ID)
		}
	}
	return nil
}

// possibleRebuildLanguageRefinement checks the range of the pinned native
// content probe. It does not pick a language; the digest-bound receipt records
// which result the probe selected. A registered but impossible ID is corrupt.
func possibleRebuildLanguageRefinement(path, lang string) bool {
	return slices.Contains(possibleRebuildLanguageRefinements(path), lang)
}

// Return all possible refinements in recipe order, even if a language still
// uses the default extractor version.
func possibleRebuildLanguageRefinements(path string) []string {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".h":
		return []string{"cpp", "objc"}
	case ".inc":
		return []string{"c"}
	case ".m":
		return []string{"mathematica", "matlab", "objc"}
	case ".json":
		return []string{"liquid_json"}
	case ".xml":
		return []string{"mybatis", "spring"}
	default:
		return nil
	}
}

// Stamp the native-compatible graph without the rebuild-only receiver facts.
// The projection shares the nodes (the fingerprint stamps belong on them) but
// copies hinted edges, so the raw receipt retains the original evidence.
func stampRebuildExtractionGraphFingerprint(result *parser.ExtractionResult) {
	projection := parser.ExtractionResult{Nodes: result.Nodes, Edges: result.Edges}
	copied := false
	for i, edge := range result.Edges {
		if edge == nil {
			continue
		}
		if _, ok := edge.BuiltinReceiverHint(); !ok {
			continue
		}
		if !copied {
			projection.Edges = slices.Clone(result.Edges)
			copied = true
		}
		copy := *edge
		copy.Meta = maps.Clone(edge.Meta)
		maps.DeleteFunc(copy.Meta, func(key string, _ any) bool {
			return strings.HasPrefix(key, "platform_receiver_")
		})
		projection.Edges[i] = &copy
	}
	stampExtractionGraphFingerprint(&projection)
}

// extractPreparedRebuildFileWithContractFacts captures portable answers on the
// file's parse turn. Only the named raw grammar facts can require a raw parse.
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
	if capture, ok := ext.(parser.OptionsExtractor); ok && capturesRebuildBuiltinReceiverFacts(lang) {
		ext = rebuildBuiltinReceiverExtractor{Extractor: ext, capture: capture}
	}
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
		stampRebuildExtractionGraphFingerprint(result)
		idx.stampContractDependencyInputs(graphPath, lang, prepared, result)
	}
	// ParseFacts v2 stores the literal receipt origin in File.FilePath and
	// derives every constant's FilePath from it on decode. Their node IDs,
	// like other graph facts, were minted with the NFC key by the extractor.
	meta, _ := idx.prepareFileMeta(rel, prepared, result)
	meta.FilePath = rel
	constants, _ := idx.prepareConstValues(result)
	for i := range constants {
		if idx.repoPrefix != "" {
			prefix := idx.repoPrefix + "/"
			constants[i].NodeID = strings.TrimPrefix(constants[i].NodeID, prefix)
			constants[i].FilePath = strings.TrimPrefix(constants[i].FilePath, prefix)
		}
		if constants[i].FilePath == graphPath {
			constants[i].FilePath = rel
		}
	}
	if result.GoBuiltinScope != "" {
		for _, n := range result.Nodes {
			if n != nil && n.Kind == graph.KindFile && n.ID == graphPath {
				if n.Meta == nil {
					n.Meta = make(map[string]any, 1)
				}
				n.Meta[store_pebble.RebuildGoBuiltinScopeReceiptMeta] = result.GoBuiltinScope
				break
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
	var rawLines []string
	for _, node := range result.Nodes {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		switch node.Kind {
		case graph.KindType, graph.KindInterface:
			var shape *contracts.Shape
			if extract := contracts.ShapeExtractorForPath(node.FilePath); extract != nil {
				if rawLines == nil {
					rawLines = splitLines(raw)
				}
				shape = extract(rawLines, node.StartLine, node.EndLine)
			}
			c.Shapes[node.ID] = shape
		case graph.KindFunction, graph.KindMethod:
			callables = true
		}
	}
	if err := idx.captureRebuildContractBodies(ctx, graphPath, lang, raw, prepared, rawLines, result, callables, c); err != nil {
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
func (idx *Indexer) captureRebuildContractBodies(ctx context.Context, path, lang string, raw, prepared []byte, rawLines []string, result *parser.ExtractionResult, callables bool, facts *contracts.FileFacts) error {
	handlerLang := detectLangFromPath(path)
	var grammar *sitter.Language
	var tree *parser.ParseTree
	treeLang := ""
	switch {
	case callables && handlerLang == "go" && len(raw) != 0:
		grammar, tree, treeLang = golang.GetLanguage(), result.Tree, "go"
	case strings.HasSuffix(path, ".php"):
		for _, c := range facts.Records {
			if key, _ := contracts.LaravelHandlerClassKey(c); key != "" {
				grammar, tree, treeLang = php.GetLanguage(), result.ContractTree, "php"
				break
			}
		}
	}
	rawTreeDeclined := false
	if grammar != nil && (lang != treeLang || !bytes.Equal(raw, prepared)) {
		start := time.Now()
		t, err := parser.ParseFileContext(ctx, raw, grammar)
		idx.logger.Debug("build contract raw parse", zap.String("language", treeLang), zap.Int("bytes", len(raw)), zap.Duration("elapsed", time.Since(start)), zap.Error(err))
		switch {
		case err == nil:
			tree = parser.NewParseTree(t, raw, treeLang)
			defer tree.Release()
		case errors.Is(err, parser.ErrUTF16Source), errors.Is(err, parser.ErrBinarySource):
			// Native treats these deterministic input refusals as a nil tree.
			rawTreeDeclined = true
			tree = nil
		default:
			return fmt.Errorf("%w: %s: raw %s: %w", contracts.ErrContractFactTree, path, treeLang, err)
		}
	}
	if grammar != nil && !rawTreeDeclined && (tree == nil || tree.Tree() == nil || tree.Lang() != treeLang || !bytes.Equal(tree.Source(), raw)) {
		return fmt.Errorf("%w: %s: %s handler facts", contracts.ErrContractFactTree, path, treeLang)
	}
	if treeLang == "php" {
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
	if rawLines == nil {
		rawLines = splitLines(raw)
	}
	var err error
	facts.Handlers, err = contracts.CaptureFileHandlerFacts(ctx, result.Nodes, rawLines, handlerLang, tree)
	return err
}

// The adapter is local to rebuild extraction, after optional tree retention is
// selected. It preserves admission's asset class and the normal timeout path.
type rebuildBuiltinReceiverExtractor struct {
	parser.Extractor
	capture parser.OptionsExtractor
}

func (e rebuildBuiltinReceiverExtractor) AssetClass() parser.AssetClass {
	return parser.AssetClassOf(e.Extractor)
}

func (e rebuildBuiltinReceiverExtractor) ExtractWithOptions(path string, src []byte, opts parser.ExtractionOptions) (*parser.ExtractionResult, error) {
	opts.CaptureBuiltinReceiverFacts = true
	return e.capture.ExtractWithOptions(path, src, opts)
}
