package store_pebble

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildCheckedRouterMountsMatchNative(t *testing.T) {
	for _, tc := range []struct{ name, missingFile string }{
		{name: "complete_mount_facts"},
		{name: "missing_mount_facts", missingFile: "z-mount.ts"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			missingFile := tc.missingFile
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
			var missingCensus []string
			if missingFile != "" {
				delete(facts, missingFile)
				missingCensus = []string{missingFile}
			}
			foreign := node("foreign-mount.ts")
			foreign.WorkspaceID = "other"
			b.AddNode(foreign)
			reg := contracts.NewRegistry()
			for _, c := range inputs {
				reg.Add(c)
			}
			_, wantMissing := contracts.JoinRebuildRouterPrefixFacts(reg, facts, missingCensus)
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
					want := reg.ByID(id)
					var seen int
					require.NoError(t, r.VisitByID(id, func(item RebuildContractRecord) error {
						require.Less(t, seen, len(want), "extra record for %s", id)
						require.Equal(t, want[seen], item.Contract)
						seen++
						return nil
					}))
					require.Len(t, want, seen, "missing records for %s", id)
					return nil
				}))
				require.ElementsMatch(t, reg.AllIDs(), ids)
				return nil
			}))
		})
	}
}

func TestRebuildCheckedRouterMountCensus(t *testing.T) {
	const repo, workspace = "r", "w"
	const rawID, joinedID = "http::GET::/items", "http::GET::/api/items"
	const routeFile, censusFile, foreignFile = "Routes.java", "scan.ts", "foreign.ts"
	for _, tc := range []struct {
		name            string
		enabled, census bool
		wantReads       []string
		wantID          string
	}{
		{name: "disabled registry with census", census: true, wantID: rawID},
		{name: "enabled registry without census", enabled: true, wantID: rawID},
		{name: "local route outside census", enabled: true, census: true, wantReads: []string{routeFile, censusFile}, wantID: joinedID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBuild(t, FullProfile)
			b.AddNode(&graph.Node{ID: foreignFile, FilePath: foreignFile, Kind: graph.KindFile, RepoPrefix: "other", WorkspaceID: workspace})
			if tc.census {
				b.AddNode(&graph.Node{ID: censusFile, FilePath: censusFile, Kind: graph.KindFile, RepoPrefix: repo, WorkspaceID: workspace})
			}
			c := contracts.Contract{
				ID: rawID, FilePath: routeFile, Line: 2, Type: contracts.ContractHTTP, Role: contracts.RoleProvider,
				Meta: map[string]any{"framework": "spring", "method": "GET", "path": "/items"},
			}
			require.NoError(t, b.WithRebuildContractRecords(t.Context(), func(r *RebuildContractRecords) error {
				require.NoError(t, r.Add(RebuildContractRecord{Contract: c}))
				if tc.enabled {
					// This joined record enables the pass without needing mount evidence.
					trigger := contracts.RewriteRouterContract(contracts.Contract{
						ID: "http::GET::/enable", FilePath: routeFile, Line: 1, Type: contracts.ContractHTTP, Role: contracts.RoleProvider,
						Meta: map[string]any{"framework": "express", "method": "GET", "path": "/enable"},
					}, "/on")
					require.NoError(t, r.Add(RebuildContractRecord{Contract: trigger}))
				}
				var reads []string
				missing, err := r.JoinCheckedRouterPrefixes(repo, workspace, func(path string) (*contracts.RouterFacts, error) {
					reads = append(reads, path)
					if path == routeFile {
						return &contracts.RouterFacts{LineCount: 2, Lines: []contracts.RouterLineFacts{{Line: 2, SpringPrefix: "/api"}}}, nil
					}
					return nil, nil // The local Spring route does not read this census answer.
				}, nil)
				require.NoError(t, err)
				require.Empty(t, missing)
				require.Equal(t, tc.wantReads, reads)
				var got []contracts.Contract
				require.NoError(t, r.VisitCurrentRepresentatives(func(item RebuildContractRecord) error {
					if item.Meta["framework"] == "spring" {
						got = append(got, item.Contract)
					}
					return nil
				}))
				require.Len(t, got, 1)
				require.Equal(t, tc.wantID, got[0].ID)
				return nil
			}))
		})
	}
}

func TestRebuildCheckedRouterMountRules(t *testing.T) {
	const routeFile = "route.py"
	c := contracts.Contract{
		ID: "http::GET::/items", FilePath: routeFile, Line: 1, Type: contracts.ContractHTTP, Role: contracts.RoleProvider,
		Meta: map[string]any{"framework": "fastapi/flask", "method": "GET", "path": "/items"},
	}
	for _, tc := range []struct {
		name, want string
		first      contracts.RouterFacts
		last       contracts.RouterFacts
	}{
		{
			name: "empty self prefix fills", want: "http::GET::/api/items",
			first: contracts.RouterFacts{SelfPrefix: map[string]string{"child": ""}},
			last:  contracts.RouterFacts{SelfPrefix: map[string]string{"child": "/api"}},
		},
		{
			name: "two nonempty prefixes conflict", want: c.ID,
			first: contracts.RouterFacts{SelfPrefix: map[string]string{"child": "/a"}},
			last:  contracts.RouterFacts{SelfPrefix: map[string]string{"child": "/b"}},
		},
		{
			name: "first parent and native return order", want: "http::GET::/foo/a/items",
			first: contracts.RouterFacts{
				MountPrefix: map[string]string{"child": "/a", "parent": "", "root": "/foo / "},
				MountParent: map[string]string{"child": "parent", "parent": "root"},
			},
			last: contracts.RouterFacts{MountParent: map[string]string{"child": "other"}},
		},
		{
			name: "cycle retains parent self", want: "http::GET::/local/top/parent/a/local/items",
			first: contracts.RouterFacts{
				SelfPrefix:  map[string]string{"child": "/local", "parent": "/parent"},
				MountPrefix: map[string]string{"child": "/a", "parent": "/top"},
				MountParent: map[string]string{"child": "parent", "parent": "child"},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			facts := map[string]contracts.RouterFacts{
				"a.py":    tc.first,
				"z.py":    tc.last,
				routeFile: {LineCount: 1, Lines: []contracts.RouterLineFacts{{Line: 1, Receivers: map[string]string{"fastapi/flask": "child"}}}},
			}
			reg := contracts.NewRegistry()
			reg.Add(c)
			contracts.JoinRouterPrefixFacts(reg, facts)
			require.Equal(t, []string{tc.want}, reg.AllIDs())
			b := newBuild(t, FullProfile)
			for path := range facts {
				b.AddNode(&graph.Node{ID: path, FilePath: path, Kind: graph.KindFile})
			}
			require.NoError(t, b.WithRebuildContractRecords(t.Context(), func(r *RebuildContractRecords) error {
				require.NoError(t, r.Add(RebuildContractRecord{Contract: c}))
				missing, err := r.JoinCheckedRouterPrefixes("", "", func(path string) (*contracts.RouterFacts, error) {
					f, ok := facts[path]
					require.True(t, ok, path)
					return &f, nil
				}, nil)
				require.NoError(t, err)
				require.Empty(t, missing)
				var got []contracts.Contract
				require.NoError(t, r.VisitCurrentRepresentatives(func(item RebuildContractRecord) error {
					got = append(got, item.Contract)
					return nil
				}))
				require.Equal(t, reg.ByID(tc.want), got)
				return nil
			}))
		})
	}
}

func TestRebuildRouterMountChainBounds(t *testing.T) {
	ctx := t.Context()
	require.NoError(t, checkRebuildRouterMountChain(ctx, goldBufferBytes))
	err := checkRebuildRouterMountChain(ctx, goldBufferBytes+1)
	require.ErrorIs(t, err, ErrRebuildLinkInputSize)
	require.ErrorContains(t, err, "router mount chain")

	// Enter the real route lookup with a cancelled chain context, rather
	// than refusing earlier in JoinCheckedRouterPrefixes's iterator.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	b := newBuild(t, FullProfile)
	m := rebuildRouterMounts{records: &RebuildContractRecords{build: b, ctx: cancelled}}
	c := contracts.Contract{Line: 1, Meta: map[string]any{"framework": "express"}}
	f := contracts.RouterFacts{
		LineCount: 1,
		Lines:     []contracts.RouterLineFacts{{Line: 1, Receivers: map[string]string{"express": "app"}}},
	}
	got, err := m.route(c, f)
	require.ErrorIs(t, err, context.Canceled)
	require.Empty(t, got)
}
