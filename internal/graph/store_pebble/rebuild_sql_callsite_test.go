package store_pebble

import (
	"iter"
	"slices"
	"strings"
	"testing"

	"github.com/cockroachdb/pebble/v2"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildSQLCallsitePlanCapsOnlyOwnedCalls(t *testing.T) {
	for _, owned := range []bool{false, true} {
		b := newBuild(t, FullProfile)
		large := sqlCallsiteStoreEdge("unresolved::sqlfn::find", "find")
		large.Context = strings.Repeat("x", rebuildSQLCallsiteRowBytes)
		if !owned {
			delete(large.Meta, "via")
		}
		key := pendingEdgeKey(large)
		raw, err := encodeEdge(large)
		require.NoError(t, err)
		require.Greater(t, len(key)+len(raw), rebuildSQLCallsiteRowBytes)
		require.NoError(t, b.pending.Set(key, raw, pebble.NoSync))
		before := rebuildFrameworkCustodyRows(t, b)
		p := rebuildSQLCallsitePass{b: b, fn: sqlCallsiteStoreMarker,
			rebind: func(*graph.Edge, string, string, bool) bool { return false }}
		err = p.plan(t.Context())
		if owned {
			require.ErrorIs(t, err, ErrRebuildLinkInputSize)
			require.ErrorContains(t, err, "SQL callsite edge row")
		} else {
			require.NoError(t, err)
		}
		require.Zero(t, p.stats)
		require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
	}
}

func TestRebuildSQLCallsiteWithoutFunctionsSkipsCallsAndReceipts(t *testing.T) {
	b, _ := seedSyntheticCustody(t, sqlCallsiteStoreSource(sqlCallsiteStoreEdge("retired", "find")))
	before := rebuildFrameworkCustodyRows(t, b)
	unread := func(func(ParseFacts, error) bool) { t.Fatal("no SQL functions: read receipts") }
	marker := func(*graph.Edge) string {
		t.Fatal("no SQL functions: read a call")
		return ""
	}
	rebind := func(*graph.Edge, string, string, bool) bool { return false }
	stats, err := b.RebuildSQLCallsites(t.Context(), unread, marker, rebind)
	require.NoError(t, err)
	require.Zero(t, stats)
	require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
}

func TestRebuildSQLCallsiteCountsOnlyInteractingDestinations(t *testing.T) {
	for _, tc := range []struct {
		name string
		last string
	}{
		{"convergence", "old-z"},
		{"chain", "new-b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first := sqlCallsiteStoreEdge("old-a", "find")
			second := sqlCallsiteStoreEdge(tc.last, "find")
			if tc.name == "chain" {
				second = sqlCallsiteStoreEdge(tc.last, "list")
			}
			b, _ := seedSyntheticCustody(t, sqlCallsiteStoreSource(first, second),
				sqlCallsiteStoreFunction("new-b", "find"), sqlCallsiteStoreFunction("new-c", "list"))
			before := rebuildFrameworkCustodyRows(t, b)
			unread := func(func(ParseFacts, error) bool) { t.Fatal("order refusal read receipts") }
			stats, err := runSQLCallsiteStore(t, b, unread)
			want := *first
			want.To = "new-b"
			var refused *RebuildSQLCallsiteOrderError
			require.ErrorIs(t, err, ErrRebuildSQLCallsite)
			require.ErrorIs(t, err, ErrRebuildSQLCallsiteOrder)
			require.ErrorAs(t, err, &refused)
			require.Equal(t, &RebuildSQLCallsiteOrderError{Count: 1, First: graph.EdgeIdentityFor(&want)}, refused)
			require.Zero(t, stats)
			require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
		})
	}
}

func TestRebuildSQLCallsiteDisjointMovesIgnoreReceiptOrder(t *testing.T) {
	first := sqlCallsiteStoreEdge("unresolved::sqlfn::find", "find")
	second := sqlCallsiteStoreEdge("unresolved::sqlfn::list", "list")
	facts := []ParseFacts{
		sqlCallsiteStoreSource(first, second),
		sqlCallsiteStoreFunction("new-b", "find"),
		sqlCallsiteStoreFunction("new-c", "list"),
	}
	move := func(facts []ParseFacts) map[string][]byte {
		b, receipts := seedSyntheticCustody(t, facts...)
		stats, err := runSQLCallsiteStore(t, b, receipts)
		require.NoError(t, err)
		require.Equal(t, RebuildSQLCallsiteStats{Stubs: 2, Resolved: 2, Moves: 2}, stats)
		for _, pair := range []struct {
			old *graph.Edge
			to  string
		}{{first, "new-b"}, {second, "new-c"}} {
			want := *pair.old
			want.To = pair.to
			key := pendingEdgeKey(&want)
			saved, err := b.pendingEdge(key)
			require.NoError(t, err)
			require.Equal(t, &want, saved)
			proof, err := readRebuildEdgeCustody(b.pending, key)
			require.NoError(t, err)
			require.Equal(t, rebuildEdgeCustody{raw: pendingEdgeKey(pair.old)}, proof)
		}
		return rebuildFrameworkCustodyRows(t, b)
	}
	after := move(facts)
	facts[0].Edges = []*graph.Edge{second, first}
	slices.Reverse(facts)
	require.Equal(t, after, move(facts))
}

func TestRebuildSQLCallsiteStaleStampIsNotAClaim(t *testing.T) {
	for _, name := range []string{"stationary", "moving", "invalid_format"} {
		t.Run(name, func(t *testing.T) {
			mover := sqlCallsiteStoreEdge("unresolved::sqlfn::find", "find")
			suspect := sqlCallsiteStoreEdge("unresolved::sqlfn::list", "list")
			if name == "moving" {
				suspect.To = "retired"
			}
			b, receipts := seedSyntheticCustody(t, sqlCallsiteStoreSource(mover, suspect), sqlCallsiteStoreFunction("new-b", "find"))
			require.NoError(t, b.withWrites(func() error {
				return b.addEdgeWithCustody(suspect, rebuildEdgeCustody{proof: rebuildProofReceiver})
			}))
			key := pendingEdgeKey(suspect)
			recorder := rebuildLinkRecorderKey(key)
			if name == "invalid_format" {
				require.NoError(t, b.pending.Set(recorder, []byte{rebuildEdgeProofFormat}, pebble.NoSync))
			} else {
				changed := *suspect
				changed.Context = "payload changed without refreshing custody"
				value, err := encodeEdge(&changed)
				require.NoError(t, err)
				require.NoError(t, b.pending.Set(key, value, pebble.NoSync))
				_, err = readRebuildEdgeCustody(b.pending, key)
				require.ErrorIs(t, err, ErrRebuildUnknownProvenance)
			}
			before := rebuildFrameworkCustodyRows(t, b)
			stats, err := runSQLCallsiteStore(t, b, receipts)
			switch name {
			case "stationary":
				require.NoError(t, err)
				require.Equal(t, RebuildSQLCallsiteStats{Stubs: 2, Resolved: 1, Moves: 1}, stats)
				after := rebuildFrameworkCustodyRows(t, b)
				for _, k := range [][]byte{key, pendingReverseKey(suspect, key), recorder} {
					require.NotEmpty(t, before[string(k)])
					require.Equal(t, before[string(k)], after[string(k)])
				}
				return
			case "moving":
				want := *suspect
				want.To = "unresolved::sqlfn::list"
				var refused *RebuildUnknownProvenanceError
				require.ErrorIs(t, err, ErrRebuildSQLCallsite)
				require.ErrorAs(t, err, &refused)
				require.Equal(t, &RebuildUnknownProvenanceError{Count: 1, First: graph.EdgeIdentityFor(&want)}, refused)
			case "invalid_format":
				require.ErrorIs(t, err, ErrRebuildSQLCallsite)
				require.ErrorIs(t, err, errInvalidRecord)
			}
			require.Zero(t, stats)
			require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
		})
	}
}

func sqlCallsiteStoreEdge(to, name string) *graph.Edge {
	return &graph.Edge{From: "app.ts::caller", To: to, Kind: graph.EdgeCalls, FilePath: "app.ts", Line: 2,
		Origin: graph.OriginASTInferred, Meta: map[string]any{"via": "sql.callsite", "sql_function": name}}
}

func sqlCallsiteStoreSource(edges ...*graph.Edge) ParseFacts {
	return ParseFacts{EffectiveLanguage: "typescript", File: graph.FileMetaRow{FilePath: "app.ts"}, Edges: edges}
}

func sqlCallsiteStoreFunction(id, name string) ParseFacts {
	return producerProofFacts(&graph.Node{ID: id, Name: name, Kind: graph.KindFunction, FilePath: "functions.sql", Language: "sql"})
}

func runSQLCallsiteStore(t *testing.T, b *BuildStore, receipts iter.Seq2[ParseFacts, error]) (RebuildSQLCallsiteStats, error) {
	t.Helper()
	rebind := func(e *graph.Edge, name, target string, matched bool) bool {
		if !matched {
			target = "unresolved::sqlfn::" + name
		}
		if e.To == target {
			return false
		}
		e.To = target
		return true
	}
	return b.RebuildSQLCallsites(t.Context(), receipts, sqlCallsiteStoreMarker, rebind)
}

func sqlCallsiteStoreMarker(e *graph.Edge) string {
	if e.Meta["via"] != "sql.callsite" {
		return ""
	}
	name, _ := e.Meta["sql_function"].(string)
	return name
}
