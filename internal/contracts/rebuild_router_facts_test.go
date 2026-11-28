package contracts

import (
	"cmp"
	"maps"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRebuildRouterPrefixFactsOrdersOverlappingBuckets(t *testing.T) {
	const aPath, bPath = "a.py", "b.py"
	const first, second, last = "http::GET::/api/x", "http::GET::/x", "http::GET::/v2/api/x"
	files := srcMap{
		aPath: `from fastapi import APIRouter

a = APIRouter(prefix="/api")

@a.get("/x")
def first():
    pass
`,
		bPath: `from fastapi import APIRouter

b = APIRouter(prefix="/v2")

@b.get("/api/x")
def second():
    pass
`,
	}
	facts := map[string]RouterFacts{
		aPath: RouterFileFacts(aPath, files.reader()(aPath)),
		bPath: RouterFileFacts(bPath, files.reader()(bPath)),
	}
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

	reg := newRegistry(t)
	overlap, missing := JoinRebuildRouterPrefixFacts(reg, facts, nil)
	require.Empty(t, missing)
	require.Equal(t, []string{aPath, bPath}, overlap, "both files are native map-order differences")
	require.Equal(t, want, liveIDs(reg))
	require.Empty(t, reg.ByID(second))
}

func TestRouterPrefixFactUse(t *testing.T) {
	shared := map[string]bool{
		"actix":         true,
		"axum":          true,
		"express":       true,
		"fastapi/flask": true,
		"fiber":         true,
		"flask":         true,
		"gin/echo/chi":  true,
		"laravel":       false,
		"nestjs":        true,
		"rails":         false,
		"spring":        false,
		"vapor":         true,
	}
	require.ElementsMatch(t, slices.Collect(maps.Keys(routePrefixFrameworks)), slices.Collect(maps.Keys(shared)))
	for fw, canShare := range shared {
		t.Run(fw, func(t *testing.T) {
			c := Contract{Type: ContractHTTP, Line: 1, Meta: map[string]any{"framework": fw}}
			withKey := &RouterFacts{
				LineCount: 1,
				Lines:     []RouterLineFacts{{Line: 1, Receivers: map[string]string{fw: "receiver"}}},
			}
			if fw == "nestjs" {
				withKey.ControllerClass = "Controller"
				withKey.Lines = nil
			}
			for _, tc := range []struct {
				name  string
				own   *RouterFacts
				cross bool
			}{
				{name: "unknown own file", cross: canShare},
				{name: "empty shared key", own: &RouterFacts{LineCount: 1}},
				{name: "nonempty shared key", own: withKey, cross: canShare},
			} {
				t.Run(tc.name, func(t *testing.T) {
					local, cross := RouterPrefixFactUse(c, tc.own)
					require.True(t, local)
					require.Equal(t, tc.cross, cross)
					joined := c
					joined.Meta = map[string]any{"framework": fw, routePrefixJoinedMeta: true}
					local, cross = RouterPrefixFactUse(joined, tc.own)
					require.False(t, local)
					require.False(t, cross)
					nonHTTP := c
					nonHTTP.Type = ContractDependency
					local, cross = RouterPrefixFactUse(nonHTTP, tc.own)
					require.False(t, local)
					require.False(t, cross)
				})
			}
		})
	}
	local, cross := RouterPrefixFactUse(Contract{Type: ContractHTTP, Meta: map[string]any{"framework": "unsupported"}}, nil)
	require.False(t, local)
	require.False(t, cross)
}
