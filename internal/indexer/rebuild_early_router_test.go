package indexer

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

func TestRebuildEarlyRouterHiddenBucketSibling(t *testing.T) {
	const own, mount, id = "routes.ts", "mount.ts", "http::GET::/x"
	for _, tc := range []struct {
		name, localPrefix string
		checkedMount      bool
		want              []string
	}{
		{name: "open bucket refuses unchecked mount", localPrefix: "/local"},
		{name: "open bucket uses checked mount", localPrefix: "/local", checkedMount: true, want: []string{"http::GET::/api/x", "http::GET::/local/x"}},
		{name: "closed bucket ignores hidden sibling", want: []string{id}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			build, err := store_pebble.OpenBuild(t.TempDir(), store_pebble.FullProfile)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, build.Close()) })
			for _, path := range []string{own, mount} {
				build.AddNode(&graph.Node{ID: path, Kind: graph.KindFile, FilePath: path, Language: "typescript"})
			}
			newRegistry := func() *contracts.Registry {
				reg := contracts.NewRegistry()
				for i, framework := range []string{"nestjs", "express"} {
					reg.Add(contracts.Contract{
						ID: id, Type: contracts.ContractHTTP, Role: contracts.RoleProvider, FilePath: own, Line: i + 1,
						Meta: map[string]any{"framework": framework, "method": "GET", "path": "/x"},
					})
				}
				return reg
			}
			reg := newRegistry()
			require.Len(t, reg.All(), 1, "the second record cannot open the bucket")
			require.Len(t, reg.ByID(id), 2)
			facts := map[string]contracts.RouterFacts{
				own: {LineCount: 2, Lines: []contracts.RouterLineFacts{
					{Line: 1, NestPrefix: tc.localPrefix},
					{Line: 2, Receivers: map[string]string{"express": "router"}},
				}},
			}
			if tc.checkedMount {
				facts[mount] = contracts.RouterFileFacts(mount, []byte("app.use('/api', router)"))
			}
			overlaps, err := joinRebuildRouterPrefixes(ctx, build.RebuildView(), reg, "", "", facts)
			require.Empty(t, overlaps)
			if tc.want == nil {
				require.ErrorIs(t, err, errRebuildRouterFactsMissing)
				require.ErrorContains(t, err, `"mount.ts"`)
				require.Equal(t, []string{id}, reg.AllIDs())
				require.Len(t, reg.ByID(id), 2, "refusal does not mutate the registry")
				return
			}
			require.NoError(t, err)
			native := newRegistry()
			contracts.JoinRouterPrefixFacts(native, facts)
			records := reg.All()
			require.ElementsMatch(t, native.All(), records)
			ids := make([]string, 0, len(records))
			for _, c := range records {
				ids = append(ids, c.ID)
			}
			require.ElementsMatch(t, tc.want, ids)
			if tc.checkedMount {
				require.Empty(t, reg.ByID(id), "the old map key holds no records")
			}
		})
	}
}
