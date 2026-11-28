package contracts

import (
	"cmp"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRebuildRouterPrefixFactsOrdersOverlappingBuckets(t *testing.T) {
	const path = "routes.py"
	const first, second, last = "http::GET::/api/x", "http::GET::/x", "http::GET::/v2/api/x"
	files := srcMap{path: `from fastapi import APIRouter

a = APIRouter(prefix="/api")
b = APIRouter(prefix="/v2")

@a.get("/x")
def first():
    pass

@b.get("/api/x")
def second():
    pass
`}
	facts := map[string]RouterFacts{path: RouterFileFacts(path, files.reader()(path))}
	// ReplaceByID leaves empty buckets in AllIDs. Compare live records,
	// as the contract writer does, not all keys in the registry map.
	liveIDs := func(reg *Registry) []string {
		records := reg.All()
		ids := make([]string, 0, len(records))
		for _, c := range records {
			ids = append(ids, c.ID)
		}
		slices.Sort(ids)
		return ids
	}
	newRegistry := func(t *testing.T) *Registry {
		t.Helper()
		reg := NewRegistry()
		extractInto(t, reg, files, "", "")
		require.Equal(t, []string{first, second}, liveIDs(reg))
		return reg
	}
	want := []string{first, last}
	native := newRegistry(t)
	JoinRouterPrefixFacts(native, facts)
	require.Contains(t, [][]string{want, {last}}, liveIDs(native))
	require.Empty(t, native.ByID(second))
	for _, ascending := range []bool{true, false} {
		// Exercise two allowed Registry.All orders through the exact native
		// collector and apply loop. Never wait for the Go map to vary in CI.
		ordered := newRegistry(t)
		representatives := ordered.All()
		slices.SortFunc(representatives, func(a, b Contract) int { return cmp.Compare(a.ID, b.ID) })
		if !ascending {
			slices.Reverse(representatives)
		}
		applyRouterRewrites(ordered, collectRouterRewrites(ordered, representatives, facts, nil))
		if ascending {
			require.Equal(t, want, liveIDs(ordered))
		} else {
			require.Equal(t, []string{last}, liveIDs(ordered))
		}
		require.Empty(t, ordered.ByID(second))
	}

	for _, cut := range []string{"early", "late"} {
		t.Run(cut, func(t *testing.T) {
			reg := newRegistry(t)
			overlap, missing := JoinRebuildRouterPrefixFacts(reg, facts, "")
			require.Empty(t, missing)
			require.Equal(t, []string{path}, overlap, "one file is a native map-order difference")
			require.Equal(t, want, liveIDs(reg))
			require.Empty(t, reg.ByID(second))
		})
	}
}
