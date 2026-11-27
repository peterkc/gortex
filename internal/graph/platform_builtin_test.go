package graph

import (
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
