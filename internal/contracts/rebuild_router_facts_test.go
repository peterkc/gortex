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
	newRegistry := func() *Registry {
		reg := NewRegistry()
		extractInto(t, reg, files, "", "")
		require.ElementsMatch(t, []string{first, second}, reg.AllIDs())
		return reg
	}
	want := []string{first, last}
	for _, ascending := range []bool{true, false} {
		// Exercise two allowed Registry.All orders through the exact native
		// collector and apply loop. Never wait for the Go map to vary in CI.
		native := newRegistry()
		representatives := native.All()
		slices.SortFunc(representatives, func(a, b Contract) int { return cmp.Compare(a.ID, b.ID) })
		if !ascending {
			slices.Reverse(representatives)
		}
		applyRouterRewrites(native, collectRouterRewrites(native, representatives, facts, nil))
		if ascending {
			require.ElementsMatch(t, want, native.AllIDs())
		} else {
			require.Equal(t, []string{last}, native.AllIDs())
		}
	}

	for _, cut := range []string{"early", "late"} {
		t.Run(cut, func(t *testing.T) {
			reg := newRegistry()
			overlap, missing := JoinRebuildRouterPrefixFacts(reg, facts, "")
			require.Empty(t, missing)
			require.Equal(t, []string{path}, overlap, "one file is a native map-order difference")
			require.ElementsMatch(t, want, reg.AllIDs())
		})
	}
}
