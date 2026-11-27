package resolver

import (
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

const (
	rebuildSQLCallsiteName   = "get_user_stats"
	rebuildSQLCallsiteStub   = "unresolved::sqlfn::get_user_stats"
	rebuildSQLCallsiteTarget = "db/functions.sql::public.get_user_stats"
	rebuildSQLCallsiteTS     = `export function load() {
  return supabase.rpc('get_user_stats', { uid: 1 });
}
function get_user_stats() {}
`
)

func TestRebuildSQLCallsiteSQLiteOracle(t *testing.T) {
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthSQLCallsite})
	require.NoError(t, err)
	for _, tc := range []struct {
		file, source string
		extractor    parser.Extractor
	}{
		{"app.js", "function load() {\n  return supabase.rpc('get_user_stats');\n}\n", languages.NewJavaScriptExtractor()},
		{"app.ts", rebuildSQLCallsiteTS, languages.NewTypeScriptExtractor()},
		{"App.vue", `<script lang="ts">` + rebuildSQLCallsiteTS + `</script>`, languages.NewVueExtractor()},
		{"app.py", "def load(db):\n    return db.execute(select(func.get_user_stats(1)))\n", languages.NewPythonExtractor()},
	} {
		t.Run(tc.extractor.Language(), func(t *testing.T) {
			app := rebuildSQLCallsiteExtract(t, tc.extractor, tc.file, tc.source)
			sql := rebuildSQLCallsiteFunction(t, "db/functions.sql", "public.get_user_stats")
			b, native := openRebuildEventStores(t, app, sql)
			frozen, err := FreezeRebuildFrameworkSelection(t.Context(), b.RebuildView(), selection)
			require.NoError(t, err)
			require.True(t, frozen.allows(SynthSQLCallsite))
			before := rebuildEventSQLiteRows(native)
			require.Equal(t, before, rebuildEventSavedRows(t, b))
			stub := rebuildSQLCallsiteEdge(t, before.edges, tc.file)
			require.Equal(t, rebuildSQLCallsiteStub, stub.To)
			require.Equal(t, graph.OriginASTInferred, stub.Origin)
			require.Equal(t, map[string]any{"via": "sql.callsite", "sql_function": rebuildSQLCallsiteName}, stub.Meta)

			landed := *stub
			landed.To = rebuildSQLCallsiteTarget
			landed.Confidence = 0.6
			landed.ConfidenceLabel = "INFERRED"
			landed.Meta = maps.Clone(stub.Meta)
			landed.Meta[MetaSynthesizedBy] = SynthSQLCallsite
			landed.Meta[MetaProvenance] = ProvenanceHeuristic
			want := rebuildSQLCallsiteWithEdge(t, before, stub, &landed)

			// The raw Call and its enclosing file prove the site. The caller
			// declaration is not a candidate for SQL target selection.
			receipt := app
			receipt.Nodes = slices.DeleteFunc(slices.Clone(app.Nodes), func(n *graph.Node) bool { return n.ID == stub.From })
			require.Len(t, receipt.Nodes, len(app.Nodes)-1)
			receipt.File.NodeCount = len(receipt.Nodes)
			receipt = roundTripRebuildRNBridgeFacts(t, receipt)

			for _, moves := range []uint64{1, 0} {
				report := RunFrameworkSynthesizersWithSelection(native, selection)
				require.NoError(t, report.Err)
				require.Equal(t, 1, report.Total)
				require.Equal(t, want, rebuildEventSQLiteRows(native))
				stats, err := RunRebuildSQLCallsites(t.Context(), b, rebuildGRPCReceipts(receipt, sql), frozen)
				require.NoError(t, err)
				require.Equal(t, store_pebble.RebuildSQLCallsiteStats{Stubs: 1, Resolved: 1, Moves: moves}, stats)
				require.Equal(t, want, rebuildEventSavedRows(t, b))
			}
		})
	}
}

func TestRebuildSQLCallsiteStaleSQLiteOracle(t *testing.T) {
	const file = "app.ts"
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthSQLCallsite})
	require.NoError(t, err)
	// Neither the SQL table nor the same-name TypeScript function is eligible.
	table := rebuildSQLCallsiteExtract(t, languages.NewSQLExtractor(), "db/table.sql", "CREATE TABLE get_user_stats (id INT);")
	require.True(t, slices.ContainsFunc(table.Nodes, func(n *graph.Node) bool {
		return n.Name == rebuildSQLCallsiteName && n.Kind == graph.KindType && n.Language == "sql"
	}))
	anchor := rebuildSQLCallsiteFunction(t, "db/anchor.sql", "public.anchor_fn")
	unrelated := rebuildSQLCallsiteFunction(t, "db/unrelated.sql", "public.unrelated_fn")
	first := rebuildSQLCallsiteFunction(t, "db/functions.sql", "public.get_user_stats")
	second := rebuildSQLCallsiteFunction(t, "db/second.sql", "other.get_user_stats")
	// Repository qualification follows the portable codec boundary.
	for _, n := range second.Nodes {
		n.RepoPrefix = "other-repo"
	}
	for _, tc := range []struct {
		name, current string
		functions     []store_pebble.ParseFacts
		proof         []store_pebble.ParseFacts
		wantStats     store_pebble.RebuildSQLCallsiteStats
	}{
		{
			name: "absent", current: rebuildSQLCallsiteTarget,
			functions: []store_pebble.ParseFacts{anchor, unrelated},
			proof:     []store_pebble.ParseFacts{anchor},
			wantStats: store_pebble.RebuildSQLCallsiteStats{Stubs: 1, Moves: 1},
		},
		{
			name: "ambiguous", current: rebuildSQLCallsiteTarget,
			functions: []store_pebble.ParseFacts{anchor, first, second},
			proof:     []store_pebble.ParseFacts{first, second},
			wantStats: store_pebble.RebuildSQLCallsiteStats{Stubs: 1, Moves: 1},
		},
		{name: "no_eligible_sql_function", current: rebuildSQLCallsiteTarget},
		{
			name: "stationary_winner", current: rebuildSQLCallsiteTarget,
			functions: []store_pebble.ParseFacts{first},
			wantStats: store_pebble.RebuildSQLCallsiteStats{Stubs: 1, Resolved: 1},
		},
		{
			name: "stationary_placeholder", current: rebuildSQLCallsiteStub,
			functions: []store_pebble.ParseFacts{anchor},
			wantStats: store_pebble.RebuildSQLCallsiteStats{Stubs: 1},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := rebuildSQLCallsiteExtract(t, languages.NewTypeScriptExtractor(), file, rebuildSQLCallsiteTS)
			require.True(t, slices.ContainsFunc(app.Nodes, func(n *graph.Node) bool {
				return n.Name == rebuildSQLCallsiteName && n.Kind == graph.KindFunction && n.Language == "typescript"
			}))
			call := rebuildSQLCallsiteEdge(t, app.Edges, file)
			// Seed the same previously evaluated Call payload in both stores.
			call.To = tc.current
			call.Confidence = 0.91
			call.ConfidenceLabel = "EXTRACTED"
			call.Meta[MetaSynthesizedBy] = "earlier-pass"
			call.Meta[MetaProvenance] = "fixture"
			// SQLite promotes these keys to columns. The full-row comparison
			// must preserve them even when the key and synthesis stamps change.
			call.Meta["resolve_terminal"] = true
			call.Meta["resolve_terminal_reason"] = "fixture"
			call.Meta["semantic_source"] = "fixture"
			app = roundTripRebuildRNBridgeFacts(t, app)

			facts := append([]store_pebble.ParseFacts{app, table}, tc.functions...)

			b, native := openRebuildEventStores(t, facts...)
			frozen, err := FreezeRebuildFrameworkSelection(t.Context(), b.RebuildView(), selection)
			require.NoError(t, err)
			require.True(t, frozen.allows(SynthSQLCallsite))
			before := rebuildEventSQLiteRows(native)
			require.Equal(t, before, rebuildEventSavedRows(t, b))
			want := before
			if tc.wantStats.Moves != 0 {
				old := rebuildSQLCallsiteEdge(t, before.edges, file)
				reset := *old
				reset.To = rebuildSQLCallsiteStub
				reset.Confidence = 0
				reset.ConfidenceLabel = ""
				reset.Origin = graph.OriginASTInferred
				reset.Meta = maps.Clone(old.Meta)
				delete(reset.Meta, MetaSynthesizedBy)
				delete(reset.Meta, MetaProvenance)
				want = rebuildSQLCallsiteWithEdge(t, before, old, &reset)
			}
			report := RunFrameworkSynthesizersWithSelection(native, selection)
			require.NoError(t, report.Err)
			require.EqualValues(t, tc.wantStats.Resolved, report.Total)
			require.Equal(t, want, rebuildEventSQLiteRows(native))

			receipts := rebuildGRPCReceipts()
			if tc.wantStats.Moves != 0 {
				receipts = rebuildGRPCReceipts(append([]store_pebble.ParseFacts{app}, tc.proof...)...)
			} else if tc.wantStats.Stubs == 0 {
				receipts = func(func(store_pebble.ParseFacts, error) bool) {
					t.Fatal("replayed receipts without a named SQL function")
				}
			}
			stats, err := RunRebuildSQLCallsites(t.Context(), b, receipts, frozen)
			require.NoError(t, err)
			require.Equal(t, tc.wantStats, stats)
			require.Equal(t, want, rebuildEventSavedRows(t, b))
		})
	}
}

func TestRebuildSQLCallsiteRejectsUnprovedMove(t *testing.T) {
	const file = "app.ts"
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthSQLCallsite})
	require.NoError(t, err)
	app := rebuildSQLCallsiteExtract(t, languages.NewTypeScriptExtractor(), file, rebuildSQLCallsiteTS)
	sql := rebuildSQLCallsiteFunction(t, "db/functions.sql", "public.get_user_stats")
	legacy := app
	legacy.EffectiveLanguage = ""
	legacy = roundTripRebuildRNBridgeFacts(t, legacy)
	require.Empty(t, legacy.EffectiveLanguage)
	noRawCall := app
	stub := rebuildSQLCallsiteEdge(t, app.Edges, file)
	noRawCall.Edges = slices.DeleteFunc(slices.Clone(app.Edges), func(e *graph.Edge) bool { return e == stub })
	noRawCall = roundTripRebuildRNBridgeFacts(t, noRawCall)
	wrongSite := app
	wrongSite.Edges = slices.Clone(app.Edges)
	wrongCall := *stub
	wrongCall.Line++
	wrongSite.Edges[slices.Index(app.Edges, stub)] = &wrongCall
	wrongSite = roundTripRebuildRNBridgeFacts(t, wrongSite)
	wrongCandidate := roundTripRebuildRNBridgeFacts(t, sql)
	for _, n := range wrongCandidate.Nodes {
		if n.ID == rebuildSQLCallsiteTarget {
			n.Name = "other_fn"
		}
	}
	wrongCandidate = roundTripRebuildRNBridgeFacts(t, wrongCandidate)

	for _, tc := range []struct {
		name  string
		proof []store_pebble.ParseFacts
	}{
		{"missing_sql_receipt", []store_pebble.ParseFacts{app}},
		{"missing_effective_source_language", []store_pebble.ParseFacts{legacy, sql}},
		{"missing_raw_call", []store_pebble.ParseFacts{noRawCall, sql}},
		{"wrong_raw_site", []store_pebble.ParseFacts{wrongSite, sql}},
		{"wrong_sql_candidate_name", []store_pebble.ParseFacts{app, wrongCandidate}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := openRebuildGRPCBuild(t, app, sql)
			frozen, err := FreezeRebuildFrameworkSelection(t.Context(), b.RebuildView(), selection)
			require.NoError(t, err)
			require.True(t, frozen.allows(SynthSQLCallsite))
			before := rebuildEventSavedRows(t, b)
			stats, err := RunRebuildSQLCallsites(t.Context(), b, rebuildGRPCReceipts(tc.proof...), frozen)
			require.ErrorIs(t, err, store_pebble.ErrRebuildSQLCallsite)
			var refused *store_pebble.RebuildUnknownProvenanceError
			require.ErrorAs(t, err, &refused)
			want := graph.EdgeIdentityFor(stub)
			want.To = rebuildSQLCallsiteTarget
			require.Equal(t, &store_pebble.RebuildUnknownProvenanceError{Count: 1, First: want}, refused)
			require.Zero(t, stats)
			require.Equal(t, before, rebuildEventSavedRows(t, b))
		})
	}
}

func rebuildSQLCallsiteExtract(t *testing.T, extractor parser.Extractor, file, source string) store_pebble.ParseFacts {
	t.Helper()
	parsed, err := extractor.Extract(file, []byte(source))
	require.NoError(t, err)
	t.Cleanup(parsed.ReleaseTree)
	return roundTripRebuildRNBridgeFacts(t, rebuildEventFacts(file, extractor.Language(), parsed.Nodes, parsed.Edges...))
}

func rebuildSQLCallsiteFunction(t *testing.T, file, qualifiedName string) store_pebble.ParseFacts {
	t.Helper()
	facts := rebuildSQLCallsiteExtract(t, languages.NewSQLExtractor(), file, `CREATE OR REPLACE FUNCTION `+qualifiedName+`(p_id uuid)
RETURNS void
LANGUAGE plpgsql
AS $$
BEGIN
  RETURN;
END;
$$;
`)
	_, name, ok := strings.Cut(qualifiedName, ".")
	require.True(t, ok)
	require.True(t, slices.ContainsFunc(facts.Nodes, func(n *graph.Node) bool {
		return n.ID == file+"::"+qualifiedName && n.Name == name && n.Kind == graph.KindFunction && n.Language == "sql" && n.FilePath == file
	}), "missing extracted SQL function %s::%s", file, qualifiedName)
	return facts
}

func rebuildSQLCallsiteEdge(t *testing.T, edges []*graph.Edge, file string) *graph.Edge {
	t.Helper()
	var calls []*graph.Edge
	for _, e := range edges {
		if e.Meta["via"] == "sql.callsite" {
			calls = append(calls, e)
		}
	}
	require.Len(t, calls, 1)
	e := calls[0]
	caller := file + "::load"
	if strings.HasSuffix(file, ".vue") {
		caller = file + "#script:1::load"
	}
	require.Equal(t, caller, e.From)
	require.Equal(t, graph.EdgeCalls, e.Kind)
	require.Equal(t, file, e.FilePath)
	require.Equal(t, 2, e.Line)
	require.Equal(t, rebuildSQLCallsiteName, e.Meta["sql_function"])
	return e
}

func rebuildSQLCallsiteWithEdge(t *testing.T, rows rebuildEventRows, old, newEdge *graph.Edge) rebuildEventRows {
	t.Helper()
	i := slices.Index(rows.edges, old)
	require.NotEqual(t, -1, i)
	rows.edges = slices.Clone(rows.edges)
	rows.edges[i] = newEdge
	rows.sort()
	return rows
}
