package resolver

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// H96's scalar calls for the newly batched families; use the real reducers,
// not a second candidate matcher. The mixed fixture has no old type/token rows.
func scalarRebuildBatchFixture(ctx context.Context, f *rebuildLinkFrontier, page []*graph.Edge) ([]graph.EdgeReindex, error) {
	var moves []graph.EdgeReindex
	for _, old := range page {
		e := *old
		target := graph.UnresolvedName(old.To)
		var err error
		switch {
		case strings.HasPrefix(target, "grpc::"):
			continue
		case strings.HasPrefix(target, "extern::"):
			err = resolveRebuildExtern(ctx, f, &e, strings.TrimPrefix(target, "extern::"))
		case strings.HasPrefix(target, "import::"):
			err = resolveRebuildImport(ctx, f, &e, strings.TrimPrefix(target, "import::"))
		case strings.HasPrefix(target, "*."):
			err = resolveRebuildMember(ctx, f, &e, strings.TrimPrefix(target, "*."))
		default:
			err = resolveRebuildFunction(ctx, f, &e, target)
		}
		if err != nil {
			return nil, err
		}
		if e.To == old.To {
			continue
		}
		if !graph.IsUnresolvedTarget(e.To) {
			e.Meta = maps.Clone(e.Meta)
			clearEdgeTerminal(&e)
		}
		moves = append(moves, graph.EdgeReindex{Edge: &e, OldTo: old.To, OldKind: old.Kind})
	}
	return moves, nil
}

func TestRebuildMainCandidateBatchRows(t *testing.T) {
	const file = "src/use.go"
	node := func(id, name, lang, repo, location string, kind graph.NodeKind) *graph.Node {
		return &graph.Node{ID: id, Name: name, Language: lang, RepoPrefix: repo, FilePath: location, Kind: kind}
	}
	edge := func(from, target string, kind graph.EdgeKind, line int) *graph.Edge {
		return &graph.Edge{From: from, To: "unresolved::" + target, Kind: kind, FilePath: file, Line: line, Origin: graph.OriginASTInferred, Confidence: .42, Meta: map[string]any{"payload": "kept"}}
	}
	for _, workers := range []int{1, 2} {
		for _, spill := range []int{1, 1 << 20} {
			t.Run(fmt.Sprintf("workers=%d/spill=%d", workers, spill), func(t *testing.T) {
				observe := func(batched bool) ([][]graph.EdgeReindex, []*graph.Edge, store_pebble.LinkRequestProgress) {
					ctx, b, opts := frontierFixture(t, workers, 0)
					opts.SpillBytes = spill
					caller := node("a", "caller", "go", "repo", file, graph.KindFunction)
					later := node("z", "later", "go", "repo", file, graph.KindFunction)
					memberA := node("aa-method", "Work", "go", "repo", file, graph.KindMethod)
					memberA.Meta = map[string]any{"receiver": "A"}
					memberB := node("zz-method", "Work", "go", "repo", file, graph.KindMethod)
					memberB.Meta = map[string]any{"receiver": "B"}
					iface := node("iface", "Reader", "go", "repo", file, graph.KindInterface)
					imported := node("pkg", "Package", "rust", "repo", "pkg.rs", graph.KindPackage)
					imported.QualName = "example.test/pkg"
					nodes := []*graph.Node{caller, later, memberA, memberB, iface, imported, node("a-fn", "First", "go", "repo", file, graph.KindFunction), node("b-fn", "Second", "go", "repo", file, graph.KindFunction), node("wrong", "First", "python", "repo", file, graph.KindFunction), node("old-live", "Live", "go", "repo", "pkg/old.go", graph.KindFunction)}
					var input []*graph.Edge
					for i, target := range []string{"First", "First", "Second", "Missing", "*.Work", "*.Work", "extern::pkg::Live", "extern::pkg::Live", "import::example.test/pkg", "import::example.test/pkg", "ctx.Done()"} {
						kind := graph.EdgeCalls
						if strings.HasPrefix(target, "import::") {
							kind = graph.EdgeImports
						}
						if target == "ctx.Done()" {
							kind = graph.EdgeRecvs
						}
						e := edge("a", target, kind, i+1)
						if target == "*.Work" {
							e.Meta["receiver_type"] = "Reader"
						}
						setEdgeTerminal(e, terminalReasonNoDefinition)
						input = append(input, e)
					}
					for i := len(input); i < resolvePendingPageRows; i++ {
						input = append(input, edge("middle", "grpc::Ignored::Run", graph.EdgeCalls, i+1))
					}
					input = append(input, edge("z", "extern::pkg::Live", graph.EdgeCalls, resolvePendingPageRows+1), edge("z", "extern::pkg::Live", graph.EdgeCalls, resolvePendingPageRows+2))
					b.AddBatch(nodes, input)
					var all [][]graph.EdgeReindex
					var progress store_pebble.LinkRequestProgress
					require.NoError(t, runRebuildLinkFrontier(ctx, b, opts, nil, func(f *rebuildLinkFrontier, page []*graph.Edge) error {
						f.imports.checkGoExtern = func(string) error { return nil }
						f.imports.goOwnership = func(GoImportCandidate) (GoPackageOwnershipResult, error) { return GoPackageOwnershipExact, nil }
						f.providesFor = map[string]map[string]struct{}{}
						if len(all) == 1 {
							// Existing live route sees the next page's working facts.
							b.AddNode(node("old-live", "Other", "go", "repo", "pkg/old.go", graph.KindFunction))
							b.AddNode(node("new-live", "Live", "go", "repo", "pkg/new.go", graph.KindFunction))
						}
						var moves []graph.EdgeReindex
						var err error
						if batched {
							moves, err = reduceRebuildMainPage(ctx, f, page)
						} else {
							moves, err = scalarRebuildBatchFixture(ctx, f, page)
						}
						if err != nil {
							return err
						}
						all = append(all, moves)
						progress = f.link.RequestProgress()
						return b.ApplyLinkReindexes(ctx, moves)
					}))
					checkFrontierScratch(t, opts.ScratchParent)
					require.Len(t, all, 2)
					require.Len(t, all[0], 9)
					require.Len(t, all[1], 2)
					for _, move := range all[1] {
						require.Equal(t, "new-live", move.Edge.To)
					}
					return all, rebuildFnPointerSaved(t, b), progress
				}
				scalar, saved, sc := observe(false)
				batch, after, bc := observe(true)
				require.Equal(t, scalar, batch, "complete ordered moves")
				require.Equal(t, saved, after, "complete stored rows")
				require.Equal(t, sc.RequestsSent, bc.RequestsSent)
				require.Equal(t, bc.FramesSent, bc.RoundTrips)
				require.Less(t, bc.CandidateCalls, sc.CandidateCalls)
				require.Less(t, bc.FramesSent, sc.FramesSent)
			})
		}
	}
}

func TestRebuildMainCandidateBatchAggregateBounds(t *testing.T) {
	for _, tc := range []struct {
		name         string
		count        int
		key, payload string
		wantCalls    int
	}{
		{name: "wire", count: 2, key: strings.Repeat("a", 1100<<10), wantCalls: 2},
		{name: "result", count: 26, key: "Huge", payload: strings.Repeat("x", 5<<20), wantCalls: 9},
	} {
		t.Run(tc.name, func(t *testing.T) {
			observe := func(batched bool) ([]graph.EdgeReindex, store_pebble.LinkRequestProgress) {
				ctx, b, opts := frontierFixture(t, 1, 0)
				b.AddNode(&graph.Node{ID: "caller", Name: "caller", Language: "go", Kind: graph.KindFunction, FilePath: "a.go"})
				if tc.payload != "" {
					b.AddNode(&graph.Node{ID: "huge", Name: tc.key, Kind: graph.KindFunction, Language: "go", FilePath: "a.go", Meta: map[string]any{"payload": tc.payload}})
				}
				for i := 0; i < tc.count; i++ {
					b.AddEdge(&graph.Edge{From: "caller", To: "unresolved::" + tc.key, Kind: graph.EdgeCalls, FilePath: "a.go", Line: i + 1})
				}
				var moves []graph.EdgeReindex
				var counts store_pebble.LinkRequestProgress
				require.NoError(t, runRebuildLinkFrontier(ctx, b, opts, nil, func(f *rebuildLinkFrontier, page []*graph.Edge) error {
					require.Len(t, page, tc.count)
					var err error
					if batched {
						moves, err = reduceRebuildMainPage(ctx, f, page)
					} else {
						moves, err = scalarRebuildBatchFixture(ctx, f, page)
					}
					counts = f.link.RequestProgress()
					return err
				}))
				checkFrontierScratch(t, opts.ScratchParent)
				return moves, counts
			}
			s, sc := observe(false)
			a, ac := observe(true)
			require.Equal(t, s, a)
			require.EqualValues(t, tc.count, sc.RequestsSent)
			require.EqualValues(t, tc.count, ac.RequestsSent)
			require.EqualValues(t, tc.wantCalls, ac.CandidateCalls)
			if tc.payload != "" {
				require.Len(t, a, tc.count)
			} else {
				require.Empty(t, a)
			}
		})
	}
}

func TestRebuildMainCandidateBatchFailure(t *testing.T) {
	for _, mode := range []string{"wire", "transport", "visitor", "canceled", "integrity", "semantic"} {
		t.Run(mode, func(t *testing.T) {
			if mode == "transport" {
				t.Setenv("GORTEX_TEST_FRONTIER_CORRUPT", "1")
			}
			ctx, b, opts := frontierFixture(t, 1, 0)
			b.AddBatch([]*graph.Node{{ID: "caller", Name: "caller", Language: "go", Kind: graph.KindFunction, FilePath: "a.go"}, {ID: "fn", Name: "A", Kind: graph.KindFunction, Language: "go", FilePath: "a.go"}, {ID: "qualified", Name: "qualified", QualName: "pkg", Kind: graph.KindFunction, Language: "go", FilePath: "pkg.go"}}, nil)
			first := &graph.Edge{From: "caller", To: "unresolved::A", Kind: graph.EdgeCalls, FilePath: "a.go", Line: 1}
			later := &graph.Edge{From: "caller", To: "unresolved::A", Kind: graph.EdgeCalls, FilePath: "a.go", Line: 2}
			if mode == "wire" {
				later.To = "unresolved::" + strings.Repeat("z", 2<<20)
			}
			if mode == "visitor" {
				later.To = "unresolved::import::pkg"
				later.Kind = graph.EdgeImports
			}
			if mode == "semantic" {
				later.To = "unresolved::zzz::unsupported"
			}
			b.AddBatch(nil, []*graph.Edge{first, later})
			before := rebuildFnPointerSaved(t, b)
			failure := errors.New("fixture candidate visitor")
			err := runRebuildLinkFrontier(ctx, b, opts, nil, func(f *rebuildLinkFrontier, page []*graph.Edge) error {
				f.imports.goOwnership = func(GoImportCandidate) (GoPackageOwnershipResult, error) { return GoPackageOwnershipUnknown, failure }
				if mode == "canceled" {
					c, cancel := context.WithCancel(ctx)
					cancel()
					ctx = c
				}
				if mode == "integrity" { // A separately failed working mutation must not be masked as a lookup miss.
					b.AddNode(&graph.Node{ID: "invalid", Meta: map[string]any{"unsupported": make(chan int)}})
				}
				moves, err := reduceRebuildMainPage(ctx, f, page)
				require.Empty(t, moves)
				return err
			})
			require.Error(t, err)
			if mode == "visitor" {
				require.ErrorIs(t, err, failure)
			}
			if mode == "canceled" {
				require.ErrorIs(t, err, context.Canceled)
			}
			if mode == "semantic" {
				require.ErrorIs(t, err, errRebuildMainRule)
			}
			if mode != "integrity" {
				require.Equal(t, before, rebuildFnPointerSaved(t, b))
			}
			_, err = b.OpenSnapshot()
			require.ErrorIs(t, err, store_pebble.ErrUnpublishedBuild)
			checkFrontierScratch(t, opts.ScratchParent)
		})
	}
}
