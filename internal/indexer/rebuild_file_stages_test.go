package indexer

import (
	"bytes"
	"context"
	"encoding/hex"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/indexer/merkle"
	"github.com/zzet/gortex/internal/indexer/source"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"go.uber.org/zap"
)

func TestRebuildFileStagesRetainFrozenFactsAcrossEdit(t *testing.T) {
	ctx := t.Context()
	for _, precache := range []bool{true, false} {
		name := "uncached refusal"
		if precache {
			name = "cached completion"
		}
		t.Run(name, func(t *testing.T) {
			root, src := rebuildInputFixture(t)
			var inputs []rebuildInput
			for _, name := range []string{"b.go", "a.go"} {
				require.NoError(t, os.WriteFile(filepath.Join(root, name), []byte("package p\nfunc Frozen() {}\n"), 0o600))
				i, _, err := readRebuildInput(ctx, src, name, "go", 1024)
				require.NoError(t, err)
				inputs = append(inputs, i)
			}
			rootHex, err := rebuildInputRoot(inputs)
			require.NoError(t, err)
			digest, err := hex.DecodeString(rootHex)
			require.NoError(t, err)
			require.Len(t, digest, 32)
			job := rebuildFileStages{
				workDir: t.TempDir(), incarnation: rebuildTestIncarnation,
				inputs: inputs, recipes: map[string]rebuildDigest{"go": {5}},
				witness: store_pebble.Witness{
					Input: store_pebble.HeadDigest(digest), Recipe: store_pebble.HeadDigest{8},
					Profile: store_pebble.FullProfile, Stage: store_pebble.StageManifest,
				},
			}
			reg := parser.NewRegistry()
			reg.Register(languages.NewGoExtractor())
			cfg := config.Default().Index
			idx := New(graph.New(), reg, cfg, zap.NewNop())
			idx.installRebuildParsePolicy(freezeRebuildParsePolicy(root, cfg))
			idx.SetRootPath(root)
			t.Cleanup(idx.Close)
			store := rebuildParseStore{dir: t.TempDir(), limit: 1 << 20}
			if precache {
				_, reused, err := idx.loadRebuildFile(ctx, src, store, inputs[0], job.recipes["go"], rebuildTransformBounds{})
				require.NoError(t, err)
				require.False(t, reused)
			}
			h, err := store_pebble.OpenHead(filepath.Join(t.TempDir(), "head"), rebuildTestIncarnation, store_pebble.FullProfile)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, h.Close()) })
			var stages []store_pebble.Stage
			private, err := store_pebble.OpenBuild(filepath.Join(job.workDir, "working"), job.witness.Profile)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, private.Close()) })
			counts, err := idx.buildRebuildFileStages(ctx, src, store, private, job, func(tables store_pebble.StageTables) error {
				_, err := h.PublishStage(ctx, tables)
				if err != nil {
					return err
				}
				stages = append(stages, tables.Witness.Stage)
				if tables.Witness.Stage == store_pebble.StageManifest {
					return os.WriteFile(filepath.Join(root, "b.go"), []byte("package p\nfunc Edited() {}\n"), 0o600)
				}
				return nil
			})
			s := h.OpenSnapshot()
			t.Cleanup(func() { require.NoError(t, s.Close()) })
			manifestRows := 0
			require.NoError(t, s.ManifestFiles(ctx, func(store_pebble.ManifestFile) error {
				manifestRows++
				return nil
			}))
			require.Equal(t, 2, manifestRows)
			require.EqualValues(t, 1, counts.parsed, "the worker parses a.go first")
			if !precache {
				require.ErrorIs(t, err, ErrBuildInputChanged)
				require.Zero(t, counts.reused)
				selected, selectErr := counts.census.selectPendingCallers([]*graph.Edge{{FilePath: "a.go"}})
				require.ErrorIs(t, selectErr, errRebuildCallerAttribution)
				require.Nil(t, selected)
				require.Equal(t, []store_pebble.Stage{store_pebble.StageManifest}, stages)
				var stagePending *store_pebble.ErrStagePending
				require.ErrorAs(t, s.RequireStage(store_pebble.StageFileGraph), &stagePending)
				return
			}
			require.NoError(t, err)
			require.EqualValues(t, 1, counts.reused)
			require.Equal(t, []store_pebble.Stage{store_pebble.StageManifest, store_pebble.StageFileGraph}, stages)
			require.Len(t, s.FindNodesByName("Frozen"), 2)
			require.Empty(t, s.FindNodesByName("Edited"))
			scratch, err := filepath.Glob(filepath.Join(job.workDir, "working", "pending", "file-graph-base-*"))
			require.NoError(t, err)
			require.Empty(t, scratch, "no Relations consumer needs retained FileGraph files")
		})
	}
}

func TestRebuildFileStagesExtractorCensus(t *testing.T) {
	ctx := t.Context()
	const raw, key = "cafe\u0301.go", "caf\u00e9.go"
	root, src := rebuildInputFixture(t)
	reg := parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())
	reg.Register(languages.NewCExtractor())
	reg.Register(languages.NewCppExtractor())
	reg.Register(languages.NewJavaScriptExtractor())
	reg.Register(languages.NewTypeScriptExtractor())
	cfg := config.Default().Index
	idx := New(graph.New(), reg, cfg, zap.NewNop())
	idx.installRebuildParsePolicy(freezeRebuildParsePolicy(root, cfg))
	idx.SetRootPath(root)
	t.Cleanup(idx.Close)

	store := rebuildParseStore{dir: t.TempDir(), limit: 1 << 20}
	recipe := rebuildDigest{5}
	job := rebuildFileStages{
		incarnation: rebuildTestIncarnation,
		recipes:     map[string]rebuildDigest{"c": recipe, "go": recipe, "javascript": recipe, "typescript": recipe},
	}
	for _, f := range []struct{ path, raw string }{
		{"main.tsx", "export function View() { return <div/>; }\n"},
		{"main.jsx", "export function View() { return <div/>; }\n"},
		{raw, "package p\nconst Label = \"orders\"\nfunc Work() { Pending() }\n"},
		{"header.h", "namespace demo {}\n"},
		{"empty.go", ""},
		{"binary.h", "namespace demo {}\x00"},
	} {
		require.NoError(t, os.WriteFile(filepath.Join(root, f.path), []byte(f.raw), 0o600))
		lang, ok := reg.DetectLanguage(f.path)
		require.True(t, ok)
		input, _, err := readRebuildInput(ctx, src, f.path, lang, 1024)
		require.NoError(t, err)
		job.inputs = append(job.inputs, input)
		if f.path == raw {
			// Keep the receipt path literal but use the recipe for the NFC
			// spelling. The worker must not reuse this wrong-key payload.
			canonical := input
			canonical.Path = key
			canonicalKey := input.parseKey(idx.rebuildExtractorRecipe(recipe, canonical))
			require.NotEqual(t, canonicalKey, input.parseKey(idx.rebuildExtractorRecipe(recipe, input)))
			payload := encodeRebuildTestParseFacts(t, store_pebble.ParseFacts{
				File: graph.FileMetaRow{FilePath: raw}, EffectiveLanguage: "go",
			})
			require.NoError(t, store.write(canonicalKey, payload))
		}
		if f.path == "empty.go" {
			// A serialized successful receipt without even a file node.
			payload := encodeRebuildTestParseFacts(t, store_pebble.ParseFacts{
				File: graph.FileMetaRow{FilePath: f.path, ContentHash: contentHashForSource(nil)}, EffectiveLanguage: "go",
			})
			require.NoError(t, store.write(input.parseKey(idx.rebuildExtractorRecipe(recipe, input)), payload))
		}
	}
	rootHex, err := rebuildInputRoot(job.inputs)
	require.NoError(t, err)
	digest, err := hex.DecodeString(rootHex)
	require.NoError(t, err)
	job.witness = store_pebble.Witness{
		Input: store_pebble.HeadDigest(digest), Recipe: store_pebble.HeadDigest{8},
		Profile: store_pebble.FullProfile, Stage: store_pebble.StageManifest,
	}
	build := func(src source.ContentSource, wantParsed, wantReused uint64) []store_pebble.Witness {
		t.Helper()
		job.workDir = t.TempDir()
		private, err := store_pebble.OpenBuild(filepath.Join(job.workDir, "working"), job.witness.Profile)
		require.NoError(t, err)
		defer func() { require.NoError(t, private.Close()) }()
		h, err := store_pebble.OpenHead(filepath.Join(t.TempDir(), "head"), job.incarnation, job.witness.Profile)
		require.NoError(t, err)
		defer func() { require.NoError(t, h.Close()) }()
		var witnesses []store_pebble.Witness
		counts, err := idx.buildRebuildFileStages(ctx, src, store, private, job, func(tables store_pebble.StageTables) error {
			witnesses = append(witnesses, tables.Witness)
			_, err := h.PublishStage(ctx, tables)
			return err
		})
		require.NoError(t, err)
		require.Len(t, witnesses, 2)
		require.Equal(t, store_pebble.StageFileGraph, witnesses[1].Stage)
		require.Equal(t, wantParsed, counts.parsed)
		require.Equal(t, wantReused, counts.reused)
		require.NotNil(t, counts.census)
		require.EqualValues(t, 6, counts.census.admitted)
		require.Equal(t, map[string]uint64{"cpp": 2, "go": 2, "javascript": 1, "typescript": 1}, counts.census.byID)
		require.Equal(t, map[string]string{
			"binary.h": "cpp", "empty.go": "go", "header.h": "cpp", raw: "go",
			"main.jsx": "javascript", "main.tsx": "typescript",
		}, counts.census.byPath)
		require.Equal(t, raw, counts.census.originByGraphPath[key])
		for _, notKey := range []string{raw, "Caf\u00e9.go"} {
			selected, err := counts.census.selectPendingCallers([]*graph.Edge{{FilePath: notKey}})
			require.ErrorIs(t, err, errRebuildCallerAttribution)
			require.Nil(t, selected)
		}

		view := private.RebuildView()
		calls := 0
		require.NoError(t, view.VisitLinkPendingPages(ctx, 64, 16<<10, func(page []*graph.Edge) error {
			selected, err := counts.census.selectPendingCallers(page)
			if err != nil {
				return err
			}
			for _, e := range selected {
				if e.Kind == graph.EdgeCalls && e.From == key+"::Work" && e.To == "unresolved::Pending" {
					require.Equal(t, key, e.FilePath)
					calls++
				}
			}
			return nil
		}))
		require.Equal(t, 1, calls)
		constant, err := view.ConstantValue(ctx, key+"::Label")
		require.NoError(t, err)
		require.NotNil(t, constant)
		require.Equal(t, graph.ConstantValueRow{FilePath: key, NodeID: key + "::Label", Value: "orders"}, constant.ConstantValueRow)

		s := h.OpenSnapshot()
		defer func() { require.NoError(t, s.Close()) }()
		var manifest []string
		require.NoError(t, s.ManifestFiles(ctx, func(f store_pebble.ManifestFile) error {
			manifest = append(manifest, f.Path+":"+f.Language)
			return nil
		}))
		require.Equal(t, []string{"binary.h:c", raw + ":go", "empty.go:go", "header.h:c", "main.jsx:javascript", "main.tsx:typescript"}, manifest)
		file := s.GetNode(key)
		require.NotNil(t, file)
		require.Equal(t, key, file.FilePath)
		require.Nil(t, s.GetNode(raw))
		decl := s.GetNode(key + "::Work")
		require.NotNil(t, decl)
		require.Equal(t, key, decl.FilePath)
		meta, err := s.FileMetadata("", key)
		require.NoError(t, err)
		require.Equal(t, key, meta.FilePath)
		require.Positive(t, meta.NodeCount)
		empty, err := s.FileMetadata("", "empty.go")
		require.NoError(t, err)
		require.Zero(t, empty.NodeCount)
		skipped, err := private.RebuildView().Node(ctx, "binary.h")
		require.NoError(t, err)
		require.NotNil(t, skipped)
		require.Equal(t, true, skipped.Meta["skipped_due_to_binary"])
		return witnesses
	}
	first := build(src, 5, 1)
	for _, input := range job.inputs {
		if input.Path == raw {
			payload, err := store.read(input.parseKey(idx.rebuildExtractorRecipe(recipe, input)))
			require.NoError(t, err)
			facts := decodeRebuildTestParseFacts(t, payload)
			require.Equal(t, raw, facts.File.FilePath)
			require.Contains(t, facts.Constants, graph.ConstantValueRow{FilePath: raw, NodeID: key + "::Label", Value: "orders"})
		}
	}
	require.NoError(t, os.Remove(filepath.Join(root, raw)))
	require.NoError(t, os.WriteFile(filepath.Join(root, "header.h"), []byte("int edited(void);\n"), 0o600))
	// No source is supplied on replay, so even a live content probe cannot succeed.
	require.Equal(t, first, build(nil, 0, 6))
}

func TestRebuildSelectPendingCallers(t *testing.T) {
	ctx := t.Context()
	census := &rebuildExtractorCensus{
		admitted: 6,
		byPath: map[string]string{
			"a.go": "go", "b.h": "cpp", "c.jsx": "javascript", "d.tsx": "typescript", "e.vue": "vue", "f.md": "markdown",
		},
		byID: map[string]uint64{"go": 1, "cpp": 1, "javascript": 1, "typescript": 1, "vue": 1, "markdown": 1},
		originByGraphPath: map[string]string{
			"a.go": "a.go", "b.h": "b.h", "c.jsx": "c.jsx", "d.tsx": "d.tsx", "e.vue": "e.vue", "f.md": "f.md",
		},
	}
	private, err := store_pebble.OpenBuild(filepath.Join(t.TempDir(), "working"), store_pebble.FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, private.Close()) })
	view := private.RebuildView()
	// Both endpoint nodes are off-language, even for supported caller paths.
	private.AddNode(&graph.Node{ID: "source", Kind: graph.KindFunction, Name: "Caller", FilePath: "b.h", Language: "cpp"})
	private.AddNode(&graph.Node{ID: "target", Kind: graph.KindFunction, Name: "Target", FilePath: "e.vue", Language: "vue"})
	const pending = "unresolved::Target"
	for _, path := range []string{"f.md", "e.vue", "d.tsx", "c.jsx", "b.h", "a.go"} {
		private.AddEdge(&graph.Edge{From: "source", Kind: graph.EdgeCalls, To: pending, FilePath: path, Line: 1})
	}
	visitPending := func(yield func([]*graph.Edge) error) error {
		return view.VisitLinkPendingPages(ctx, 10, 4096, yield)
	}
	var seen []string
	require.NoError(t, visitPending(func(page []*graph.Edge) error {
		require.Len(t, page, 6)
		incomplete := *census
		incomplete.admitted++
		unproved, err := incomplete.selectPendingCallers(page)
		require.ErrorIs(t, err, errRebuildCallerAttribution)
		require.Nil(t, unproved)
		for _, invalid := range []*graph.Edge{nil, {}, {FilePath: "./a.go"}, {FilePath: "../a.go"},
			{FilePath: "A.go"}, {FilePath: "unknown.go"}, {FilePath: "a.go\x00"}} {
			// A supported first row must not leak through a later refusal.
			selected, err := census.selectPendingCallers([]*graph.Edge{page[0], invalid})
			require.ErrorIs(t, err, errRebuildCallerAttribution)
			require.Nil(t, selected)
		}
		selected, err := census.selectPendingCallers(page)
		require.NoError(t, err)
		for _, e := range selected {
			seen = append(seen, e.FilePath)
		}
		// A deferred-only page is valid and schedules no work.
		deferred, err := census.selectPendingCallers(page[4:])
		require.NoError(t, err)
		require.Empty(t, deferred)

		// This is the unfiltered candidate source used by StartRebuildLink.
		var candidates []string
		require.NoError(t, view.VisitNodes(ctx, func(n *graph.Node) error {
			candidates = append(candidates, n.ID+":"+n.Language)
			return nil
		}))
		require.Equal(t, []string{"source:cpp", "target:vue"}, candidates)
		// Exercise the selected input only, not D72 endpoint projection.
		var moves []graph.EdgeReindex
		for _, e := range selected {
			updated := *e
			updated.To = "target"
			moves = append(moves, graph.EdgeReindex{Edge: &updated, OldTo: e.To})
		}
		return private.ApplyLinkReindexes(ctx, moves)
	}))
	require.Equal(t, []string{"a.go", "c.jsx", "d.tsx"}, seen)
	saved := make(map[string]graph.Edge)
	require.NoError(t, view.VisitEdges(ctx, func(e *graph.Edge) error {
		saved[e.FilePath] = *e
		return nil
	}))
	require.Len(t, saved, 6)
	for _, path := range []string{"a.go", "c.jsx", "d.tsx"} {
		require.Equal(t, "target", saved[path].To)
	}
	for _, path := range []string{"b.h", "e.vue", "f.md"} {
		require.Equal(t, graph.Edge{From: "source", Kind: graph.EdgeCalls, To: pending, FilePath: path, Line: 1}, saved[path])
	}
}

func TestRebuildFileStagesRefusesNFCPathCollision(t *testing.T) {
	ctx := t.Context()
	// An escaped pair exceeds the generic detail limit but both complete raw
	// names still fit in the worker's typed refusal. No such paths are opened.
	prefix := strings.Repeat(strings.Repeat("\x01", 200)+"/", 12)
	a, b := prefix+"cafe\u0301.go", prefix+"caf\u00e9.go"
	input := func(path string) rebuildInput {
		return rebuildInput{Path: path, Language: "go", Kind: merkle.RegularFile, Size: 24, Content: rebuildDigest{1}}
	}
	cfg := config.Default().Index
	cfg.MaxFileSize = 128
	reg := parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())
	idx := New(graph.New(), reg, cfg, zap.NewNop())
	t.Cleanup(idx.Close)
	var detail string
	for _, tc := range []struct {
		name   string
		inputs []rebuildInput
		rows   map[string]rebuildClassificationFact
	}{
		{name: "two parses", inputs: []rebuildInput{input(a), input(b)}},
		{name: "reversed parses", inputs: []rebuildInput{input(b), input(a)}},
		{name: "parse and skip", inputs: []rebuildInput{input(a)}, rows: map[string]rebuildClassificationFact{b: {Language: "go", Size: 129, MaxFileSize: 128}}},
		{name: "two skips", rows: map[string]rebuildClassificationFact{a: {Language: "go", Size: 129, MaxFileSize: 128}, b: {Language: "go", Size: 129, MaxFileSize: 128}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, err := rebuildInputRoot(tc.inputs)
			require.NoError(t, err)
			digest, err := hex.DecodeString(root)
			require.NoError(t, err)
			require.Len(t, digest, 32)
			lease := t.TempDir()
			job := rebuildFileStages{
				workDir: lease, incarnation: rebuildTestIncarnation, inputs: tc.inputs,
				recipes: map[string]rebuildDigest{"go": {1}}, classifications: tc.rows,
				witness: store_pebble.Witness{Input: store_pebble.HeadDigest(digest), Profile: store_pebble.FullProfile, Stage: store_pebble.StageManifest},
			}
			store := rebuildParseStore{dir: t.TempDir(), limit: 1 << 20}
			sent := 0
			counts, err := idx.buildRebuildFileStages(ctx, nil, store, nil, job, func(store_pebble.StageTables) error {
				sent++
				return nil
			})
			require.ErrorIs(t, err, errRebuildGraphPathCollision)
			require.Zero(t, counts)
			require.Zero(t, sent)
			if detail == "" {
				detail = err.Error()
			}
			require.EqualError(t, err, detail)
			var reply, acks bytes.Buffer
			require.NoError(t, writeRebuildBuildReply(&reply, buildRefusal(err)))
			err = receiveRebuildStages(ctx, lease, rebuildBuildRequest{}, &reply, &acks, func(context.Context, store_pebble.StageTables) error {
				t.Fatal("collision must not offer a stage")
				return nil
			})
			require.ErrorIs(t, err, errRebuildGraphPathCollision)
			require.ErrorContains(t, err, strconv.Quote(a))
			require.ErrorContains(t, err, strconv.Quote(b))
			require.Zero(t, acks.Len())
			entries, err := os.ReadDir(lease)
			require.NoError(t, err)
			require.Empty(t, entries)
			entries, err = os.ReadDir(store.dir)
			require.NoError(t, err)
			require.Empty(t, entries)
		})
	}
}

func TestRebuildFileStagesContractReceiptLifecycle(t *testing.T) {
	ctx := t.Context()
	root, src := rebuildInputFixture(t)
	const original = "package p\nfunc Original() {}\n"
	const edited = "package p\nfunc Edited() {}\n"
	for _, name := range []string{"a.go", "b.go"} {
		writeFile(t, filepath.Join(root, name), original)
	}
	store := rebuildParseStore{dir: t.TempDir(), limit: 1 << 20}
	recipe := rebuildDigest{1}
	newWorker := func() *Indexer {
		idx := newTestIndexer(graph.New())
		idx.installRebuildParsePolicy(freezeRebuildParsePolicy(root, idx.config))
		idx.SetRootPath(root)
		t.Cleanup(idx.Close)
		return idx
	}
	inputs := func(names ...string) []rebuildInput {
		t.Helper()
		var result []rebuildInput
		for _, name := range names {
			input, _, err := readRebuildInput(ctx, src, name, "go", 1024)
			require.NoError(t, err)
			result = append(result, input)
		}
		return result
	}
	build := func(idx *Indexer, admitted []rebuildInput) (rebuildParsedCounts, []store_pebble.Stage, error) {
		t.Helper()
		work := t.TempDir()
		job := rebuildFileStages{
			workDir:     work,
			incarnation: rebuildTestIncarnation,
			inputs:      admitted,
			recipes:     map[string]rebuildDigest{"go": recipe},
			witness: store_pebble.Witness{
				Recipe:  store_pebble.HeadDigest{2},
				Profile: store_pebble.FullProfile,
				Stage:   store_pebble.StageManifest,
			},
		}
		if inputRoot, err := rebuildInputRoot(admitted); err == nil {
			digest, err := hex.DecodeString(inputRoot)
			require.NoError(t, err)
			job.witness.Input = store_pebble.HeadDigest(digest)
		}
		private, err := store_pebble.OpenBuild(filepath.Join(work, "working"), job.witness.Profile)
		require.NoError(t, err)
		defer func() { require.NoError(t, private.Close()) }()
		var offers []store_pebble.Stage
		counts, err := idx.buildRebuildFileStages(ctx, src, store, private, job, func(tables store_pebble.StageTables) error {
			offers = append(offers, tables.Witness.Stage)
			return nil
		})
		return counts, offers, err
	}
	checkBuild := func(idx *Indexer, admitted []rebuildInput, parsed uint64) {
		t.Helper()
		before := parser.ParseCount()
		counts, offers, err := build(idx, admitted)
		require.NoError(t, err)
		require.Equal(t, []store_pebble.Stage{store_pebble.StageManifest, store_pebble.StageFileGraph}, offers)
		require.Equal(t, parsed, counts.parsed)
		require.Equal(t, uint64(len(admitted))-parsed, counts.reused)
		require.Equal(t, before+parsed, parser.ParseCount())
	}
	idx := newWorker()
	initial := inputs("a.go", "b.go")
	legacyKey := initial[0].parseKey(idx.rebuildBaseExtractorRecipe(recipe, initial[0]))
	v2, err := store_pebble.EncodeParseFacts(store_pebble.ParseFacts{
		File: graph.FileMetaRow{FilePath: "a.go"}, EffectiveLanguage: "go",
	})
	require.NoError(t, err)
	require.NoError(t, store.write(legacyKey, v2))
	checkBuild(idx, initial, 2)
	entries, err := os.ReadDir(store.dir)
	require.NoError(t, err)
	require.Len(t, entries, 3, "two new contract receipts and one untouched old entry")
	for _, in := range initial {
		payload, err := store.read(in.parseKey(idx.rebuildExtractorRecipe(recipe, in)))
		require.NoError(t, err)
		base, facts, _, err := store_pebble.DecodeContractParseFacts(payload)
		require.NoError(t, err)
		require.Equal(t, in.Path, base.File.FilePath)
		require.Contains(t, facts.Handlers, in.Path+"::Original")
	}
	saved, err := store.read(legacyKey)
	require.NoError(t, err)
	require.Equal(t, v2, saved)
	checkBuild(idx, initial, 0)
	restarted := newWorker()
	checkBuild(restarted, initial, 0)
	writeFile(t, filepath.Join(root, "b.go"), edited)
	checkBuild(restarted, inputs("a.go", "b.go"), 1)
	require.NoError(t, os.Remove(filepath.Join(root, "b.go")))
	checkBuild(restarted, inputs("a.go"), 0)
	writeFile(t, filepath.Join(root, "b.go"), edited)
	checkBuild(restarted, inputs("a.go", "b.go"), 0)

	before := parser.ParseCount()
	_, offers, err := build(restarted, []rebuildInput{initial[0], initial[0]})
	require.ErrorContains(t, err, "duplicate build input path")
	require.Empty(t, offers)
	require.Equal(t, before, parser.ParseCount())

	// An invalid current entry is not an old-key miss. The existing
	// manifest offer precedes receipt loading; no file-graph offer follows.
	badInput := inputs("a.go")[0]
	badKey := badInput.parseKey(restarted.rebuildExtractorRecipe(recipe, badInput))
	valid, err := store.read(badKey)
	require.NoError(t, err)
	for _, tc := range []string{"legacy payload", "truncated payload", "wrong key", "wrong digest", "wrong embedded path"} {
		t.Run(tc, func(t *testing.T) {
			store = rebuildParseStore{dir: t.TempDir(), limit: 1 << 20}
			var wire []byte
			switch tc {
			case "legacy payload":
				header := makeRebuildParseEntryHeader(badKey, v2)
				wire = append(header[:], v2...)
			case "truncated payload":
				short := valid[:len(valid)-1]
				header := makeRebuildParseEntryHeader(badKey, short)
				wire = append(header[:], short...)
			case "wrong key":
				header := makeRebuildParseEntryHeader(legacyKey, valid)
				wire = append(header[:], valid...)
			case "wrong digest":
				header := makeRebuildParseEntryHeader(badKey, valid)
				wire = append(header[:], valid...)
				wire[len(wire)-1] ^= 1
			case "wrong embedded path":
				other := encodeRebuildTestParseFacts(t, store_pebble.ParseFacts{
					File: graph.FileMetaRow{FilePath: "other.go"}, EffectiveLanguage: "go",
				})
				header := makeRebuildParseEntryHeader(badKey, other)
				wire = append(header[:], other...)
			}
			require.NoError(t, os.WriteFile(store.entry(badKey), wire, 0o600))
			if tc == "wrong embedded path" {
				got, reused, err := restarted.loadRebuildFile(ctx, src, store, badInput, recipe, rebuildTransformBounds{})
				require.ErrorIs(t, err, errParseEntryCorrupt)
				require.ErrorContains(t, err, "file identity differs")
				require.Empty(t, got)
				require.False(t, reused)
			}
			_, offers, err := build(restarted, []rebuildInput{badInput})
			require.ErrorIs(t, err, errParseEntryCorrupt)
			require.Equal(t, []store_pebble.Stage{store_pebble.StageManifest}, offers)
			require.Equal(t, before, parser.ParseCount())
			saved, err := os.ReadFile(store.entry(badKey))
			require.NoError(t, err)
			require.Equal(t, wire, saved, "a current corrupt entry is never overwritten")
		})
	}
}
