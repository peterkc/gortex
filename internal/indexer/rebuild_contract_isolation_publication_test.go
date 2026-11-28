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

func TestRebuildContractIsolationPublishesAffectedRouteScope(t *testing.T) {
	const routes, held = "routes.go", "held.rb"
	const keepID, heldID = "http::GET::/keep", "http::ANY::/held"
	const heldPattern = "/held"
	const routeSource = `package p
import "net/http"
func Serve(w http.ResponseWriter, r *http.Request) {}
func Register(m *http.ServeMux) {
 m.HandleFunc("GET /keep", Serve)
 m.HandleFunc(Route, Serve)
}
`
	for _, tc := range []struct {
		name, heldName string
		omitted        uint64
	}{
		{"held name matches", "Route", 1},
		{"held name differs", "OtherRoute", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			g := graph.New()
			idx := newTestIndexer(g)
			idx.SetRootPath(t.TempDir())
			idx.registry.Register(languages.NewRubyExtractor())
			t.Cleanup(idx.Close)
			b, err := store_pebble.OpenBuild(t.TempDir(), store_pebble.FullProfile)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, b.Close()) })
			head, err := store_pebble.OpenHead(t.TempDir(), rebuildTestIncarnation, store_pebble.FullProfile)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, head.Close()) })

			// Given real route facts and a held declaration. Seed its value at
			// the receipt boundary because Ruby does not emit constant values.
			store := rebuildParseStore{dir: t.TempDir(), limit: 1 << 20}
			job := rebuildFileStages{recipes: map[string]rebuildDigest{"go": {1}, "ruby": {2}}}
			census := &rebuildExtractorCensus{byPath: map[string]string{}, originByGraphPath: map[string]string{}}
			var manifestFiles []store_pebble.ManifestFile
			for _, src := range []struct{ file, lang, text string }{
				{held, "ruby", tc.heldName + ` = "` + heldPattern + `"` + "\n"},
				{routes, "go", routeSource},
			} {
				raw := []byte(src.text)
				require.NoError(t, os.WriteFile(filepath.Join(idx.rootPath, src.file), raw, 0o600))
				ext, ok := idx.registry.GetByLanguage(src.lang)
				require.True(t, ok)
				parsed, facts, err := idx.extractPreparedRebuildFileWithContractFacts(ctx, src.file, ext, raw, raw)
				require.NoError(t, err)
				g.AddBatch(parsed.Nodes, parsed.Edges)
				if src.file == held {
					constID := held + "::" + tc.heldName
					n := g.GetNode(constID)
					require.NotNil(t, n)
					require.Equal(t, graph.KindConstant, n.Kind)
					parsed.Constants = []graph.ConstantValueRow{{NodeID: constID, FilePath: held, Value: heldPattern}}
					require.NoError(t, g.BulkSetConstantValues("", parsed.Constants))
					require.Empty(t, facts.Records)
					require.Empty(t, facts.Questions)
					require.NoError(t, b.AddRebuildDeferredFile("", parsed, func(string) (bool, error) { return true, nil }))
				} else {
					require.Len(t, facts.Questions, 1)
					require.Equal(t, contracts.EndpointGoRoute, facts.Questions[0].Kind)
					require.NoError(t, b.AddParsedFile("", parsed))
				}
				input := rebuildInput{Path: src.file, Language: src.lang, Size: int64(len(raw))}
				wire, err := store_pebble.EncodeContractParseFacts(parsed, facts)
				require.NoError(t, err)
				require.NoError(t, store.write(input.parseKey(idx.rebuildExtractorRecipe(job.recipes[src.lang], input)), wire))
				job.inputs = append(job.inputs, input)
				census.admitted++
				census.byPath[src.file], census.originByGraphPath[src.file] = parsed.EffectiveLanguage, src.file
				manifestFiles = append(manifestFiles, store_pebble.ManifestFile{Path: src.file, Language: src.lang, Kind: 'f', Size: input.Size})
			}
			native := (&contracts.HTTPExtractor{}).ExtractWithStore(routes, []byte(routeSource), g.GetFileNodes(routes), nil, nil, g, "")
			var nativeIDs []string
			for _, c := range native {
				nativeIDs = append(nativeIDs, c.ID)
			}
			wantNative := []string{keepID}
			if tc.omitted != 0 {
				wantNative = append(wantNative, heldID)
			}
			require.ElementsMatch(t, wantNative, nativeIDs)

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

			// When early and late readers both see the isolated cut, the
			// producer must preserve its affected scope through restore and seal.
			require.NoError(t, b.IsolateRebuildLinks(ctx))
			view := b.RebuildView()
			var affected uint64
			r := rebuildContractReceiptReader{idx: idx, job: job, store: store, census: census, linkView: view,
				markIsolationAffected: func(file string, kind contracts.ContractType, language string) error {
					return b.MarkRebuildContractIsolationAffected(ctx, "", file, kind, language)
				},
				visitIsolationAffected: func(yield func(store_pebble.RebuildContractBatch) error) error {
					count, err := b.VisitRebuildContractIsolationAffected(ctx, yield)
					affected = count
					return err
				},
			}
			f, err := idx.readRebuildEarlyContractFacts(ctx, view, job, store, census, nil, r.markIsolationAffected)
			require.NoError(t, err)
			early, err := f.finish(ctx, view, "", "", "")
			require.NoError(t, err)
			var tables store_pebble.StageTables
			_, err = r.withRebuildContractSources(ctx, b, t.TempDir(), rebuildGoManifest{Path: "go.mod"}, 1, func(read func(func(contracts.Contract) error) error) error {
				return r.withRebuildContractProducer(ctx, b, read, early, func(_ iter.Seq2[store_pebble.RebuildContractBatch, error], seal func(string, store_pebble.RelationsBase) (store_pebble.StageTables, error)) error {
					require.NoError(t, b.RestoreRebuildDeclarations(ctx))
					relations, err := b.PrepareRelations(ctx, t.TempDir(), fileBase, r.extractorOf)
					require.NoError(t, err)
					relations.Witness.Relations = &store_pebble.RelationCoverage{DeferredLanguages: []string{"ruby"}}
					base, err := b.RetainRelations(ctx, fileBase, relations)
					require.NoError(t, err)
					_, err = head.PublishStage(ctx, relations)
					require.NoError(t, err)
					tables, err = seal(t.TempDir(), base)
					return err
				})
			})
			require.NoError(t, err)
			_, err = head.PublishStage(ctx, tables)
			require.NoError(t, err)

			// Then an unrelated name leaves native output complete. A matching
			// name leaves the literal route intact but makes absence unknowable.
			snap := head.OpenSnapshot()
			t.Cleanup(func() { require.NoError(t, snap.Close()) })
			witness, err := snap.Publication()
			require.NoError(t, err)
			require.Equal(t, store_pebble.StageContracts, witness.Stage)
			require.NotNil(t, witness.Contracts)
			require.Equal(t, tc.omitted, affected, "unique affected rows despite early and late reads")
			require.Equal(t, tc.omitted, witness.Contracts.OmittedRecords)
			published := make(map[string]string)
			for n := range snap.NodesByKind(graph.KindContract) {
				published[n.ID] = n.FilePath
			}
			require.Equal(t, map[string]string{keepID: routes}, published)
			node, err := snap.GetNodeContext(ctx, heldID)
			require.NoError(t, err)
			require.Nil(t, node)

			http := []contracts.ContractType{contracts.ContractHTTP}
			partial, err := snap.ContractCoverageContext(ctx, contracts.ReadScope{FilePaths: []string{routes}, Types: http}, false)
			require.NoError(t, err)
			require.Equal(t, tc.omitted != 0, partial)
			partial, err = snap.ContractCoverageContext(ctx, contracts.ReadScope{ContractIDs: []string{heldID}}, true)
			if tc.omitted != 0 {
				require.ErrorIs(t, err, contracts.ErrContractCoverageIncomplete)
				require.True(t, partial)
			} else {
				require.NoError(t, err)
				require.False(t, partial)
			}
			partial, err = snap.ContractCoverageContext(ctx, contracts.ReadScope{FilePaths: []string{held}, Types: http}, true)
			require.NoError(t, err)
			require.False(t, partial)
		})
	}
}
