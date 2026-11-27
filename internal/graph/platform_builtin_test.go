package graph

import (
	"crypto/sha256"
	"fmt"
	"maps"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPlatformCatalogReceiverEvidence(t *testing.T) {
	const revision = PlatformCatalogRevision
	require.Len(t, platformGoByIDV1, 44) // Pinned D94 set; the resolver test checks identity of each entry.
	for _, tc := range []struct {
		family, form, member string
		exact, probable      string
	}{
		{"array", "instance", "slice", "array", "array/string"},
		{"array", "static", "from", "array.static", "array.static"},
		{"string", "instance", "slice", "string", "array/string"},
		{"string", "static", "fromCharCode", "string.static", "string.static"},
		{"map", "instance", "get", "map", "map"},
		{"set", "instance", "has", "set", "map/set"},
		{"promise", "instance", "then", "promise", "promise"},
		{"promise", "static", "resolve", "promise.static", "promise.static"},
		{"number", "instance", "toFixed", "number", "number"},
		{"number", "static", "isFinite", "number.static", "number.static"},
		{"json", "static", "parse", "json", "json"},
		{"object", "static", "keys", "object", "array/map/object"},
		{"console", "static", "log", "console", "console"},
	} {
		t.Run(tc.family+"/"+tc.form, func(t *testing.T) {
			hint := BuiltinReceiverHint{Language: "typescript", Family: tc.family, Form: tc.form}
			exact, ok := LookupPlatformMember(revision, hint, tc.member)
			require.True(t, ok)
			require.Equal(t, "repo::builtin::ts::"+tc.exact+"::"+tc.member, exact.ID("repo"))
			read, ok := LookupPlatformBuiltin(revision, exact.ID("repo"))
			require.True(t, ok)
			require.Equal(t, exact, read)
			for _, form := range []string{"", "unknown"} {
				hint.Form = form
				_, ok := LookupPlatformMember(revision, hint, tc.member)
				require.False(t, ok)
				probable, ok := LookupPlatformCandidate(revision, hint.Language, tc.member)
				require.True(t, ok)
				require.Equal(t, "builtin::ts::"+tc.probable+"::"+tc.member, probable.ID(""))
			}
		})
	}
	for _, id := range []string{"builtin::js::string::find", "builtin::js::map::size", "builtin::js::array/string::slice", "builtin::go::type::Far", "stdlib::go::len", "::builtin::go::len"} {
		_, ok := LookupPlatformBuiltin(revision, id)
		require.False(t, ok, id)
	}
	_, ok := LookupPlatformBuiltin(revision+1, "builtin::go::len")
	require.False(t, ok)
	hint := BuiltinReceiverHint{Language: "typescript", Family: "array", Form: "instance"}
	_, ok = LookupPlatformMember(revision+1, hint, "slice")
	require.False(t, ok)
	_, ok = LookupPlatformCandidate(revision+1, hint.Language, "slice")
	require.False(t, ok)
	hint.Module = "user-module"
	_, ok = LookupPlatformMember(revision, hint, "slice")
	require.False(t, ok, "module attribution cannot prove a platform receiver")
}

func TestBuiltinCallLinkStatus(t *testing.T) {
	for _, tc := range []struct {
		target string
		kind   EdgeKind
		raw    any
		want   LinkStatus
	}{
		{"builtin::go::len", EdgeCalls, "platform", LinkStatusPlatform},
		{"repo::builtin::js::array/string::slice", EdgeCalls, "probable-platform", LinkStatusProbablePlatform},
		{"builtin::go::len", EdgeCalls, "resolved", ""},
		{"builtin::go::len", EdgeCalls, nil, ""},
		{"builtin::go::len", EdgeCalls, true, ""},
		{"builtin::go::len", EdgeReads, "platform", ""},
		{"app.js::ordinary", EdgeCalls, "platform", LinkStatusResolved},
		{"unresolved::*.slice", EdgeCalls, nil, LinkStatusUnresolved},
	} {
		edge := &Edge{Kind: tc.kind, To: tc.target, Meta: map[string]any{MetaLinkStatus: tc.raw}}
		got, ok := edge.LinkStatus()
		require.Equal(t, tc.want != "", ok)
		require.Equal(t, tc.want, got)
	}
}

// Revision 1's probable IDs preserve the original classifier labels. Add a
// revision rather than altering either count or name/category pair here.
func TestPlatformCatalogLegacyV1Pinned(t *testing.T) {
	for _, tc := range []struct {
		lang, digest string
		count        int
		labels       map[string]string
	}{
		{"js", "02b58a6f875ca4e21e419c9bd6d309b3dbfbf664467ac3b243f6965f8c63c0e2", 120, legacyJSBuiltinsV1},
		{"py", "359d3812b207940b3f251a36df50266f6e8a7ba8a66e32998f60e8b29038015b", 38, legacyPythonBuiltinsV1},
	} {
		t.Run(tc.lang, func(t *testing.T) {
			require.Len(t, tc.labels, tc.count)
			h := sha256.New()
			for _, name := range slices.Sorted(maps.Keys(tc.labels)) {
				entry, ok := LookupPlatformCandidate(1, tc.lang, name)
				require.True(t, ok, name)
				_, _ = fmt.Fprintf(h, "%s=%s\n", name, entry.Category)
			}
			require.Equal(t, tc.digest, fmt.Sprintf("%x", h.Sum(nil)))
		})
	}
}
