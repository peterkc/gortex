package indexer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/parser/languages"
)

func TestRebuildEarlyContractIDsNativeCuts(t *testing.T) {
	ctx, root := t.Context(), t.TempDir()
	nativeGraph := graph.New()
	idx := newTestIndexer(nativeGraph)
	idx.SetRootPath(root)
	idx.registry.Register(languages.NewPythonExtractor())
	t.Cleanup(idx.Close)
	build, err := store_pebble.OpenBuild(t.TempDir(), store_pebble.FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, build.Close()) })
	store := rebuildParseStore{dir: t.TempDir(), limit: 16 << 20}
	job := rebuildFileStages{recipes: map[string]rebuildDigest{"go": {1}, "python": {2}}}
	census := rebuildExtractorCensus{byPath: make(map[string]string)}

	type sourceFile struct {
		path, language, body string
	}
	files := []sourceFile{
		{path: "routes.go", language: "go", body: "package app\nfunc Register(r Router) { r.GET(Route, Handle) }\n"},
		{path: "constants.go", language: "go", body: "package app\nconst Route = \"/items\"\n"},
		{path: "goframe.go", language: "go", body: "package app\ntype CreateReq struct { g.Meta `path:\"/goframe\" method:\"POST\"` }\n"},
		{path: "users.py", language: "python", body: `from fastapi import APIRouter
router = APIRouter(prefix="/users")
@router.get("/{id}")
def get_user(id: int):
    return id
`},
		{path: "main.py", language: "python", body: `from fastapi import FastAPI
from users import router
app = FastAPI()
app.include_router(router, prefix="/api")
`},
	}
	for i := range files {
		f := &files[i]
		src := []byte(f.body)
		require.NoError(t, os.WriteFile(filepath.Join(root, f.path), src, 0o600))
		ext, ok := idx.registry.GetByLanguage(f.language)
		require.True(t, ok)
		parsed, facts, err := idx.extractPreparedRebuildFileWithContractFacts(ctx, f.path, ext, src, src)
		require.NoError(t, err)
		payload, err := store_pebble.EncodeContractParseFacts(parsed, facts)
		require.NoError(t, err)
		input := rebuildInput{Path: f.path, Language: f.language, Size: int64(len(src))}
		recipe := idx.rebuildExtractorRecipe(job.recipes[input.Language], input)
		require.NoError(t, store.write(input.parseKey(recipe), payload))
		job.inputs = append(job.inputs, input)
		census.byPath[f.path] = parsed.EffectiveLanguage
		require.NoError(t, build.AddParsedFile("", parsed))
		nativeGraph.AddBatch(parsed.Nodes, parsed.Edges)
		require.NoError(t, nativeGraph.BulkSetConstantValues("", parsed.Constants))
	}

	gomod := []byte("module example.com/app\n\nrequire example.com/lib v1.0.0\n")
	require.NoError(t, os.WriteFile(filepath.Join(root, "go.mod"), gomod, 0o600))
	manifest := rebuildGoManifest{Path: "go.mod"}
	require.NoError(t, manifest.captureContractDeps(gomod))
	f, err := idx.readRebuildEarlyContractFacts(ctx, build.RebuildView(), job, store, &census, manifest.DirectDeps)
	require.NoError(t, err)
	_, byLang := idx.buildPerFileContractExtractors()
	native := contracts.NewRegistry()
	var nativeRecords []contracts.Contract
	for _, file := range files {
		src := []byte(file.body)
		tree := contracts.ParseTreeForLang(file.language, src)
		nodes, edges := idx.contractGraphFrontier([]string{file.path})
		records := idx.runContractExtractorsForFile(file.path, src, nodes[file.path], edges[file.path], byLang[file.language], tree)
		tree.Release()
		native.AddAllScoped(records, "", "", "")
		nativeRecords = append(nativeRecords, records...)
	}
	idx.extractGoModContracts(native)
	nativeRecords = append(nativeRecords, native.ByFile("go.mod")...)
	reader := rebuildContractReceiptReader{idx: idx, job: job, store: store, census: &census}
	var replayed []contracts.Contract
	require.NoError(t, os.Remove(filepath.Join(root, "go.mod")))
	require.NoError(t, reader.visitLateContractSourcesAtParseCut(ctx, build.RebuildView(), manifest, func(c contracts.Contract) error {
		replayed = append(replayed, c)
		return nil
	}))
	require.Equal(t, nativeRecords, replayed)
	for _, root := range []rebuildGoManifest{{}, {Path: "go.mod", DirectDeps: manifest.DirectDeps}} {
		err := reader.visitLateContractSourcesAtParseCut(ctx, build.RebuildView(), root, func(contracts.Contract) error {
			t.Fatal("unproved root returned source records")
			return nil
		})
		require.ErrorIs(t, err, errRebuildGoEvidence)
	}

	// The DI marker is present at the post-link cut, not in the parse facts.
	di := &graph.Edge{From: "routes.go", To: "Token", Kind: graph.EdgeConsumes, FilePath: "routes.go",
		Meta: map[string]any{"via": "@Inject", graph.MetaDIToken: "Token"}}
	nativeGraph.AddEdge(di)
	build.AddEdge(di)
	idx.extractDIContracts(native)
	const goframe = "route::goframe::POST::/goframe"
	parseRoute := nativeGraph.GetNode(goframe)
	require.NotNil(t, parseRoute, "GoFrame really emitted a parse-time route")
	require.Equal(t, graph.KindContract, parseRoute.Kind)
	require.Equal(t, "go", parseRoute.Language)
	idx.commitContracts(native)
	want := make(rebuildEarlyContractIDs)
	for n := range nativeGraph.NodesByKind(graph.KindContract) {
		if n.Language == "contract" {
			want[n.ID] = n.FilePath
		}
	}
	require.NotContains(t, want, goframe)
	require.Equal(t, rebuildEarlyContractIDs{
		"dep::example.com/lib":       "go.mod",
		"di::Token":                  "routes.go",
		"http::GET::/items":          "routes.go",
		"http::GET::/api/users/{p1}": "users.py",
	}, want)
	got, err := f.finish(ctx, build.RebuildView(), "", "", "")
	require.NoError(t, err)
	require.Equal(t, want, got)
	p := rebuildClonePredecessors{contracts: got}
	for id := range want {
		require.True(t, p.mayReplaceWithContract(id), id)
	}
	require.False(t, p.mayReplaceWithContract("http::GET::/{p1}"))
	row, err := build.RebuildView().Node(ctx, "http::GET::/api/users/{p1}")
	require.NoError(t, err)
	require.Nil(t, row)

	require.NoError(t, build.BeginRebuildContractOverlay(ctx, got))
	for id := range want {
		row, err := build.RebuildView().Node(ctx, id)
		require.NoError(t, err)
		nativeRow := *nativeGraph.GetNode(id)
		// Full contract records are materialized by the later Contracts stage.
		// The framework cut needs node identity and no displaced role stamps.
		nativeRow.Meta = nil
		require.Equal(t, &nativeRow, row)
	}
	liveRoute, err := build.RebuildView().Node(ctx, goframe)
	require.NoError(t, err)
	require.NotNil(t, liveRoute)
	require.Equal(t, "go", liveRoute.Language)
	require.NoError(t, build.EndRebuildContractOverlay(ctx))
}

func TestRebuildEarlyContractIDsBuiltEmpty(t *testing.T) {
	build, err := store_pebble.OpenBuild(t.TempDir(), store_pebble.FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, build.Close()) })
	f := rebuildEarlyContractFacts{registry: contracts.NewRegistry()}
	ids, err := f.finish(t.Context(), build.RebuildView(), "", "", "")
	require.NoError(t, err)
	require.NotNil(t, ids)
	require.Empty(t, ids)
	p := rebuildClonePredecessors{contracts: ids}
	require.False(t, p.mayReplaceWithContract("http::GET::/none"))
	require.True(t, (rebuildClonePredecessors{}).mayReplaceWithContract("http::GET::/none"))
}
