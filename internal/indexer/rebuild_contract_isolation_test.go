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
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

func TestRebuildContractIsolationOmitsRawObservations(t *testing.T) {
	ctx := t.Context()
	idx := newTestIndexer(graph.New())
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
	const py, goFile = "a.py", "z.go"
	var manifestFiles []store_pebble.ManifestFile
	var heldKey rebuildDigest
	var heldBytes []byte
	fullNative := contracts.NewRegistry()
	for _, src := range []struct{ file, lang, text string }{
		{py, "python", `import os
from fastapi import APIRouter
router = APIRouter()
@router.get('/held')
def route():
    return os.getenv("SHARED")
def env_only():
    return os.getenv("HELD_ONLY")
`},
		{goFile, "go", `package p
import "os"
func Read() { _ = os.Getenv("SHARED") }
`},
	} {
		raw := []byte(src.text)
		name := filepath.Join(idx.rootPath, src.file)
		require.NoError(t, os.WriteFile(name, raw, 0o600))
		ext, ok := idx.registry.GetByLanguage(src.lang)
		require.True(t, ok)
		parsed, facts, err := idx.extractPreparedRebuildFileWithContractFacts(ctx, src.file, ext, raw, raw)
		require.NoError(t, err)
		input := rebuildInput{Path: src.file, Language: src.lang, Size: int64(len(raw))}
		wire, err := store_pebble.EncodeContractParseFacts(parsed, facts)
		require.NoError(t, err)
		key := input.parseKey(idx.rebuildExtractorRecipe(job.recipes[input.Language], input))
		require.NoError(t, store.write(key, wire))
		job.inputs = append(job.inputs, input)
		census.admitted++
		census.byPath[src.file], census.originByGraphPath[src.file] = parsed.EffectiveLanguage, src.file
		manifestFiles = append(manifestFiles, store_pebble.ManifestFile{Path: src.file, Language: src.lang, Kind: 'f', Size: input.Size})
		fullNative.AddAllScoped(cloneContractRecords(facts.Records), "", "", "")
		if src.file == py {
			heldKey, heldBytes = key, wire
			require.NoError(t, b.AddRebuildDeferredFile("", parsed, func(string) (bool, error) { return true, nil }))
		} else {
			require.NoError(t, b.AddParsedFile("", parsed))
		}
		require.NoError(t, os.Remove(name))
	}
	const shared, held = "env::SHARED", "env::HELD_ONLY"
	require.Len(t, fullNative.ByID(shared), 2)
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
	r := rebuildContractReceiptReader{idx: idx, job: job, store: store, census: census, isolate: true, linkView: view}
	parsedBefore := parser.ParseCount()
	// Even a missing held-out receipt cannot reach early/late link readers.
	// A separate checked inventory must, however, refuse that same miss.
	require.NoError(t, os.Remove(store.entry(heldKey)))
	earlyFacts, err := idx.readRebuildEarlyContractFacts(ctx, view, job, store, census, nil, true)
	require.NoError(t, err)
	early, err := earlyFacts.finish(ctx, view, "", "", "")
	require.NoError(t, err)
	require.Equal(t, rebuildEarlyContractIDs{shared: goFile}, early)
	var late []contracts.Contract
	require.NoError(t, r.visitLateContractSourcesAtParseCut(ctx, view, rebuildGoManifest{Path: "go.mod"}, func(c contracts.Contract) error {
		late = append(late, c)
		return nil
	}))
	require.Len(t, late, 1)
	require.Equal(t, shared, late[0].ID)
	r.proofCache = &rebuildContractReceiptCache{path: py, facts: &contracts.FileFacts{}}
	_, _, err = r.forGraphPath(ctx, py)
	require.ErrorIs(t, err, graph.ErrContractProjectionStale)
	require.ErrorIs(t, r.visitHeldOutContractOmissions(ctx, func(store_pebble.RebuildContractBatch) error { return nil }), errParseEntryCorrupt)
	require.NoError(t, store.write(heldKey, heldBytes))
	var omissions []store_pebble.RebuildContractOmission
	var tables store_pebble.StageTables
	_, err = r.withRebuildContractSources(ctx, b, t.TempDir(), rebuildGoManifest{Path: "go.mod"}, 1, func(read func(func(contracts.Contract) error) error) error {
		return r.withRebuildContractProducer(ctx, b, read, early, func(preflight iter.Seq2[store_pebble.RebuildContractBatch, error], seal func(string, store_pebble.RelationsBase) (store_pebble.StageTables, error)) error {
			for batch, err := range preflight {
				require.NoError(t, err)
				omissions = append(omissions, batch.Omitted...)
				for _, n := range batch.Nodes {
					require.Equal(t, shared, n.ID)
					require.Equal(t, goFile, n.FilePath)
				}
			}
			var sourceIDs []string
			var unknownRoute bool
			for _, o := range omissions {
				require.Equal(t, py, o.FilePath)
				if o.Type == contracts.ContractEnv {
					sourceIDs = append(sourceIDs, o.ID)
				}
				unknownRoute = unknownRoute || o.Type == contracts.ContractHTTP && o.ID == ""
			}
			require.ElementsMatch(t, []string{shared, held}, sourceIDs)
			require.True(t, unknownRoute)
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
	require.Equal(t, parsedBefore, parser.ParseCount())
	require.EqualValues(t, len(omissions), tables.Witness.Contracts.OmittedRecords)
	_, err = head.PublishStage(ctx, tables)
	require.NoError(t, err)
	snap := head.OpenSnapshot()
	defer snap.Close()
	require.Equal(t, goFile, snap.GetNode(shared).FilePath)
	require.Nil(t, snap.GetNode(held))
	require.Nil(t, snap.GetNode("http::GET::/held"))
	require.NotNil(t, snap.GetNode(py+"::route"))
}
