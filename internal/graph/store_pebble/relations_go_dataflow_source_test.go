package store_pebble

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser/languages"
)

func TestRelationsCheckedGoDataflowSources(t *testing.T) {
	const file = "source.go"
	const source = "unresolved::extern::net/http::StatusCreated"
	parsed, err := languages.NewGoExtractor().Extract(file, []byte(`package p
import ("net/http"; local "./util")
func Arg() { Sink(http.StatusCreated) }
func Value() int { return http.StatusCreated }
func Bare() int { return UNKNOWN }
func Member() int { return unknown.Field }
func Local() int { return local.Constant }
`))
	require.NoError(t, err)
	t.Cleanup(parsed.ReleaseTree)
	for _, tc := range []struct {
		name, from string
		kind       graph.EdgeKind
		line       int
		change     string
		keep       bool
	}{
		{"qualified argument", source, graph.EdgeArgOf, 3, "", true},
		{"argument target move", source, graph.EdgeArgOf, 3, "target", true},
		{"qualified value", source, graph.EdgeValueFlow, 4, "", true},
		{"bare value", "unresolved::UNKNOWN", graph.EdgeValueFlow, 5, "", true},
		{"relative import value", "unresolved::extern::./util::Constant", graph.EdgeValueFlow, 7, "", true},
		{"selector value", "unresolved::*.Field", graph.EdgeValueFlow, 6, "", true},
		{"wrong origin", source, graph.EdgeArgOf, 3, "origin", false},
		{"wrong path", source, graph.EdgeArgOf, 3, "path", false},
		{"foreign extractor", source, graph.EdgeArgOf, 3, "extractor", false},
		{"wrong extractor at seal", source, graph.EdgeArgOf, 3, "seal extractor", false},
		{"same chunk target chain", source, graph.EdgeArgOf, 3, "chain", true},
		{"forged pair", source, graph.EdgeArgOf, 3, "pair", false},
		{"forged source move", source, graph.EdgeArgOf, 3, "source", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			var raw *graph.Edge
			for _, e := range parsed.Edges {
				if e.Kind == tc.kind && e.From == tc.from && e.Line == tc.line {
					raw = e
					break
				}
			}
			require.NotNil(t, raw, "native producer")
			edge := *raw
			facts := ParseFacts{EffectiveLanguage: "go", Nodes: parsed.Nodes, Edges: []*graph.Edge{&edge},
				File: graph.FileMetaRow{FilePath: file, NodeCount: len(parsed.Nodes)}}
			switch tc.change {
			case "origin":
				edge.Origin = graph.OriginASTResolved
			case "path":
				edge.FilePath = "other.go"
			case "extractor":
				facts.EffectiveLanguage = "typescript"
			}
			// Give the source receipt its own manifest and FileGraph, not a
			// file-meta mutation after the retained predecessor.
			worker := newBuild(t, FullProfile)
			head, err := OpenHead(t.TempDir(), testHeadIncarnation, FullProfile)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, head.Close()) })
			manifest, err := PrepareManifest(ctx, t.TempDir(), []ManifestFile{{Path: file, Language: facts.EffectiveLanguage, Kind: 'f'}}, Witness{
				Input: HeadDigest{1}, Recipe: HeadDigest{2}, Profile: FullProfile, Stage: StageManifest,
				PlatformCatalogRevision: graph.PlatformCatalogRevision,
			}, testHeadIncarnation)
			require.NoError(t, err)
			_, err = head.PublishStage(ctx, manifest)
			require.NoError(t, err)
			require.NoError(t, worker.AddParsedFile("", facts))
			fg, err := worker.PrepareFileGraph(ctx, t.TempDir(), manifest.Witness, testHeadIncarnation)
			require.NoError(t, err)
			base, err := worker.RetainFileGraph(ctx, fg)
			require.NoError(t, err)
			_, err = head.PublishStage(ctx, fg)
			require.NoError(t, err)
			switch tc.change {
			case "target":
				move := edge
				move.To = file + "::Arg"
				require.NoError(t, worker.ApplyLinkReindexes(ctx, []graph.EdgeReindex{{Edge: &move, OldTo: edge.To}}))
				edge = move
			case "chain":
				middle, last := edge, edge
				middle.To, last.To = file+"::Arg", file+"::Value"
				worker.ReindexEdges([]graph.EdgeReindex{{Edge: &middle, OldTo: edge.To}, {Edge: &last, OldTo: middle.To}})
				edge = last
			case "pair":
				forged := edge
				forged.To = file + "::Arg"
				worker.AddEdge(&forged)
			case "source":
				forged := edge
				forged.From = "unresolved::extern::net/http::StatusOK"
				worker.ReindexEdges([]graph.EdgeReindex{{Edge: &forged, OldFrom: edge.From, OldTo: edge.To,
					OldLine: edge.Line, OldFilePath: edge.FilePath, RefreshIdentity: true}})
			}
			sealedExtractor := "go"
			if tc.change == "seal extractor" {
				sealedExtractor = "typescript"
			}
			tables, err := worker.PrepareRelations(ctx, t.TempDir(), base, func(string) string { return sealedExtractor })
			if !tc.keep {
				require.ErrorIs(t, err, ErrRelationsSeal)
				require.ErrorContains(t, err, "dangling relations endpoint")
				require.Empty(t, tables)
				return
			}
			require.NoError(t, err)
			_, err = head.PublishStage(ctx, tables)
			require.NoError(t, err)
			snap := stageRead(t, head)
			require.Nil(t, snap.GetNode(edge.From))
			require.Equal(t, []*graph.Edge{&edge}, snap.GetOutEdges(edge.From))
			require.Equal(t, 1, snap.count("go dataflow source endpoints"))
		})
	}
}
