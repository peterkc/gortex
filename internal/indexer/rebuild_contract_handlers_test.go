package indexer

import (
	"fmt"
	"path"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/parser"
)

func TestRebuildProviderHandlerCandidateParity(t *testing.T) {
	ctx := t.Context()
	b, err := store_pebble.OpenBuild(t.TempDir(), store_pebble.FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	byName := make(map[string][]*graph.Node)
	add := func(repo, file, name string, kind graph.NodeKind) {
		id := file + "::" + name
		if file == "" {
			id = repo + "/" + name
		}
		n := &graph.Node{ID: id, Name: name, Kind: kind, RepoPrefix: repo, FilePath: file, Language: "go"}
		b.AddNode(n)
		byName[name] = append(byName[name], n)
	}
	add("r", "r/pkg/main.go", "Wrap", graph.KindFunction)
	add("r", "r/pkg/local.go", "Run", graph.KindMethod)
	add("z", "z/pkg/foreign.go", "Run", graph.KindMethod)
	add("r", "r/pkg/models.go", "Run", graph.KindType)
	for i := range 12 {
		add("r", fmt.Sprintf("r/other/file-%d.go", i), "Run", graph.KindFunction)
	}
	add("r", "r/a.go", "Tie", graph.KindFunction)
	add("r", "r/b.go", "Tie", graph.KindFunction)
	add("z", "z/pkg/foreign.go", "Foreign", graph.KindFunction)
	add("r", "", "Stub", graph.KindFunction)
	add("", "pkg/local.go", "Local", graph.KindFunction)
	add("z", "z/pkg/foreign.go", "Local", graph.KindFunction)
	idx := &Indexer{}
	reader := rebuildContractReceiptReader{}
	for _, tt := range []struct {
		name, trail, fallback, repo, file, want string
	}{
		{name: "same dir after many other candidates", trail: "Wrap(r.Run)", repo: "r", file: "r/pkg/routes.go", want: "r/pkg/local.go::Run"},
		{name: "innermost ambiguous keeps wrapper", trail: "Wrap(r.Tie)", repo: "r", file: "r/pkg/routes.go", want: "r/pkg/main.go::Wrap"},
		{name: "single fallback", fallback: "r.Wrap", repo: "r", file: "r/pkg/routes.go", want: "r/pkg/main.go::Wrap"},
		{name: "empty repo still uses directory", fallback: "Local", file: "pkg/routes.go", want: "pkg/local.go::Local"},
		{name: "foreign cannot own", trail: "Wrap(Foreign)", repo: "r", file: "r/pkg/routes.go"},
		{name: "pathless cannot own", trail: "Wrap(Stub)", repo: "r", file: "r/pkg/routes.go"},
		{name: "ambiguous keeps registration", trail: "Tie", repo: "r", file: "r/pkg/routes.go"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			native := idx.resolveInnermostHandlerFromCandidates(tt.trail, tt.fallback, tt.repo, path.Dir(tt.file), "", byName)
			if native != nil && (native.FilePath == "" || native.RepoPrefix != tt.repo) {
				native = nil
			}
			c := contracts.Contract{ID: "http::route", Type: contracts.ContractHTTP, Role: contracts.RoleProvider, RepoPrefix: tt.repo, FilePath: tt.file,
				Meta: map[string]any{"handler_trail": tt.trail, "handler_ident": tt.fallback}}
			got, err := reader.providerHandler(ctx, b.RebuildView(), c)
			require.NoError(t, err)
			if tt.want == "" {
				require.Nil(t, native)
				require.Nil(t, got)
				return
			}
			require.NotNil(t, native)
			require.NotNil(t, got)
			require.Equal(t, tt.want, native.ID)
			require.Equal(t, native.ID, got.ID)
		})
	}
}

func TestRebuildProviderHandlerCheckedLaravelClass(t *testing.T) {
	const file = "routes/api.php"
	ctx := t.Context()
	idx := newTestIndexer(graph.New())
	t.Cleanup(idx.Close)
	// Capture the actual alias while the accepted bytes are available. The
	// replay root has no source file, so the checked receipt is its only input.
	idx.SetRootPath(t.TempDir())
	src := []byte("<?php\nuse Acme\\OrderController as Orders;\n")
	route := contracts.Contract{ID: "http::route", Type: contracts.ContractHTTP, Role: contracts.RoleProvider, FilePath: file, Line: 3,
		Meta: map[string]any{"handler_ident": "index", "handler_class": "Orders"}}
	facts := &contracts.FileFacts{Records: []contracts.Contract{route}, Shapes: map[string]*contracts.Shape{}, Handlers: map[string]contracts.HandlerFacts{}}
	require.NoError(t, idx.captureRebuildContractBodies(ctx, file, "go", src, src, nil, &parser.ExtractionResult{}, false, facts))
	key, _ := contracts.LaravelHandlerClassKey(route)
	require.Equal(t, `Acme\OrderController`, facts.LaravelHandlerClasses[key])
	base := store_pebble.ParseFacts{File: graph.FileMetaRow{FilePath: file}, EffectiveLanguage: "go"}
	wire, err := store_pebble.EncodeContractParseFacts(base, facts)
	require.NoError(t, err)
	input := rebuildInput{Path: file, Language: "go", Size: int64(len(src))}
	job := rebuildFileStages{inputs: []rebuildInput{input}, recipes: map[string]rebuildDigest{"go": {1}}}
	store := rebuildParseStore{dir: t.TempDir(), limit: 1 << 20}
	require.NoError(t, store.write(input.parseKey(idx.rebuildExtractorRecipe(job.recipes[input.Language], input)), wire))
	reader := rebuildContractReceiptReader{idx: idx, job: job, store: store,
		census: &rebuildExtractorCensus{byPath: map[string]string{file: "go"}, originByGraphPath: map[string]string{file: file}}}
	b, err := store_pebble.OpenBuild(t.TempDir(), store_pebble.FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	const want = "app/Orders.php::OrderController.index"
	for _, row := range []struct{ file, ns string }{{"app/Orders.php", "Acme"}, {"routes/Orders.php", "Other"}} {
		id := row.file + "::OrderController"
		b.AddNode(&graph.Node{ID: id, Name: "OrderController", Kind: graph.KindType, FilePath: row.file, Language: "php",
			Meta: map[string]any{"scope_ns": row.ns}})
		b.AddNode(&graph.Node{ID: id + ".index", Name: "index", Kind: graph.KindMethod, FilePath: row.file, Language: "php"})
	}
	got, err := reader.providerHandler(ctx, b.RebuildView(), route)
	require.NoError(t, err)
	require.NotNil(t, got)
	require.Equal(t, want, got.ID)
	b.AddNode(&graph.Node{ID: "other.php::OrderController", Name: "OrderController", Kind: graph.KindType, FilePath: "other.php",
		Language: "php", Meta: map[string]any{"scope_ns": "Acme"}})
	got, err = reader.providerHandler(ctx, b.RebuildView(), route)
	require.NoError(t, err)
	require.Nil(t, got, "ambiguous controller does not fall back to a namesake action")
	route.Line++
	got, err = reader.providerHandler(ctx, b.RebuildView(), route)
	require.ErrorIs(t, err, graph.ErrContractProjectionStale)
	require.Nil(t, got)
	route.Line--
	reader.store.dir = t.TempDir()
	_, err = reader.providerHandler(ctx, b.RebuildView(), route)
	require.ErrorIs(t, err, errParseEntryCorrupt)
}
