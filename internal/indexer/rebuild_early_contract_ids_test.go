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
	idx.registry.Register(languages.NewTypeScriptExtractor())
	t.Cleanup(idx.Close)
	build, err := store_pebble.OpenBuild(t.TempDir(), store_pebble.FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, build.Close()) })
	store := rebuildParseStore{dir: t.TempDir(), limit: 16 << 20}
	job := rebuildFileStages{recipes: map[string]rebuildDigest{"go": {1}, "typescript": {2}}}
	census := rebuildExtractorCensus{byPath: make(map[string]string), originByGraphPath: make(map[string]string)}

	type sourceFile struct {
		path, language, body string
	}
	files := []sourceFile{
		{path: "routes.go", language: "go", body: "package app\nfunc Register(r Router) { r.GET(Route, Handle) }\n"},
		{path: "constants.go", language: "go", body: "package app\nconst Route = \"/items\"\n"},
		{path: "goframe.go", language: "go", body: "package app\ntype CreateReq struct { g.Meta `path:\"/goframe\" method:\"POST\"` }\n"},
		{path: "users.ts", language: "typescript", body: `import express from 'express';
const app = express();
const router = express.Router();
function user(req, res) {}
router.get("/users", user);
app.use("/api", router);
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
		census.admitted++
		census.byPath[f.path] = parsed.EffectiveLanguage
		census.originByGraphPath[f.path] = f.path
		require.NoError(t, build.AddParsedFile("", parsed))
		nativeGraph.AddBatch(parsed.Nodes, parsed.Edges)
		require.NoError(t, nativeGraph.BulkSetConstantValues("", parsed.Constants))
	}

	gomod := []byte("module example.com/app\n\nrequire example.com/lib v1.0.0\n")
	require.NoError(t, os.WriteFile(filepath.Join(root, "go.mod"), gomod, 0o600))
	manifest := rebuildGoManifest{Path: "go.mod"}
	require.NoError(t, manifest.captureContractDeps(gomod))
	require.NoError(t, build.IsolateRebuildLinks(ctx))
	f, err := idx.readRebuildEarlyContractFacts(ctx, build.RebuildView(), job, store, &census, manifest.DirectDeps, nil)
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
	stats, err := store_pebble.WithRebuildContractSources(ctx, t.TempDir(), 1,
		func(appendRecord func(contracts.Contract) error) error {
			return reader.visitLateContractSourcesAtParseCut(ctx, build.RebuildView(), manifest, appendRecord)
		},
		func(read func(func(contracts.Contract) error) error) error {
			return read(func(c contracts.Contract) error {
				replayed = append(replayed, c)
				return nil
			})
		})
	require.NoError(t, err)
	require.Positive(t, stats.SpillBytes)
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
		"http::GET::/api/users": "users.ts",
	}, want)
	got, err := f.finish(ctx, build.RebuildView(), "", "", "")
	require.NoError(t, err)
	require.Equal(t, want, got)
	p := rebuildClonePredecessors{contracts: got}
	for id := range want {
		require.True(t, p.mayReplaceWithContract(id), id)
	}
	require.False(t, p.mayReplaceWithContract("http::GET::/users"))
	row, err := build.RebuildView().Node(ctx, "http::GET::/api/users")
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

func TestRebuildEarlyRouterHandlerElection(t *testing.T) {
	for _, tc := range []struct {
		name, firstDecl          string
		wantMounted, skipHandler bool
		direct, unadmitted       bool
	}{
		{name: "handler_move_opens_bucket", firstDecl: "export function first(req, res) {}", wantMounted: true},
		{name: "unresolved_handler_keeps_bucket"},
		{name: "direct_express_calls", direct: true},
		{name: "unadmitted_handler_refuses", unadmitted: true},
		{name: "size_skipped_handler_still_moves", wantMounted: true, skipHandler: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, root := t.Context(), t.TempDir()
			g := graph.New()
			idx := newTestIndexer(g)
			idx.SetRootPath(root)
			idx.registry.Register(languages.NewTypeScriptExtractor())
			t.Cleanup(idx.Close)
			b, err := store_pebble.OpenBuild(t.TempDir(), store_pebble.FullProfile)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, b.Close()) })
			store := rebuildParseStore{dir: t.TempDir(), limit: 16 << 20}
			job := rebuildFileStages{recipes: map[string]rebuildDigest{"typescript": {1}}, classifications: make(map[string]rebuildClassificationFact)}
			census := rebuildExtractorCensus{byPath: map[string]string{}, originByGraphPath: map[string]string{}}
			handlerPath, handlerLanguage := "handlers.ts", "typescript"
			handlerSource := tc.firstDecl + "\nexport function second(req, res) {}\n"
			if tc.skipHandler {
				// A Go handler falls outside the TypeScript router census.
				handlerPath, handlerLanguage, handlerSource = "handlers.go", "go", "package handlers\nfunc first() {}\n"
			}
			// A state member call is still an Express route for the native
			// contract and mount patterns. Its root is not app/router, so
			// the parser does not give each line its own synthetic owner.
			routes := `import express from 'express';
const router = express.Router();
const state = { app: express(), router };
export function register() {
    state.app.get("/x", first);
    state.router.get("/x", second);
    state.app.use("/api", router);
}
`
			if tc.direct {
				routes = `import express from 'express';
const router = express.Router();
const app = express();
export function register() {
    app.get("/x", first);
    router.get("/x", second);
    app.use("/api", router);
}
`
			}
			sources := []struct{ path, language, text string }{
				{handlerPath, handlerLanguage, handlerSource},
				{"routes.ts", "typescript", routes},
			}
			native := contracts.NewRegistry()
			for _, file := range sources {
				src := []byte(file.text)
				require.NoError(t, os.WriteFile(filepath.Join(root, file.path), src, 0o600))
				ext, ok := idx.registry.GetByLanguage(file.language)
				require.True(t, ok)
				parsed, facts, err := idx.extractPreparedRebuildFileWithContractFacts(ctx, file.path, ext, src, src)
				require.NoError(t, err)
				g.AddBatch(parsed.Nodes, parsed.Edges)
				census.originByGraphPath[file.path] = file.path
				if tc.skipHandler && file.path == handlerPath {
					skip := rebuildClassificationFact{Language: file.language, Size: int64(len(src)), MaxFileSize: 1}
					job.classifications[file.path] = skip
					b.AddNode(rebuildCheckedSkipNode(file.path, skip))
					// Represent a live linked candidate from the skipped path.
					for _, n := range parsed.Nodes {
						if n.Kind == graph.KindFunction {
							b.AddNode(n)
						}
					}
					continue
				}
				payload, err := store_pebble.EncodeContractParseFacts(parsed, facts)
				require.NoError(t, err)
				input := rebuildInput{Path: file.path, Language: file.language, Size: int64(len(src))}
				require.NoError(t, store.write(input.parseKey(idx.rebuildExtractorRecipe(job.recipes[input.Language], input)), payload))
				job.inputs = append(job.inputs, input)
				census.byPath[file.path] = parsed.EffectiveLanguage
				require.NoError(t, b.AddParsedFile("", parsed))
				native.AddAllScoped(cloneContractRecords(facts.Records), "", "", "")
			}
			const raw, mounted = "http::GET::/x", "http::GET::/api/x"
			registrations := native.ByID(raw)
			require.Len(t, registrations, 2)
			if tc.direct {
				require.NotEqual(t, registrations[0].SymbolID, registrations[1].SymbolID)
				require.Len(t, native.All(), 2)
			} else {
				require.Equal(t, "routes.ts::register", registrations[0].SymbolID)
				require.Equal(t, registrations[0].SymbolID, registrations[1].SymbolID)
				require.NotEqual(t, registrations[0].Line, registrations[1].Line)
				require.Len(t, native.All(), 1, "the raw registry hides the mounted registration")
				require.Equal(t, "first", native.All()[0].Meta["handler_ident"])
			}
			f, err := idx.readRebuildEarlyContractFacts(ctx, b.RebuildView(), job, store, &census, nil, nil)
			require.NoError(t, err)
			idx.commitContracts(native)
			want := rebuildEarlyContractIDs{raw: "routes.ts"}
			if tc.wantMounted {
				want[mounted] = "routes.ts"
			}
			nativeIDs := make(rebuildEarlyContractIDs)
			for n := range g.NodesByKind(graph.KindContract) {
				if n.Language == "contract" {
					nativeIDs[n.ID] = n.FilePath
				}
			}
			if !tc.direct {
				require.Equal(t, want, nativeIDs)
			}
			for _, file := range sources {
				require.NoError(t, os.Remove(filepath.Join(root, file.path)))
			}
			if tc.skipHandler {
				require.NoError(t, b.WithRebuildContractRecords(ctx, func(records *store_pebble.RebuildContractRecords) error {
					for _, c := range cloneContractRecords(f.registry.ByID(raw)) {
						if c.Meta["handler_ident"] == "first" {
							c.Meta["response_expr"] = "payload"
						}
						require.NoError(t, records.Add(store_pebble.RebuildContractRecord{Contract: c}))
					}
					require.NoError(t, f.receipts.bindRebuildProviderHandlers(ctx, b.RebuildView(), records))
					moved := func() store_pebble.RebuildContractRecord {
						var rows []store_pebble.RebuildContractRecord
						require.NoError(t, records.VisitByID(raw, func(item store_pebble.RebuildContractRecord) error {
							if item.SymbolID == handlerPath+"::first" {
								rows = append(rows, item)
							}
							return nil
						}))
						require.Len(t, rows, 1)
						return rows[0]
					}
					before := moved()
					require.True(t, before.BodyUnproved)
					require.Equal(t, []string{"payload"}, unresolvedContractBindingNames(before.Contract))
					require.NoError(t, f.receipts.resolveLateCallReturnTypes(ctx, b.RebuildView(), records))
					require.Equal(t, before, moved())
					return nil
				}))
			}
			if tc.unadmitted {
				b.AddNode(&graph.Node{ID: "outside.go::first", FilePath: "outside.go", Name: "first", Kind: graph.KindFunction, Language: "go"})
				_, err := f.finish(ctx, b.RebuildView(), "", "", "")
				require.ErrorIs(t, err, graph.ErrContractProjectionStale)
				require.ErrorContains(t, err, `no admitted router handler source for "outside.go::first"`)
				return
			}
			got, err := f.finish(ctx, b.RebuildView(), "", "", "")
			require.NoError(t, err)
			require.Equal(t, nativeIDs, got)
			require.NoError(t, b.BeginRebuildContractOverlay(ctx, got))
			n, err := b.RebuildView().Node(ctx, mounted)
			require.NoError(t, err)
			_, nativeMounted := nativeIDs[mounted]
			require.Equal(t, nativeMounted, n != nil)
			require.NoError(t, b.EndRebuildContractOverlay(ctx))
		})
	}
}
