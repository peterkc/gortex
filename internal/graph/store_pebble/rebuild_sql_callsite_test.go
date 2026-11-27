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
		p := newRebuildSQLCallsitePass(b, sqlCallsiteStoreMarker, func(*graph.Edge, string, string, bool) bool { return false })
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
		name  string
		last  string
		count uint64
	}{
		{"convergence", "old-z", 1},
		{"chain", "new-b", 1},
		{"two interacting destinations", "old-z", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first := sqlCallsiteStoreEdge("old-a", "find")
			second := sqlCallsiteStoreEdge(tc.last, "find")
			if tc.name == "chain" {
				second = sqlCallsiteStoreEdge(tc.last, "list")
			}
			edges := []*graph.Edge{first, second}
			if tc.count == 2 {
				edges = append(edges, sqlCallsiteStoreEdge("old-c", "list"), sqlCallsiteStoreEdge("old-d", "list"))
			}
			b, _ := seedSyntheticCustody(t, sqlCallsiteStoreSource(edges...),
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
			require.Equal(t, &RebuildSQLCallsiteOrderError{Count: tc.count, First: graph.EdgeIdentityFor(&want)}, refused)
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

func TestRebuildSQLCallsiteRestubProof(t *testing.T) {
	for _, tc := range []struct {
		name       string
		cohort     int
		exact      int
		anchor     bool
		wantRestub bool
	}{
		{name: "empty without exact anchor"},
		{name: "empty with exact anchor", anchor: true, wantRestub: true},
		{name: "ambiguous with zero exact members", cohort: 3, anchor: true},
		{name: "ambiguous with one exact member", cohort: 3, exact: 1, anchor: true},
		{name: "ambiguous with two exact members", cohort: 3, exact: 2, wantRestub: true},
		{name: "ambiguous with all exact members", cohort: 3, exact: 3, wantRestub: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := sqlCallsiteStoreEdge("retired", "find")
			source := sqlCallsiteStoreSource(old)
			anchor := sqlCallsiteStoreFunction("anchor", "other")
			facts := []ParseFacts{source, anchor}
			proof := []ParseFacts{source}
			if tc.anchor {
				proof = append(proof, anchor)
			}
			ids := []string{"find-a", "find-b", "find-c"}
			for i, id := range ids[:tc.cohort] {
				candidate := sqlCallsiteStoreFunction(id, "find")
				facts = append(facts, candidate)
				if i < tc.exact {
					proof = append(proof, candidate)
				}
			}
			b, _ := seedSyntheticCustody(t, facts...)
			before := rebuildFrameworkCustodyRows(t, b)
			stats, err := runSQLCallsiteStore(t, b, sqlCallsiteTestReceipts(proof...))
			want := *old
			want.To = "unresolved::sqlfn::find"
			if !tc.wantRestub {
				requireSQLCallsiteProofRefusal(t, err, stats, &want)
				require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
				return
			}
			require.NoError(t, err)
			require.Equal(t, RebuildSQLCallsiteStats{Stubs: 1, Moves: 1}, stats)
			key := pendingEdgeKey(&want)
			saved, err := b.pendingEdge(key)
			require.NoError(t, err)
			require.Equal(t, &want, saved)
			custody, err := readRebuildEdgeCustody(b.pending, key)
			require.NoError(t, err)
			require.Equal(t, rebuildEdgeCustody{raw: pendingEdgeKey(old)}, custody)
		})
	}
}

func TestRebuildSQLCallsitePreservesOversizedIncumbent(t *testing.T) {
	old := sqlCallsiteStoreEdge("unresolved::sqlfn::find", "find")
	incumbent := *sqlCallsiteStoreEdge("new-b", "find")
	incumbent.Meta = map[string]any{"via": "other.producer"}
	incumbent.Context = strings.Repeat("x", rebuildSQLCallsiteRowBytes)
	b, receipts := seedSyntheticCustody(t, sqlCallsiteStoreSource(old), sqlCallsiteStoreFunction("new-b", "find"))
	require.NoError(t, b.withWrites(func() error {
		return b.addEdgeWithCustody(&incumbent, rebuildEdgeCustody{proof: rebuildProofReceiver})
	}))
	before := rebuildFrameworkCustodyRows(t, b)
	key := pendingEdgeKey(&incumbent)
	require.Greater(t, len(key)+len(before[string(key)]), rebuildSQLCallsiteRowBytes)
	stats, err := runSQLCallsiteStore(t, b, receipts)
	require.NoError(t, err)
	require.Equal(t, RebuildSQLCallsiteStats{Stubs: 1, Resolved: 1, Moves: 1}, stats)
	delete(before, string(pendingEdgeKey(old)))
	delete(before, string(pendingReverseKey(old, pendingEdgeKey(old))))
	require.Equal(t, before, rebuildFrameworkCustodyRows(t, b), "keep the incumbent bytes and custody")
}

func TestRebuildSQLCallsiteRejectsStationaryRawClaimCollision(t *testing.T) {
	// The stationary key sorts between these two aliases, so each run
	// exercises a different order of claims against the same receipt.
	for _, alias := range []string{"a-mover", "z-mover"} {
		t.Run(alias, func(t *testing.T) {
			const target = "middle"
			raw := sqlCallsiteStoreEdge(target, "find")
			b, receipts := seedSyntheticCustody(t, sqlCallsiteStoreSource(raw), sqlCallsiteStoreFunction(target, "find"))
			mover := *raw
			mover.To = alias
			require.NoError(t, b.ApplyLinkReindexes(t.Context(), []graph.EdgeReindex{{Edge: &mover, OldTo: raw.To}}))
			b.AddEdge(raw)
			before := rebuildFrameworkCustodyRows(t, b)
			stats, err := runSQLCallsiteStore(t, b, receipts)
			requireSQLCallsiteProofRefusal(t, err, stats, raw)
			require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
		})
	}
}

func TestRebuildSQLCallsiteReceiptOwnershipFields(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*ParseFacts, *ParseFacts)
	}{
		{name: "raw call file", change: func(app, _ *ParseFacts) { app.File.FilePath = "other.ts" }},
		{name: "raw call function", change: func(app, _ *ParseFacts) { app.Edges[0].Meta["sql_function"] = "other" }},
		{name: "node receipt language", change: func(_, sql *ParseFacts) { sql.EffectiveLanguage = "typescript" }},
		{name: "node receipt file", change: func(_, sql *ParseFacts) { sql.File.FilePath = "other.sql" }},
		{name: "node file", change: func(_, sql *ParseFacts) { sql.Nodes[0].FilePath = "other.sql" }},
		{name: "node language", change: func(_, sql *ParseFacts) { sql.Nodes[0].Language = "typescript" }},
		{name: "node kind", change: func(_, sql *ParseFacts) { sql.Nodes[0].Kind = graph.KindMethod }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := sqlCallsiteStoreEdge("unresolved::sqlfn::find", "find")
			app, sql := sqlCallsiteStoreSource(old), sqlCallsiteStoreFunction("new-b", "find")
			b, _ := seedSyntheticCustody(t, app, sql)
			before := rebuildFrameworkCustodyRows(t, b)
			tc.change(&app, &sql)
			stats, err := runSQLCallsiteStore(t, b, sqlCallsiteTestReceipts(app, sql))
			want := *old
			want.To = "new-b"
			requireSQLCallsiteProofRefusal(t, err, stats, &want)
			require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
		})
	}
}

func TestRebuildSQLCallsiteOccupiedScratch(t *testing.T) {
	b := newBuild(t, FullProfile)
	require.NoError(t, b.pending.Set(rebuildSQLCallsiteKey('z'), []byte("sentinel"), pebble.NoSync))
	before := rebuildFrameworkCustodyRows(t, b)
	stats, err := runSQLCallsiteStore(t, b, func(func(ParseFacts, error) bool) { t.Fatal("read receipts with occupied scratch") })
	require.ErrorIs(t, err, ErrRebuildSQLCallsite)
	require.ErrorIs(t, err, errInvalidRecord)
	require.ErrorContains(t, err, "SQL callsite scratch range occupied")
	require.Zero(t, stats)
	require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
}

func TestRebuildSQLCallsiteStationarySkipsReceipts(t *testing.T) {
	incumbent := sqlCallsiteStoreEdge("new-b", "find")
	b, _ := seedSyntheticCustody(t, sqlCallsiteStoreSource(incumbent), sqlCallsiteStoreFunction("new-b", "find"))
	before := rebuildFrameworkCustodyRows(t, b)
	stats, err := runSQLCallsiteStore(t, b, func(func(ParseFacts, error) bool) { t.Fatal("read receipts without moves") })
	require.NoError(t, err)
	require.Equal(t, RebuildSQLCallsiteStats{Stubs: 1, Resolved: 1}, stats)
	require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
}

func sqlCallsiteTestReceipts(facts ...ParseFacts) iter.Seq2[ParseFacts, error] {
	return func(yield func(ParseFacts, error) bool) {
		for _, f := range facts {
			if !yield(f, nil) {
				return
			}
		}
	}
}

func requireSQLCallsiteProofRefusal(t *testing.T, err error, stats RebuildSQLCallsiteStats, want *graph.Edge) {
	t.Helper()
	require.ErrorIs(t, err, ErrRebuildSQLCallsite)
	var refused *RebuildUnknownProvenanceError
	require.ErrorAs(t, err, &refused)
	require.Equal(t, &RebuildUnknownProvenanceError{Count: 1, First: graph.EdgeIdentityFor(want)}, refused)
	require.Zero(t, stats)
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
