package resolver

import (
	"maps"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

func TestRebuildMyBatisSQLiteOracle(t *testing.T) {
	const namespace = "example.UserMapper"
	const localFile, remoteFile = "local/UserMapper.java", "remote/UserMapper.java"
	const stmtID, methodName = namespace + "::findUser", "findUser"
	const oldTarget, newTarget = "unresolved::mybatis::" + stmtID, localFile + "::" + methodName
	const mapper = `<mapper namespace="example.UserMapper">
    <select id="findUser">SELECT 1</select>
</mapper>`
	const java = `public interface UserMapper {
    int findUser();
}`
	const kotlin = `class UserMapper {
    fun findUser(): Int = 1
}`

	for _, tc := range []struct {
		name, callerRepo, secondRepo string
		kotlinDecoy                  bool
		resolved                     uint64
	}{
		{name: "same_repo_wins", callerRepo: "local", secondRepo: "remote", resolved: 1},
		{name: "ambiguous_in_same_repo", callerRepo: "local", secondRepo: "local"},
		{name: "workspace_fallback", callerRepo: "mapper", resolved: 1},
		{name: "kotlin_decoy", callerRepo: "local", kotlinDecoy: true, resolved: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			xml := rebuildMyBatisExtract(t, languages.NewMyBatisExtractor(), tc.callerRepo+"/UserMapper.xml", tc.callerRepo, mapper)
			facts := []store_pebble.ParseFacts{xml}
			if tc.secondRepo != "" {
				facts = append(facts, rebuildMyBatisExtract(t, languages.NewJavaExtractor(), remoteFile, tc.secondRepo, java))
			}
			if tc.kotlinDecoy {
				// Same repo and join key as the Java winner: admitting Kotlin would make the call ambiguous.
				facts = append(facts, rebuildMyBatisExtract(t, languages.NewKotlinExtractor(), "local/UserMapper.kt", "local", kotlin))
			}
			facts = append(facts, rebuildMyBatisExtract(t, languages.NewJavaExtractor(), localFile, "local", java))
			for _, f := range facts[1:] {
				wantID := f.File.FilePath + "::" + methodName
				if f.EffectiveLanguage == "kotlin" {
					wantID = f.File.FilePath + "::UserMapper." + methodName
				}
				i := slices.IndexFunc(f.Nodes, func(n *graph.Node) bool { return n.ID == wantID })
				require.NotEqual(t, -1, i)
				require.Equal(t, graph.KindMethod, f.Nodes[i].Kind)
				require.Equal(t, methodName, f.Nodes[i].Name)
				require.Equal(t, "UserMapper", f.Nodes[i].Meta["receiver"])
				require.Equal(t, f.EffectiveLanguage, f.Nodes[i].Language)
			}

			native := openRebuildTemporalOracle(t, facts...)
			caller := native.GetNode(stmtID)
			require.NotNil(t, caller)
			require.Equal(t, tc.callerRepo, caller.RepoPrefix)
			privateFacts := slices.Clone(facts)
			slices.Reverse(privateFacts)
			private := openRebuildGRPCBuild(t, privateFacts...)
			before := rebuildEventSQLiteRows(native)
			require.Equal(t, before, rebuildEventSavedRows(t, private))
			i := slices.IndexFunc(before.edges, func(e *graph.Edge) bool {
				return e.From == stmtID && e.Kind == graph.EdgeCalls
			})
			require.NotEqual(t, -1, i)
			stub := before.edges[i]
			require.Equal(t, oldTarget, stub.To)
			require.Equal(t, map[string]any{
				"via":               "mybatis.mapper",
				"mybatis_namespace": namespace,
				"mybatis_statement": methodName,
			}, stub.Meta)

			selection, err := NewFrameworkSynthesizerSelection([]string{SynthMyBatis})
			require.NoError(t, err)
			frozen, err := FreezeRebuildFrameworkSelection(t.Context(), private.RebuildView(), selection)
			require.NoError(t, err)
			require.True(t, frozen.allows(SynthMyBatis))

			want := before
			if tc.resolved != 0 {
				moved := *stub
				moved.To = newTarget
				moved.Origin = graph.OriginASTInferred
				moved.Confidence = 0.7
				moved.ConfidenceLabel = graph.ConfidenceLabelFor(graph.EdgeCalls, 0.7)
				moved.Meta = maps.Clone(stub.Meta)
				moved.Meta["synthesized_by"] = "mybatis"
				moved.Meta["provenance"] = "heuristic"
				moved.Meta["mybatis_resolution"] = graph.OriginASTInferred
				want.edges = slices.Clone(before.edges)
				want.edges[i] = &moved
				want.sort()
			}
			if tc.name == "same_repo_wins" {
				// The elected same-repo method still exists in the private graph.
				// Losing its receipt must refuse, not elect the proved remote method.
				stats, err := RunRebuildMyBatis(t.Context(), private, rebuildGRPCReceipts(facts[:len(facts)-1]...), frozen)
				require.ErrorIs(t, err, store_pebble.ErrRebuildUnknownProvenance)
				require.Zero(t, stats)
				require.Equal(t, before, rebuildEventSavedRows(t, private))
			}

			for _, wantMoves := range []uint64{tc.resolved, 0} {
				report := RunFrameworkSynthesizersWithSelection(native, selection)
				require.NoError(t, report.Err)
				require.EqualValues(t, tc.resolved, report.Total)
				gotNative := rebuildEventSQLiteRows(native)
				require.Equal(t, want, gotNative)
				stats, err := RunRebuildMyBatis(t.Context(), private, rebuildGRPCReceipts(facts...), frozen)
				require.NoError(t, err)
				require.EqualValues(t, 1, stats.Stubs)
				require.EqualValues(t, tc.resolved, stats.Resolved)
				require.EqualValues(t, wantMoves, stats.Moves)
				require.Equal(t, gotNative, rebuildEventSavedRows(t, private))
			}
		})
	}
}

func rebuildMyBatisExtract(t *testing.T, extractor parser.Extractor, file, repo, source string) store_pebble.ParseFacts {
	t.Helper()
	parsed, err := extractor.Extract(file, []byte(source))
	require.NoError(t, err)
	t.Cleanup(parsed.ReleaseTree)
	facts := rebuildEventFacts(file, extractor.Language(), parsed.Nodes, parsed.Edges...)
	facts = roundTripRebuildRNBridgeFacts(t, facts)
	for _, n := range facts.Nodes {
		n.RepoPrefix = repo
	}
	return facts
}

func TestRebuildMyBatisExtractedSourceKeys(t *testing.T) {
	const file = "mapper/UserMapper.xml"
	const namespace = "example.UserMapper"
	const alpha, beta = namespace + "::findAlpha", namespace + "::findBeta"
	const mapper = `<mapper namespace="example.UserMapper">
    <select id="findAlpha">SELECT 1</select><select id="findBeta">SELECT 2</select>
</mapper>`
	xml := rebuildMyBatisExtract(t, languages.NewMyBatisExtractor(), file, "", mapper)
	b := openRebuildGRPCBuild(t, xml)

	// Both statements start on line 2. Their From IDs give them separate
	// sites, each with one old key in the live graph before the MyBatis turn.
	var keys []graph.EdgeIdentity
	for _, e := range rebuildEventSavedRows(t, b).edges {
		if e.Kind == graph.EdgeCalls && e.FilePath == file {
			keys = append(keys, graph.EdgeIdentityFor(e))
		}
	}
	require.Equal(t, []graph.EdgeIdentity{
		{From: alpha, To: "unresolved::mybatis::" + alpha, Kind: graph.EdgeCalls, FilePath: file, Line: 2},
		{From: beta, To: "unresolved::mybatis::" + beta, Kind: graph.EdgeCalls, FilePath: file, Line: 2},
	}, keys)
}

func TestRebuildMyBatisOrder(t *testing.T) {
	const namespace = "example.UserMapper"
	const xmlFile, javaFile = "mapper/UserMapper.xml", "mapper/UserMapper.java"
	const alpha, beta = "findAlpha", "findBeta"
	const from = namespace + "::" + alpha
	const alphaTarget, betaTarget = javaFile + "::" + alpha, javaFile + "::" + beta
	const mapper = `<mapper namespace="example.UserMapper">
    <select id="findAlpha">SELECT 1</select>
</mapper>`
	const java = `public interface UserMapper {
    int findAlpha();
    int findBeta();
}`
	xml := rebuildMyBatisExtract(t, languages.NewMyBatisExtractor(), xmlFile, "", mapper)
	methods := rebuildMyBatisExtract(t, languages.NewJavaExtractor(), javaFile, "", java)
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthMyBatis})
	require.NoError(t, err)
	definitions := slices.DeleteFunc(slices.Clone(xml.Edges), func(e *graph.Edge) bool {
		return e.Kind == graph.EdgeCalls
	})

	for _, tc := range []struct {
		name, secondOld, secondStatement string
		affected                         uint64
	}{
		{name: "commuting", secondOld: "stale::beta", secondStatement: beta},
		{name: "convergent", secondOld: "stale::beta", secondStatement: alpha, affected: 1},
		{name: "chain", secondOld: alphaTarget, secondStatement: beta, affected: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Use the same real caller and source site for two synthetic raw
			// calls. The XML receipt carries both exact old keys and markers;
			// the Java receipt carries every method in the affected cohorts.
			stub := func(to, statement, fixture string) *graph.Edge {
				return &graph.Edge{From: from, To: to, Kind: graph.EdgeCalls, FilePath: xmlFile, Line: 2,
					Meta: map[string]any{"via": "mybatis.mapper", "mybatis_namespace": namespace,
						"mybatis_statement": statement, "fixture": fixture}}
			}
			stored := xml
			stored.Edges = append(slices.Clone(definitions), stub("stale::alpha", alpha, "left"),
				stub(tc.secondOld, tc.secondStatement, "right"))
			stored = roundTripRebuildRNBridgeFacts(t, stored)
			if tc.affected != 0 {
				b := openRebuildGRPCBuild(t, stored, methods)
				before := rebuildEventSavedRows(t, b)
				stats, err := RunRebuildMyBatis(t.Context(), b, rebuildGRPCReceipts(stored, methods), selection)
				require.ErrorIs(t, err, store_pebble.ErrRebuildMyBatisOrder)
				var order *store_pebble.RebuildMyBatisOrderError
				require.ErrorAs(t, err, &order)
				require.Equal(t, tc.affected, order.Count)
				// Convergent moves share alpha. In the chain, only alpha
				// is also a moving old key; beta is not affected.
				require.Equal(t, graph.EdgeIdentity{
					From: from, To: alphaTarget, Kind: graph.EdgeCalls, FilePath: xmlFile, Line: 2,
				}, order.First)
				require.Zero(t, stats)
				require.Equal(t, before, rebuildEventSavedRows(t, b))
				return
			}

			var forward rebuildEventRows
			for _, reversed := range []bool{false, true} {
				if reversed {
					stored.Edges = slices.Clone(stored.Edges)
					slices.Reverse(stored.Edges[len(definitions):])
				}
				b := openRebuildGRPCBuild(t, stored, methods)
				withoutCalls := stored
				withoutCalls.Edges = definitions
				native := openRebuildTemporalOracle(t, withoutCalls, methods)
				var wantOrder []string
				for _, call := range stored.Edges[len(definitions):] {
					require.NoError(t, native.AddBatchChecked(nil, []*graph.Edge{call}))
					wantOrder = append(wantOrder, call.To)
				}
				var censusOrder []string
				for e := range native.EdgesByKind(graph.EdgeCalls) {
					if e.From == from {
						censusOrder = append(censusOrder, e.To)
					}
				}
				require.Equal(t, wantOrder, censusOrder, "reversed=%t", reversed)
				require.Equal(t, rebuildEventSQLiteRows(native), rebuildEventSavedRows(t, b))
				report := RunFrameworkSynthesizersWithSelection(native, selection)
				require.NoError(t, report.Err)
				require.EqualValues(t, 2, report.Total)
				want := rebuildEventSQLiteRows(native)
				var targets []string
				for _, e := range want.edges {
					if e.From == from && e.Kind == graph.EdgeCalls {
						targets = append(targets, e.To)
					}
				}
				require.Equal(t, []string{alphaTarget, betaTarget}, targets)

				stats, err := RunRebuildMyBatis(t.Context(), b, rebuildGRPCReceipts(stored, methods), selection)
				require.NoError(t, err, "reversed=%t", reversed)
				require.EqualValues(t, 2, stats.Stubs)
				require.EqualValues(t, 2, stats.Resolved)
				require.EqualValues(t, 2, stats.Moves)
				got := rebuildEventSavedRows(t, b)
				require.Equal(t, want, got, "reversed=%t", reversed)
				if reversed {
					require.Equal(t, forward, got)
				} else {
					forward = got
				}
			}
		})
	}
}

func TestRebuildMyBatisRestubAndIncumbent(t *testing.T) {
	const namespace, file, javaFile = "example.UserMapper", "mapper.xml", "UserMapper.java"
	const method = javaFile + "::findUser"
	const mapper = `<mapper namespace="example.UserMapper">
    <select id="findUser">SELECT 1</select>
    <select id="removed">SELECT 2</select>
</mapper>`
	const java = `public interface UserMapper {
    int findUser();
}`
	xml := rebuildMyBatisExtract(t, languages.NewMyBatisExtractor(), file, "", mapper)
	methods := rebuildMyBatisExtract(t, languages.NewJavaExtractor(), javaFile, "", java)
	var incumbent, restub graph.Edge
	for _, e := range xml.Edges {
		if e.Kind != graph.EdgeCalls {
			continue
		}
		switch e.From {
		case namespace + "::findUser":
			incumbent = *e
			incumbent.To = method
			incumbent.Meta = map[string]any{"fixture": "keep this payload"}
		case namespace + "::removed":
			restub = *e
			e.To = "OldMapper.java::removed"
			e.Origin = graph.OriginASTInferred
			e.Confidence = 0.7
			e.ConfidenceLabel = graph.ConfidenceLabelFor(graph.EdgeCalls, e.Confidence)
			e.Meta["synthesized_by"] = "mybatis"
			e.Meta["provenance"] = "heuristic"
			e.Meta["mybatis_resolution"] = e.Origin
			e.Meta["fixture"] = "preserve on restub"
			restub.Meta = map[string]any{"via": "mybatis.mapper", "mybatis_namespace": namespace,
				"mybatis_statement": "removed", "fixture": "preserve on restub"}
		}
	}
	require.NotEmpty(t, incumbent.From)
	require.NotEmpty(t, restub.From)
	xml.Edges = append(xml.Edges, &incumbent)
	b, native := openRebuildEventStores(t, xml, methods)
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthMyBatis})
	require.NoError(t, err)
	report := RunFrameworkSynthesizersWithSelection(native, selection)
	require.NoError(t, report.Err)
	require.EqualValues(t, 1, report.Total)
	stats, err := RunRebuildMyBatis(t.Context(), b, rebuildGRPCReceipts(xml, methods), selection)
	require.NoError(t, err)
	require.EqualValues(t, 2, stats.Moves)
	got := rebuildEventSavedRows(t, b)
	require.Equal(t, rebuildEventSQLiteRows(native), got)
	calls := slices.DeleteFunc(slices.Clone(got.edges), func(e *graph.Edge) bool { return e.Kind != graph.EdgeCalls })
	require.Equal(t, []*graph.Edge{&incumbent, &restub}, calls)
	raw, found, err := b.RebuildView().OriginalLinkRecorder(t.Context(), graph.EdgeIdentityFor(&restub))
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "OldMapper.java::removed", raw.To)
	_, found, err = b.RebuildView().OriginalLinkRecorder(t.Context(), graph.EdgeIdentityFor(&incumbent))
	require.NoError(t, err)
	require.False(t, found)
}
