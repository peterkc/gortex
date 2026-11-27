package indexer

import (
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/indexer/merkle"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"go.uber.org/zap"
)

func TestRebuildFileGraphRejectsExplicitBuiltinReceipt(t *testing.T) {
	ctx := t.Context()
	const file = "a.go"
	reg := parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())
	idx := New(graph.New(), reg, config.Default().Index, zap.NewNop())
	t.Cleanup(idx.Close)
	idx.installRebuildParsePolicy(rebuildParsePolicy{})
	build, dir := openRebuildTestFileStore(t)
	recipe := rebuildDigest{5}
	input := rebuildInput{Path: file, Language: "go", Kind: merkle.RegularFile, Size: 1, Content: rebuildDigest{1}}
	job := rebuildFileStages{
		workDir: dir, incarnation: rebuildTestIncarnation,
		inputs: []rebuildInput{input}, recipes: map[string]rebuildDigest{"go": recipe},
	}
	root, err := rebuildInputRoot(job.inputs)
	require.NoError(t, err)
	digest, err := hex.DecodeString(root)
	require.NoError(t, err)
	job.witness = store_pebble.Witness{
		Input: store_pebble.HeadDigest(digest), Recipe: store_pebble.HeadDigest{8},
		Profile: store_pebble.FullProfile, Stage: store_pebble.StageManifest,
	}
	symbol := &graph.Node{ID: file + "::use", Kind: graph.KindFunction, Name: "use", FilePath: file, Language: "go"}
	builtin := &graph.Node{ID: "builtin::go::len", Kind: graph.KindBuiltin, Name: "len", Language: "go"}
	store := rebuildParseStore{dir: t.TempDir(), limit: 1 << 20}
	prepared := []byte("x")
	payload := encodeRebuildTestParseFacts(t, store_pebble.ParseFacts{
		File:  graph.FileMetaRow{FilePath: file, Size: len(prepared), ContentHash: contentHashForSource(prepared), NodeCount: 2},
		Nodes: []*graph.Node{symbol, builtin}, EffectiveLanguage: "go",
	})
	key := input.parseKey(idx.rebuildExtractorRecipe(recipe, input))
	header := makeRebuildParseEntryHeader(key, payload)
	_, err = checkPendingRebuildReply(header[:], payload, key, file, input.Language, prepared)
	require.ErrorIs(t, err, errParseEntryCorrupt)
	require.ErrorContains(t, err, "explicit builtin node")
	require.NoError(t, store.write(key, payload)) // A checked cache hit must refuse too.

	var offered []store_pebble.Stage
	_, err = idx.buildRebuildFileStages(ctx, nil, store, build, job, func(tables store_pebble.StageTables) error {
		offered = append(offered, tables.Witness.Stage)
		return nil
	})
	require.ErrorIs(t, err, errParseEntryCorrupt)
	require.ErrorContains(t, err, `explicit builtin node "builtin::go::len"`)
	require.Equal(t, []store_pebble.Stage{store_pebble.StageManifest}, offered)
	for _, id := range []string{symbol.ID, builtin.ID} {
		n, err := build.RebuildView().Node(ctx, id)
		require.NoError(t, err)
		require.Nil(t, n)
	}
}
