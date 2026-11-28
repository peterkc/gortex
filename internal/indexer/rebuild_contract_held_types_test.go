package indexer

import (
	"iter"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/parser/languages"
)

func TestRebuildContractHeldTypeCoverage(t *testing.T) {
	ctx := t.Context()
	g := graph.New()
	idx := newTestIndexer(g)
	idx.SetRootPath(t.TempDir())
	idx.registry.Register(languages.NewPythonExtractor())
	t.Cleanup(idx.Close)
	b, err := store_pebble.OpenBuild(t.TempDir(), store_pebble.FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	head, err := store_pebble.OpenHead(t.TempDir(), rebuildTestIncarnation, store_pebble.FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, head.Close()) })
	store := rebuildParseStore{dir: t.TempDir(), limit: 1 << 20}
	job := rebuildFileStages{recipes: map[string]rebuildDigest{"go": {1}, "python": {2}}}
	census := &rebuildExtractorCensus{byPath: map[string]string{}, originByGraphPath: map[string]string{}}
	var manifestFiles []store_pebble.ManifestFile
	native := contracts.NewRegistry()
	add := func(file, lang, text string, rows ...contracts.Contract) {
		t.Helper()
		src := []byte(text)
		require.NoError(t, os.WriteFile(filepath.Join(idx.rootPath, file), src, 0o600))
		ext, ok := idx.registry.GetByLanguage(lang)
		require.True(t, ok)
		base, facts, err := idx.extractPreparedRebuildFileWithContractFacts(ctx, file, ext, src, src)
		require.NoError(t, err)
		require.Empty(t, facts.Records)
		require.Empty(t, facts.Questions)
		facts.Records = rows
		g.AddBatch(base.Nodes, base.Edges)
		native.AddAllScoped(cloneContractRecords(rows), "", "", "")
		input := rebuildInput{Path: file, Language: lang, Size: int64(len(src))}
		wire, err := store_pebble.EncodeContractParseFacts(base, facts)
		require.NoError(t, err)
		require.NoError(t, store.write(input.parseKey(idx.rebuildExtractorRecipe(job.recipes[lang], input)), wire))
		job.inputs = append(job.inputs, input)
		census.admitted++
		census.byPath[file], census.originByGraphPath[file] = base.EffectiveLanguage, file
		manifestFiles = append(manifestFiles, store_pebble.ManifestFile{Path: file, Language: lang, Kind: 'f', Size: input.Size})
		if lang == "go" {
			require.NoError(t, b.AddParsedFile("", base))
		} else {
			require.NoError(t, b.AddRebuildDeferredFile("", base, func(string) (bool, error) { return true, nil }))
		}
	}
	const heldFile, heldModels, liveModels, selected = "held.py", "a_models.py", "live.go", "selected.go"
	provider := func(file, route, field, value string) contracts.Contract {
		return contracts.Contract{ID: "http::GET::/" + route, FilePath: file, Type: contracts.ContractHTTP, Role: contracts.RoleProvider,
			Line: 1, Meta: map[string]any{field: value}}
	}
	bareMissing := provider("bare_missing.go", "bare-missing", "response_type", "HeldType")
	bareShared := provider("bare_shared.go", "bare-shared", "response_type", "BareShared")
	call := func(file, route, name string) contracts.Contract {
		c := provider(file, route, "response_expr", "result")
		c.SymbolID = file + "::" + name
		return c
	}
	callMissing := call("call_missing.go", "call-missing", "ServeMissing")
	callShared := call("call_shared.go", "call-shared", "ServeShared")
	callHeld := call("call_held.go", "call-held", "ServeHeld")
	kept := provider(selected, "kept", "response_type", selected+"::SharedModel")
	add(bareMissing.FilePath, "go", "package p\n", bareMissing)
	add(bareShared.FilePath, "go", "package p\ntype BareShared struct{ ID string }\n", bareShared)
	add(callHeld.FilePath, "go", `package p
func ServeHeld() { result := ReadHeld(); _ = result }
`, callHeld)
	add(callMissing.FilePath, "go", `package p
func ReadMissing() CallMissing { panic("") }
func ServeMissing() { result := ReadMissing(); _ = result }
`, callMissing)
	add(callShared.FilePath, "go", `package p
type CallShared struct{ ID string }
func ReadShared() CallShared { return CallShared{} }
func ServeShared() { result := ReadShared(); _ = result }
`, callShared)
	add(heldFile, "python", "pass\n",
		provider(heldFile, "held-full", "response_type", liveModels+"::FullModel"),
		provider(heldFile, "held-bare", "response_type", "BareModel"),
		provider(heldFile, "held-py", "response_type", "PyOnly"),
		provider(heldFile, "held-ambiguous", "response_type", "BareShared"),
		provider(heldFile, "held-covered", "response_type", kept.Meta["response_type"].(string)),
	)
	add(liveModels, "go", "package p\ntype FullModel struct{ ID string }\ntype BareModel struct{ ID string }\n")
	add(heldModels, "python", `class HeldType: pass
class BareShared: pass
class CallMissing: pass
class CallShared: pass
class PyOnly: pass
def ReadHeld(): pass
`)
	add(selected, "go", "package p\ntype SharedModel struct{ ID string }\n", kept)

	idx.upgradeContractBareTypeRefs(native)
	require.Equal(t, heldModels+"::HeldType", native.ByID(bareMissing.ID)[0].Meta["response_type"])
	require.Equal(t, "BareShared", native.ByID(bareShared.ID)[0].Meta["response_type"])
	require.Equal(t, liveModels+"::BareModel", native.ByID("http::GET::/held-bare")[0].Meta["response_type"])
	require.Equal(t, heldModels+"::PyOnly", native.ByID("http::GET::/held-py")[0].Meta["response_type"])
	idx.resolveCallReturnTypes(native)
	require.Equal(t, heldModels+"::CallMissing", native.ByID(callMissing.ID)[0].Meta["response_type"])
	for _, input := range job.inputs {
		require.NoError(t, os.Remove(filepath.Join(idx.rootPath, input.Path)))
	}

	manifest, err := store_pebble.PrepareManifest(ctx, t.TempDir(), manifestFiles, store_pebble.Witness{
		Input: store_pebble.HeadDigest{1}, Recipe: store_pebble.HeadDigest{2}, Stage: store_pebble.StageManifest,
		Profile: store_pebble.FullProfile, PlatformCatalogRevision: graph.PlatformCatalogRevision,
	}, rebuildTestIncarnation)
	require.NoError(t, err)
	_, err = head.PublishStage(ctx, manifest)
	require.NoError(t, err)
	files, err := b.PrepareFileGraph(ctx, t.TempDir(), manifest.Witness, rebuildTestIncarnation)
	require.NoError(t, err)
	fileBase, err := b.RetainFileGraph(ctx, files)
	require.NoError(t, err)
	_, err = head.PublishStage(ctx, files)
	require.NoError(t, err)
	require.NoError(t, b.IsolateRebuildLinks(ctx))
	view := b.RebuildView()
	r := rebuildContractReceiptReader{idx: idx, job: job, store: store, census: census, linkView: view,
		markIsolationAffected: func(file string, kind contracts.ContractType, language string) error {
			return b.MarkRebuildContractIsolationAffected(ctx, "", file, kind, language)
		},
		visitIsolationAffected: func(yield func(store_pebble.RebuildContractBatch) error) error {
			return b.VisitRebuildContractIsolationAffected(ctx, yield)
		},
	}
	f, err := idx.readRebuildEarlyContractFacts(ctx, view, job, store, census, nil, nil)
	require.NoError(t, err)
	early, err := f.finish(ctx, view, "", "", "")
	require.NoError(t, err)
	var tables store_pebble.StageTables
	_, err = r.withRebuildContractSources(ctx, b, t.TempDir(), rebuildGoManifest{Path: "go.mod"}, 1, func(read func(func(contracts.Contract) error) error) error {
		return r.withRebuildContractProducer(ctx, b, read, early, func(preflight iter.Seq2[store_pebble.RebuildContractBatch, error], seal func(string, store_pebble.RelationsBase) (store_pebble.StageTables, error)) error {
			var omitted []string
			for batch, err := range preflight {
				require.NoError(t, err)
				for _, row := range batch.Omitted {
					require.Equal(t, contracts.ContractHTTP, row.Type)
					omitted = append(omitted, row.FilePath+":"+row.ID)
				}
			}
			require.ElementsMatch(t, []string{
				bareMissing.FilePath + ":", bareShared.FilePath + ":", callHeld.FilePath + ":", callMissing.FilePath + ":", callShared.FilePath + ":",
				heldFile + ":http::GET::/held-full", heldFile + ":http::GET::/held-bare", heldFile + ":http::GET::/held-py",
				heldFile + ":http::GET::/held-ambiguous", heldFile + ":http::GET::/held-covered",
				liveModels + ":http::GET::/held-full", liveModels + ":http::GET::/held-bare", heldModels + ":http::GET::/held-py",
			}, omitted)
			require.NoError(t, b.RestoreRebuildDeclarations(ctx))
			relations, err := b.PrepareRelations(ctx, t.TempDir(), fileBase, r.extractorOf)
			require.NoError(t, err)
			relations.Witness.Relations = &store_pebble.RelationCoverage{DeferredLanguages: []string{"python"}}
			base, err := b.RetainRelations(ctx, fileBase, relations)
			require.NoError(t, err)
			_, err = head.PublishStage(ctx, relations)
			require.NoError(t, err)
			tables, err = seal(t.TempDir(), base)
			return err
		})
	})
	require.NoError(t, err)
	require.EqualValues(t, 13, tables.Witness.Contracts.OmittedRecords)
	_, err = head.PublishStage(ctx, tables)
	require.NoError(t, err)
	snap := head.OpenSnapshot()
	defer snap.Close()
	for _, file := range []string{bareMissing.FilePath, bareShared.FilePath, callHeld.FilePath, callMissing.FilePath, callShared.FilePath, liveModels, heldModels} {
		partial, err := snap.ContractCoverageContext(ctx, contracts.ReadScope{FilePaths: []string{file}, Types: []contracts.ContractType{contracts.ContractHTTP}}, true)
		require.ErrorIs(t, err, contracts.ErrContractCoverageIncomplete, file)
		require.True(t, partial, file)
	}
	partial, err := snap.ContractCoverageContext(ctx, contracts.ReadScope{FilePaths: []string{selected}, Types: []contracts.ContractType{contracts.ContractHTTP}}, true)
	require.NoError(t, err)
	require.False(t, partial)
	for _, tc := range []struct {
		c    contracts.Contract
		want string
	}{
		{bareMissing, "HeldType"},
		{bareShared, bareShared.FilePath + "::BareShared"},
		{callHeld, ""},
		{callMissing, "CallMissing"},
		{callShared, callShared.FilePath + "::CallShared"},
	} {
		n := snap.GetNode(tc.c.ID)
		require.NotNil(t, n, tc.c.ID)
		meta, ok := n.Meta["contract_meta"].(map[string]any)
		require.True(t, ok)
		got, _ := meta["response_type"].(string)
		require.Equal(t, tc.want, got, tc.c.ID)
	}
	shapes, err := snap.ContractShapesContext(ctx, []string{liveModels + "::FullModel", liveModels + "::BareModel", heldModels + "::PyOnly", selected + "::SharedModel"})
	require.NoError(t, err)
	require.Len(t, shapes, 1)
	require.NotNil(t, shapes[selected+"::SharedModel"])
}
