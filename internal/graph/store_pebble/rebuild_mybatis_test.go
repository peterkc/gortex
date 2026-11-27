package store_pebble

import (
	"strings"
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
		secondRepo               string
		wantMove                 bool
	}{
		{name: "workspace_unique", callerRepo: "mapper", target: myBatisTestLocal, wantMove: true},
		{name: "workspace_many", callerRepo: "mapper", secondRepo: "remote", target: myBatisTestPlaceholder("find")},
		{name: "repo_elects_winner", callerRepo: "local", secondRepo: "remote", target: myBatisTestLocal},
		{name: "no_unique_repo", callerRepo: "local", secondRepo: "local", target: myBatisTestPlaceholder("find"), wantMove: true},
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
			if tc.secondRepo != "" {
				const file = "SecondMapper.java"
				second := &graph.Node{ID: file + "::UserMapper.find", Name: "find", FilePath: file, Language: "java", Kind: graph.KindMethod,
					Meta: map[string]any{"receiver": myBatisTestNamespace}}
				facts := myBatisTestQualifiedFacts(t, tc.secondRepo, second)
				require.NoError(t, b.AddParsedFile(tc.secondRepo, facts))
				receipts = append(receipts, facts)
			}
			before := rebuildFrameworkCustodyRows(t, b)
			stats, err := runTestRebuildMyBatis(t, b, receipts...)
			if tc.wantMove {
				require.NoError(t, err)
				want := RebuildMyBatisStats{Stubs: 1, Moves: 1}
				if tc.target == myBatisTestLocal {
					want.Resolved = 1
				}
				require.Equal(t, want, stats)
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
	return b.RebuildMyBatis(t.Context(), receipts, RebuildMyBatisRules{
		MethodKey: func(n *graph.Node) string {
			receiver, _ := n.Meta["receiver"].(string)
			return receiver + "." + n.Name
		},
		Stub: func(e *graph.Edge) (string, string) {
			if via, _ := e.Meta["via"].(string); via != "mybatis.mapper" {
				return "", ""
			}
			ns, _ := e.Meta["mybatis_namespace"].(string)
			stmt, _ := e.Meta["mybatis_statement"].(string)
			return ns, stmt
		},
		Join: func(ns, stmt string) string { return ns + "." + stmt },
		Rebind: func(e *graph.Edge, _, stmt, method string) bool {
			target := method
			if target == "" {
				target = myBatisTestPlaceholder(stmt)
			}
			if e.To == target {
				return false
			}
			e.To = target
			return true
		},
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

func TestRebuildMyBatisMoverReceiptFields(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*ParseFacts, *graph.Edge)
	}{
		{name: "exact"},
		{name: "from", change: func(_ *ParseFacts, e *graph.Edge) { e.From += "::other" }},
		{name: "to", change: func(_ *ParseFacts, e *graph.Edge) { e.To = myBatisTestLocal }},
		{name: "kind", change: func(_ *ParseFacts, e *graph.Edge) { e.Kind = graph.EdgeReferences }},
		{name: "edge_file", change: func(f *ParseFacts, e *graph.Edge) {
			// Keep receipt attribution valid so only the old edge key differs.
			e.FilePath = "other/UserMapper.xml"
			f.File.FilePath = e.FilePath
		}},
		{name: "line", change: func(_ *ParseFacts, e *graph.Edge) { e.Line++ }},
		{name: "receipt_file", change: func(f *ParseFacts, _ *graph.Edge) { f.File.FilePath = "other/UserMapper.xml" }},
		{name: "effective_language", change: func(f *ParseFacts, _ *graph.Edge) { f.EffectiveLanguage = "xml" }},
		{name: "legacy_no_effective_language", change: func(f *ParseFacts, _ *graph.Edge) { f.EffectiveLanguage = "" }},
		{name: "via", change: func(_ *ParseFacts, e *graph.Edge) { e.Meta["via"] = "other.mapper" }},
		{name: "namespace", change: func(_ *ParseFacts, e *graph.Edge) { e.Meta["mybatis_namespace"] = "other.UserMapper" }},
		{name: "statement", change: func(_ *ParseFacts, e *graph.Edge) { e.Meta["mybatis_statement"] = "other" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBuild(t, FullProfile)
			old := myBatisTestStub(myBatisTestPlaceholder("find"), "find")
			xml := myBatisTestXML(t, "local", old)
			require.NoError(t, b.AddParsedFile("local", xml))
			java := myBatisTestMethod(t, b, "local")

			// A unique Java candidate does not need a caller receipt.
			xml.Nodes, xml.File.NodeCount = nil, 0
			raw := myBatisTestStub(old.To, "find")
			if tc.change != nil {
				tc.change(&xml, raw)
			}
			xml.Edges = []*graph.Edge{raw}
			before := rebuildFrameworkCustodyRows(t, b)
			stats, err := runTestRebuildMyBatis(t, b, xml, java)
			if tc.change == nil {
				require.NoError(t, err)
				require.Equal(t, RebuildMyBatisStats{Stubs: 1, Resolved: 1, Moves: 1}, stats)
				requireRebuildMyBatisMove(t, b, old, old, myBatisTestLocal)
				return
			}
			requireRebuildMyBatisRefusal(t, err, old, myBatisTestLocal)
			require.Zero(t, stats)
			require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
		})
	}
}

func TestRebuildMyBatisMoverPriorRawCustody(t *testing.T) {
	for _, name := range []string{"raw_not_live", "duplicate_with_stationary"} {
		t.Run(name, func(t *testing.T) {
			b := newBuild(t, FullProfile)
			const statement = "missing"
			target := myBatisTestPlaceholder(statement)
			raw := myBatisTestStub("original::UserMapper.missing", statement)
			if name == "duplicate_with_stationary" {
				raw.To = target
			}
			live := *raw
			live.To = "previous::UserMapper.missing"
			xml := myBatisTestXML(t, "local", raw)
			require.NoError(t, b.AddParsedFile("local", xml))
			require.NoError(t, b.ApplyLinkReindexes(t.Context(), []graph.EdgeReindex{{Edge: &live, OldTo: raw.To}}))
			custody, err := readRebuildEdgeCustody(b.pending, pendingEdgeKey(&live))
			require.NoError(t, err)
			require.Equal(t, rebuildEdgeCustody{raw: pendingEdgeKey(raw)}, custody)

			receipt := xml
			if name == "duplicate_with_stationary" {
				// The reinserted original stays at the canonical placeholder.
				// Both live rows claim its raw receipt; only one is a mover.
				require.NoError(t, b.withWrites(func() error { return b.addEdge(raw) }))
			} else {
				receipt.Edges = []*graph.Edge{&live}
			}
			before := rebuildFrameworkCustodyRows(t, b)
			stats, err := runTestRebuildMyBatis(t, b, receipt)
			requireRebuildMyBatisRefusal(t, err, &live, target)
			require.Zero(t, stats)
			require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
			if name == "raw_not_live" {
				stats, err = runTestRebuildMyBatis(t, b, xml)
				require.NoError(t, err)
				require.Equal(t, RebuildMyBatisStats{Stubs: 1, Moves: 1}, stats)
				requireRebuildMyBatisMove(t, b, &live, raw, target)
			}
		})
	}
}

func TestRebuildMyBatisMoverCustodyRefusalBatch(t *testing.T) {
	b := newBuild(t, FullProfile)
	raw := myBatisTestStub("original::UserMapper.zulu", "zulu")
	untracked := *raw
	untracked.To = "previous::a_untracked"
	producer := myBatisTestStub("previous::b_producer", "bravo")
	missing := myBatisTestStub("previous::c_missing", "alpha")
	proved := myBatisTestStub("previous::0_proved", "aardvark")
	xml := myBatisTestXML(t, "local", raw)
	xml.Edges = append(xml.Edges, missing, proved)
	require.NoError(t, b.AddParsedFile("local", xml))
	b.ReindexEdges([]graph.EdgeReindex{{Edge: &untracked, OldTo: raw.To}})
	stamp := rebuildEdgeCustody{proof: rebuildProofReceiver}
	require.NoError(t, b.withWrites(func() error { return b.addEdgeWithCustody(producer, stamp) }))
	got, err := readRebuildEdgeCustody(b.pending, pendingEdgeKey(producer))
	require.NoError(t, err)
	require.Equal(t, stamp, got)
	got, err = readRebuildEdgeCustody(b.pending, pendingEdgeKey(&untracked))
	require.NoError(t, err)
	require.Equal(t, rebuildEdgeCustody{proof: rebuildProofUntrackedMove}, got)

	// Both stamps are intact. The live-key lookalikes cannot prove them as
	// raw receipts. The unstamped alpha mover has no receipt at all.
	xml.Edges = []*graph.Edge{&untracked, producer, proved}
	before := rebuildFrameworkCustodyRows(t, b)
	stats, err := runTestRebuildMyBatis(t, b, xml)
	require.ErrorIs(t, err, ErrRebuildMyBatis)
	var refusal *RebuildUnknownProvenanceError
	require.ErrorAs(t, err, &refusal)
	// Alpha has the last refused old key but the first refused destination.
	first := graph.EdgeIdentityFor(missing)
	first.To = myBatisTestPlaceholder("alpha")
	require.Equal(t, &RebuildUnknownProvenanceError{Count: 3, First: first}, refusal)
	require.Zero(t, stats)
	require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
}

func TestRebuildMyBatisMoverCallerReceiptFields(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*ParseFacts, *graph.Node)
	}{
		{name: "exact"},
		{name: "id", change: func(_ *ParseFacts, n *graph.Node) { n.ID += "::other" }},
		{name: "node_file", change: func(_ *ParseFacts, n *graph.Node) { n.FilePath = "other/UserMapper.xml" }},
		{name: "receipt_file", change: func(f *ParseFacts, n *graph.Node) {
			f.File.FilePath = "other/UserMapper.xml"
			n.FilePath = f.File.FilePath
		}},
		{name: "missing_effective_language", change: func(f *ParseFacts, _ *graph.Node) { f.EffectiveLanguage = "" }},
		{name: "node_language", change: func(_ *ParseFacts, n *graph.Node) { n.Language = "xml" }},
		{name: "kind", change: func(_ *ParseFacts, n *graph.Node) { n.Kind = graph.KindFunction }},
		{name: "repo", change: func(_ *ParseFacts, n *graph.Node) { n.RepoPrefix = "remote" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBuild(t, FullProfile)
			old := myBatisTestStub(myBatisTestPlaceholder("find"), "find")
			xml := myBatisTestXML(t, "local", old)
			require.NoError(t, b.AddParsedFile("local", xml))
			local := myBatisTestMethod(t, b, "local")
			remote := myBatisTestMethod(t, b, "remote")

			// Only the caller receipt can differ. The live caller resolves
			// the workspace tie, and the Call and both methods have exact receipts.
			caller := *xml.Nodes[0]
			callerFacts := xml
			callerFacts.Nodes, callerFacts.Edges = []*graph.Node{&caller}, nil
			xml.Nodes, xml.File.NodeCount = nil, 0
			if tc.change != nil {
				tc.change(&callerFacts, &caller)
			}
			before := rebuildFrameworkCustodyRows(t, b)
			stats, err := runTestRebuildMyBatis(t, b, xml, callerFacts, local, remote)
			if tc.change == nil {
				require.NoError(t, err)
				require.Equal(t, RebuildMyBatisStats{Stubs: 1, Resolved: 1, Moves: 1}, stats)
				requireRebuildMyBatisMove(t, b, old, old, myBatisTestLocal)
				return
			}
			requireRebuildMyBatisRefusal(t, err, old, myBatisTestLocal)
			require.Zero(t, stats)
			require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
		})
	}
}

func TestRebuildMyBatisCandidateCohortProof(t *testing.T) {
	for _, name := range []string{"unrelated_unproved", "loser_missing", "loser_repo", "loser_join_key", "loser_extractor_language"} {
		t.Run(name, func(t *testing.T) {
			b := newBuild(t, FullProfile)
			old := myBatisTestStub(myBatisTestPlaceholder("find"), "find")
			xml := myBatisTestXML(t, "local", old)
			require.NoError(t, b.AddParsedFile("local", xml))
			winner := myBatisTestMethod(t, b, "local")
			loser := myBatisTestMethod(t, b, "remote")

			// This method has the same name but is in a different join cohort.
			// Keep it live and omit its receipt in every case.
			const otherFile = "local/OtherMapper.java"
			other := &graph.Node{ID: otherFile + "::OtherMapper.find", Name: "find", Kind: graph.KindMethod,
				FilePath: otherFile, Language: "java", Meta: map[string]any{"receiver": "OtherMapper"}}
			require.NoError(t, b.AddParsedFile("local", myBatisTestQualifiedFacts(t, "local", other)))
			// The stationary stub makes the unrelated join live without making
			// its missing method receipt relevant to the move.
			stationary := myBatisTestStub(other.ID, "find")
			stationary.Meta["mybatis_namespace"] = "OtherMapper"
			b.AddEdge(stationary)

			// Change only the losing receipt, after installing the exact live candidates.
			switch name {
			case "loser_repo":
				loser.Nodes[0].RepoPrefix = "elsewhere"
			case "loser_join_key":
				loser.Nodes[0].Meta["receiver"] = "ChangedMapper"
			case "loser_extractor_language":
				loser.EffectiveLanguage = "kotlin"
			}
			receipts := []ParseFacts{xml, winner}
			if name != "loser_missing" {
				receipts = append(receipts, loser)
			}
			before := rebuildFrameworkCustodyRows(t, b)
			stats, err := runTestRebuildMyBatis(t, b, receipts...)
			if name == "unrelated_unproved" {
				require.NoError(t, err)
				require.Equal(t, RebuildMyBatisStats{Stubs: 2, Resolved: 2, Moves: 1}, stats)
				requireRebuildMyBatisMove(t, b, old, old, myBatisTestLocal)
				saved, err := b.pendingEdge(pendingEdgeKey(stationary))
				require.NoError(t, err)
				require.Equal(t, stationary, saved)
				return
			}
			requireRebuildMyBatisRefusal(t, err, old, myBatisTestLocal)
			require.Zero(t, stats)
			require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
		})
	}
}

func TestRebuildMyBatisCollectsOnlyLiveJoins(t *testing.T) {
	b := newBuild(t, FullProfile)
	old := myBatisTestStub(myBatisTestPlaceholder("find"), "find")
	// Two distinct live stubs request the same cohort.
	otherSite := *old
	otherSite.Line++
	xml := myBatisTestXML(t, "local", old)
	xml.Edges = append(xml.Edges, &otherSite)
	require.NoError(t, b.AddParsedFile("local", xml))
	local := myBatisTestMethod(t, b, "local").Nodes[0]
	remote := myBatisTestMethod(t, b, "remote").Nodes[0]
	const unusedFile = "local/OtherMapper.java"
	unused := &graph.Node{ID: unusedFile + "::OtherMapper.find", Name: "find", Kind: graph.KindMethod,
		FilePath: unusedFile, Language: "java", Meta: map[string]any{"receiver": "OtherMapper"}}
	require.NoError(t, b.AddParsedFile("local", myBatisTestQualifiedFacts(t, "local", unused)))

	ctx := t.Context()
	p := rebuildMyBatisPass{b: b}
	rules := RebuildMyBatisRules{
		Stub: func(e *graph.Edge) (string, string) {
			ns, _ := e.Meta["mybatis_namespace"].(string)
			stmt, _ := e.Meta["mybatis_statement"].(string)
			return ns, stmt
		},
		Join: func(ns, stmt string) string { return ns + "." + stmt },
	}
	require.NoError(t, p.capture(ctx, rules))
	require.EqualValues(t, 2, p.stats.Stubs)
	require.NoError(t, p.collect(ctx, func(n *graph.Node) string {
		receiver, _ := n.Meta["receiver"].(string)
		return rules.Join(receiver, n.Name)
	}))

	const join = "UserMapper.find"
	byColumn := func(col byte) []string {
		var keys []string
		require.NoError(t, pendingVisit(ctx, b.pending, rebuildMyBatisKey(col), func(r row) error {
			keys = append(keys, string(r.key))
			return nil
		}))
		return keys
	}
	require.Equal(t, []string{string(rebuildMyBatisKey('j', join))}, byColumn('j'))
	require.ElementsMatch(t, []string{
		string(rebuildMyBatisKey('c', join, local.ID)),
		string(rebuildMyBatisKey('c', join, remote.ID)),
	}, byColumn('c'))
	require.ElementsMatch(t, []string{
		string(rebuildMyBatisKey('e', join, "")),
		string(rebuildMyBatisKey('e', join, "local")),
		string(rebuildMyBatisKey('e', join, "remote")),
	}, byColumn('e'))
	require.ElementsMatch(t, []string{
		string(rebuildMyBatisKey('n', local.ID)),
		string(rebuildMyBatisKey('n', remote.ID)),
	}, byColumn('n'))
}

func TestRebuildMyBatisRejectsInvalidInputAndOccupiedScratch(t *testing.T) {
	b := newBuild(t, FullProfile)
	ctx := t.Context()
	receipts := func(func(ParseFacts, error) bool) { t.Fatal("refused pass must not read receipts") }
	rules := RebuildMyBatisRules{
		MethodKey: func(*graph.Node) string { return "" },
		Stub:      func(*graph.Edge) (string, string) { return "", "" },
		Join:      func(string, string) string { return "" },
		Rebind:    func(*graph.Edge, string, string, string) bool { return false },
	}

	before := rebuildFrameworkCustodyRows(t, b)
	stats, err := b.RebuildMyBatis(ctx, nil, rules)
	require.ErrorIs(t, err, ErrRebuildMyBatis)
	require.ErrorIs(t, err, ErrRebuildLinkInputSize)
	require.Zero(t, stats)
	missingRebind := rules
	missingRebind.Rebind = nil
	stats, err = b.RebuildMyBatis(ctx, receipts, missingRebind)
	require.ErrorIs(t, err, ErrRebuildMyBatis)
	require.ErrorIs(t, err, ErrRebuildLinkInputSize)
	require.Zero(t, stats)
	require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))

	key := rebuildMyBatisKey('s', "busy")
	require.NoError(t, b.pending.Set(key, []byte("sentinel"), pebble.NoSync))
	before = rebuildFrameworkCustodyRows(t, b)
	stats, err = b.RebuildMyBatis(ctx, receipts, rules)
	require.ErrorIs(t, err, ErrRebuildMyBatis)
	require.ErrorIs(t, err, errInvalidRecord)
	require.ErrorContains(t, err, "MyBatis scratch range occupied")
	require.Zero(t, stats)
	require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
}

func TestRebuildMyBatisCaptureCapsOnlyMapperCalls(t *testing.T) {
	for _, owned := range []bool{false, true} {
		b := newBuild(t, FullProfile)
		large := myBatisTestStub(myBatisTestPlaceholder("find"), "find")
		large.Context = strings.Repeat("x", rebuildMyBatisRowBytes)
		if !owned {
			delete(large.Meta, "via")
		}
		key := pendingEdgeKey(large)
		raw, err := encodeEdge(large)
		require.NoError(t, err)
		require.Greater(t, len(key)+len(raw), rebuildMyBatisRowBytes)
		require.NoError(t, b.pending.Set(key, raw, pebble.NoSync))
		before := rebuildFrameworkCustodyRows(t, b)
		p := rebuildMyBatisPass{b: b}
		err = p.capture(t.Context(), RebuildMyBatisRules{
			Stub: func(e *graph.Edge) (string, string) {
				if e.Meta["via"] != "mybatis.mapper" {
					return "", ""
				}
				return myBatisTestNamespace, "find"
			},
			Join: func(ns, stmt string) string { return ns + "." + stmt },
		})
		if owned {
			require.ErrorIs(t, err, ErrRebuildLinkInputSize)
			require.ErrorContains(t, err, "MyBatis input row")
		} else {
			require.NoError(t, err)
		}
		require.Zero(t, p.stats)
		require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
	}
}
