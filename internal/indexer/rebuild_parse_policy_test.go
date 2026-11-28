package indexer

import (
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"go.uber.org/zap"
)

func TestRebuildParsePolicyKeysUseFrozenEffectiveInputs(t *testing.T) {
	root, src := rebuildInputFixture(t)
	ctx := t.Context()
	t.Setenv(config.LocalTemporalOptInEnv, "true")
	owners := filepath.Join(root, "CODEOWNERS")
	allowDir := filepath.Join(root, ".gortex")
	require.NoError(t, os.Mkdir(allowDir, 0o700))
	allow := filepath.Join(allowDir, "temporal-allowlist.yaml")
	write := func(path, text string) {
		t.Helper()
		require.NoError(t, os.WriteFile(path, []byte(text), 0o600))
	}
	write(owners, "/owned.go @first\n")
	write(allow, "env_helpers: [HelperA, HelperB]\n")
	for _, p := range []string{"owned.go", "unowned.go"} {
		write(filepath.Join(root, p), "package demo\nfunc Run() {}\n")
	}
	read := func(path string) rebuildInput {
		t.Helper()
		input, _, err := readRebuildInput(ctx, src, path, "go", 1024)
		require.NoError(t, err)
		return input
	}
	owned, unowned := read("owned.go"), read("unowned.go")
	cfg := config.Default().Index
	enabled := true
	cfg.Coverage.Ownership.Enabled = &enabled
	frozen := freezeRebuildParsePolicy(root, cfg)
	reg := parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())
	start := func(policy rebuildParsePolicy) *Indexer {
		t.Helper()
		idx := New(graph.New(), reg, cfg, zap.NewNop())
		idx.installRebuildParsePolicy(policy)
		idx.SetRootPath(root)
		t.Cleanup(idx.Close)
		return idx
	}
	base := rebuildDigest{2}
	store := rebuildParseStore{dir: t.TempDir(), limit: 1 << 20}
	save := func(idx *Indexer, input rebuildInput, wantReuse bool) store_pebble.ParseFacts {
		t.Helper()
		_, reused, err := idx.loadRebuildFile(ctx, src, store, input, base, rebuildTransformBounds{})
		require.NoError(t, err)
		require.Equal(t, wantReuse, reused)
		key := input.parseKey(idx.rebuildExtractorRecipe(base, input))
		raw, err := store.read(key)
		require.NoError(t, err)
		facts := decodeRebuildTestParseFacts(t, raw)
		require.Positive(t, facts.File.NodeCount)
		return facts
	}
	checkOwner := func(facts store_pebble.ParseFacts, want string) {
		t.Helper()
		var saved []string
		for _, edge := range facts.Edges {
			if edge.Kind == graph.EdgeOwns {
				saved = append(saved, edge.From)
			}
		}
		require.Equal(t, []string{want}, saved)
	}

	write(owners, "/owned.go @second\n")
	old := start(frozen) // started after the edit; must not read the new rules
	checkOwner(save(old, owned, false), "team::first")
	save(old, unowned, false)
	current := start(freezeRebuildParsePolicy(root, cfg))
	checkOwner(save(current, owned, false), "team::second")
	save(current, unowned, true)

	write(allow, "env_helpers: [' HelperB ', HelperA, HelperB]\n")
	cfg.Workers++ // worker count does not change parse bytes
	same := start(freezeRebuildParsePolicy(root, cfg))
	save(same, owned, true)
	write(allow, "env_helpers: [DifferentHelper]\n")
	updated := start(freezeRebuildParsePolicy(root, cfg))
	checkOwner(save(updated, owned, false), "team::second")
	save(updated, unowned, false)
}

func TestRebuildContractRecipeVersions(t *testing.T) {
	idx := &Indexer{}
	off := false
	idx.config.Coverage.Clones.Enabled = &off
	idx.config.Coverage.Ownership.Enabled = &off
	// Pin the recipe framing with fixed serialized settings, independent of
	// the environment and config defaults. Update only on a recipe change.
	idx.frozenRebuildContractPolicy.Store(&rebuildContractPolicy{suffix: rebuildContractRecipeSuffix([]byte("{}"))})
	base := rebuildDigest{1}
	header := rebuildInput{Path: "x.h", Language: "c"}
	cFile := rebuildInput{Path: "x.c", Language: "c"}
	goFile := rebuildInput{Path: "x.go", Language: "go"}
	beforeHeader := idx.rebuildExtractorRecipe(base, header)
	// Contract capture v2 records empty local AST type lookups.
	require.Equal(t, "7765e758782d6a33203bfbfa774bf8d6ad6ee5077f85b88135c3749c6e5fb8b6", hex.EncodeToString(beforeHeader[:]))
	require.NotEqual(t, beforeHeader, idx.rebuildExtractorRecipe(rebuildDigest{2}, header))
	beforeC := idx.rebuildExtractorRecipe(base, cFile)
	beforeGo := idx.rebuildExtractorRecipe(base, goFile)
	require.NotEqual(t, beforeC, beforeHeader)
	bump := func(lang string) {
		prior := extractorVersions[lang]
		t.Cleanup(func() { extractorVersions[lang] = prior })
		extractorVersions[lang]++
	}
	bump("cpp")
	require.NotEqual(t, beforeHeader, idx.rebuildExtractorRecipe(base, header))
	require.Equal(t, beforeC, idx.rebuildExtractorRecipe(base, cFile))
	require.Equal(t, beforeGo, idx.rebuildExtractorRecipe(base, goFile))
	bump("go")
	require.NotEqual(t, beforeGo, idx.rebuildExtractorRecipe(base, goFile))
}

func TestRebuildContractRecipeBindsStampedConfig(t *testing.T) {
	ctx := t.Context()
	t.Setenv("CODEGRAPH_EVENT_CONFIG", "")
	t.Setenv("GORTEX_EVENT_BUS_DISABLE", "1")
	root, src := rebuildInputFixture(t)
	const path = "routes.go"
	writeFile(t, filepath.Join(root, path), "package p\nfunc Route() {}\n")
	input, _, err := readRebuildInput(ctx, src, path, "go", 1024)
	require.NoError(t, err)
	base := rebuildDigest{1}
	start := func(cfg config.IndexConfig) *Indexer {
		t.Helper()
		reg := parser.NewRegistry()
		reg.Register(languages.NewGoExtractor())
		idx := New(graph.New(), reg, cfg, zap.NewNop())
		idx.installRebuildParsePolicy(rebuildParsePolicy{})
		t.Cleanup(idx.Close)
		return idx
	}
	readFresh := func(idx *Indexer) (rebuildDigest, []byte, contractDependencyInputs) {
		t.Helper()
		store := rebuildParseStore{dir: t.TempDir(), limit: 1 << 20}
		key := input.parseKey(idx.rebuildExtractorRecipe(base, input))
		facts, reused, err := idx.loadRebuildFile(ctx, src, store, input, base, rebuildTransformBounds{})
		require.NoError(t, err)
		require.False(t, reused)
		stamp, ok := storedContractDependencyInputs(facts.Nodes)
		require.True(t, ok)
		wire, err := store.read(key)
		require.NoError(t, err)
		return key, wire, stamp
	}
	cfg := config.Default().Index
	cfg.EventBus = []config.EventBusBoundarySpec{{Name: "bus", Callee: "publish"}}
	original := start(cfg)
	key, wire, stamp := readFresh(original)

	// These are the only construction settings excluded from the stamp.
	construction := cfg
	construction.Workers++
	construction.DirtyChain = &config.DirtyChainConfig{}
	otherKey, otherWire, otherStamp := readFresh(start(construction))
	require.Equal(t, key, otherKey)
	require.Equal(t, stamp, otherStamp)
	require.Equal(t, wire, otherWire)

	for _, change := range []struct {
		name string
		edit func(*config.IndexConfig)
	}{
		{"aliases", func(cfg *config.IndexConfig) { cfg.HTTPClientAliases = []string{"apiGet"} }},
		{"disabled bus specs", func(cfg *config.IndexConfig) { cfg.EventBus = nil }},
		{"generated parser setting", func(cfg *config.IndexConfig) { cfg.IndexGeneratedParsers = !cfg.IndexGeneratedParsers }},
	} {
		t.Run(change.name, func(t *testing.T) {
			other := cfg
			change.edit(&other)
			otherKey, otherWire, otherStamp := readFresh(start(other))
			require.NotEqual(t, stamp.Config, otherStamp.Config)
			require.NotEqual(t, wire, otherWire)
			require.NotEqual(t, key, otherKey)
		})
	}

	// A running job uses the same effective boundaries for admission,
	// capture and replay even if the environment changes mid-job.
	t.Setenv("GORTEX_EVENT_BUS_DISABLE", "")
	otherKey, otherWire, otherStamp = readFresh(original)
	require.Equal(t, key, otherKey)
	require.Equal(t, stamp, otherStamp)
	require.Equal(t, wire, otherWire)
	otherKey, otherWire, otherStamp = readFresh(start(cfg))
	require.NotEqual(t, stamp.Config, otherStamp.Config)
	require.NotEqual(t, wire, otherWire)
	require.NotEqual(t, key, otherKey)
}
