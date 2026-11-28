package store_pebble

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildCheckedRouterMountsMatchNative(t *testing.T) {
	for _, missingFile := range []string{"", "z-mount.ts"} {
		t.Run("mount_"+missingFile, func(t *testing.T) {
			b, err := OpenBuild(t.TempDir(), FullProfile)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, b.Close()) })
			const repo, workspace = "r", "w"
			const pyID, nestID = "http::GET::/items", "http::GET::/users"
			node := func(path string) *graph.Node {
				return &graph.Node{ID: path, FilePath: path, Kind: graph.KindFile, RepoPrefix: repo, WorkspaceID: workspace}
			}
			record := func(id, path, file, framework string) contracts.Contract {
				return contracts.Contract{ID: id, FilePath: file, Line: 1, Type: contracts.ContractHTTP, Role: contracts.RoleProvider,
					Meta: map[string]any{"framework": framework, "method": "GET", "path": path}}
			}
			inputs := []contracts.Contract{
				record(pyID, "/items", "routes.py", "fastapi/flask"),
				record(nestID, "/users", "controller.ts", "nestjs"),
			}
			facts := map[string]contracts.RouterFacts{
				"routes.py": {LineCount: 1, Lines: []contracts.RouterLineFacts{{Line: 1, Receivers: map[string]string{"fastapi/flask": "router"}}},
					SelfPrefix: map[string]string{"router": "/local", "parent": "/parent"}},
				"controller.ts": {ControllerClass: "Users", LineCount: 1, Lines: []contracts.RouterLineFacts{{Line: 1, NestPrefix: "/ctrl"}}},
				"a-mount.py": {MountPrefix: map[string]string{"router": "/ignored", "parent": "/top"},
					MountParent: map[string]string{"router": "parent", "parent": "router"}},
				"m-mount.ts": {MountPrefix: map[string]string{"router": "/conflict"},
					ControllerModule: map[string]string{"Users": "Module"}, ModulePrefix: map[string]string{"Module": "/old"}},
				"z-mount.ts": {ControllerModule: map[string]string{"Users": "Later"},
					ModulePrefix: map[string]string{"Module": "/new", "Later": "/not-chosen"}},
			}
			for path := range facts {
				b.AddNode(node(path))
			}
			if missingFile != "" {
				delete(facts, missingFile)
			}
			foreign := node("foreign-mount.ts")
			foreign.WorkspaceID = "other"
			b.AddNode(foreign)
			reg := contracts.NewRegistry()
			for _, c := range inputs {
				reg.Add(c)
			}
			_, wantMissing := contracts.JoinRebuildRouterPrefixFacts(reg, facts, missingFile)
			require.Equal(t, missingFile, wantMissing)
			require.NoError(t, b.WithRebuildContractRecords(t.Context(), func(r *RebuildContractRecords) error {
				for _, c := range inputs {
					require.NoError(t, r.Add(RebuildContractRecord{Contract: c}))
				}
				gotMissing, err := r.JoinCheckedRouterPrefixes(repo, workspace, func(path string) (*contracts.RouterFacts, error) {
					require.NotEqual(t, foreign.ID, path)
					f, ok := facts[path]
					if !ok {
						return nil, nil
					}
					return &f, nil
				}, func(path string) error {
					t.Fatalf("unexpected router rewrite overlap %s", path)
					return nil
				})
				require.Equal(t, wantMissing, gotMissing)
				if wantMissing == "" {
					require.NoError(t, err)
					require.Len(t, reg.ByID("http::GET::/new/ctrl/users"), 1)
				} else {
					require.Error(t, err)
				}
				var ids []string
				require.NoError(t, r.VisitIDs(func(id string) error {
					ids = append(ids, id)
					var actual []contracts.Contract
					require.NoError(t, r.VisitByID(id, func(item RebuildContractRecord) error {
						actual = append(actual, item.Contract)
						return nil
					}))
					require.Equal(t, reg.ByID(id), actual)
					return nil
				}))
				require.ElementsMatch(t, reg.AllIDs(), ids)
				return nil
			}))
		})
	}
}
