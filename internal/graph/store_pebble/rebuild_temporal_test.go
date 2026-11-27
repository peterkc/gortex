package store_pebble

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildTemporalWinnerCustody(t *testing.T) {
	const file, caller, target = "workflow.go", "caller", "activity"
	b, err := OpenBuild(filepath.Join(t.TempDir(), "working"), FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	ctx := t.Context()
	call := func(to string, line int) *graph.Edge {
		return &graph.Edge{From: caller, To: to, Kind: graph.EdgeCalls, FilePath: file, Line: line}
	}
	raw, prior, direct := call("unresolved::raw", 1), call("unresolved::prior", 1), call("unresolved::direct", 2)
	trigger := call("unresolved::wrapper", 3)
	shadow := call("unresolved::shadow", 3)
	keeper := call(target, 5)
	keeper.Meta = map[string]any{"keeper": true}
	facts := ParseFacts{
		EffectiveLanguage: "go", File: graph.FileMetaRow{FilePath: file, NodeCount: 2},
		Nodes: []*graph.Node{
			{ID: caller, Name: "Run", Kind: graph.KindFunction, Language: "go", FilePath: file},
			{ID: target, Name: "Activity", Kind: graph.KindFunction, Language: "go", FilePath: file},
		},
		Edges: []*graph.Edge{raw, direct, trigger, shadow, keeper},
	}
	require.NoError(t, b.AddParsedFile("", facts))
	require.NoError(t, b.ApplyLinkReindexes(ctx, []graph.EdgeReindex{{Edge: prior, OldTo: raw.To}}))
	// Give the shadowed live key real raw custody. The staged payload at
	// that key must never borrow it, even if it displaces the durable row.
	shadowed := call("unresolved::staged", 3)
	require.NoError(t, b.ApplyLinkReindexes(ctx, []graph.EdgeReindex{{Edge: shadowed, OldTo: shadow.To}}))
	_, err = b.RebuildTemporal(ctx, func(yield func(ParseFacts, error) bool) { yield(facts, nil) }, 1,
		func(p *RebuildTemporalPass) (RebuildTemporalStats, error) {
			for _, e := range []*graph.Edge{prior, direct, trigger} {
				proof := p.EdgeProof(call(target, e.Line))
				require.NoError(t, proof.WantRecorder(ctx, e))
				require.NoError(t, proof.WantNode(ctx, caller))
				require.NoError(t, proof.WantNode(ctx, target))
			}
			require.NoError(t, p.QueueMove(ctx, prior.To, call(target, 1), false))
			require.NoError(t, p.QueueMove(ctx, direct.To, call(target, 2), false))
			require.NoError(t, p.QueueMove(ctx, shadowed.To, call(target, 3), true))
			require.NoError(t, p.QueueAdd(ctx, call(target, 3))) // duplicate of the staged move
			require.NoError(t, p.QueueAdd(ctx, call(target, 4)))
			require.NoError(t, p.QueueAdd(ctx, call(target, 5))) // loses to the durable keeper
			return RebuildTemporalStats{}, nil
		})
	require.NoError(t, err)
	for _, tc := range []struct {
		line  int
		raw   *graph.Edge
		proof rebuildEdgeProof
	}{
		{1, raw, 0}, {2, direct, 0}, {3, nil, rebuildProofTemporalSynthetic},
		{4, nil, rebuildProofTemporalSynthetic}, {5, nil, 0},
	} {
		key := pendingEdgeKey(call(target, tc.line))
		stored, err := b.pendingEdge(key)
		require.NoError(t, err)
		require.NotNil(t, stored)
		got, err := readRebuildEdgeCustody(b.pending, key)
		require.NoError(t, err)
		require.Equal(t, tc.proof, got.proof)
		if tc.raw != nil {
			require.Equal(t, pendingEdgeKey(tc.raw), got.raw)
		} else {
			require.Empty(t, got.raw)
		}
		if tc.line == keeper.Line {
			require.Equal(t, true, stored.Meta["keeper"])
		}
	}
	requireRebuildCustodyCount(t, b, 4)
}

func TestRebuildTemporalAddRunsAndTail(t *testing.T) {
	for _, refuse := range []bool{false, true} {
		name := "apply in ordinal order"
		if refuse {
			name = "remove runs on proof refusal"
		}
		t.Run(name, func(t *testing.T) {
			b := newBuild(t, FullProfile)
			ctx := t.Context()
			const threshold = 1 << 10
			first := &graph.Edge{From: "caller", To: "a", Kind: graph.EdgeCalls, FilePath: "workflow.go", Line: 1, Via: strings.Repeat("a", threshold)}
			second, duplicate, last := *first, *first, *first
			second.To = "b"
			duplicate.Via = "later payload for the same key"
			last.To, last.Via = "c", "tail"
			before := rebuildFrameworkCustodyRows(t, b)
			var physical uint64
			var runDir string
			stats, err := b.RebuildTemporal(ctx, func(func(ParseFacts, error) bool) {}, threshold,
				func(p *RebuildTemporalPass) (RebuildTemporalStats, error) {
					for _, e := range []*graph.Edge{first, &second, &duplicate, &last} {
						require.NoError(t, p.QueueAdd(ctx, e))
					}
					runDir = p.addDir
					require.Equal(t, 2, p.addRuns)
					require.Len(t, p.addRows, 2)
					require.NoError(t, pendingVisit(ctx, b.pending, rebuildTemporalKey('a'), func(row) error {
						return fmt.Errorf("queued Temporal insert entered the pending overlay")
					}))
					for i := range p.addRuns {
						info, err := os.Stat(p.addRunPath(i))
						require.NoError(t, err)
						physical += uint64(info.Size())
					}
					prefix := rebuildTemporalKey('a')
					var next uint64
					require.NoError(t, p.visitAdds(ctx, func(r row) error {
						require.Len(t, r.key, len(prefix)+8)
						require.Equal(t, next, binary.BigEndian.Uint64(r.key[len(prefix):]))
						next++
						return nil
					}))
					require.EqualValues(t, 4, next)
					if refuse {
						return RebuildTemporalStats{}, p.EdgeProof(first).WantNode(ctx, "missing")
					}
					return RebuildTemporalStats{}, nil
				})
			if refuse {
				var refusal *RebuildUnknownProvenanceError
				require.ErrorIs(t, err, ErrRebuildTemporal)
				require.ErrorAs(t, err, &refusal)
				require.Equal(t, &RebuildUnknownProvenanceError{Count: 1, First: graph.EdgeIdentityFor(first)}, refusal)
				require.Zero(t, stats)
				require.Equal(t, before, rebuildFrameworkCustodyRows(t, b))
			} else {
				require.NoError(t, err)
				require.Positive(t, physical)
				require.Equal(t, physical, stats.SpillBytes)
				want := make(map[graph.EdgeIdentity]rebuildEdgeProof)
				for _, e := range []*graph.Edge{first, &second, &last} {
					saved, err := b.pendingEdge(pendingEdgeKey(e))
					require.NoError(t, err)
					require.Equal(t, e, saved)
					want[graph.EdgeIdentityFor(e)] = rebuildProofTemporalSynthetic
				}
				requireSyntheticCustody(t, b, want)
			}
			_, err = os.Stat(runDir)
			require.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}

func TestRebuildTemporalSpillsBeforeAppend(t *testing.T) {
	b := newBuild(t, FullProfile)
	ctx := t.Context()
	const threshold = 1 << 10
	// Each encoded row fits alone, but the two together exceed the threshold.
	first := graph.Edge{From: "caller", To: "a", Kind: graph.EdgeCalls, Via: strings.Repeat("x", 3*threshold/4)}
	second := first
	second.To = "b"
	_, err := b.RebuildTemporal(ctx, func(func(ParseFacts, error) bool) {}, threshold,
		func(p *RebuildTemporalPass) (RebuildTemporalStats, error) {
			require.NoError(t, p.QueueAdd(ctx, &first))
			require.NoError(t, p.QueueAdd(ctx, &second))

			require.Equal(t, 1, p.addRuns)
			require.Len(t, p.addRows, 1)
			tail, err := decodeEdge(p.addRows[0].value)
			require.NoError(t, err)
			require.Equal(t, &second, tail)
			var added []*graph.Edge
			require.NoError(t, visitLinkRun(ctx, p.addRunPath(0), func(r row) error {
				e, err := decodeEdge(r.value)
				if err != nil {
					return err
				}
				added = append(added, e)
				return nil
			}))
			require.Equal(t, []*graph.Edge{&first}, added)
			return RebuildTemporalStats{}, nil
		})
	require.NoError(t, err)
}

func TestRebuildTemporalQueueAddRefusesOversizedRow(t *testing.T) {
	b := newBuild(t, FullProfile)
	ctx := t.Context()
	edge := &graph.Edge{From: "caller", To: "target", Kind: graph.EdgeCalls, Via: strings.Repeat("x", rebuildTemporalRowLimit)}
	stats, err := b.RebuildTemporal(ctx, func(func(ParseFacts, error) bool) {}, 1,
		func(p *RebuildTemporalPass) (RebuildTemporalStats, error) {
			err := p.QueueAdd(ctx, edge)
			require.Empty(t, p.addRows)
			require.Empty(t, p.addDir)
			return RebuildTemporalStats{}, err
		})
	require.ErrorIs(t, err, ErrRebuildTemporal)
	require.ErrorIs(t, err, ErrRebuildLinkInputSize)
	require.ErrorContains(t, err, "Temporal row")
	require.Zero(t, stats)
}

func TestRebuildTemporalRefusesAffectedOutputRows(t *testing.T) {
	for _, tc := range []struct {
		name       string
		wantCount  uint64
		keepCaller bool
		keepTarget bool
		keepRecord bool
		firstNode  bool
	}{
		{name: "shared missing declaration", wantCount: 3, keepCaller: true, keepRecord: true},
		{name: "several failed demands per row", wantCount: 3},
		{name: "node role only", wantCount: 1, keepCaller: true, keepTarget: true, firstNode: true},
		{name: "all proved", keepCaller: true, keepTarget: true, keepRecord: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBuild(t, FullProfile)
			ctx := t.Context()
			const file = "flow.go"
			caller := &graph.Node{ID: "caller", Name: "Run", Kind: graph.KindFunction, Language: "go", FilePath: file}
			target := &graph.Node{ID: "target", Name: "Activity", Kind: graph.KindFunction, Language: "go", FilePath: file}
			raw := &graph.Edge{From: caller.ID, To: "unresolved::Activity", Kind: graph.EdgeCalls, FilePath: file, Line: 1}
			other := *raw
			other.Line = 2
			facts := ParseFacts{
				EffectiveLanguage: "go",
				File:              graph.FileMetaRow{FilePath: file, NodeCount: 2},
				Nodes:             []*graph.Node{caller, target},
				Edges:             []*graph.Edge{raw, &other},
			}
			require.NoError(t, b.AddParsedFile("", facts))
			proofFacts := facts
			proofFacts.Nodes = nil
			if tc.keepCaller {
				proofFacts.Nodes = append(proofFacts.Nodes, caller)
			}
			if tc.keepTarget {
				proofFacts.Nodes = append(proofFacts.Nodes, target)
			}
			if !tc.keepRecord {
				proofFacts.Edges = nil
			}
			first := *raw
			first.To = target.ID
			flushed := b.pending.Metrics().Levels[0].TableBytesFlushed
			stats, err := b.RebuildTemporal(ctx, func(yield func(ParseFacts, error) bool) {
				yield(proofFacts, nil)
			}, 1, func(p *RebuildTemporalPass) (RebuildTemporalStats, error) {
				stamped := *target
				stamped.Meta = map[string]any{"temporal_role": "activity"}
				role := p.NodeProof(&stamped)
				require.NoError(t, role.WantRecorder(ctx, raw))
				require.NoError(t, role.WantNode(ctx, target.ID))
				require.NoError(t, p.StampNode(ctx, &stamped))
				for _, e := range facts.Edges {
					out := *e
					out.To = target.ID
					proof := p.EdgeProof(&out)
					if !tc.firstNode {
						require.NoError(t, proof.WantRecorder(ctx, e))
					}
					require.NoError(t, proof.WantNode(ctx, caller.ID))
					require.NoError(t, proof.WantNode(ctx, target.ID))
					require.NoError(t, p.QueueMove(ctx, e.To, &out, false))
				}
				return RebuildTemporalStats{}, nil
			})
			require.Equal(t, flushed, b.pending.Metrics().Levels[0].TableBytesFlushed)
			if tc.wantCount == 0 {
				require.NoError(t, err)
				require.Zero(t, stats.SpillBytes)
				for _, e := range facts.Edges {
					out := *e
					out.To = target.ID
					stored, err := b.pendingEdge(pendingEdgeKey(&out))
					require.NoError(t, err)
					require.Equal(t, &out, stored)
				}
			} else {
				var refused *RebuildUnknownProvenanceError
				require.ErrorAs(t, err, &refused)
				require.ErrorIs(t, err, ErrRebuildTemporal)
				require.Equal(t, tc.wantCount, refused.Count)
				wantFirst := graph.EdgeIdentityFor(&first)
				if tc.firstNode {
					wantFirst = graph.EdgeIdentity{From: target.ID, FilePath: target.FilePath}
				}
				require.Equal(t, wantFirst, refused.First)
				require.Zero(t, stats)
				for _, e := range facts.Edges {
					stored, err := b.pendingEdge(pendingEdgeKey(e))
					require.NoError(t, err)
					require.Equal(t, e, stored)
				}
				stored, err := b.pendingNode(target.ID)
				require.NoError(t, err)
				require.Equal(t, target, stored)
				storedEdge, err := b.pendingEdge(pendingEdgeKey(&first))
				require.NoError(t, err)
				require.Nil(t, storedEdge)
			}
			busy, err := rebuildGuardHasPrefix(ctx, b.pending, rebuildTemporalPrefix)
			require.NoError(t, err)
			require.False(t, busy)
		})
	}
}
