package store_pebble

import (
	"errors"
	"fmt"
	"testing"

	"github.com/cockroachdb/pebble/v2"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildTemporalConstScratchRows(t *testing.T) {
	b := newBuild(t, FullProfile)
	ctx := t.Context()
	const file = "dispatch.go"
	const id = file + "::Name"
	const literal = "saved\x00value"
	require.NoError(t, b.AddParsedFile("repo", ParseFacts{
		File:      graph.FileMetaRow{FilePath: file, NodeCount: 1},
		Nodes:     []*graph.Node{{ID: id, Name: "Name", Kind: graph.KindConstant, FilePath: file}},
		Constants: []graph.ConstantValueRow{{NodeID: id, FilePath: file, Value: literal}},
	}))
	var result *TemporalConstDeref
	flushed := b.pending.Metrics().Levels[0].TableBytesFlushed
	stats, err := b.RebuildView().WithTemporalConstDeref(ctx, func(yield func(string) error) error {
		return yield("Name")
	}, func(*graph.Node) TemporalConstCandidate {
		return TemporalConstCandidate{ReadSidecar: true}
	}, func(r *TemporalConstDeref) error {
		result = r
		var rows []string
		prefix := temporalConstKey(temporalConstValue)
		require.NoError(t, pendingVisit(ctx, b.pending, prefix, func(row row) error {
			name, tail, err := readKeyString(row.key[len(prefix):])
			require.NoError(t, err)
			require.Empty(t, tail)
			rows = append(rows, name+"="+string(row.value))
			return nil
		}))
		require.Equal(t, []string{"Name=" + literal}, rows)
		v, err := r.Value(ctx, "Name")
		require.NoError(t, err)
		require.Equal(t, literal, v)
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, 1, stats.Names)
	require.Equal(t, flushed, b.pending.Metrics().Levels[0].TableBytesFlushed)
	value, err := result.Value(ctx, "Name")
	require.ErrorIs(t, err, ErrRebuildTemporalConst)
	require.Empty(t, value)
	assertNoTemporalConstScratch(t, b)
}

func TestRebuildTemporalConstRefusesErrorsAndCleansScratch(t *testing.T) {
	for _, where := range []string{"seed", "name index", "sidecar", "consume", "consume lookup"} {
		t.Run(where, func(t *testing.T) {
			b := newBuild(t, FullProfile)
			ctx := t.Context()
			n := &graph.Node{ID: "a.go::Name", Name: "Name", Kind: graph.KindConstant, FilePath: "a.go", Language: "go"}
			b.AddBatch([]*graph.Node{n}, nil)
			injected := errors.New("injected Temporal failure")
			want := error(injected)
			switch where {
			case "name index":
				require.NoError(t, b.pending.Set(rebuildCandidateKey(nameRebuildScoped, n), []byte("missing"), pebble.NoSync))
				want = errInvalidRecord
			case "sidecar":
				require.NoError(t, b.pending.Set(parsedConstantKey(n.ID), []byte{0xff}, pebble.NoSync))
				want = errInvalidRecord
			}
			consumes := 0
			stats, err := b.RebuildView().WithTemporalConstDeref(ctx, func(yield func(string) error) error {
				if err := yield(n.Name); err != nil {
					return err
				}
				if where == "seed" {
					return injected
				}
				return nil
			}, func(*graph.Node) TemporalConstCandidate {
				return TemporalConstCandidate{ReadSidecar: true}
			}, func(values *TemporalConstDeref) error {
				consumes++
				if where == "consume lookup" {
					require.NoError(t, b.pending.Set(temporalConstKey(temporalConstValue, n.Name), nil, pebble.NoSync))
					_, err := values.Value(ctx, n.Name)
					return err
				}
				return injected
			})
			require.Zero(t, stats)
			switch where {
			case "consume":
				require.ErrorIs(t, err, injected)
				require.NotErrorIs(t, err, ErrRebuildTemporalConst)
				require.Equal(t, 1, consumes)
			case "consume lookup":
				require.ErrorIs(t, err, errInvalidRecord)
				require.ErrorIs(t, err, ErrRebuildTemporalConst)
				require.Equal(t, 1, consumes)
			default:
				require.ErrorIs(t, err, want)
				require.ErrorIs(t, err, ErrRebuildTemporalConst)
				require.Zero(t, consumes)
			}
			assertNoTemporalConstScratch(t, b)
		})
	}
}

func TestRebuildTemporalConstDiscoveryOrdinals(t *testing.T) {
	const file, literal = "aliases.go", "registered"
	// The nine-hop chain is in descending name order. Two seeds reach
	// the literal in eight discovery rounds. Swapping only those seeds
	// can carry its value one additional hop in the eighth collapse sweep.
	names := []string{"Z", "Y", "X", "W", "V", "U", "T", "S", "R", "Literal"}
	for _, reversed := range []bool{false, true} {
		t.Run(fmt.Sprintf("reversed=%t", reversed), func(t *testing.T) {
			b := newBuild(t, FullProfile)
			var nodes []*graph.Node
			aliases := make(map[string]string, len(names)-1)
			for i, name := range names {
				nodes = append(nodes, &graph.Node{ID: file + "::" + name, Kind: graph.KindConstant, Name: name, FilePath: file})
				if i+1 < len(names) {
					aliases[name] = names[i+1]
				}
			}
			b.AddBatch(nodes, nil)
			first, second := names[0], names[1]
			if reversed {
				first, second = second, first
			}
			ctx := t.Context()
			stats, err := b.RebuildView().WithTemporalConstDeref(ctx, func(yield func(string) error) error {
				if err := yield(first); err != nil {
					return err
				}
				return yield(second)
			}, func(n *graph.Node) TemporalConstCandidate {
				if n.Name == "Literal" {
					return TemporalConstCandidate{JavaLiteral: literal}
				}
				return TemporalConstCandidate{Alias: aliases[n.Name]}
			}, func(values *TemporalConstDeref) error {
				pass := temporalConstPass{view: b.RebuildView()}
				var aliasOrder []string
				require.NoError(t, pass.visit(ctx, temporalConstAlias, func(name, _ string) error {
					aliasOrder = append(aliasOrder, name)
					return nil
				}))
				require.Equal(t, append([]string{first, second}, names[2:len(names)-1]...), aliasOrder)
				got, err := values.Value(ctx, "Y")
				require.NoError(t, err)
				require.Equal(t, literal, got)
				got, err = values.Value(ctx, "Z")
				require.NoError(t, err)
				if reversed {
					require.Equal(t, literal, got)
				} else {
					require.Empty(t, got)
				}
				return nil
			})
			require.NoError(t, err)
			require.Equal(t, len(names), stats.Names)
			require.Equal(t, 8, stats.DiscoveryPasses)
			require.Equal(t, 8, stats.CollapsePasses)
			assertNoTemporalConstScratch(t, b)
		})
	}
}

func assertNoTemporalConstScratch(t *testing.T, b *BuildStore) {
	t.Helper()
	require.NoError(t, pendingVisit(t.Context(), b.pending, temporalConstPrefix, func(row) error {
		t.Fatal("Temporal scratch survived its callback")
		return nil
	}))
}
