package store_pebble

import (
	"testing"

	"github.com/cockroachdb/pebble/v2"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

const (
	myBatisTestFile      = "mapper/UserMapper.xml"
	myBatisTestNamespace = "UserMapper"
	myBatisTestCaller    = myBatisTestNamespace + "::find"
	myBatisTestLocal     = "local/UserMapper.java::UserMapper.find"
)

func TestRebuildMyBatisCallerReceiptForAmbiguousWorkspace(t *testing.T) {
	for _, tc := range []struct {
		name, callerRepo, target string
		secondMethod, wantMove   bool
	}{
		{name: "workspace_unique", callerRepo: "mapper", target: myBatisTestLocal, wantMove: true},
		{name: "workspace_many", callerRepo: "mapper", secondMethod: true, target: myBatisTestPlaceholder("find")},
		{name: "repo_elects_winner", callerRepo: "local", secondMethod: true, target: myBatisTestLocal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBuild(t, FullProfile)
			// Start away from both possible destinations so workspace-many
			// still plans a move (back to the canonical placeholder).
			old := myBatisTestStub("retired/UserMapper.java::UserMapper.find", "find")
			xml := myBatisTestXML(t, tc.callerRepo, old)
			require.NoError(t, b.AddParsedFile(tc.callerRepo, xml))
			caller, err := b.pendingNode(myBatisTestCaller)
			require.NoError(t, err)
			require.NotNil(t, caller)
			require.Equal(t, tc.callerRepo, caller.RepoPrefix)

			// Only the caller declaration is missing from the proof stream.
			xml.Nodes, xml.File.NodeCount = nil, 0
			receipts := []ParseFacts{xml, myBatisTestMethod(t, b, "local")}
			if tc.secondMethod {
				receipts = append(receipts, myBatisTestMethod(t, b, "remote"))
			}
			before := rebuildFrameworkCustodyRows(t, b)
			stats, err := runTestRebuildMyBatis(t, b, receipts...)
			if tc.wantMove {
				require.NoError(t, err)
				require.Equal(t, RebuildMyBatisStats{Stubs: 1, Resolved: 1, Moves: 1}, stats)
				requireRebuildMyBatisMove(t, b, old, old, tc.target)
				return
			}
			requireRebuildMyBatisRefusal(t, err, old, tc.target)
			require.Zero(t, stats)
			require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
		})
	}
}

func TestRebuildMyBatisStaleCustody(t *testing.T) {
	for _, name := range []string{"stationary", "moving", "invalid_format"} {
		t.Run(name, func(t *testing.T) {
			b := newBuild(t, FullProfile)
			// The proved mover inherits the raw key now occupied by the
			// stationary synthetic stub. The marker, not To, sets its join.
			raw := myBatisTestStub(myBatisTestPlaceholder("missing"), "find")
			live := *raw
			live.To = myBatisTestPlaceholder("find")
			xml := myBatisTestXML(t, "local", raw)
			require.NoError(t, b.AddParsedFile("local", xml))
			require.NoError(t, b.ApplyLinkReindexes(t.Context(), []graph.EdgeReindex{{Edge: &live, OldTo: raw.To}}))

			stamped := myBatisTestStub(raw.To, "missing")
			if name == "moving" {
				// This stub must restub. Its old key has a matching raw
				// receipt, so downgrading its stale proof to raw would pass.
				stamped.To = "retired/UserMapper.java::UserMapper.missing"
				xml.Edges = append(xml.Edges, stamped)
			}
			proof := rebuildEdgeCustody{proof: rebuildProofReceiver}
			require.NoError(t, b.withWrites(func() error { return b.addEdgeWithCustody(stamped, proof) }))
			key := pendingEdgeKey(stamped)
			recorder := rebuildLinkRecorderKey(key)
			got, err := readRebuildEdgeCustody(b.pending, key)
			require.NoError(t, err)
			require.Equal(t, proof, got)

			if name == "invalid_format" {
				require.NoError(t, b.pending.Set(recorder, []byte{rebuildEdgeProofFormat}, pebble.NoSync))
			} else {
				// Mutate only non-key payload bytes; keep the valid custody record.
				changed := *stamped
				changed.Context = "payload changed without refreshing custody"
				value, err := encodeEdge(&changed)
				require.NoError(t, err)
				require.NoError(t, b.pending.Set(key, value, pebble.NoSync))
				saved, err := b.pendingEdge(key)
				require.NoError(t, err)
				require.Equal(t, &changed, saved)
				_, err = readRebuildEdgeCustody(b.pending, key)
				require.ErrorIs(t, err, ErrRebuildUnknownProvenance)
			}

			java := myBatisTestMethod(t, b, "local")
			before := rebuildFrameworkCustodyRows(t, b)
			stats, err := runTestRebuildMyBatis(t, b, xml, java)
			switch name {
			case "stationary":
				require.NoError(t, err)
				require.Equal(t, RebuildMyBatisStats{Stubs: 2, Resolved: 1, Moves: 1}, stats)
				requireRebuildMyBatisMove(t, b, &live, raw, myBatisTestLocal)
				after := rebuildFrameworkCustodyRows(t, b)
				for _, k := range [][]byte{key, pendingReverseKey(stamped, key), recorder} {
					require.NotEmpty(t, before[string(k)])
					require.Equal(t, before[string(k)], after[string(k)])
				}
			case "moving":
				requireRebuildMyBatisRefusal(t, err, stamped, myBatisTestPlaceholder("missing"))
			case "invalid_format":
				require.ErrorIs(t, err, ErrRebuildMyBatis)
				require.ErrorIs(t, err, errInvalidRecord)
			}
			if name != "stationary" {
				require.Zero(t, stats)
				require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
			}
		})
	}
}

func myBatisTestPlaceholder(statement string) string {
	return "unresolved::mybatis::" + myBatisTestNamespace + "::" + statement
}

func myBatisTestStub(to, statement string) *graph.Edge {
	return &graph.Edge{
		From: myBatisTestCaller, To: to, Kind: graph.EdgeCalls, FilePath: myBatisTestFile, Line: 2,
		Meta: map[string]any{"via": "mybatis.mapper", "mybatis_namespace": myBatisTestNamespace, "mybatis_statement": statement},
	}
}

func myBatisTestXML(t *testing.T, repo string, raw *graph.Edge) ParseFacts {
	t.Helper()
	caller := &graph.Node{ID: myBatisTestCaller, Name: "find", Kind: graph.KindMethod, FilePath: myBatisTestFile, Language: "mybatis"}
	return myBatisTestQualifiedFacts(t, repo, caller, raw)
}

func myBatisTestMethod(t *testing.T, b *BuildStore, repo string) ParseFacts {
	t.Helper()
	file := repo + "/UserMapper.java"
	method := &graph.Node{ID: file + "::UserMapper.find", Name: "find", Kind: graph.KindMethod,
		FilePath: file, Language: "java", Meta: map[string]any{"receiver": myBatisTestNamespace}}
	facts := myBatisTestQualifiedFacts(t, repo, method)
	require.NoError(t, b.AddParsedFile(repo, facts))
	return facts
}

func myBatisTestQualifiedFacts(t *testing.T, repo string, node *graph.Node, edges ...*graph.Edge) ParseFacts {
	t.Helper()
	wire, err := EncodeParseFacts(ParseFacts{
		EffectiveLanguage: node.Language, File: graph.FileMetaRow{FilePath: node.FilePath, NodeCount: 1},
		Nodes: []*graph.Node{node}, Edges: edges,
	})
	require.NoError(t, err)
	facts, err := DecodeParseFacts(wire)
	require.NoError(t, err)
	// RepoPrefix is workspace context, not part of the portable receipt.
	facts.Nodes[0].RepoPrefix = repo
	return facts
}

func runTestRebuildMyBatis(t *testing.T, b *BuildStore, facts ...ParseFacts) (RebuildMyBatisStats, error) {
	t.Helper()
	receipts := func(yield func(ParseFacts, error) bool) {
		for _, f := range facts {
			if !yield(f, nil) {
				return
			}
		}
	}
	return b.RebuildMyBatis(t.Context(), receipts, 1,
		func(n *graph.Node) string {
			receiver, _ := n.Meta["receiver"].(string)
			return receiver + "." + n.Name
		},
		func(e *graph.Edge) (string, string) {
			ns, _ := e.Meta["mybatis_namespace"].(string)
			stmt, _ := e.Meta["mybatis_statement"].(string)
			return ns, stmt
		},
		func(ns, stmt string) string { return ns + "." + stmt },
		func(e *graph.Edge, _, stmt, method string) bool {
			target := method
			if target == "" {
				target = myBatisTestPlaceholder(stmt)
			}
			if e.To == target {
				return false
			}
			e.To = target
			return true
		})
}

func requireRebuildMyBatisMove(t *testing.T, b *BuildStore, old, raw *graph.Edge, to string) {
	t.Helper()
	saved, err := b.pendingEdge(pendingEdgeKey(old))
	require.NoError(t, err)
	require.Nil(t, saved)
	want := *old
	want.To = to
	key := pendingEdgeKey(&want)
	saved, err = b.pendingEdge(key)
	require.NoError(t, err)
	require.Equal(t, &want, saved)
	custody, err := readRebuildEdgeCustody(b.pending, key)
	require.NoError(t, err)
	require.Equal(t, rebuildEdgeCustody{raw: pendingEdgeKey(raw)}, custody)
}

func requireRebuildMyBatisRefusal(t *testing.T, err error, old *graph.Edge, to string) {
	t.Helper()
	require.ErrorIs(t, err, ErrRebuildMyBatis)
	var refusal *RebuildUnknownProvenanceError
	require.ErrorAs(t, err, &refusal)
	want := graph.EdgeIdentityFor(old)
	want.To = to
	require.Equal(t, &RebuildUnknownProvenanceError{Count: 1, First: want}, refusal)
}
