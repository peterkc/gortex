package indexer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/parser"
)

func TestRebuildCallReturnBindingsMatchNative(t *testing.T) {
	ctx := t.Context()
	const file = "handler.go"
	src := []byte(`package fixture

type Item struct{ ID string }

func Read() ([]*Item, error) { return nil, nil }

func Serve() {
    items, err := Read()
    note := "ok"
    _ = items
    _ = err
    _ = note
}

func Other() {
    note := 1
    _ = note
}
`)
	g := graph.New()
	idx := newTestIndexer(g)
	root := t.TempDir()
	idx.SetRootPath(root)
	t.Cleanup(idx.Close)
	path := filepath.Join(root, file)
	require.NoError(t, os.WriteFile(path, src, 0o600))
	ext, ok := idx.registry.GetByLanguage("go")
	require.True(t, ok)
	parsed, facts, err := idx.extractPreparedRebuildFileWithContractFacts(ctx, file, ext, src, src)
	require.NoError(t, err)
	g.AddBatch(parsed.Nodes, parsed.Edges)
	const id = "http::GET::/items"
	registration := func(path, symbol, expr string, line int) contracts.Contract {
		return contracts.Contract{ID: id, Type: contracts.ContractHTTP, Role: contracts.RoleProvider, FilePath: path, SymbolID: file + "::" + symbol,
			Line: line, Meta: map[string]any{"response_expr": expr, "schema_source": "partial"}}
	}
	first := registration("routes_a.go", "Serve", "items", 10)
	first.Meta["response_envelope"] = []map[string]any{
		{"name": "items", "expr": "&items"}, {"name": "note", "expr": "note"},
		{"name": "unresolved", "expr": "absent"}, {"name": "existing", "expr": "note", "type": "mixed"},
	}
	inputs := []contracts.Contract{first, registration("routes_a.go", "Serve", "hidden", 11),
		registration("routes_b.go", "Serve", "absent", 12), registration("routes_c.go", "Other", "note", 13)}
	native := contracts.NewRegistry()
	for _, c := range cloneContractRecords(inputs) {
		native.Add(c)
	}
	idx.resolveCallReturnTypes(native)
	want := native.ByID(id)
	require.Len(t, want, len(inputs))
	for _, c := range want[:3] {
		require.Equal(t, file+"::Item", c.Meta["response_type"])
		require.Equal(t, true, c.Meta["response_repeated"])
		require.NotContains(t, c.Meta, "response_expr")
	}
	env := want[0].Meta["response_envelope"].([]map[string]any)
	require.Equal(t, file+"::Item", env[0]["type"])
	require.Equal(t, true, env[0]["repeated"])
	require.Equal(t, "string", env[1]["type"])
	require.NotContains(t, env[2], "type")
	require.Equal(t, "mixed", env[3]["type"])
	require.Equal(t, "int", want[3].Meta["response_type"])

	input := rebuildInput{Path: file, Language: "go", Size: int64(len(src))}
	job := rebuildFileStages{inputs: []rebuildInput{input}, recipes: map[string]rebuildDigest{"go": {1}}}
	store := rebuildParseStore{dir: t.TempDir(), limit: 1 << 20}
	wire, err := store_pebble.EncodeContractParseFacts(parsed, facts)
	require.NoError(t, err)
	key := input.parseKey(idx.rebuildExtractorRecipe(job.recipes[input.Language], input))
	require.NoError(t, store.write(key, wire))
	reader := rebuildContractReceiptReader{idx: idx, job: job, store: store,
		census: &rebuildExtractorCensus{byPath: map[string]string{file: "go"}, originByGraphPath: map[string]string{file: file}}}
	b, err := store_pebble.OpenBuild(t.TempDir(), store_pebble.FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	require.NoError(t, b.AddParsedFile("", parsed))
	require.NoError(t, os.Remove(path))
	parses := parser.ParseCount()
	replay := func(expected []contracts.Contract, wantErr error) error {
		return b.WithRebuildContractRecords(ctx, func(records *store_pebble.RebuildContractRecords) error {
			for i, c := range inputs {
				require.NoError(t, records.Add(store_pebble.RebuildContractRecord{Contract: c, BodyUnproved: i == 1}))
			}
			err := reader.resolveLateCallReturnTypes(ctx, b.RebuildView(), records)
			if wantErr == nil {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, wantErr)
			}
			var actual []contracts.Contract
			require.NoError(t, records.VisitByID(id, func(item store_pebble.RebuildContractRecord) error {
				actual = append(actual, item.Contract)
				require.Equal(t, item.Line == 11, item.BodyUnproved)
				return nil
			}))
			require.Equal(t, expected, actual)
			return nil
		})
	}
	require.NoError(t, replay(want, nil))
	require.NoError(t, os.Remove(store.entry(key)))
	require.NoError(t, replay(inputs, errParseEntryCorrupt))
	require.Equal(t, parses, parser.ParseCount())
}
