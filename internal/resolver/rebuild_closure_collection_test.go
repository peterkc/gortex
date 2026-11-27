package resolver

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

const (
	rebuildCCDispatch = "cc_dispatch_field"
	rebuildCCAppend   = "cc_append_field"
)

func TestRebuildClosureCollectionSQLiteOracle(t *testing.T) {
	for _, spillAt := range []int{1, 1 << 20} {
		t.Run(fmt.Sprint(spillAt), func(t *testing.T) {
			const file, language = "request.swift", "swift"
			dispatch := rebuildClosureCollectionNode(file, "dispatch", language, rebuildCCDispatch, "handlers")
			keep := rebuildClosureCollectionNode(file, "keep", language, rebuildCCAppend, "handlers")
			register := rebuildClosureCollectionNode(file, "register", language, rebuildCCAppend, "handlers")
			incumbent := rebuildObserverEdge(dispatch, keep, graph.EdgeCalls, dispatch.StartLine)
			incumbent.Meta = map[string]any{"fixture": "durable incumbent"}
			nodes := []*graph.Node{dispatch, keep, register}
			var extra int
			if spillAt == 1<<20 {
				// Each role's candidate stream exceeds 1 MiB, but no field is over cap.
				const pairs = 40
				padding := strings.Repeat("x", 32<<10)
				for i := range pairs {
					field := fmt.Sprintf("items%02d%s", i, padding)
					dispatchName, registerName := fmt.Sprintf("dispatch%02d", i), fmt.Sprintf("register%02d", i)
					nodes = append(nodes,
						rebuildClosureCollectionNode(file, dispatchName, language, rebuildCCDispatch, field),
						rebuildClosureCollectionNode(file, registerName, language, rebuildCCAppend, field),
					)
				}
				extra = pairs
			}
			facts := rebuildEventFacts(file, language, nodes, incumbent)
			proof, b, native := openRebuildObserverStores(t, nil, facts)
			receipts := rebuildGRPCReceipts(proof...)
			before := rebuildEventSavedRows(t, b)
			disabled, err := NewFrameworkSynthesizerSelection(nil)
			require.NoError(t, err)
			stats, err := RunRebuildClosureCollection(t.Context(), b, receipts, disabled, spillAt)
			require.NoError(t, err)
			require.Zero(t, stats)
			require.Equal(t, before, rebuildEventSavedRows(t, b))

			selection := rebuildClosureCollectionSelection(t)
			var first rebuildEventRows
			for run := range 2 {
				require.NoError(t, RunFrameworkSynthesizersWithSelection(native, selection).Err)
				want := rebuildEventSQLiteRows(native)
				require.Len(t, want.edges, 2+extra)
				require.Contains(t, want.edges, incumbent)
				stats, err := RunRebuildClosureCollection(t.Context(), b, receipts, selection, spillAt)
				require.NoError(t, err)
				got := rebuildEventSavedRows(t, b)
				require.Equal(t, want, got)
				require.Zero(t, stats.SpillBytes)
				if run == 0 {
					require.Positive(t, stats.Edges)
					first = got
					continue
				}
				require.Equal(t, first, got)
			}
		})
	}
}

func TestRebuildClosureCollectionRawRoleCaps(t *testing.T) {
	for _, role := range []struct {
		name, capped, opposite string
	}{
		{"registrars", rebuildCCAppend, rebuildCCDispatch},
		{"dispatchers", rebuildCCDispatch, rebuildCCAppend},
	} {
		for _, raw := range []int{8, 9} {
			t.Run(fmt.Sprintf("%s/%d", role.name, raw), func(t *testing.T) {
				const file, language, field = "request.swift", "swift", "handlers"
				both := rebuildClosureCollectionNode(file, "both", language, role.capped, field)
				both.Meta[role.opposite] = field
				nonCallable := rebuildClosureCollectionNode(file, "notCallable", language, role.capped, field)
				nonCallable.Kind = graph.KindField
				nodes := []*graph.Node{both, nonCallable}
				for i := range raw - 3 {
					nodes = append(nodes,
						rebuildClosureCollectionNode(file, fmt.Sprintf("peer%02d", i), language, role.capped, field))
				}
				// The only opposite-role callable is also a self candidate. The
				// foreign Python row still counts, and the Go peer can pair at 8.
				py := rebuildClosureCollectionNode("foreign.py", "foreign", "python", role.capped, field)
				goPeer := rebuildClosureCollectionNode("peer.go", "peer", "go", role.capped, field)
				goPeer.Kind = graph.KindFunction
				proof, b, native := openRebuildObserverStores(t, map[string]string{py.ID: "elsewhere"},
					rebuildEventFacts(file, language, nodes),
					rebuildEventFacts(py.FilePath, py.Language, []*graph.Node{py}),
					rebuildEventFacts(goPeer.FilePath, goPeer.Language, []*graph.Node{goPeer}),
				)
				var wantCount int
				if raw == 8 {
					wantCount = 6
				}
				selection := rebuildClosureCollectionSelection(t)
				report := RunFrameworkSynthesizersWithSelection(native, selection)
				require.NoError(t, report.Err)
				want := rebuildEventSQLiteRows(native)
				require.Len(t, want.edges, wantCount)
				stats, err := RunRebuildClosureCollection(t.Context(), b, rebuildGRPCReceipts(proof...), selection, 1)
				require.NoError(t, err)
				require.Equal(t, uint64(wantCount), stats.Edges)
				require.Equal(t, want, rebuildEventSavedRows(t, b))
				if raw > 8 {
					// The foreign row cannot pair, but its count turns
					// off a field that would otherwise have valid pairs.
					proof[1].Nodes = nil
					proof[1].File.NodeCount = 0
					stats, err := RunRebuildClosureCollection(t.Context(), b, rebuildGRPCReceipts(proof...), selection, 1)
					require.ErrorIs(t, err, store_pebble.ErrRebuildClosureCollection)
					require.ErrorIs(t, err, store_pebble.ErrRebuildUnknownProvenance)
					require.Zero(t, stats)
					require.Equal(t, want, rebuildEventSavedRows(t, b))
				}
			})
		}
	}
}

func TestRebuildClosureCollectionSkipsReceiptsWithoutRegistrars(t *testing.T) {
	for _, name := range []string{"no roles", "dispatcher only"} {
		t.Run(name, func(t *testing.T) {
			const file, language = "bus.swift", "swift"
			dispatch := rebuildClosureCollectionNode(file, "dispatch", language, rebuildCCDispatch, "listeners")
			if name == "no roles" {
				dispatch.Meta = nil
			}
			_, b, native := openRebuildObserverStores(t, nil, rebuildEventFacts(file, language, []*graph.Node{dispatch}))
			selection := rebuildClosureCollectionSelection(t)
			report := RunFrameworkSynthesizersWithSelection(native, selection)
			require.NoError(t, report.Err)
			require.Zero(t, report.Total)
			receipts := func(func(store_pebble.ParseFacts, error) bool) {
				t.Fatal("replayed receipts without a registrar")
			}
			stats, err := RunRebuildClosureCollection(t.Context(), b, receipts, selection, 1)
			require.NoError(t, err)
			require.Zero(t, stats.Edges)
			require.Equal(t, rebuildEventSQLiteRows(native), rebuildEventSavedRows(t, b))
		})
	}
}

func TestRebuildClosureCollectionRefusesUnprovedNode(t *testing.T) {
	for _, mode := range []string{"complete", "missing", "changed field", "absent versus nil", "nil versus absent", "absent versus empty", "absent versus non-string", "different non-strings"} {
		t.Run(mode, func(t *testing.T) {
			const earlyFile, lateFile, language = "early.swift", "late.swift", "swift"
			earlyDispatch := rebuildClosureCollectionNode(earlyFile, "dispatch", language, rebuildCCDispatch, "alpha")
			earlyRegister := rebuildClosureCollectionNode(earlyFile, "register", language, rebuildCCAppend, "alpha")
			lateDispatch := rebuildClosureCollectionNode(lateFile, "dispatch", language, rebuildCCDispatch, "zeta")
			lateRegister := rebuildClosureCollectionNode(lateFile, "register", language, rebuildCCAppend, "zeta")
			switch mode {
			case "different non-strings":
				lateRegister.Meta[rebuildCCDispatch] = []string{"zeta"}
			case "nil versus absent":
				lateRegister.Meta[rebuildCCDispatch] = nil
			}
			proof, b, native := openRebuildObserverStores(t, nil,
				rebuildEventFacts(earlyFile, language, []*graph.Node{earlyDispatch, earlyRegister}),
				rebuildEventFacts(lateFile, language, []*graph.Node{lateDispatch, lateRegister}),
			)
			before := rebuildEventSavedRows(t, b)
			selection := rebuildClosureCollectionSelection(t)
			require.NoError(t, RunFrameworkSynthesizersWithSelection(native, selection).Err)
			want := rebuildEventSQLiteRows(native)
			require.Len(t, want.edges, 2)
			// Change only the later pair's receipt. Equal native field
			// names still link; a missing or different field refuses.
			switch mode {
			case "missing":
				proof[1].Nodes = proof[1].Nodes[:1]
				proof[1].File.NodeCount = 1
			case "changed field":
				proof[1].Nodes[1].Meta[rebuildCCAppend] = "different"
			case "absent versus nil":
				proof[1].Nodes[1].Meta[rebuildCCDispatch] = nil
			case "nil versus absent":
				_, present := proof[1].Nodes[1].Meta[rebuildCCDispatch]
				require.True(t, present)
				delete(proof[1].Nodes[1].Meta, rebuildCCDispatch)
			case "absent versus empty":
				proof[1].Nodes[1].Meta[rebuildCCDispatch] = ""
			case "absent versus non-string":
				proof[1].Nodes[1].Meta[rebuildCCDispatch] = true
			case "different non-strings":
				proof[1].Nodes[1].Meta[rebuildCCDispatch] = []string{"other"}
			}
			stats, err := RunRebuildClosureCollection(t.Context(), b, rebuildGRPCReceipts(proof...), selection, 1)
			if mode != "missing" && mode != "changed field" {
				require.NoError(t, err)
				require.Equal(t, uint64(2), stats.Edges)
				require.Equal(t, want, rebuildEventSavedRows(t, b))
				return
			}
			var refusal *store_pebble.RebuildUnknownProvenanceError
			require.ErrorAs(t, err, &refusal)
			require.Zero(t, stats)
			require.Equal(t, before, rebuildEventSavedRows(t, b))
		})
	}
}

func rebuildClosureCollectionNode(file, name, language, role, field string) *graph.Node {
	n := rebuildEventNode(file, name, language, "")
	n.Kind = graph.KindMethod
	n.StartLine = 12
	n.Meta = map[string]any{role: field}
	return n
}

func rebuildClosureCollectionSelection(t *testing.T) FrameworkSynthesizerSelection {
	t.Helper()
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthClosureCollection})
	require.NoError(t, err)
	return selection
}
