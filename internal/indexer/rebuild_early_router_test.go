package indexer

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/parser/languages"
)

func TestRebuildEarlyRouterHiddenBucketSibling(t *testing.T) {
	const own, mount, id = "routes.ts", "mount.ts", "http::GET::/x"
	for _, tc := range []struct {
		name, localPrefix string
		checkedMount      bool
		want              []string
	}{
		{name: "open bucket refuses unchecked mount", localPrefix: "/local"},
		{name: "open bucket uses checked mount", localPrefix: "/local", checkedMount: true, want: []string{"http::GET::/api/x", "http::GET::/local/x"}},
		{name: "closed bucket ignores hidden sibling", want: []string{id}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			build, err := store_pebble.OpenBuild(t.TempDir(), store_pebble.FullProfile)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, build.Close()) })
			for _, path := range []string{own, mount} {
				build.AddNode(&graph.Node{ID: path, Kind: graph.KindFile, FilePath: path, Language: "typescript"})
			}
			newRegistry := func() *contracts.Registry {
				reg := contracts.NewRegistry()
				for i, framework := range []string{"nestjs", "express"} {
					reg.Add(contracts.Contract{
						ID: id, Type: contracts.ContractHTTP, Role: contracts.RoleProvider, FilePath: own, Line: i + 1,
						Meta: map[string]any{"framework": framework, "method": "GET", "path": "/x"},
					})
				}
				return reg
			}
			reg := newRegistry()
			require.Len(t, reg.All(), 1, "the second record cannot open the bucket")
			require.Len(t, reg.ByID(id), 2)
			facts := map[string]contracts.RouterFacts{
				own: {LineCount: 2, Lines: []contracts.RouterLineFacts{
					{Line: 1, NestPrefix: tc.localPrefix},
					{Line: 2, Receivers: map[string]string{"express": "router"}},
				}},
			}
			if tc.checkedMount {
				facts[mount] = contracts.RouterFileFacts(mount, []byte("app.use('/api', router)"))
			}
			overlaps, err := joinRebuildRouterPrefixes(ctx, build.RebuildView(), reg, "", "", facts)
			require.Empty(t, overlaps)
			if tc.want == nil {
				require.ErrorIs(t, err, errRebuildRouterFactsMissing)
				require.ErrorContains(t, err, `"mount.ts"`)
				require.Equal(t, []string{id}, reg.AllIDs())
				require.Len(t, reg.ByID(id), 2, "refusal does not mutate the registry")
				return
			}
			require.NoError(t, err)
			native := newRegistry()
			contracts.JoinRouterPrefixFacts(native, facts)
			records := reg.All()
			require.ElementsMatch(t, native.All(), records)
			ids := make([]string, 0, len(records))
			for _, c := range records {
				ids = append(ids, c.ID)
			}
			require.ElementsMatch(t, tc.want, ids)
			if tc.checkedMount {
				require.Empty(t, reg.ByID(id), "the old map key holds no records")
			}
		})
	}
}

func TestRebuildRouterHiddenOwnFile(t *testing.T) {
	const own, id = "open.ts", "http::GET::/x"
	for _, tc := range []struct {
		name, hidden, language string
		checked, joinedRep     bool
		wantMissing            string
	}{
		{name: "outside census", hidden: "hidden.java", language: "java"},
		{name: "outside census with facts", hidden: "hidden.java", language: "java", checked: true},
		{name: "inside census checked", hidden: "z-hidden.ts", language: "typescript", checked: true},
		{name: "inside census unchecked", hidden: "z-hidden.ts", language: "typescript", wantMissing: "z-hidden.ts"},
		{name: "selected by joined representative", hidden: "hidden.java", language: "java", joinedRep: true, wantMissing: "hidden.java"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			build, err := store_pebble.OpenBuild(t.TempDir(), store_pebble.FullProfile)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, build.Close()) })
			// The smallest unchecked census path is not the hidden sibling.
			// Neither local-only route can read its shared facts.
			const missingMount = "a-missing.ts"
			for path, language := range map[string]string{own: "typescript", missingMount: "typescript", tc.hidden: tc.language} {
				build.AddNode(&graph.Node{ID: path, Kind: graph.KindFile, FilePath: path, Language: language})
			}
			record := func(path, framework string, line int) contracts.Contract {
				return contracts.Contract{ID: id, Type: contracts.ContractHTTP, Role: contracts.RoleProvider, FilePath: path, Line: line,
					Meta: map[string]any{"framework": framework, "method": "GET", "path": "/x"}}
			}
			first := record(own, "nestjs", 1)
			hiddenRep := record(tc.hidden, "unsupported", 1)
			if tc.joinedRep {
				hiddenRep.Meta["framework"] = "spring"
				hiddenRep.Meta["route_prefix_joined"] = true
			}
			hidden := record(tc.hidden, "spring", 2)
			inputs := []contracts.Contract{first, hiddenRep, hidden}
			newRegistry := func() *contracts.Registry {
				reg := contracts.NewRegistry()
				for _, c := range inputs {
					reg.Add(c)
				}
				return reg
			}
			reg := newRegistry()
			require.ElementsMatch(t, []contracts.Contract{first, hiddenRep}, reg.All())
			require.Equal(t, inputs, reg.ByID(id))
			sources := map[string][]byte{
				own:       []byte("@Controller('local')"),
				tc.hidden: []byte("@RequestMapping(\"/hidden\")\n@GetMapping(\"/x\")"),
			}
			// Keep the representative Nest prefix local (no controller class).
			available := map[string]contracts.RouterFacts{own: contracts.RouterFileFacts(own, sources[own])}
			if tc.checked {
				available[tc.hidden] = contracts.RouterFileFacts(tc.hidden, sources[tc.hidden])
			}
			selectedHidden := tc.language == "typescript" || tc.joinedRep
			scan := []string{own, missingMount}
			if tc.language == "typescript" {
				scan = append(scan, tc.hidden)
			}
			native := newRegistry()
			contracts.JoinRouterPrefixes(native, scan, func(path string) []byte {
				if !selectedHidden {
					require.NotEqual(t, tc.hidden, path)
				}
				return sources[path]
			})
			want := native
			if tc.wantMissing != "" {
				want = newRegistry()
			} else {
				require.Len(t, native.ByID("http::GET::/local/x"), 1)
				if selectedHidden {
					require.Len(t, native.ByID(id), 1)
					require.Len(t, native.ByID("http::GET::/hidden/x"), 1)
				} else {
					require.Equal(t, inputs[1:], native.ByID(id))
					require.Empty(t, native.ByID("http::GET::/hidden/x"))
				}
			}
			overlaps, err := joinRebuildRouterPrefixes(ctx, build.RebuildView(), reg, "", "", available)
			require.Empty(t, overlaps)
			if tc.wantMissing != "" {
				require.ErrorIs(t, err, errRebuildRouterFactsMissing)
				require.ErrorContains(t, err, tc.wantMissing)
				require.Equal(t, uint8(100), buildRefusal(err).Refusal)
			} else {
				require.NoError(t, err)
			}
			require.ElementsMatch(t, want.AllIDs(), reg.AllIDs())
			for _, bucket := range want.AllIDs() {
				require.Equal(t, want.ByID(bucket), reg.ByID(bucket))
			}
			require.NoError(t, build.WithRebuildContractRecords(ctx, func(records *store_pebble.RebuildContractRecords) error {
				for _, c := range inputs {
					require.NoError(t, records.Add(store_pebble.RebuildContractRecord{Contract: c}))
				}
				missing, err := records.JoinCheckedRouterPrefixes("", "", func(path string) (*contracts.RouterFacts, error) {
					if !selectedHidden {
						require.NotEqual(t, tc.hidden, path)
					}
					f, ok := available[path]
					if !ok {
						return nil, nil
					}
					return &f, nil
				}, func(path string) error {
					t.Fatalf("unexpected overlap %q", path)
					return nil
				})
				require.Equal(t, tc.wantMissing, missing)
				if tc.wantMissing != "" {
					require.ErrorContains(t, err, tc.wantMissing)
				} else {
					require.NoError(t, err)
				}
				var ids []string
				require.NoError(t, records.VisitIDs(func(bucket string) error {
					ids = append(ids, bucket)
					var actual []contracts.Contract
					require.NoError(t, records.VisitByID(bucket, func(item store_pebble.RebuildContractRecord) error {
						actual = append(actual, item.Contract)
						return nil
					}))
					require.Equal(t, want.ByID(bucket), actual)
					return nil
				}))
				require.ElementsMatch(t, want.AllIDs(), ids)
				return nil
			}))
		})
	}
}

// The two raw buckets rewrite /x -> /api/x -> /v2/api/x. Native
// application order can erase /api/x, involving both source files.
func rebuildRouterOverlapSources() []struct{ path, source string } {
	return []struct{ path, source string }{
		{"a.py", `from fastapi import APIRouter

a = APIRouter(prefix="/api")

@a.get("/x")
def first():
    pass
`},
		{"b.py", `from fastapi import APIRouter

b = APIRouter(prefix="/v2")

@b.get("/api/x")
def second():
    pass
`},
	}
}

func TestRebuildLateRouterOverlap(t *testing.T) {
	ctx := t.Context()
	idx := newTestIndexer(graph.New())
	t.Cleanup(idx.Close)
	idx.SetRootPath(t.TempDir())
	idx.registry.Register(languages.NewPythonExtractor())
	ext, ok := idx.registry.GetByLanguage("python")
	require.True(t, ok)
	build, err := store_pebble.OpenBuild(t.TempDir(), store_pebble.FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, build.Close()) })
	job := rebuildFileStages{recipes: map[string]rebuildDigest{"python": {1}}}
	store := rebuildParseStore{dir: t.TempDir(), limit: 1 << 20}
	census := &rebuildExtractorCensus{byPath: map[string]string{}, originByGraphPath: map[string]string{}}
	var raw []contracts.Contract
	for _, file := range rebuildRouterOverlapSources() {
		src := []byte(file.source)
		parsed, facts, err := idx.extractPreparedRebuildFileWithContractFacts(ctx, file.path, ext, src, src)
		require.NoError(t, err)
		require.Len(t, facts.Records, 1)
		require.NotNil(t, facts.Router)
		raw = append(raw, facts.Records...)
		for _, n := range parsed.Nodes {
			build.AddNode(n)
		}
		input := rebuildInput{Path: file.path, Language: "python", Size: int64(len(src))}
		job.inputs = append(job.inputs, input)
		census.byPath[file.path] = "python"
		census.originByGraphPath[file.path] = file.path
		wire, err := store_pebble.EncodeContractParseFacts(parsed, facts)
		require.NoError(t, err)
		key := input.parseKey(idx.rebuildExtractorRecipe(job.recipes[input.Language], input))
		require.NoError(t, store.write(key, wire))
	}
	require.Equal(t, "http::GET::/x", raw[0].ID)
	require.Equal(t, "http::GET::/api/x", raw[1].ID)
	reader := rebuildContractReceiptReader{idx: idx, job: job, store: store, census: census}
	require.NoError(t, build.WithRebuildContractRecords(ctx, func(records *store_pebble.RebuildContractRecords) error {
		for _, c := range raw {
			require.NoError(t, records.Add(store_pebble.RebuildContractRecord{Contract: c}))
		}
		var overlap []string
		require.NoError(t, reader.joinLateRouterPrefixes(ctx, records, func(path string) error {
			overlap = append(overlap, path)
			return nil
		}))
		require.Equal(t, []string{"a.py", "b.py"}, overlap)
		var got []contracts.Contract
		require.NoError(t, records.VisitRepresentatives(func(item store_pebble.RebuildContractRecord) error {
			got = append(got, item.Contract)
			return nil
		}))
		require.Equal(t, []contracts.Contract{
			contracts.RewriteRouterContract(raw[0], "/api"),
			contracts.RewriteRouterContract(raw[1], "/v2"),
		}, got)
		require.NoError(t, records.VisitByID("http::GET::/x", func(item store_pebble.RebuildContractRecord) error {
			t.Fatalf("unexpected old bucket row %v", item)
			return nil
		}))
		return nil
	}))
}
