package resolver

import (
	"cmp"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestRebuildTemporalConstCollapseOrder(t *testing.T) {
	for _, tc := range []struct {
		name      string
		reverse   bool
		threshold int
		passes    int
		rows      int
	}{
		{"dependent-first-1B", false, 1, 8, 9},
		{"referent-first-1MiB", true, 1 << 20, 1, 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			facts, aliases := temporalConstChain(tc.reverse)
			view, native := temporalConstFixture(t, facts)
			order := slices.Sorted(maps.Keys(aliases))
			backward := slices.Clone(order)
			slices.Reverse(backward)
			seed := buildConstDerefMap(native, []string{"Terminal"})
			require.Equal(t, map[string]string{"Terminal": "workflow"}, seed)
			// Read raw aliases from the same saved native candidates. The
			// fixture has one candidate per alias, so only sweep order varies.
			saved := native.FindNodesByNames(order)
			for _, name := range order {
				require.Len(t, saved[name], 1)
				require.Equal(t, aliases[name], saved[name][0].Meta["const_ref"])
			}
			forwardRows := orderedTemporalConstRows(forceTemporalConstSweep(order, aliases, maps.Clone(seed)))
			backwardRows := orderedTemporalConstRows(forceTemporalConstSweep(backward, aliases, maps.Clone(seed)))
			require.Len(t, forwardRows, tc.rows)
			if tc.reverse {
				require.Len(t, backwardRows, 9)
			} else {
				require.Len(t, backwardRows, 10)
			}
			t.Logf("raw sorted sweep: %v; raw reverse sweep: %v", forwardRows, backwardRows)

			names := append(slices.Clone(backward), "Terminal", "A01", "")
			if tc.threshold > 1 {
				// More than 1 MiB of real name-set rows, without adding any
				// candidate or resolving any alias outside the raw witness.
				wide := strings.Repeat("n", 20<<10)
				for i := range 52 {
					names = append(names, fmt.Sprintf("absent-%02d-%s", i, wide))
				}
			}
			got, stats := readRebuildTemporalConst(t, view, tc.threshold, names, names)
			require.Equal(t, forwardRows, got)
			require.Equal(t, len(names)-2, stats.Names)
			require.Zero(t, stats.DiscoveryPasses)
			require.Equal(t, tc.passes, stats.CollapsePasses)
			require.Greater(t, stats.ScratchBytes, uint64(tc.threshold))
			require.Zero(t, stats.SpillBytes)
			t.Logf("saved order-locked results (%d): %v; scratch bytes=%d, L0 table bytes flushed=%d",
				len(got), got, stats.ScratchBytes, stats.SpillBytes)
		})
	}
}

func TestRebuildTemporalConstDiscoveryCap(t *testing.T) {
	facts, aliases := temporalConstChain(false)
	view, native := temporalConstFixture(t, facts)
	queries := append(slices.Sorted(maps.Keys(aliases)), "Terminal")
	for _, tc := range []struct {
		root string
		rows int
	}{
		{"A01", 0}, // the literal is nine hops away, and was never selected
		{"A02", 9}, // the literal is eight hops away, and was selected
	} {
		t.Run(tc.root, func(t *testing.T) {
			seeds := []string{tc.root}
			want := orderedTemporalConstRows(buildConstDerefMap(native, seeds))
			got, stats := readRebuildTemporalConst(t, view, 1, seeds, queries)
			require.Len(t, got, tc.rows)
			require.Equal(t, want, got)
			require.Equal(t, 9, stats.Names)
			require.Equal(t, 8, stats.DiscoveryPasses)
			require.Zero(t, stats.SpillBytes)
		})
	}
}

func TestRebuildTemporalConstCandidateOrderAndLiterals(t *testing.T) {
	// The smaller ID sorts last in the T4 transport's repo order. Native
	// last-wins slots must use the complete exact-name candidate identity.
	facts := []temporalConstFact{
		{id: "a", repo: "z", name: "ChooseAlias", kind: graph.KindMethod, ref: "Left"},
		{id: "z", repo: "a", name: "ChooseAlias", kind: graph.KindFunction, ref: "Right"},
		{id: "b", repo: "z", name: "ChooseJava", kind: graph.KindField, java: "left-java", sidecar: "ignored"},
		{id: "y", repo: "a", name: "ChooseJava", kind: graph.KindConstant, java: "right-java"},
		{id: "left", name: "Left", kind: graph.KindConstant, sidecar: "left"},
		{id: "right", name: "Right", kind: graph.KindMethod, sidecar: "right"},
		{id: "conflict1", name: "Conflict", kind: graph.KindConstant, sidecar: "str"},
		{id: "conflict2", name: "Conflict", kind: graph.KindField, java: "other"},
		{id: "fallback1", name: "AliasAfterConflict", kind: graph.KindConstant, ref: "Left", sidecar: "str"},
		{id: "fallback2", name: "AliasAfterConflict", kind: graph.KindConstant, sidecar: "other"},
		{id: "own", name: "OwnLiteral", kind: graph.KindConstant, ref: "Right", java: "own"},
		{id: "agree1", name: "Agree", kind: graph.KindFunction, sidecar: "same"},
		{id: "agree2", name: "Agree", kind: graph.KindConstant, java: "same", sidecar: "same"},
		{id: "cycle1", name: "Cycle1", kind: graph.KindConstant, ref: "Cycle2"},
		{id: "cycle2", name: "Cycle2", kind: graph.KindConstant, ref: "Cycle1"},
		{id: "ignored", name: "Ignored", kind: graph.KindVariable, ref: "Unselected", sidecar: "ignored"},
		{id: "unselected", name: "Unselected", kind: graph.KindConstant, sidecar: "not selected"},
		{id: "empty", name: "Empty", kind: graph.KindConstant},
	}
	seeds := []string{"ChooseAlias", "ChooseJava", "Conflict", "AliasAfterConflict", "OwnLiteral", "Agree", "Cycle1", "Ignored", "Missing", "Empty"}
	queries := append(slices.Clone(seeds), "Left", "Right", "Unselected", "Cycle2")
	for _, reverse := range []bool{false, true} {
		t.Run(fmt.Sprintf("reversed-ingest=%t", reverse), func(t *testing.T) {
			if reverse {
				slices.Reverse(facts)
			}
			view, native := temporalConstFixture(t, facts)
			rawReverse := buildConstDerefMap(temporalConstNativeOrder{Store: native, descending: true}, seeds)
			locked := buildConstDerefMap(temporalConstNativeOrder{Store: native}, seeds)
			require.Equal(t, "left", rawReverse["ChooseAlias"])
			require.Equal(t, "left-java", rawReverse["ChooseJava"])
			require.Equal(t, map[string]string{
				"ChooseAlias": "right", "ChooseJava": "right-java", "Left": "left", "Right": "right",
				"AliasAfterConflict": "left", "OwnLiteral": "own", "Agree": "same",
			}, locked)
			got, stats := readRebuildTemporalConst(t, view, 1, seeds, queries)
			require.Equal(t, orderedTemporalConstRows(locked), got)
			require.Equal(t, 13, stats.Names)
			require.Zero(t, stats.SpillBytes)
			t.Logf("raw reverse candidates: %v; saved order-locked results (%d): %v", orderedTemporalConstRows(rawReverse), len(got), got)
		})
	}
}

// Pinned temporal_calls.go:1837–1851 with only map iteration forced. Keep
// same-pass writes visible, so this witnesses the raw cap, not graph closure.
func forceTemporalConstSweep(names []string, aliasRef, out map[string]string) map[string]string {
	for pass := 0; pass < 8; pass++ {
		progressed := false
		for _, name := range names {
			ref := aliasRef[name]
			if _, has := out[name]; has {
				continue
			}
			if v, ok := out[ref]; ok && v != "" {
				out[name] = v
				progressed = true
			}
		}
		if !progressed {
			break
		}
	}
	return out
}

// Force candidate order only, then call the unchanged native component.
// Bulk sidecar reads still use the native SQLite ConstantValueReader.
type temporalConstNativeOrder struct {
	*store_sqlite.Store
	descending bool
}

func (g temporalConstNativeOrder) FindNodesByNames(names []string) map[string][]*graph.Node {
	out := g.Store.FindNodesByNames(names)
	for _, nodes := range out {
		slices.SortFunc(nodes, func(a, b *graph.Node) int { return cmp.Compare(a.ID, b.ID) })
		if g.descending {
			slices.Reverse(nodes)
		}
	}
	return out
}

type temporalConstFact struct {
	id, repo, name, ref, java, sidecar string

	kind graph.NodeKind
}

func temporalConstChain(reverse bool) ([]temporalConstFact, map[string]string) {
	aliases := map[string]string{}
	names := make([]string, 9)
	for i := range names {
		names[i] = fmt.Sprintf("A%02d", i+1)
	}
	if reverse {
		slices.Reverse(names)
	}
	names = append(names, "Terminal")
	facts := []temporalConstFact{{id: "terminal", name: "Terminal", kind: graph.KindConstant, sidecar: "workflow"}}
	for i, name := range names[:9] {
		aliases[name] = names[i+1]
		facts = append(facts, temporalConstFact{id: name, name: name, kind: graph.KindConstant, ref: names[i+1]})
	}
	return facts, aliases
}

func temporalConstFixture(t *testing.T, facts []temporalConstFact) (store_pebble.RebuildView, *store_sqlite.Store) {
	t.Helper()
	b, err := store_pebble.OpenBuild(filepath.Join(t.TempDir(), "worker"), store_pebble.FullProfile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, b.Close()) })
	native, err := store_sqlite.Open(filepath.Join(t.TempDir(), "native.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, native.Close()) })
	for _, f := range facts {
		language := "go"
		if f.java != "" {
			language = "java"
		}
		file := f.id + "." + language
		node := &graph.Node{ID: file + "::" + f.name, Name: f.name, Kind: f.kind, Language: language, FilePath: file, RepoPrefix: f.repo,
			Meta: map[string]any{"const_ref": f.ref, "value": f.java}}
		parsed := store_pebble.ParseFacts{
			File:  graph.FileMetaRow{FilePath: file, NodeCount: 1},
			Nodes: []*graph.Node{node},
		}
		if f.sidecar != "" || f.name == "Empty" {
			parsed.Constants = []graph.ConstantValueRow{{NodeID: node.ID, FilePath: file, Value: f.sidecar}}
		}
		require.NoError(t, b.AddParsedFile(f.repo, parsed))
		require.NoError(t, native.AddBatchChecked(parsed.Nodes, nil))
		require.NoError(t, native.BulkSetConstantValues(f.repo, parsed.Constants))
	}
	return b.RebuildView(), native
}

type temporalConstRow struct{ name, value string }

func orderedTemporalConstRows(values map[string]string) []temporalConstRow {
	var rows []temporalConstRow
	for _, name := range slices.Sorted(maps.Keys(values)) {
		rows = append(rows, temporalConstRow{name, values[name]})
	}
	return rows
}

func readRebuildTemporalConst(t *testing.T, view store_pebble.RebuildView, threshold int, seeds, query []string) ([]temporalConstRow, store_pebble.TemporalConstStats) {
	t.Helper()
	ctx := t.Context()
	var rows []temporalConstRow
	stats, err := withRebuildTemporalConstants(ctx, view, threshold, func(yield func(string) error) error {
		for _, name := range seeds {
			if err := yield(name); err != nil {
				return err
			}
		}
		return nil
	}, func(values *store_pebble.TemporalConstDeref) error {
		ordered := slices.Clone(query)
		slices.Sort(ordered)
		for _, name := range slices.Compact(ordered) {
			value, err := values.Value(ctx, name)
			if err != nil {
				return err
			}
			if value != "" {
				rows = append(rows, temporalConstRow{name, value})
			}
		}
		return nil
	})
	require.NoError(t, err)
	return rows, stats
}
