package store_pebble

import (
	"encoding/binary"
	"errors"
	"testing"

	"github.com/cockroachdb/pebble/v2"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildContractRecordsKeepLiveAndRawBucketViews(t *testing.T) {
	ctx := t.Context()
	b, err := OpenBuild(t.TempDir(), FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	newRecord := func(id, symbol string, line int) RebuildContractRecord {
		return RebuildContractRecord{Contract: contracts.Contract{ID: id, Type: contracts.ContractHTTP,
			Role: contracts.RoleProvider, FilePath: "routes.go", Line: line, SymbolID: symbol,
			Meta: map[string]any{"status_codes": []int{200, 202}}}}
	}
	a, duplicate, z := newRecord("a", "Register", 3), newRecord("a", "Register", 8), newRecord("z", "Register", 10)
	stop := errors.New("stop contract pass")
	var escaped *RebuildContractRecords
	err = b.WithRebuildContractRecords(ctx, func(r *RebuildContractRecords) error {
		escaped = r
		require.Error(t, b.WithRebuildContractRecords(ctx, func(*RebuildContractRecords) error {
			t.Fatal("nested borrower entered the same scratch range")
			return nil
		}))
		for _, item := range []RebuildContractRecord{z, a, duplicate} {
			require.NoError(t, r.Add(item))
		}
		var seen []RebuildContractRecord
		require.NoError(t, r.VisitRepresentatives(func(item RebuildContractRecord) error {
			seen = append(seen, item)
			if item.ID != a.ID {
				return nil
			}
			return r.UpdateByID(z.ID, func(live *RebuildContractRecord) (bool, error) {
				live.SymbolID, live.BodyUnproved = "Linked", true
				return true, nil
			})
		}))
		require.Equal(t, []RebuildContractRecord{a, z}, seen, "representatives read the original rows")
		seen = nil
		require.NoError(t, r.VisitByID(a.ID, func(item RebuildContractRecord) error {
			seen = append(seen, item)
			return nil
		}))
		require.Equal(t, []RebuildContractRecord{a, duplicate}, seen)
		seen = nil
		require.NoError(t, r.VisitByID(z.ID, func(item RebuildContractRecord) error {
			seen = append(seen, item)
			return nil
		}))
		changed := z
		changed.SymbolID, changed.BodyUnproved = "Linked", true
		require.Equal(t, []RebuildContractRecord{changed}, seen)
		require.NoError(t, r.ClearID(a.ID))
		var ids []string
		require.NoError(t, r.VisitIDs(func(id string) error {
			ids = append(ids, id)
			return nil
		}))
		require.Equal(t, []string{"a", "z"}, ids, "the cleared native key persists")
		seen = nil
		require.NoError(t, r.VisitRepresentatives(func(item RebuildContractRecord) error {
			seen = append(seen, item)
			return nil
		}))
		require.Equal(t, []RebuildContractRecord{changed}, seen, "no live record for a")
		require.NoError(t, r.Add(duplicate))
		seen = nil
		require.NoError(t, r.VisitByID(a.ID, func(item RebuildContractRecord) error {
			seen = append(seen, item)
			return nil
		}))
		require.Equal(t, []RebuildContractRecord{duplicate}, seen)
		return stop
	})
	require.ErrorIs(t, err, stop)
	require.Error(t, escaped.Add(a))
	has, err := rebuildGuardHasPrefix(ctx, b.pending, rebuildContractRecordPrefix)
	require.NoError(t, err)
	require.False(t, has, "cleanup owns only this scratch range")
	require.NoError(t, b.WithRebuildContractRecords(ctx, func(r *RebuildContractRecords) error {
		for _, c := range []contracts.Contract{
			{ID: "a", FilePath: "b|c", SymbolID: "S", Role: contracts.RoleProvider},
			{ID: "a|b", FilePath: "c", SymbolID: "S", Role: contracts.RoleProvider},
		} {
			require.NoError(t, r.Add(RebuildContractRecord{Contract: c}))
		}
		require.ErrorContains(t, r.VisitRepresentatives(func(RebuildContractRecord) error { return nil }), "ambiguous contract representative identity across IDs")
		for _, relevantID := range []string{"", "a", "a|b"} {
			var ids []string
			err := r.VisitRepresentatives(func(item RebuildContractRecord) error {
				ids = append(ids, item.ID)
				return nil
			}, func(item RebuildContractRecord) bool { return item.ID == relevantID })
			if relevantID == "" {
				require.NoError(t, err)
				require.Equal(t, []string{"a"}, ids)
			} else {
				require.ErrorContains(t, err, "ambiguous contract representative identity across IDs")
			}
		}
		return nil
	}))
}

func TestRebuildContractOutputEdgesKeepFirstPhysicalKey(t *testing.T) {
	b, err := OpenBuild(t.TempDir(), FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	ctx := t.Context()
	winner := &graph.Edge{From: "a.java::Make", To: "di::Service", Kind: graph.EdgeProvides, FilePath: "a.java", Line: 5,
		Origin: graph.OriginASTResolved, Meta: map[string]any{"binding": "original"}}
	later := *winner
	later.Origin = graph.OriginASTInferred
	later.Meta = map[string]any{"binding": "later"}
	var readAfter *RebuildContractRecords
	stop := errors.New("stop after edge election")
	require.ErrorIs(t, b.WithRebuildContractRecords(ctx, func(r *RebuildContractRecords) error {
		readAfter = r
		first, err := r.FirstOutputEdge(winner)
		require.NoError(t, err)
		require.True(t, first)
		// D83 omits winner after claiming it. The later proved payload
		// must not get another chance at the same native insert key.
		first, err = r.FirstOutputEdge(&later)
		require.NoError(t, err)
		require.False(t, first)
		for _, distinct := range []graph.Edge{
			{From: "b.java::Make", To: winner.To, Kind: winner.Kind, FilePath: winner.FilePath, Line: winner.Line},
			{From: winner.From, To: "di::Other", Kind: winner.Kind, FilePath: winner.FilePath, Line: winner.Line},
			{From: winner.From, To: winner.To, Kind: graph.EdgeConsumes, FilePath: winner.FilePath, Line: winner.Line},
			{From: winner.From, To: winner.To, Kind: winner.Kind, FilePath: "b.java", Line: winner.Line},
			{From: winner.From, To: winner.To, Kind: winner.Kind, FilePath: winner.FilePath, Line: 6},
		} {
			first, err := r.FirstOutputEdge(&distinct)
			require.NoError(t, err)
			require.True(t, first)
		}
		return stop
	}), stop)
	_, err = readAfter.FirstOutputEdge(winner)
	require.Error(t, err)
	require.NoError(t, b.WithRebuildContractRecords(ctx, func(r *RebuildContractRecords) error {
		first, err := r.FirstOutputEdge(&later)
		require.NoError(t, err)
		require.True(t, first)
		first, err = r.FirstOutputEdge(winner)
		require.NoError(t, err)
		require.False(t, first)
		return nil
	}))
}

func TestRebuildContractRecordsRejectCorruptRows(t *testing.T) {
	for _, mode := range []string{"empty ID", "empty clear", "bucket key", "record key", "record identity", "changed ID", "nested representative", "live identity"} {
		t.Run(mode, func(t *testing.T) {
			ctx := t.Context()
			b := newBuild(t, FullProfile)
			item := RebuildContractRecord{Contract: contracts.Contract{ID: "a", FilePath: "route.go", SymbolID: "handler"}}
			called := false
			err := b.WithRebuildContractRecords(ctx, func(r *RebuildContractRecords) error {
				visit := func(RebuildContractRecord) error {
					called = true
					return nil
				}
				switch mode {
				case "empty ID":
					return r.Add(RebuildContractRecord{})
				case "empty clear":
					return r.ClearID("")
				case "bucket key":
					require.NoError(t, b.pending.Set(rebuildContractRecordKey('i', ""), nil, pebble.NoSync))
					return r.VisitIDs(func(string) error { return visit(item) })
				case "record key":
					require.NoError(t, r.write(rebuildContractRecordKey('r', "a"), item))
					return r.VisitByID("a", visit)
				case "record identity":
					key := binary.BigEndian.AppendUint64(rebuildContractRecordKey('r', "a"), 0)
					bad := item
					bad.ID = "wrong"
					require.NoError(t, r.write(key, bad))
					return r.VisitByID("a", visit)
				case "changed ID":
					require.NoError(t, r.Add(item))
					return r.UpdateByID("a", func(live *RebuildContractRecord) (bool, error) {
						live.ID = "wrong"
						return true, nil
					})
				case "nested representative":
					require.NoError(t, r.Add(item))
					return r.VisitRepresentatives(func(RebuildContractRecord) error { return r.VisitRepresentatives(visit) })
				default:
					require.NoError(t, r.Add(item))
					second := item
					second.ID = "b"
					require.NoError(t, r.Add(second))
					return r.VisitCurrentRepresentatives(func(c RebuildContractRecord) error {
						if c.ID == "b" {
							return visit(c)
						}
						return r.UpdateByID("b", func(live *RebuildContractRecord) (bool, error) {
							live.SymbolID = "wrong"
							return true, nil
						})
					})
				}
			})
			require.ErrorIs(t, err, errInvalidRecord)
			require.False(t, called)
			has, err := rebuildGuardHasPrefix(ctx, b.pending, rebuildContractRecordPrefix)
			require.NoError(t, err)
			require.False(t, has)
		})
	}
}
