package resolver

import (
	"fmt"
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
	stub := func(to, statement, fixture string) *graph.Edge {
		return &graph.Edge{From: from, To: to, Kind: graph.EdgeCalls, FilePath: xmlFile, Line: 2,
			Meta: map[string]any{"via": "mybatis.mapper", "mybatis_namespace": namespace,
				"mybatis_statement": statement, "fixture": fixture}}
	}

	for _, tc := range []struct {
		name                       string
		firstOld, firstStatement   string
		secondOld, secondStatement string
		incumbent                  string
		fillers                    int
		want, reversedWant         map[string]string
	}{
		{name: "commuting", firstOld: "stale::alpha", firstStatement: alpha, secondOld: "stale::beta", secondStatement: beta,
			want: map[string]string{alphaTarget: "left", betaTarget: "right"}},
		{name: "convergent", firstOld: "stale::alpha", firstStatement: alpha, secondOld: "stale::beta", secondStatement: alpha,
			want: map[string]string{alphaTarget: "left"}, reversedWant: map[string]string{alphaTarget: "right"}},
		{name: "convergent occupied", firstOld: "stale::alpha", firstStatement: alpha, secondOld: "stale::beta", secondStatement: alpha,
			incumbent: alphaTarget, want: map[string]string{alphaTarget: "incumbent"}},
		{name: "chain ascending", firstOld: "stale::alpha", firstStatement: alpha, secondOld: alphaTarget, secondStatement: beta,
			want: map[string]string{alphaTarget: "left", betaTarget: "right"}},
		{name: "chain descending", firstOld: "stale::beta", firstStatement: beta, secondOld: betaTarget, secondStatement: alpha,
			want: map[string]string{betaTarget: "left", alphaTarget: "right"}},
		{name: "chain occupied end", firstOld: "stale::alpha", firstStatement: alpha, secondOld: alphaTarget, secondStatement: beta,
			incumbent: betaTarget, want: map[string]string{alphaTarget: "left", betaTarget: "incumbent"}},
		{name: "cycle", firstOld: alphaTarget, firstStatement: beta, secondOld: betaTarget, secondStatement: alpha,
			want: map[string]string{betaTarget: "left", alphaTarget: "right"}},
		{name: "full window chain", firstOld: "stale::alpha", firstStatement: alpha, secondOld: alphaTarget, secondStatement: beta,
			fillers: graph.SQLiteReindexChunkSize - 2, want: map[string]string{alphaTarget: "left", betaTarget: "right"}},
		{name: "unresolved to occupied", firstOld: "unresolved::alpha", firstStatement: alpha, secondOld: "unresolved::beta", secondStatement: beta,
			incumbent: alphaTarget, want: map[string]string{alphaTarget: "incumbent", betaTarget: "right"}},
		{name: "split chain", firstOld: "stale::alpha", firstStatement: alpha, secondOld: alphaTarget, secondStatement: beta,
			fillers: graph.SQLiteReindexChunkSize - 1, want: map[string]string{betaTarget: "right"}, reversedWant: map[string]string{alphaTarget: "left", betaTarget: "right"}},
	} {
		for _, reversed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/reversed=%t", tc.name, reversed), func(t *testing.T) {
				first := stub(tc.firstOld, tc.firstStatement, "left")
				second := stub(tc.secondOld, tc.secondStatement, "right")
				wantPayloads := tc.want
				if reversed {
					first, second = second, first
					if tc.reversedWant != nil {
						wantPayloads = tc.reversedWant
					}
				}
				calls := []*graph.Edge{first}
				if tc.fillers > 0 {
					for i := 1; i <= tc.fillers; i++ {
						filler := stub("stale::filler", alpha, "filler")
						filler.Line += i
						calls = append(calls, filler)
					}
				}
				calls = append(calls, second)
				moves, resolved := len(calls), len(calls)
				if tc.fillers > 0 {
					// This marked Call is already at its target. It must not
					// advance the move window even though it has an exact receipt.
					stationary := stub(alphaTarget, alpha, "stationary")
					stationary.Line += graph.SQLiteReindexChunkSize
					calls = slices.Insert(calls, 1, stationary)
					resolved++
				}
				if tc.incumbent != "" {
					incumbent := stub(tc.incumbent, alpha, "incumbent")
					delete(incumbent.Meta, "via")
					calls = append(calls, incumbent)
				}
				stored := xml
				stored.Edges = append(slices.Clone(definitions), calls...)
				stored = roundTripRebuildRNBridgeFacts(t, stored)
				b := openRebuildGRPCBuild(t, stored, methods)
				withoutCalls := stored
				withoutCalls.Edges = definitions
				native := openRebuildTemporalOracle(t, withoutCalls, methods)
				require.NoError(t, native.AddBatchChecked(nil, stored.Edges[len(definitions):]))
				var census []graph.EdgeIdentity
				for e := range native.EdgesByKind(graph.EdgeCalls) {
					census = append(census, graph.EdgeIdentityFor(e))
				}
				var wantOrder []graph.EdgeIdentity
				for _, e := range calls {
					wantOrder = append(wantOrder, graph.EdgeIdentityFor(e))
				}
				require.Equal(t, wantOrder, census)
				require.Equal(t, rebuildEventSQLiteRows(native), rebuildEventSavedRows(t, b))
				require.Equal(t, resolved, ResolveMyBatisCalls(native))
				want := rebuildEventSQLiteRows(native)
				payloads := map[string]string{}
				for _, e := range want.edges {
					if e.Kind == graph.EdgeCalls && e.From == from && e.Line == 2 {
						payloads[e.To], _ = e.Meta["fixture"].(string)
					}
				}
				require.Equal(t, wantPayloads, payloads)

				stats, err := RunRebuildMyBatis(t.Context(), b, rebuildGRPCReceipts(stored, methods), selection)
				require.NoError(t, err)
				require.Equal(t, store_pebble.RebuildMyBatisStats{Stubs: uint64(resolved), Resolved: uint64(resolved), Moves: uint64(moves)}, stats)
				require.Equal(t, want, rebuildEventSavedRows(t, b))
			})
		}
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
}
