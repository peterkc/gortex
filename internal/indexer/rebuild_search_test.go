package indexer

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"go.uber.org/zap"
)

func TestRebuildSearchRendersFinalFacts(t *testing.T) {
	ctx := t.Context()
	const repo, path = "repo", "repo/workbench/call.go"
	build, err := store_pebble.OpenBuild(filepath.Join(t.TempDir(), "working"), store_pebble.FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, build.Close()) })
	head, err := store_pebble.OpenHead(filepath.Join(t.TempDir(), "head"), rebuildTestIncarnation, store_pebble.FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, head.Close()) })
	manifest, err := store_pebble.PrepareManifest(ctx, t.TempDir(), []store_pebble.ManifestFile{{Path: path, Kind: 'f', Language: "go"}}, store_pebble.Witness{
		Input: store_pebble.HeadDigest{1}, Recipe: store_pebble.HeadDigest{2}, Stage: store_pebble.StageManifest, Profile: store_pebble.FullProfile,
		PlatformCatalogRevision: graph.PlatformCatalogRevision,
	}, rebuildTestIncarnation)
	require.NoError(t, err)
	_, err = head.PublishStage(ctx, manifest)
	require.NoError(t, err)

	decl := &graph.Node{ID: path + "::Call", Name: "Call", Kind: graph.KindFunction, QualName: "oldqualifier", FilePath: path, RepoPrefix: repo, Language: "go"}
	nodes := []*graph.Node{decl,
		{ID: path, Kind: graph.KindFile, Name: "noise", FilePath: path, RepoPrefix: repo, Language: "go"},
		{ID: "import", Kind: graph.KindImport, Name: "noise", RepoPrefix: repo, Language: "go"},
		{ID: "local", Kind: graph.KindLocal, Name: "noise", RepoPrefix: repo, Language: "go"},
		{ID: "skip", Kind: graph.KindFunction, Name: "noise", RepoPrefix: repo, Language: "yaml"},
		{ID: "content", Kind: graph.KindDoc, Name: "noise", RepoPrefix: repo, Language: "markdown", Meta: map[string]any{"data_class": "content"}},
		{ID: "prose", Kind: graph.KindDoc, Name: "proseanchor", RepoPrefix: repo, Language: "markdown"},
	}
	require.NoError(t, build.AddParsedFile(repo, store_pebble.ParseFacts{
		Nodes: nodes, File: graph.FileMetaRow{FilePath: path, NodeCount: len(nodes)},
		Symbols: []graph.SymbolFTSItem{{NodeID: decl.ID, Tokens: "receiptstale"}},
	}))
	fileGraph, err := build.PrepareFileGraph(ctx, t.TempDir(), manifest.Witness, rebuildTestIncarnation)
	require.NoError(t, err)
	retained, err := build.RetainFileGraph(ctx, fileGraph)
	require.NoError(t, err)
	_, err = head.PublishStage(ctx, fileGraph)
	require.NoError(t, err)
	linked := *decl
	linked.QualName = "linkedqualifier"
	global := &graph.Node{ID: "external-call::dep::example.org/lib", Kind: graph.KindModule, Name: "lib", QualName: "globalanchor", Language: "go"}
	builtinID := graph.StubID(repo, graph.StubKindBuiltin, "go", "len")
	call := &graph.Edge{From: decl.ID, To: builtinID, Kind: graph.EdgeCalls, FilePath: path, Line: 2,
		Meta: map[string]any{graph.MetaLinkStatus: string(graph.LinkStatusProbablePlatform)}}
	build.AddBatch([]*graph.Node{global, &linked}, []*graph.Edge{call})
	relations, err := build.PrepareRelations(ctx, t.TempDir(), retained, func(file string) string {
		if file == path {
			return "go"
		}
		return ""
	})
	require.NoError(t, err)
	digest, err := head.PublishStage(ctx, relations)
	require.NoError(t, err)
	derived, err := store_pebble.PrepareDeferredDerived(t.TempDir(), relations)
	require.NoError(t, err)
	require.Equal(t, digest, derived.Base)
	_, err = head.PublishStage(ctx, derived)
	require.NoError(t, err)

	idx := &Indexer{config: config.IndexConfig{
		IndexProse: true,
		SkipSearch: []config.SkipEmbedRule{{Language: "yaml", Kinds: []string{"function"}}},
	}}
	stage, err := idx.prepareRebuildSearch(ctx, build, t.TempDir(), derived.Witness, rebuildTestIncarnation, "workbench")
	require.NoError(t, err)
	_, err = head.PublishStage(ctx, stage)
	require.NoError(t, err)
	snap := head.OpenSnapshot()
	t.Cleanup(func() { require.NoError(t, snap.Close()) })
	require.Equal(t, []*graph.Edge{call}, snap.GetInEdges(builtinID))
	require.Nil(t, snap.GetNode(builtinID))
	for _, tc := range []struct{ term, id string }{
		{"linkedqualifier", decl.ID}, {"globalanchor", global.ID}, {"proseanchor", "prose"},
		{"oldqualifier", ""}, {"receiptstale", ""}, {"workbench", ""}, {"noise", ""}, {"len", ""},
	} {
		t.Run(tc.term, func(t *testing.T) {
			// Two terms bypass the tier-0 exact-name lookup.
			hits, err := snap.SearchSymbolsRepoScopedContext(ctx, tc.term+" "+tc.term, []string{repo}, 10)
			require.NoError(t, err)
			if tc.id == "" {
				require.Empty(t, hits)
				return
			}
			require.Len(t, hits, 1)
			require.Equal(t, tc.id, hits[0].NodeID)
		})
	}
}

// Preserve the raw named-repo baseline split; do not make direct mode the
// oracle for membership of final unowned resolver nodes.
func TestRebuildSearchNativeColdMembership(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module example.org/app\n")
	writeFile(t, filepath.Join(root, "main.go"), `package app
import "example.org/library"
func Caller() { library.Hello() }
`)
	const repo = "repo"
	const caller = repo + "/main.go::Caller"
	const external = "external-call::dep::example.org/library"
	for _, mode := range []struct {
		name, shadowMaxFiles string
		wantGlobal           bool
	}{{"shadow", "1000000", true}, {"direct", "0", false}} {
		t.Run(mode.name, func(t *testing.T) {
			t.Setenv("GORTEX_SHADOW_MAX_FILES", mode.shadowMaxFiles)
			t.Setenv("GORTEX_SHADOW_MAX_BYTES", "1073741824")
			t.Setenv("GORTEX_STREAMING_FLUSH", "0")
			s := newFTSStore(t)
			cfg := config.Default()
			cfg.Index.Workers = 2
			cfg.Semantic.Enabled = false // D2: keep providers out of the native cold comparison.
			reg := parser.NewRegistry()
			reg.Register(languages.NewGoExtractor())
			idx := New(s, reg, cfg.Index, zap.NewNop())
			t.Cleanup(idx.Close)
			idx.SetRepoPrefix(repo)
			require.False(t, cfg.Semantic.Enabled)
			require.NoError(t, idx.checkRebuildProvidersOff())
			_, err := idx.IndexCtx(t.Context(), root)
			require.NoError(t, err)
			node := s.GetNode(external)
			require.NotNil(t, node, "both modes must synthesize the external node")
			require.Empty(t, node.RepoPrefix)
			rows, next, err := s.SymbolFTSGenerationRows(t.Context(), 0, "", 0, 100)
			require.NoError(t, err)
			require.Zero(t, next, "log the entire saved corpus")
			byID, counts := make(map[string]string), make(map[graph.NodeKind]int)
			for _, r := range rows {
				n := s.GetNode(r.NodeID)
				require.NotNil(t, n)
				require.Equal(t, n.RepoPrefix, r.RepoPrefix)
				byID[r.NodeID] = r.Tokens
				counts[n.Kind]++
			}
			var ids []string
			for id := range byID {
				ids = append(ids, id)
			}
			slices.Sort(ids)
			t.Logf("saved symbol corpus (%d): IDs=%q counts by kind=%v", len(rows), ids, counts)
			require.NotEmpty(t, byID[caller])
			_, savedGlobal := byID[external]
			require.Equal(t, mode.wantGlobal, savedGlobal, "global synthetic module FTS membership")
			if mode.wantGlobal {
				require.True(t, strings.Contains(byID[external], "library"))
			}
			for _, scope := range [][]string{nil, {repo}} {
				hits, err := s.SearchSymbolsRepoScopedContext(t.Context(), "library library", scope, 20)
				require.NoError(t, err)
				t.Logf("raw external module ranks scope=%v hits=%v", scope, hits)
				require.Equal(t, mode.wantGlobal, slices.ContainsFunc(hits, func(h graph.SymbolHit) bool { return h.NodeID == external }))
			}
		})
	}
}
