package indexer

import (
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

func TestRebuildContractProducerNativeSeal(t *testing.T) {
	const route, partial, env, bean = "http::POST::/items", "http::POST::/partial", "env::PY_KEEP", "di::Service"
	for _, spillAt := range []int{1, 1 << 20} {
		t.Run(fmt.Sprint(spillAt), func(t *testing.T) {
			ctx, root, lease := t.Context(), t.TempDir(), t.TempDir()
			g := graph.New()
			idx := newTestIndexer(g)
			idx.SetRootPath(root)
			idx.registry.Register(languages.NewJavaExtractor())
			idx.registry.Register(languages.NewPythonExtractor())
			t.Cleanup(idx.Close)
			b, err := store_pebble.OpenBuild(t.TempDir(), store_pebble.FullProfile)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, b.Close()) })
			head, err := store_pebble.OpenHead(t.TempDir(), rebuildTestIncarnation, store_pebble.FullProfile)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, head.Close()) })
			var envSrc strings.Builder
			envSrc.WriteString("package p\nimport \"os\"\nfunc Read() {\n")
			for i := range 80 {
				fmt.Fprintf(&envSrc, " _ = os.Getenv(\"V%03d%s\")\n", i, strings.Repeat("X", 8192))
			}
			envSrc.WriteString("}\n")
			sources := []struct{ file, language, text string }{
				{"Bean.java", "java", `package p;
@Configuration
class Bean {
    @Bean
    public Service make() { return null; }
}
class Client {
    public Client(Service service) {}
}
class Service {}
`},
				{"deferred.py", "python", `from fastapi import APIRouter
import os
router = APIRouter()
class Payload:
    id: str
@router.post('/partial', response_model=Payload)
def partial(payload: Payload) -> Payload:
    return payload

def read_env():
    return os.getenv("PY_KEEP")
`},
				{"env.go", "go", envSrc.String()},
				{"handler.go", "go", `package p
import "net/http"
type Payload struct {
 ID string
}
func Serve(w http.ResponseWriter, r *http.Request) {
 WriteJSON(w, http.StatusCreated, Payload{ID: "p"})
}
`},
				{"routes.go", "go", `package p
import "net/http"
func Register(m *http.ServeMux) {
 m.HandleFunc("POST /items", Serve)
}
`},
			}
			store := rebuildParseStore{dir: t.TempDir(), limit: 16 << 20}
			job := rebuildFileStages{recipes: map[string]rebuildDigest{"go": {1}, "java": {2}, "python": {3}}}
			census := &rebuildExtractorCensus{byPath: map[string]string{}, originByGraphPath: map[string]string{}}
			var manifestFiles []store_pebble.ManifestFile
			native := contracts.NewRegistry()
			for _, src := range sources {
				raw := []byte(src.text)
				require.NoError(t, os.WriteFile(filepath.Join(root, src.file), raw, 0o600))
				ext, ok := idx.registry.GetByLanguage(src.language)
				require.True(t, ok)
				parsed, facts, err := idx.extractPreparedRebuildFileWithContractFacts(ctx, src.file, ext, raw, raw)
				require.NoError(t, err)
				input := rebuildInput{Path: src.file, Language: src.language, Size: int64(len(raw))}
				wire, err := store_pebble.EncodeContractParseFacts(parsed, facts)
				require.NoError(t, err)
				require.NoError(t, store.write(input.parseKey(idx.rebuildExtractorRecipe(job.recipes[input.Language], input)), wire))
				job.inputs = append(job.inputs, input)
				census.admitted++
				census.byPath[src.file], census.originByGraphPath[src.file] = parsed.EffectiveLanguage, src.file
				manifestFiles = append(manifestFiles, store_pebble.ManifestFile{Path: src.file, Language: src.language, Kind: 'f', Size: input.Size})
				require.NoError(t, b.AddParsedFile("", parsed))
				g.AddBatch(parsed.Nodes, parsed.Edges)
				native.AddAllScoped(cloneContractRecords(facts.Records), "", "", "")
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
			r := rebuildContractReceiptReader{idx: idx, job: job, store: store, census: census}
			earlyFacts, err := idx.readRebuildEarlyContractFacts(ctx, b.RebuildView(), job, store, census, nil, nil)
			require.NoError(t, err)
			idx.extractDIContracts(native)
			idx.commitContracts(native)
			wantIDs := make(rebuildEarlyContractIDs)
			for n := range g.NodesByKind(graph.KindContract) {
				if n.Language == "contract" {
					wantIDs[n.ID] = n.FilePath
				}
			}
			for _, id := range []string{route, partial, env, bean} {
				require.Contains(t, wantIDs, id)
			}
			require.NotEmpty(t, native.ByID(partial)[0].Meta["response_type"], "deferred payload has a linked field consumer")
			const typeID = "handler.go::Payload"
			wantShape, ok := g.GetNode(typeID).Meta["shape"].(*contracts.Shape)
			require.True(t, ok)
			require.NotNil(t, wantShape)
			wantDeferredShape, ok := g.GetNode("deferred.py::Payload").Meta["shape"].(*contracts.Shape)
			require.True(t, ok, "native shape exists even though its record is omitted")
			require.NotNil(t, wantDeferredShape)
			var nativeCalls []*graph.Edge
			for _, e := range g.GetOutEdges("Bean.java::Client") {
				if e.Kind == graph.EdgeCalls && e.Meta["via"] == "spring.Bean" {
					nativeCalls = append(nativeCalls, e)
				}
			}
			require.NotEmpty(t, nativeCalls)
			for _, src := range sources {
				require.NoError(t, os.Remove(filepath.Join(root, src.file)))
			}
			parsedBefore := parser.ParseCount()
			var tables store_pebble.StageTables
			stats, err := r.withRebuildContractSources(ctx, b, lease, rebuildGoManifest{Path: "go.mod"}, spillAt, func(read func(func(contracts.Contract) error) error) error {
				early, err := earlyFacts.finish(ctx, b.RebuildView(), "", "", "")
				if err != nil {
					return err
				}
				require.Equal(t, wantIDs, early)
				return r.withRebuildContractProducer(ctx, b, read, early, func(preflight iter.Seq2[store_pebble.RebuildContractBatch, error], seal func(string, store_pebble.RelationsBase) (store_pebble.StageTables, error)) error {
					for range 2 {
						var nodes, edges, shapes int
						for batch, err := range preflight {
							require.NoError(t, err)
							nodes += len(batch.Nodes)
							edges += len(batch.Edges)
							shapes += len(batch.Shapes)
							for _, n := range batch.Nodes {
								require.NotEqual(t, "changed detached row", n.ID)
								n.ID = "changed detached row"
							}
						}
						require.Positive(t, nodes)
						require.Positive(t, edges)
						require.Positive(t, shapes)
					}
					require.NoError(t, b.BeginRebuildContractOverlay(ctx, early))
					require.NoError(t, b.EndRebuildContractOverlay(ctx))
					relations, err := b.PrepareRelations(ctx, t.TempDir(), fileBase, r.extractorOf)
					require.NoError(t, err)
					relations.Witness.Relations = &store_pebble.RelationCoverage{DeferredLanguages: []string{"java", "python"}}
					base, err := b.RetainRelations(ctx, fileBase, relations)
					require.NoError(t, err)
					_, err = head.PublishStage(ctx, relations)
					require.NoError(t, err)
					tables, err = seal(t.TempDir(), base)
					if err == nil {
						for batch, err := range preflight {
							require.Zero(t, batch)
							require.ErrorIs(t, err, errRebuildContractIDs)
						}
					}
					return err
				})
			})
			require.NoError(t, err)
			require.Equal(t, parsedBefore, parser.ParseCount())
			require.Positive(t, stats.SpillRuns)
			require.Positive(t, stats.SpillBytes)
			t.Logf("source spill: threshold=%d records=%d runs=%d bytes=%d peakBatch=%d", spillAt, stats.Records, stats.SpillRuns, stats.SpillBytes, stats.PeakBatchBytes)
			require.EqualValues(t, 1, tables.Witness.Contracts.OmittedRecords)
			_, err = head.PublishStage(ctx, tables)
			require.NoError(t, err)
			snap := head.OpenSnapshot()
			defer snap.Close()
			delete(wantIDs, partial)
			actual := make(rebuildEarlyContractIDs)
			for n := range snap.NodesByKind(graph.KindContract) {
				if n.Language != "contract" {
					continue
				}
				actual[n.ID] = n.FilePath
				require.Equal(t, g.GetNode(n.ID), n)
				var gotEdges, wantEdges []*graph.Edge
				gotEdges = append(gotEdges, snap.GetInEdges(n.ID)...)
				wantEdges = append(wantEdges, g.GetInEdges(n.ID)...)
				require.ElementsMatch(t, wantEdges, gotEdges)
			}
			require.Equal(t, wantIDs, actual)
			require.Nil(t, snap.GetNode(partial))
			shapeRows, err := snap.ContractShapesContext(ctx, []string{typeID, "deferred.py::Payload"})
			require.NoError(t, err)
			require.Equal(t, map[string]*contracts.Shape{typeID: wantShape, "deferred.py::Payload": wantDeferredShape}, shapeRows)
			require.NotContains(t, snap.GetNode(typeID).Meta, "shape")
			var actualCalls []*graph.Edge
			for _, e := range snap.GetOutEdges("Bean.java::Client") {
				if e.Kind == graph.EdgeCalls && e.Meta["via"] == "spring.Bean" {
					actualCalls = append(actualCalls, e)
				}
			}
			require.ElementsMatch(t, nativeCalls, actualCalls)
			partialScope, err := snap.ContractCoverageContext(ctx, contracts.ReadScope{ContractIDs: []string{partial}}, true)
			require.True(t, partialScope)
			require.ErrorIs(t, err, contracts.ErrContractCoverageIncomplete)
		})
	}
}

func TestRebuildContractProducerRefusesUnbuiltAndUnsealed(t *testing.T) {
	ctx := t.Context()
	idx := newTestIndexer(graph.New())
	idx.SetRootPath(t.TempDir())
	t.Cleanup(idx.Close)
	b, err := store_pebble.OpenBuild(t.TempDir(), store_pebble.FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	r := rebuildContractReceiptReader{idx: idx, census: &rebuildExtractorCensus{
		byPath: map[string]string{}, originByGraphPath: map[string]string{},
	}}
	source := func(func(contracts.Contract) error) error { return nil }
	unexpected := func(iter.Seq2[store_pebble.RebuildContractBatch, error], func(string, store_pebble.RelationsBase) (store_pebble.StageTables, error)) error {
		t.Fatal("unproved complete IDs reached the native tail")
		return nil
	}
	require.ErrorIs(t, r.withRebuildContractProducer(ctx, b, source, nil, unexpected), errRebuildContractIDs)
	require.ErrorIs(t, r.withRebuildContractProducer(ctx, b, source, rebuildEarlyContractIDs{"env::MISSING": "a.go"}, unexpected), errRebuildContractIDs)
	var escaped func(string, store_pebble.RelationsBase) (store_pebble.StageTables, error)
	var escapedPreflight iter.Seq2[store_pebble.RebuildContractBatch, error]
	err = r.withRebuildContractProducer(ctx, b, source, make(rebuildEarlyContractIDs), func(preflight iter.Seq2[store_pebble.RebuildContractBatch, error], seal func(string, store_pebble.RelationsBase) (store_pebble.StageTables, error)) error {
		escaped = seal
		escapedPreflight = preflight
		for _, err := range preflight {
			require.NoError(t, err)
			t.Fatal("built empty producer returned a row")
		}
		return nil
	})
	require.ErrorIs(t, err, errRebuildContractIDs)
	require.ErrorContains(t, err, "was not sealed")
	for batch, err := range escapedPreflight {
		require.Zero(t, batch)
		require.ErrorIs(t, err, errRebuildContractIDs)
	}
	stage, err := escaped(t.TempDir(), store_pebble.RelationsBase{})
	require.Zero(t, stage)
	require.ErrorIs(t, err, errRebuildContractIDs)
}

func TestRebuildContractUnprovedShapeRefLabelsTypeFile(t *testing.T) {
	ctx := t.Context()
	b, err := store_pebble.OpenBuild(t.TempDir(), store_pebble.FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	idx := newTestIndexer(graph.New())
	t.Cleanup(idx.Close)
	const file, typeFile, typeID = "routes.go", "body.go", "body.go::Payload"
	src := []byte("package p\nfunc Register() {}\nfunc ZRegister() {}\n")
	ext, ok := idx.registry.GetByLanguage("go")
	require.True(t, ok)
	parsed, facts, err := idx.extractPreparedRebuildFileWithContractFacts(ctx, file, ext, src, src)
	require.NoError(t, err)
	input := rebuildInput{Path: file, Language: "go", Size: int64(len(src))}
	job := rebuildFileStages{inputs: []rebuildInput{input}, recipes: map[string]rebuildDigest{"go": {1}}}
	store := rebuildParseStore{dir: t.TempDir(), limit: 1 << 20}
	wire, err := store_pebble.EncodeContractParseFacts(parsed, facts)
	require.NoError(t, err)
	require.NoError(t, store.write(input.parseKey(idx.rebuildExtractorRecipe(job.recipes[input.Language], input)), wire))
	r := rebuildContractReceiptReader{idx: idx, job: job, store: store, census: &rebuildExtractorCensus{
		byPath: map[string]string{file: "go"}, originByGraphPath: map[string]string{file: file},
	}}
	require.NoError(t, b.AddParsedFile("", parsed))
	n := &graph.Node{ID: typeID, Name: "Payload", Kind: graph.KindType, FilePath: typeFile, Language: "go", StartLine: 1}
	b.AddNode(n)
	const nonType, provedType = "body.go::Helper", "proved.go::Payload"
	b.AddBatch([]*graph.Node{
		{ID: nonType, Kind: graph.KindFunction, FilePath: typeFile, StartLine: 1},
		{ID: provedType, Kind: graph.KindType, FilePath: "proved.go", StartLine: 1},
	}, nil)
	c := contracts.Contract{ID: "http::GET::/x", Type: contracts.ContractHTTP, Role: contracts.RoleProvider,
		SymbolID: file + "::Register", FilePath: file, Meta: map[string]any{"response_type": typeID, "request_type": nonType}}
	proved := c
	proved.SymbolID = file + "::ZRegister"
	proved.Meta = map[string]any{"response_type": provedType}
	require.NoError(t, b.WithRebuildContractRecords(ctx, func(records *store_pebble.RebuildContractRecords) error {
		require.NoError(t, records.Add(store_pebble.RebuildContractRecord{Contract: c, BodyUnproved: true}))
		require.NoError(t, records.Add(store_pebble.RebuildContractRecord{Contract: proved}))
		return records.WithContractSelection(func(s *store_pebble.RebuildContractSelection) error {
			require.NoError(t, s.FreezeCompleteIDs())
			var omitted []store_pebble.RebuildContractOmission
			require.NoError(t, r.selectRebuildShapeRefs(ctx, b.RebuildView(), records, s, func(batch store_pebble.RebuildContractBatch) error {
				omitted = append(omitted, batch.Omitted...)
				return nil
			}))
			var refs []string
			for id, err := range s.ShapeRefs() {
				require.NoError(t, err)
				refs = append(refs, id)
			}
			require.Equal(t, []string{provedType}, refs, "proved sibling survives an omitted canonical winner")
			require.Len(t, omitted, 1)
			require.Equal(t, typeFile, omitted[0].FilePath)
			require.Equal(t, c.ID, omitted[0].ID)
			require.NotEqual(t, rebuildContractRecordOmission(c).RecordKey, omitted[0].RecordKey)
			return nil
		})
	}))
}
