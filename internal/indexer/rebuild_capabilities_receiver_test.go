package indexer

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

const (
	receiverBoundFile   = "bounded.go"
	receiverBoundMethod = receiverBoundFile + "::T.Run"
	receiverBoundField  = receiverBoundFile + "::T.mu"
)

func rebuildCapabilityBoundFacts(sites int) store_pebble.ParseFacts {
	nodes := []*graph.Node{
		{ID: receiverBoundMethod, Kind: graph.KindMethod, Name: "Run", FilePath: receiverBoundFile,
			Language: "go", Meta: map[string]any{"receiver": "T"}},
		{ID: receiverBoundField, Kind: graph.KindField, Name: "mu", FilePath: receiverBoundFile,
			Language: "go", Meta: map[string]any{"receiver": "T"}},
	}
	// Read is a direct winner, but cannot seed the receiver mutation.
	edges := []*graph.Edge{{From: receiverBoundMethod, To: receiverBoundField, Kind: graph.EdgeReads,
		FilePath: receiverBoundFile, Line: 1, Origin: graph.OriginASTResolved}}
	callee := "unresolved::Lock"
	if sites > 1 {
		callee = "unresolved::" + strings.Repeat("p", 4096) + "::Lock"
	}
	for i := range sites {
		edges = append(edges, &graph.Edge{From: receiverBoundMethod, To: callee, Kind: graph.EdgeCalls,
			FilePath: receiverBoundFile, Line: i + 2, Meta: map[string]any{"recv_field": "mu"}})
	}
	return store_pebble.ParseFacts{EffectiveLanguage: "go", File: graph.FileMetaRow{FilePath: receiverBoundFile, NodeCount: len(nodes)},
		Nodes: nodes, Edges: edges}
}

func TestRebuildCapabilitiesReceiverBoundAndSpill(t *testing.T) {
	for _, spillAt := range []int{1, 1 << 20} {
		t.Run(fmt.Sprint(spillAt), func(t *testing.T) {
			facts := rebuildCapabilityBoundFacts(320)
			if spillAt == 1 {
				slices.Reverse(facts.Edges)
			}
			// One possible fact, one round to add it, then one to stop.
			native := rebuildDirectCapabilitiesNative(t, facts.Nodes, facts.Edges, [3]int{0, 0, 2})
			want := slices.Clone(native.AllEdges())
			slices.SortFunc(want, compareRebuildTestFileEdgeKeys)
			build, work := openRebuildTestFileStore(t)
			receipts := seedRebuildTestSymbolReceipts(t, build, []store_pebble.ParseFacts{facts})
			for range 2 {
				stats, err := build.RebuildCapabilities(t.Context(), receipts, processExecMechanism, rebuildProcessImport,
					rebuildReceiverRules(), spillAt)
				require.NoError(t, err)
				require.Equal(t, uint64(1), stats.Facts)
				require.Equal(t, uint64(1), stats.Emitted)
				require.Equal(t, uint64(2), stats.RoundBound)
				require.Equal(t, stats.RoundBound, stats.Rounds)
				require.Positive(t, stats.SpillBytes)
				requireRebuildCapabilityScratchClean(t, work)
				requireRebuildTestFileRows(t, build, native.AllNodes(), want)
			}
		})
	}
}

func TestRebuildCapabilitiesReceiverEmptyViaPair(t *testing.T) {
	// A resolved nameless callee whose ID has no bare name creates an
	// empty-via claim. Native add treats it as a direct pair at any site.
	const callee = receiverBoundFile + "::"
	facts := rebuildCapabilityBoundFacts(1)
	facts.Nodes = append(facts.Nodes, &graph.Node{ID: callee, Kind: graph.KindMethod,
		FilePath: receiverBoundFile, Language: "go", Meta: map[string]any{"receiver": "T"}})
	facts.File.NodeCount++
	facts.Edges[1].To = callee
	facts.Edges[1].Meta = map[string]any{"recv_self": true}
	facts.Edges = append(facts.Edges, &graph.Edge{From: callee, To: receiverBoundField, Kind: graph.EdgeWrites,
		FilePath: receiverBoundFile, Line: 3})
	native := rebuildDirectCapabilitiesNative(t, facts.Nodes, facts.Edges, [3]int{0, 0, 2})
	want := slices.Clone(native.AllEdges())
	slices.SortFunc(want, compareRebuildTestFileEdgeKeys)
	build, _ := openRebuildTestFileStore(t)
	receipts := seedRebuildTestSymbolReceipts(t, build, []store_pebble.ParseFacts{facts})
	stats, err := build.RebuildCapabilities(t.Context(), receipts, processExecMechanism, rebuildProcessImport,
		rebuildReceiverRules(), 1)
	require.NoError(t, err)
	require.Zero(t, stats.Emitted)
	requireRebuildTestFileRows(t, build, native.AllNodes(), want)
}

func TestRebuildCapabilitiesReceiverRefusesWithoutOffer(t *testing.T) {
	readFailure := fmt.Errorf("%w: planted capability receipt read failure", errParseEntryCorrupt)
	for _, mode := range []string{"round", "read", "unproved first claim"} {
		t.Run(mode, func(t *testing.T) {
			ctx := t.Context()
			build, work := openRebuildTestFileStore(t)
			facts := rebuildCapabilityBoundFacts(1)
			rules := rebuildReceiverRules()
			var wantErr error
			owned := []store_pebble.ParseFacts{facts}
			switch mode {
			case "round":
				rules.RoundLimit = 1
				wantErr = store_pebble.ErrReceiverMutationNonconvergent
			case "read":
				wantErr = readFailure
			case "unproved first claim":
				// A supported runner-up at the same via must not inherit the
				// first claim when that earlier live call has no proof.
				later := *facts.Edges[1]
				later.Line++
				facts.Edges = []*graph.Edge{facts.Edges[0], &later}
				first := later
				first.Line--
				owned = []store_pebble.ParseFacts{facts, {
					EffectiveLanguage: "python", File: graph.FileMetaRow{FilePath: "competitor.py"}, Edges: []*graph.Edge{&first},
				}}
				wantErr = store_pebble.ErrRebuildUnknownProvenance
			}
			receipts := seedRebuildTestSymbolReceipts(t, build, owned)
			var before []*graph.Edge
			require.NoError(t, build.RebuildView().VisitEdges(ctx, func(e *graph.Edge) error {
				before = append(before, e)
				return nil
			}))
			stats, err := build.RebuildCapabilities(ctx, func(yield func(store_pebble.ParseFacts, error) bool) {
				for receipt, err := range receipts {
					if !yield(receipt, err) {
						return
					}
				}
				if mode == "read" {
					yield(store_pebble.ParseFacts{}, readFailure)
				}
			}, processExecMechanism, rebuildProcessImport, rules, 1)
			require.ErrorIs(t, err, wantErr)
			if mode == "unproved first claim" {
				var refusal *store_pebble.RebuildUnknownProvenanceError
				require.ErrorAs(t, err, &refusal)
				require.Equal(t, uint64(1), refusal.Count)
				require.Equal(t, graph.EdgeIdentity{
					From: receiverBoundMethod, To: receiverBoundField, Kind: graph.EdgeAccessesField,
					FilePath: receiverBoundFile, Line: 2,
				}, refusal.First)
			}
			require.Zero(t, stats)
			requireRebuildTestFileRows(t, build, facts.Nodes, before)
			requireRebuildCapabilityScratchClean(t, work)

			var reply, acks bytes.Buffer
			require.NoError(t, writeRebuildBuildReply(&reply, buildRefusal(err)))
			err = receiveRebuildStages(ctx, work, rebuildBuildRequest{}, &reply, &acks,
				func(context.Context, store_pebble.StageTables) error {
					t.Fatal("a failed capability stage must not publish")
					return nil
				})
			require.ErrorIs(t, err, ErrRebuildWorker)
			if mode == "read" {
				wantErr = errParseEntryCorrupt
			}
			require.ErrorIs(t, err, wantErr)
			require.Empty(t, acks.Bytes())
		})
	}
}

func requireRebuildCapabilityScratchClean(t *testing.T, work string) {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(work, "working", "pending", "receiver-*"))
	require.NoError(t, err)
	require.Empty(t, paths)
}
