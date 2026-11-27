package store_pebble

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/cockroachdb/pebble/v2"
	"github.com/stretchr/testify/require"
	"github.com/zeebo/blake3"
	"github.com/zzet/gortex/internal/codegen"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestRelationsStagePublishesExactDiff(t *testing.T) {
	ctx := t.Context()
	worker, head, base, declared, raw, file, receipt := relationsStageFixture(t, FullProfile)
	pinned := stageRead(t, head)
	prior, err := pinned.Publication()
	require.NoError(t, err)
	require.Equal(t, StageFileGraph, prior.Stage)
	require.Zero(t, pinned.EdgeCount())
	relationsStageLevels(t, head, "file_graph")

	replacement := *declared
	replacement.Name, replacement.QualName, replacement.RepoPrefix, replacement.FilePath = "NewName", "new.NewName", "new", "new.go"
	replacement.Kind = graph.KindType
	synth := &graph.Node{ID: "synth::F", Name: "F", Kind: graph.KindFunction, RepoPrefix: "new", FilePath: "synth.go", Language: "go"}
	worker.AddBatch([]*graph.Node{&replacement, synth}, nil)
	linked := *raw
	linked.To = synth.ID
	worker.ReindexEdge(&linked, raw.To)
	unresolved := linked
	unresolved.To, unresolved.Line = "unresolved::StillExternal", linked.Line+1
	worker.AddEdge(&unresolved)

	tables, err := worker.PrepareRelations(ctx, t.TempDir(), base, nil)
	require.NoError(t, err)
	require.NotEmpty(t, tables.Files)
	_, err = head.PublishStage(ctx, tables)
	require.NoError(t, err)
	relationsStageLevels(t, head, "relations")
	current := stageRead(t, head)
	w, err := current.Publication()
	require.NoError(t, err)
	require.Equal(t, StageRelations, w.Stage)
	require.Equal(t, prior.Version(), w.Version())
	require.Equal(t, 2, current.NodeCount())
	require.Equal(t, 2, current.EdgeCount())
	require.Equal(t, &replacement, current.GetNode(declared.ID))
	require.Equal(t, synth, current.GetNode(synth.ID))
	require.Len(t, current.GetOutEdges(declared.ID), 2)
	require.Equal(t, []*graph.Edge{&linked}, current.GetInEdges(synth.ID))
	require.Equal(t, []*graph.Edge{&unresolved}, current.GetInEdges(unresolved.To))
	require.Empty(t, current.GetInEdges(raw.To))
	require.Empty(t, current.FindNodesByName(declared.Name))
	require.Nil(t, current.GetNodeByQualName(declared.QualName))
	require.Empty(t, current.GetRepoNodes(declared.RepoPrefix))
	require.Empty(t, current.GetFileNodes(declared.FilePath))
	require.Empty(t, slices.Collect(current.NodesByKind(graph.KindImport)))
	require.Equal(t, []*graph.Node{&replacement}, current.FindNodesByName(replacement.Name))
	require.Equal(t, &replacement, current.GetNodeByQualName(replacement.QualName))
	require.Len(t, current.GetRepoNodes(replacement.RepoPrefix), 2)
	require.Equal(t, []*graph.Node{&replacement}, current.GetFileNodes(replacement.FilePath))
	require.Equal(t, []*graph.Node{&replacement}, slices.Collect(current.NodesByKind(replacement.Kind)))
	seenFile, err := current.ManifestFile(file.Path)
	require.NoError(t, err)
	require.Equal(t, file, seenFile)
	seenReceipt, err := current.FileMetadata(declared.RepoPrefix, file.Path)
	require.NoError(t, err)
	require.Equal(t, receipt, seenReceipt)

	// Compare every witness family with the actual owner-ingested bytes.
	// In particular an old secondary key surviving ingest changes a digest.
	require.Equal(t, w.Families, relationsSavedWitness(t, current))
	require.Len(t, w.Families, 9)
	oldWitness, err := pinned.Publication()
	require.NoError(t, err)
	require.Equal(t, prior, oldWitness)
	require.Equal(t, prior.Families, relationsSavedWitness(t, pinned))
	require.Equal(t, []*graph.Node{declared}, pinned.FindNodesByName(declared.Name))
	require.Nil(t, pinned.GetNode(synth.ID))
	require.Zero(t, pinned.EdgeCount())
	_, err = head.PublishStage(ctx, tables)
	require.ErrorIs(t, err, ErrStageOrder)
}

func TestRelationsStageRemoveEdgesExactRejectsMismatchedIdentity(t *testing.T) {
	ctx := t.Context()
	worker, _, base, _, raw, _, _ := relationsStageFixture(t, FullProfile)
	wrong := *raw
	wrong.Line++
	data, err := encodeEdge(&wrong)
	require.NoError(t, err)
	key := pendingEdgeKey(raw)
	require.NoError(t, worker.pending.Set(key, data, pebble.NoSync))
	require.Zero(t, graph.RemoveEdgesExact(worker, []*graph.Edge{raw}))
	// The mutation must not erase either key or mask the bad record.
	stored, err := worker.pendingEdge(key)
	require.NoError(t, err)
	require.Equal(t, &wrong, stored)
	_, err = worker.RebuildView().Edge(ctx, graph.EdgeIdentityFor(raw))
	require.ErrorIs(t, err, ErrRebuildView)
	require.ErrorIs(t, err, errInvalidRecord)
	_, err = worker.PrepareRelations(ctx, t.TempDir(), base, nil)
	require.ErrorIs(t, err, ErrRelationsSeal)
	require.ErrorIs(t, err, errInvalidRecord)
}

func TestRelationsStageRemoveEdgesExact(t *testing.T) {
	ctx := t.Context()
	worker, head, base, declared, raw, file, receipt := relationsStageFixture(t, FullProfile)
	pinned := stageRead(t, head)
	prior, err := pinned.Publication()
	require.NoError(t, err)
	require.Equal(t, StageFileGraph, prior.Stage)
	priorRows := relationsSavedRows(t, pinned)
	require.NotEmpty(t, priorRows)

	target := &graph.Node{ID: "target.go::Target", Name: "Target", Kind: graph.KindFunction, RepoPrefix: "target", FilePath: "target.go", Language: "go"}
	worker.AddNode(target)
	require.True(t, worker.RemoveEdge(raw.From, raw.To, raw.Kind))
	first := *raw
	first.To, first.Origin, first.Confidence = target.ID, graph.OriginASTResolved, 0.95
	first.Meta = map[string]any{"route": "direct", "position": int64(1), "semantic": true}
	second := first
	second.Line, second.Origin, second.Confidence = first.Line+1, graph.OriginASTInferred, 0.65
	second.Meta = map[string]any{"route": "receiver", "position": int64(2), "semantic": false}
	nodes := []*graph.Node{declared, target}
	edges := []*graph.Edge{&first, &second}
	worker.AddBatch(nil, edges)
	native := graph.New()
	native.AddBatch(nodes, edges)

	view := worker.RebuildView()
	var before []*graph.Edge
	require.NoError(t, view.VisitEdges(ctx, func(e *graph.Edge) error {
		before = append(before, e)
		return nil
	}))
	require.ElementsMatch(t, edges, before)
	require.ElementsMatch(t, native.AllEdges(), before)
	require.ElementsMatch(t, nodes, native.AllNodes())
	var selected *graph.Edge
	for _, e := range before {
		if e.Line == first.Line {
			selected = e
		}
	}
	require.Equal(t, &first, selected)
	absent := first
	absent.Line = second.Line + 1
	remove := []*graph.Edge{nil, &absent, selected, selected}
	require.Equal(t, 1, graph.RemoveEdgesExact(native, remove))
	require.Equal(t, 1, graph.RemoveEdgesExact(worker, remove))
	require.Zero(t, graph.RemoveEdgesExact(native, remove))
	require.Zero(t, graph.RemoveEdgesExact(worker, remove))

	var after []*graph.Edge
	require.NoError(t, view.VisitEdges(ctx, func(e *graph.Edge) error {
		after = append(after, e)
		return nil
	}))
	want := []*graph.Edge{&second}
	require.Equal(t, want, after)
	require.Equal(t, native.AllEdges(), after)
	var privateNodes []*graph.Node
	require.NoError(t, view.VisitNodes(ctx, func(n *graph.Node) error {
		privateNodes = append(privateNodes, n)
		return nil
	}))
	require.ElementsMatch(t, nodes, privateNodes)

	tables, err := worker.PrepareRelations(ctx, t.TempDir(), base, nil)
	require.NoError(t, err)
	_, err = head.PublishStage(ctx, tables)
	require.NoError(t, err)
	current := stageRead(t, head)
	w, err := current.Publication()
	require.NoError(t, err)
	require.Equal(t, StageRelations, w.Stage)
	require.Equal(t, prior.Version(), w.Version())
	require.Equal(t, w.Families, relationsSavedWitness(t, current))
	require.ElementsMatch(t, nodes, current.AllNodes())
	require.ElementsMatch(t, native.AllNodes(), current.AllNodes())
	require.Equal(t, want, current.AllEdges())
	require.Equal(t, native.GetOutEdges(declared.ID), current.GetOutEdges(declared.ID))
	require.Equal(t, want, current.GetOutEdges(declared.ID))
	require.Equal(t, native.GetInEdges(target.ID), current.GetInEdges(target.ID))
	require.Equal(t, want, current.GetInEdges(target.ID))
	require.Equal(t, want, slices.Collect(current.EdgesByKind(first.Kind)))
	require.Equal(t, want, slices.Collect(native.EdgesByKind(first.Kind)))
	require.Equal(t, want, current.GetRepoEdges(declared.RepoPrefix))
	require.Equal(t, want, native.GetRepoEdges(declared.RepoPrefix))
	require.Empty(t, current.GetRepoEdges(target.RepoPrefix))
	require.Empty(t, current.GetInEdges(raw.To))
	require.Equal(t, 2, current.NodeCount())
	require.Equal(t, 1, current.EdgeCount())
	require.Equal(t, native.EdgeCount(), current.EdgeCount())
	require.Equal(t, 1, current.count("out total", declared.ID))
	require.Equal(t, 1, current.count("in total", target.ID))
	require.Equal(t, 1, current.count("out kind", declared.ID, string(first.Kind)))
	require.Equal(t, 1, current.count("in kind", target.ID, string(first.Kind)))
	require.Equal(t, 1, current.count("edge kind", string(first.Kind)))
	require.Equal(t, 1, current.count("repo edges", declared.RepoPrefix))
	require.Zero(t, current.count("repo edges", target.RepoPrefix))
	seenFile, err := current.ManifestFile(file.Path)
	require.NoError(t, err)
	require.Equal(t, file, seenFile)
	seenReceipt, err := current.FileMetadata(declared.RepoPrefix, file.Path)
	require.NoError(t, err)
	require.Equal(t, receipt, seenReceipt)
	require.NoError(t, current.ReadError())

	oldWitness, err := pinned.Publication()
	require.NoError(t, err)
	require.Equal(t, prior, oldWitness)
	require.Equal(t, priorRows, relationsSavedRows(t, pinned))
	require.Equal(t, prior.Families, relationsSavedWitness(t, pinned))
	require.Equal(t, declared, pinned.GetNode(declared.ID))
	require.Nil(t, pinned.GetNode(target.ID))
	require.Zero(t, pinned.EdgeCount())
	require.NoError(t, pinned.ReadError())
}

func TestRelationsStageRefusesUnreadableTable(t *testing.T) {
	ctx := t.Context()
	worker, head, base, declared, raw, _, _ := relationsStageFixture(t, FullProfile)
	prior := stageRead(t, head)
	require.NoError(t, prior.RequireStage(StageFileGraph))
	priorRows := relationsSavedRows(t, prior)

	target := &graph.Node{ID: "synth::F", Kind: graph.KindFunction, Name: "F", FilePath: "synth.go", Language: "go"}
	worker.AddBatch([]*graph.Node{target}, nil)
	linked := *raw
	linked.To = target.ID
	worker.ReindexEdge(&linked, raw.To)
	tables, err := worker.PrepareRelations(ctx, t.TempDir(), base, nil)
	require.NoError(t, err)
	require.NotEmpty(t, tables.Files)

	// A regular file passes the owner's path checks. Ingest must refuse the
	// whole input, including the valid tables and the publication witness.
	const unreadable = "unreadable.sst"
	require.NoError(t, os.WriteFile(filepath.Join(tables.Dir, unreadable), []byte("not an SST"), 0o600))
	tables.Files = append(tables.Files, unreadable)
	digest, err := head.PublishStage(ctx, tables)
	require.ErrorContains(t, err, "file size is too small")
	require.Equal(t, HeadDigest{}, digest)

	current := stageRead(t, head)
	require.NoError(t, current.RequireStage(StageFileGraph))
	require.ErrorAs(t, current.RequireStage(StageRelations), new(*ErrStagePending))
	require.Equal(t, priorRows, relationsSavedRows(t, current))
	require.Equal(t, declared, current.GetNode(declared.ID))
	require.NoError(t, current.ReadError())
}

func TestRelationsStageExternalEndpointEvidence(t *testing.T) {
	for _, profile := range []Profile{DefaultProfile, FullProfile} {
		t.Run(string(profile), func(t *testing.T) {
			for _, mode := range []string{"import", "extern", "go returns", "go throws", "wrong import target", "stale import payload", "no receipt", "incumbent", "unsupported producer", "stale go payload", "wrong go origin", "wrong go language", "wrong receipt path", "wrong go import spec", "go import", "wrong extern spec", "extern symbol", "raw site"} {
				t.Run(mode, func(t *testing.T) {
					ctx := t.Context()
					worker, head, base, _, _, _, _ := relationsStageFixture(t, profile)
					const file = "receipt.go"
					n := &graph.Node{ID: "receipt", Kind: graph.KindFunction, Name: "receipt", FilePath: file}
					raw := &graph.Edge{From: n.ID, To: "unresolved::import::pkg/sub::Export", Kind: graph.EdgeImports,
						FilePath: file, Line: 2, Origin: graph.OriginASTInferred}
					lang, spec := "typescript", "@scope/pkg/sub::Export"
					switch mode {
					case "extern", "wrong extern spec", "extern symbol":
						lang, spec = "go", "outside"
						raw.To, raw.Kind = "unresolved::extern::outside", graph.EdgeCalls
						if mode == "wrong extern spec" {
							spec = "other"
						} else if mode == "extern symbol" {
							raw.To, spec = "unresolved::extern::outside::F", "outside::F"
						}
					case "go import", "wrong go import spec":
						lang = "go"
						if mode == "go import" {
							spec = "pkg/sub::Export"
						}
					case "go returns", "go throws", "unsupported producer", "stale go payload", "wrong go origin", "wrong go language", "wrong receipt path":
						lang, spec = "go", ""
						raw.To = "external::error"
						switch mode {
						case "go returns", "stale go payload", "wrong go origin", "wrong go language", "wrong receipt path":
							raw.Kind = graph.EdgeReturns
							raw.Meta = map[string]any{"position": 1}
							switch mode {
							case "wrong go origin":
								raw.Origin = graph.OriginASTResolved
							case "wrong go language":
								lang = "typescript"
							case "wrong receipt path":
								raw.FilePath = "elsewhere.go"
							}
						case "go throws":
							raw.Kind = graph.EdgeThrows
						default:
							raw.Kind = graph.EdgeReferences
						}
					case "no receipt":
						lang = ""
					}
					require.NoError(t, worker.AddParsedFile("", ParseFacts{EffectiveLanguage: lang,
						File: graph.FileMetaRow{FilePath: file, NodeCount: 1}, Nodes: []*graph.Node{n}, Edges: []*graph.Edge{raw}}))
					want := *raw
					if spec != "" {
						var ok bool
						want.To, ok = RebuildExternalImportTarget(spec)
						require.True(t, ok)
						if mode == "incumbent" {
							worker.AddEdge(&want)
						}
						moves := []graph.EdgeReindex{{Edge: &want, OldTo: raw.To, OldKind: raw.Kind}}
						require.NoError(t, worker.ApplyLinkReindexes(ctx, moves))
						require.NoError(t, worker.RecordRebuildExternalMoves(ctx, moves, map[*graph.Edge]string{&want: spec}))
					}
					switch mode {
					case "wrong import target":
						old := want.To
						want.To = "external::@scope/pkg/sub::Wrong"
						require.NoError(t, worker.ApplyLinkReindexes(ctx, []graph.EdgeReindex{{Edge: &want, OldTo: old, OldKind: want.Kind}}))
					case "raw site":
						other := *raw
						other.Line++
						key := pendingEdgeKey(&other)
						require.NoError(t, worker.pending.Set(rebuildExternalEvidenceKey('r', key), []byte(lang), pebble.NoSync))
						require.NoError(t, worker.pending.Set(rebuildLinkRecorderKey(pendingEdgeKey(&want)), key, pebble.NoSync))
					case "stale import payload", "stale go payload":
						want.Context = "changed after emitter"
						val, err := encodeEdge(&want)
						require.NoError(t, err)
						require.NoError(t, worker.pending.Set(pendingEdgeKey(&want), val, pebble.NoSync))
					}
					tables, err := worker.PrepareRelations(ctx, t.TempDir(), base, nil)
					switch mode {
					case "import", "extern", "go returns", "go throws", "go import":
						require.NoError(t, err)
						_, err = head.PublishStage(ctx, tables)
						require.NoError(t, err)
						current := stageRead(t, head)
						require.Nil(t, current.GetNode(want.To))
						require.Equal(t, 1, current.count("in kind", want.To, string(want.Kind)))
						require.Equal(t, 1, current.count("external import endpoints"))
						if profile == FullProfile || want.Kind != graph.EdgeReturns {
							require.Equal(t, []*graph.Edge{&want}, current.GetInEdges(want.To))
						}
					default:
						require.ErrorIs(t, err, ErrRelationsSeal)
						var refused *RebuildUnknownProvenanceError
						require.ErrorAs(t, err, &refused)
						require.Equal(t, graph.EdgeIdentityFor(&want), refused.First)
						require.Empty(t, tables)
						require.NoError(t, stageRead(t, head).RequireStage(StageFileGraph))
					}
				})
			}
		})
	}
}

func TestRelationsStageGoArgumentSourceReceipt(t *testing.T) {
	keys := graph.UnresolvedNameCandidateIDsForName("Field", "")
	for _, tc := range []struct {
		name, source, origin, extractor string
		kind                            graph.EdgeKind
		keep                            bool
	}{
		{"selector", keys[1], graph.OriginASTInferred, "go", graph.EdgeArgOf, true},
		{"bare", keys[0], graph.OriginASTInferred, "go", graph.EdgeArgOf, true},
		{"foreign receipt", keys[1], graph.OriginASTInferred, "typescript", graph.EdgeArgOf, false},
		{"no receipt", keys[1], graph.OriginASTInferred, "", graph.EdgeArgOf, false},
		{"wrong kind", keys[1], graph.OriginASTInferred, "go", graph.EdgeReferences, false},
		{"wrong origin", keys[1], graph.OriginASTResolved, "go", graph.EdgeArgOf, false},
		{"malformed", keys[1] + ".Field", graph.OriginASTInferred, "go", graph.EdgeArgOf, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			worker, head, base, declared, raw, file, _ := relationsStageFixture(t, FullProfile)
			edge := &graph.Edge{From: tc.source, To: declared.ID, Kind: tc.kind, FilePath: file.Path, Line: raw.Line,
				Origin: tc.origin, Meta: map[string]any{"arg_position": int64(0)}}
			worker.AddEdge(edge)
			tables, err := worker.PrepareRelations(ctx, t.TempDir(), base, func(key string) string {
				if key == file.Path {
					return tc.extractor
				}
				return ""
			})
			if !tc.keep {
				require.ErrorIs(t, err, ErrRelationsSeal)
				require.ErrorContains(t, err, "dangling relations endpoint")
				require.Empty(t, tables)
				saved := stageRead(t, head)
				w, err := saved.Publication()
				require.NoError(t, err)
				require.Equal(t, StageFileGraph, w.Stage)
				require.Zero(t, saved.EdgeCount())
				return
			}
			require.NoError(t, err)
			_, err = head.PublishStage(ctx, tables)
			require.NoError(t, err)
			saved := stageRead(t, head)
			require.Nil(t, saved.GetNode(tc.source))
			require.Equal(t, []*graph.Edge{edge}, saved.GetInEdges(declared.ID))
			require.Equal(t, 1, saved.count("go dataflow source endpoints"))
		})
	}
}

func TestRelationsStageRefusesDanglingOrProxyEndpoint(t *testing.T) {
	for _, tc := range []struct {
		label     string
		from      string
		to        string
		proxyNode bool
		message   string
	}{
		{label: "unknown source", from: "missing.go::Missing", message: "dangling relations endpoint"},
		{label: "unknown target", to: "missing.go::Missing", message: "dangling relations endpoint"},
		{label: "empty external payload", to: "external::", message: "malformed external import endpoint"},
		{label: "external source is not a node", from: "external::outside", message: "dangling relations endpoint"},
		{label: "proxy ID", to: graph.ProxyNodeID("remote", "pkg.go::F"), message: "proxy edge cannot be published"},
		{label: "proxy node", to: "local.go::Proxy", proxyNode: true, message: "proxy relations endpoint"},
	} {
		t.Run(tc.label, func(t *testing.T) {
			worker, head, base, declared, raw, _, _ := relationsStageFixture(t, FullProfile)
			pinned := stageRead(t, head)
			prior, err := pinned.Publication()
			require.NoError(t, err)
			priorRows := relationsSavedRows(t, pinned)
			require.NotEmpty(t, priorRows)
			if tc.proxyNode {
				worker.AddNode(&graph.Node{ID: tc.to, Name: "Proxy", Kind: graph.KindFunction, Origin: "remote:remote", Stub: true})
			}
			linked := *raw
			if tc.from == "" {
				linked.To = tc.to
				worker.ReindexEdge(&linked, raw.To)
			} else {
				linked.From = tc.from
				linked.To = declared.ID
				worker.AddEdge(&linked)
			}
			tables, err := worker.PrepareRelations(t.Context(), t.TempDir(), base, nil)
			require.ErrorContains(t, err, tc.message)
			require.ErrorIs(t, err, ErrRelationsSeal)
			require.ErrorIs(t, err, errInvalidRecord)
			require.Empty(t, tables)

			current := stageRead(t, head)
			w, err := current.Publication()
			require.NoError(t, err)
			require.Equal(t, prior, w)
			require.Equal(t, priorRows, relationsSavedRows(t, current))
			require.Equal(t, prior.Families, relationsSavedWitness(t, current))
			require.Equal(t, 1, current.NodeCount())
			require.Equal(t, declared, current.GetNode(declared.ID))
			require.Zero(t, current.EdgeCount())
		})
	}
}

func TestRelationsStageProjectsOmittedEndpoints(t *testing.T) {
	for _, profile := range []Profile{DefaultProfile, FullProfile} {
		t.Run(string(profile), func(t *testing.T) {
			ctx := t.Context()
			worker, head, base, declared, raw, _, _ := relationsStageFixture(t, profile)
			peer := &graph.Node{ID: "peer", Name: "peer", Kind: graph.KindFunction, RepoPrefix: declared.RepoPrefix, FilePath: declared.FilePath}
			nodes := []*graph.Node{peer}
			edges := []*graph.Edge{
				raw, // A real unresolved target is not an omitted declaration.
				{From: declared.ID, To: peer.ID, Kind: graph.EdgeCalls, FilePath: declared.FilePath, Line: raw.Line + 1},
				{From: peer.ID, To: declared.ID, Kind: graph.EdgeContains, FilePath: declared.FilePath, Line: raw.Line + 2},
			}
			for _, kind := range []graph.NodeKind{graph.KindLocal, graph.KindParam, graph.KindGenericParam, graph.KindClosure} {
				n := &graph.Node{ID: string(kind), Name: string(kind), Kind: kind, RepoPrefix: declared.RepoPrefix, FilePath: declared.FilePath}
				nodes = append(nodes, n)
				edges = append(edges,
					&graph.Edge{From: n.ID, To: declared.ID, Kind: graph.EdgeCalls, FilePath: declared.FilePath},
					&graph.Edge{From: declared.ID, To: n.ID, Kind: graph.EdgeReferences, FilePath: declared.FilePath},
					&graph.Edge{From: n.ID, To: peer.ID, Kind: graph.EdgeContains, FilePath: declared.FilePath},
				)
			}
			worker.AddBatch(nodes, edges[1:])
			tables, err := worker.PrepareRelations(ctx, t.TempDir(), base, nil)
			require.NoError(t, err)
			_, err = head.PublishStage(ctx, tables)
			require.NoError(t, err)
			current := stageRead(t, head)
			w, err := current.Publication()
			require.NoError(t, err)
			require.Equal(t, profile, w.Profile)
			require.Equal(t, w.Families, relationsSavedWitness(t, current))

			// Build independent controls from an explicit fixture cut. The
			// off-group edge between retained nodes still contributes a count.
			keptNodes, keptEdges, visibleEdges := nodes[:1], edges[:3], edges[:2]
			if profile == FullProfile {
				keptNodes, keptEdges, visibleEdges = nodes, edges, edges
			}
			native, err := store_sqlite.Open(filepath.Join(t.TempDir(), "oracle.sqlite"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, native.Close()) })
			native.AddBatch(append([]*graph.Node{declared}, keptNodes...), visibleEdges)
			require.Equal(t, native.NodeCount(), current.NodeCount())
			require.Equal(t, native.EdgeCount(), current.EdgeCount())
			for _, id := range []string{declared.ID, peer.ID, raw.To} {
				require.ElementsMatch(t, native.GetInEdges(id), current.GetInEdges(id))
				require.ElementsMatch(t, native.GetOutEdges(id), current.GetOutEdges(id))
			}
			oracle := newBuild(t, profile)
			oracle.AddBatch(append([]*graph.Node{declared}, keptNodes...), keptEdges)
			require.NoError(t, oracle.Publish(ctx))
			var wantRows, gotRows []row
			for _, r := range relationsSavedRows(t, snapshot(t, oracle)) {
				if r.key[0] == familyEdge {
					wantRows = append(wantRows, r)
				}
			}
			for _, r := range relationsSavedRows(t, current) {
				if r.key[0] == familyEdge {
					gotRows = append(gotRows, r)
				}
			}
			require.Equal(t, wantRows, gotRows)
			adj, err := current.ReadKindAdjacency(ctx, peer.ID, graph.DirectionForward, []graph.EdgeKind{graph.EdgeCalls})
			require.NoError(t, err)
			require.Equal(t, 1, adj.Total)
			require.Empty(t, adj.Edges)
			if profile == DefaultProfile {
				for _, n := range nodes[1:] {
					require.Nil(t, current.GetNode(n.ID))
					require.Zero(t, current.count("out total", n.ID))
					require.Zero(t, current.count("in total", n.ID))
					require.Zero(t, current.count("off incoming owner", declared.ID, n.RepoPrefix))
				}
			}
			privateNodes, privateEdges := 0, 0
			require.NoError(t, worker.RebuildView().VisitNodes(ctx, func(*graph.Node) error { privateNodes++; return nil }))
			require.NoError(t, worker.RebuildView().VisitEdges(ctx, func(*graph.Edge) error { privateEdges++; return nil }))
			require.Equal(t, len(nodes)+1, privateNodes)
			require.Equal(t, len(edges), privateEdges)
		})
	}
}

func TestRelationsStageRefusesIncompleteRetainedBase(t *testing.T) {
	worker, head, base, _, _, _, _ := relationsStageFixture(t, FullProfile)
	prior, err := stageRead(t, head).Publication()
	require.NoError(t, err)
	require.NotEmpty(t, base.paths)
	base.paths = base.paths[:len(base.paths)-1]
	tables, err := worker.PrepareRelations(t.Context(), t.TempDir(), base, nil)
	require.ErrorIs(t, err, ErrRelationsSeal)
	require.ErrorContains(t, err, "retained file-graph rows differ from their witness")
	require.Empty(t, tables)
	current := stageRead(t, head)
	require.Equal(t, prior.Families, relationsSavedWitness(t, current))
	require.Zero(t, current.EdgeCount())
}

func relationsStageFixture(t *testing.T, profile Profile) (*BuildStore, *Head, FileGraphBase, *graph.Node, *graph.Edge, ManifestFile, graph.FileMetaRow) {
	t.Helper()
	return relationsStageFixtureRevision(t, profile, 0)
}

func relationsStageFixtureRevision(t *testing.T, profile Profile, revision uint32) (*BuildStore, *Head, FileGraphBase, *graph.Node, *graph.Edge, ManifestFile, graph.FileMetaRow) {
	t.Helper()
	ctx := t.Context()
	dir := t.TempDir()
	worker, err := OpenBuild(filepath.Join(dir, "working"), profile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, worker.Close()) })
	head, err := OpenHead(filepath.Join(dir, "head"), testHeadIncarnation, profile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, head.Close()) })
	file := ManifestFile{Path: "old.go", Language: "go", Size: 17, Kind: 'f', Content: HeadDigest{3}, Extractor: HeadDigest{4}}
	w := Witness{Input: HeadDigest{1}, Recipe: HeadDigest{2}, Profile: profile, Stage: StageManifest, PlatformCatalogRevision: revision}
	manifest, err := PrepareManifest(ctx, t.TempDir(), []ManifestFile{file}, w, testHeadIncarnation)
	require.NoError(t, err)
	_, err = head.PublishStage(ctx, manifest)
	require.NoError(t, err)
	declared := &graph.Node{ID: "old.go::OldName", Name: "OldName", QualName: "old.OldName", Kind: graph.KindImport, RepoPrefix: "old", FilePath: file.Path, Language: "go"}
	raw := &graph.Edge{From: declared.ID, To: "unresolved::Raw", Kind: graph.EdgeCalls, FilePath: file.Path, Line: 2}
	receipt := graph.FileMetaRow{FilePath: file.Path, Size: 17, ContentHash: "content", NodeCount: 1, Errors: "[1:7]"}
	require.NoError(t, worker.AddParsedFile(declared.RepoPrefix, ParseFacts{Nodes: []*graph.Node{declared}, Edges: []*graph.Edge{raw}, File: receipt}))
	stage, err := worker.PrepareFileGraph(ctx, t.TempDir(), manifest.Witness, testHeadIncarnation)
	require.NoError(t, err)
	require.NotEmpty(t, stage.Files)
	base, err := worker.RetainFileGraph(ctx, stage)
	require.NoError(t, err)
	_, err = head.PublishStage(ctx, stage)
	require.NoError(t, err)
	for _, name := range stage.Files {
		_, err := os.Stat(filepath.Join(stage.Dir, name))
		require.ErrorIs(t, err, os.ErrNotExist, "owner ingest must consume the outward name")
	}
	return worker, head, base, declared, raw, file, receipt
}

func relationsSavedRows(t *testing.T, snap *Snapshot) []row {
	t.Helper()
	var rows []row
	require.NoError(t, scan(t.Context(), snap, nil, func(key, value []byte) (bool, error) {
		rows = append(rows, row{bytes.Clone(key), bytes.Clone(value)})
		return true, nil
	}))
	return rows
}

func relationsSavedWitness(t *testing.T, snap *Snapshot) []FamilyWitness {
	t.Helper()
	var families []FamilyWitness
	hash := blake3.New()
	err := scan(t.Context(), snap, nil, func(key, value []byte) (bool, error) {
		if len(key) == 1 && key[0] == familyWitness {
			return true, nil
		}
		if len(families) == 0 || key[0] != families[len(families)-1].Family {
			families = append(families, FamilyWitness{Family: key[0]})
			hash.Reset()
		}
		writeWitnessRow(hash, key, value)
		last := &families[len(families)-1]
		last.Count++
		last.Digest = HeadDigest(hash.Sum(nil))
		return true, nil
	})
	require.NoError(t, err)
	return families
}

func relationsStageLevels(t *testing.T, head *Head, stage string) {
	t.Helper()
	var tables []int64
	for _, level := range head.db.Metrics().Levels {
		tables = append(tables, int64(level.TablesCount))
	}
	t.Logf("%s: Pebble tables by level: %v", stage, tables)
}

func TestRelationsStageCodegenEndpoints(t *testing.T) {
	for _, profile := range []Profile{DefaultProfile, FullProfile} {
		for _, mode := range []string{"source", "tool", "unknown", "annotation", "member", "wrong target", "changed payload", "not from receipt"} {
			t.Run(string(profile)+"/"+mode, func(t *testing.T) {
				ctx := t.Context()
				worker, head, base, _, _, _, _ := relationsStageFixture(t, profile)
				const file = "generated.java"
				fileNode := &graph.Node{ID: file, Kind: graph.KindFile, Name: file, FilePath: file}
				nodes := []*graph.Node{fileNode}
				header := "// Code generated by protoc. DO NOT EDIT.\n// source: api/schema.proto\n"
				switch mode {
				case "tool":
					header = "// @generated by protoc\n"
				case "unknown":
					header = "// @generated\n"
				}
				edges := codegen.BuildGraphArtifacts(file, codegen.Scan([]byte(header)))
				if mode == "annotation" || mode == "member" {
					host := &graph.Node{ID: file + "::Host", Name: "Host", FilePath: file, Kind: graph.KindType}
					anno := &graph.Node{ID: file + "::Data", Name: "Data", FilePath: file, Kind: graph.KindType, Meta: map[string]any{"kind": "annotation"}}
					nodes = append(nodes, host, anno)
					annotation := &graph.Edge{From: host.ID, To: anno.ID, Kind: graph.EdgeAnnotated, FilePath: file}
					edges, _ = codegen.MarkAnnotatedGenerated(nodes, []*graph.Edge{annotation})
					if mode == "member" {
						field := &graph.Node{ID: host.ID + ".name", Name: "name", FilePath: file, Kind: graph.KindField, Meta: map[string]any{"receiver": "Host", "field_type": "String"}}
						nodes = append(nodes, field)
						members, emitted := codegen.MaterializeLombokAccessors(nodes)
						nodes = append(nodes, members...)
						edges = emitted
					}
				}
				require.NotEmpty(t, edges)
				if mode == "wrong target" {
					edges[0].To = "external::generator-tool:wrong"
				}
				f := ParseFacts{EffectiveLanguage: "java", Nodes: nodes, Edges: edges,
					File: graph.FileMetaRow{FilePath: file, NodeCount: len(nodes)}}
				if mode == "not from receipt" {
					f.Edges = nil
				}
				require.NoError(t, worker.AddParsedFile("", f))
				if mode == "not from receipt" {
					worker.AddEdge(edges[0])
				}
				if mode == "changed payload" {
					edges[0].Context = "changed"
					worker.AddEdge(edges[0])
				}
				tables, err := worker.PrepareRelations(ctx, t.TempDir(), base, nil)
				if mode == "wrong target" || mode == "changed payload" || mode == "not from receipt" {
					require.ErrorAs(t, err, new(*RebuildUnknownProvenanceError))
					require.Empty(t, tables)
					return
				}
				require.NoError(t, err)
				_, err = head.PublishStage(ctx, tables)
				require.NoError(t, err)
				current := stageRead(t, head)
				for _, e := range edges {
					if e.Kind != graph.EdgeGeneratedBy {
						continue
					}
					require.Nil(t, current.GetNode(e.To))
					require.Equal(t, 1, current.count("out kind", e.From, string(e.Kind)))
					if profile == FullProfile {
						require.Contains(t, current.GetOutEdges(e.From), e)
					}
				}
			})
		}
	}
}
