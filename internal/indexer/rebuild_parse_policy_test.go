package indexer

import (
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
