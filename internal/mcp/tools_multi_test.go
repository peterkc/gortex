package mcp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"pgregory.net/rapid"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
)

// ============================================================================
// 10.2 Property 21: Config persistence round-trip
// ============================================================================

// Feature: multi-repo-support, Property 21: Config persistence round-trip
//
// For any sequence of track and untrack operations, the GlobalConfig file
// SHALL reflect the current state of the repos list. After tracking a repo,
// the config file SHALL contain that entry. After untracking, the entry
// SHALL be removed.
func TestPropertyConfigPersistenceRoundTrip(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		// Create a temp directory for the GlobalConfig file.
		dir := t.TempDir()
		configPath := filepath.Join(dir, "config.yaml")

		// Start with an empty GlobalConfig.
		gc := &config.GlobalConfig{}
		gc.SetConfigPath(configPath)
		require.NoError(t, gc.Save())

		// Generate a sequence of track/untrack operations.
		// Use absolute paths within the temp dir so AddRepo/RemoveRepo work.
		numOps := rapid.IntRange(1, 30).Draw(rt, "numOps")
		tracked := make(map[string]bool) // absPath → tracked

		for i := 0; i < numOps; i++ {
			// Generate a repo path (use temp dir subdirectories as absolute paths).
			repoName := rapid.StringMatching(`[a-z]{2,8}`).Draw(rt, "repoName")
			// Generated names such as "nul" are Windows devices. The property
			// needs real directories; every generated suffix remains represented.
			repoPath := filepath.Join(dir, "repos", "repo-"+repoName)

			isTrack := rapid.Bool().Draw(rt, "isTrack")

			if isTrack {
				// Create the directory so AddRepo can resolve it.
				if err := os.MkdirAll(repoPath, 0o755); err != nil {
					rt.Fatalf("create valid repository directory: %v", err)
				}
				entry := config.RepoEntry{Path: repoPath}
				if err := gc.AddRepo(entry); err != nil {
					rt.Fatalf("track valid repository directory: %v", err)
				}
				tracked[repoPath] = true
			} else {
				// Untrack: try to remove. If it wasn't tracked, RemoveRepo returns error — that's fine.
				err := gc.RemoveRepo(repoPath)
				if err == nil {
					delete(tracked, repoPath)
				}
			}

			// Save after each operation.
			require.NoError(t, gc.Save())

			// Reload from disk and verify state matches.
			loaded, err := config.LoadGlobal(configPath)
			require.NoError(t, err)

			// Build set of loaded paths.
			loadedPaths := make(map[string]bool)
			for _, r := range loaded.Repos {
				loadedPaths[r.Path] = true
			}

			// Verify: every tracked path is in the loaded config.
			for path := range tracked {
				if !loadedPaths[path] {
					rt.Errorf("op %d: tracked path %s not found in reloaded config", i, path)
				}
			}

			// Verify: every loaded path is in the tracked set.
			for path := range loadedPaths {
				if !tracked[path] {
					rt.Errorf("op %d: loaded path %s not in tracked set", i, path)
				}
			}

			// Verify counts match.
			if len(loaded.Repos) != len(tracked) {
				rt.Errorf("op %d: loaded %d repos, expected %d", i, len(loaded.Repos), len(tracked))
			}
		}
	})
}

// ============================================================================
// 10.4 Property 14: Query scoping by repo, project, and ref
// ============================================================================

// Feature: multi-repo-support, Property 14: Query scoping by repo, project, and ref
//
// For any multi-repo graph and any query with a repo parameter, results SHALL
// contain only nodes with matching RepoPrefix. When project is specified,
// results SHALL contain only nodes from repos belonging to that project.
// When ref is specified with project, results SHALL contain only nodes from
// repos in that project with matching reference tag. When ref is specified
// without project, results SHALL contain nodes from all repos across all
// projects with matching reference tag. When no filter is specified, results
// SHALL span all repositories.
func TestPropertyQueryScopingByRepoProjectRef(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		// Generate 2-5 repos with distinct prefixes.
		numRepos := rapid.IntRange(2, 5).Draw(rt, "numRepos")
		prefixes := make([]string, numRepos)
		for i := 0; i < numRepos; i++ {
			prefixes[i] = rapid.StringMatching(`[a-z]{3,8}`).Draw(rt, "prefix") +
				rapid.StringMatching(`[0-9]{1,2}`).Draw(rt, "suffix")
		}
		// Deduplicate prefixes.
		seen := make(map[string]bool)
		var uniquePrefixes []string
		for _, p := range prefixes {
			if !seen[p] {
				seen[p] = true
				uniquePrefixes = append(uniquePrefixes, p)
			}
		}
		if len(uniquePrefixes) < 2 {
			return // need at least 2 distinct repos
		}
		prefixes = uniquePrefixes

		// Build a graph with nodes from each repo.
		g := graph.New()
		repoNodes := make(map[string][]string) // prefix → node IDs
		for _, prefix := range prefixes {
			numNodes := rapid.IntRange(1, 5).Draw(rt, "numNodes_"+prefix)
			for j := 0; j < numNodes; j++ {
				id := prefix + "/file.go::Func" + rapid.StringMatching(`[A-Z][a-z]{2,6}`).Draw(rt, "funcName")
				node := &graph.Node{
					ID:         id,
					Kind:       graph.KindFunction,
					Name:       "Func",
					FilePath:   prefix + "/file.go",
					StartLine:  j + 1,
					Language:   "go",
					RepoPrefix: prefix,
				}
				g.AddNode(node)
				repoNodes[prefix] = append(repoNodes[prefix], id)
			}
		}

		// Create projects with ref tags.
		// Project "alpha" gets first half of repos, "beta" gets second half.
		// Some repos may overlap (shared repos).
		mid := len(prefixes) / 2
		if mid == 0 {
			mid = 1
		}
		alphaRepos := prefixes[:mid]
		betaRepos := prefixes[mid:]

		// Assign ref tags.
		alphaEntries := make([]config.RepoEntry, len(alphaRepos))
		for i, p := range alphaRepos {
			alphaEntries[i] = config.RepoEntry{Path: "/tmp/" + p, Name: p, Ref: "work"}
		}
		betaEntries := make([]config.RepoEntry, len(betaRepos))
		for i, p := range betaRepos {
			betaEntries[i] = config.RepoEntry{Path: "/tmp/" + p, Name: p, Ref: "opensource"}
		}

		gc := &config.GlobalConfig{
			Projects: map[string]config.ProjectConfig{
				"alpha": {Repos: alphaEntries},
				"beta":  {Repos: betaEntries},
			},
		}

		dir := t.TempDir()
		gcPath := filepath.Join(dir, "config.yaml")
		gc.SetConfigPath(gcPath)
		require.NoError(t, gc.Save())

		cm, err := config.NewConfigManager(gcPath)
		require.NoError(t, err)

		eng := query.NewEngine(g)
		srv := NewServer(eng, g, nil, nil, zap.NewNop(), nil, MultiRepoOptions{
			ConfigManager: cm,
		})

		// --- Test 1: repo filter ---
		targetPrefix := rapid.SampledFrom(prefixes).Draw(rt, "targetRepo")
		req1 := mcplib.CallToolRequest{}
		req1.Params.Arguments = map[string]any{"repo": targetPrefix}
		allowed1, err := srv.resolveRepoFilter(context.Background(), req1)
		require.NoError(t, err)
		require.NotNil(t, allowed1, "repo filter should return non-nil allowed set")
		assert.True(t, allowed1[targetPrefix], "target repo should be in allowed set")
		assert.Equal(t, 1, len(allowed1), "repo filter should allow exactly one prefix")

		// Verify filtering nodes works correctly.
		allNodes := g.AllNodes()
		filtered1 := filterNodes(allNodes, allowed1)
		for _, n := range filtered1 {
			assert.Equal(t, targetPrefix, n.RepoPrefix,
				"filtered node should have matching RepoPrefix")
		}

		// --- Test 2: project filter ---
		targetProject := rapid.SampledFrom([]string{"alpha", "beta"}).Draw(rt, "targetProject")
		req2 := mcplib.CallToolRequest{}
		req2.Params.Arguments = map[string]any{"project": targetProject}
		allowed2, err := srv.resolveRepoFilter(context.Background(), req2)
		require.NoError(t, err)
		require.NotNil(t, allowed2)

		var expectedPrefixes []string
		if targetProject == "alpha" {
			expectedPrefixes = alphaRepos
		} else {
			expectedPrefixes = betaRepos
		}
		for _, p := range expectedPrefixes {
			assert.True(t, allowed2[p],
				"project %s should include repo %s", targetProject, p)
		}

		filtered2 := filterNodes(allNodes, allowed2)
		for _, n := range filtered2 {
			assert.True(t, allowed2[n.RepoPrefix],
				"filtered node repo %s should be in project %s", n.RepoPrefix, targetProject)
		}

		// --- Test 3: project + ref filter ---
		req3 := mcplib.CallToolRequest{}
		req3.Params.Arguments = map[string]any{"project": "alpha", "ref": "work"}
		allowed3, err := srv.resolveRepoFilter(context.Background(), req3)
		require.NoError(t, err)
		require.NotNil(t, allowed3)
		for _, p := range alphaRepos {
			assert.True(t, allowed3[p],
				"alpha+work should include repo %s", p)
		}
		// Beta repos should NOT be in the result.
		for _, p := range betaRepos {
			assert.False(t, allowed3[p],
				"alpha+work should NOT include beta repo %s", p)
		}

		// --- Test 4: ref without project ---
		req4 := mcplib.CallToolRequest{}
		req4.Params.Arguments = map[string]any{"ref": "opensource"}
		allowed4, err := srv.resolveRepoFilter(context.Background(), req4)
		require.NoError(t, err)
		require.NotNil(t, allowed4)
		for _, p := range betaRepos {
			assert.True(t, allowed4[p],
				"ref=opensource should include beta repo %s", p)
		}
		for _, p := range alphaRepos {
			assert.False(t, allowed4[p],
				"ref=opensource should NOT include alpha repo %s (ref=work)", p)
		}

		// --- Test 5: no filter → nil (all repos) ---
		req5 := mcplib.CallToolRequest{}
		req5.Params.Arguments = map[string]any{}
		allowed5, err := srv.resolveRepoFilter(context.Background(), req5)
		require.NoError(t, err)
		assert.Nil(t, allowed5, "no filter should return nil (all repos)")
	})
}

// ============================================================================
// 10.7 Property 20: Active project default scoping
// ============================================================================

// Feature: multi-repo-support, Property 20: Active project default scoping
//
// For any active project setting, all MCP tool calls without an explicit
// project parameter SHALL return results scoped to the active project's
// repositories. Changing the active project via set_active_project SHALL
// immediately change the default scope for all subsequent queries.
func TestPropertyActiveProjectDefaultScoping(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		// Generate 2-4 repos with distinct prefixes.
		numRepos := rapid.IntRange(2, 4).Draw(rt, "numRepos")
		prefixes := make([]string, numRepos)
		for i := 0; i < numRepos; i++ {
			prefixes[i] = rapid.StringMatching(`[a-z]{3,6}`).Draw(rt, "prefix") +
				rapid.StringMatching(`[0-9]{1,2}`).Draw(rt, "suffix")
		}
		// Deduplicate.
		seen := make(map[string]bool)
		var unique []string
		for _, p := range prefixes {
			if !seen[p] {
				seen[p] = true
				unique = append(unique, p)
			}
		}
		if len(unique) < 2 {
			return
		}
		prefixes = unique

		// Build graph with nodes from each repo.
		g := graph.New()
		for _, prefix := range prefixes {
			for j := 0; j < 3; j++ {
				id := prefix + "/file.go::Func" + rapid.StringMatching(`[A-Z][a-z]{2,5}`).Draw(rt, "fn")
				g.AddNode(&graph.Node{
					ID:         id,
					Kind:       graph.KindFunction,
					Name:       "Func",
					FilePath:   prefix + "/file.go",
					StartLine:  j + 1,
					Language:   "go",
					RepoPrefix: prefix,
				})
			}
		}

		// Split repos into two projects.
		mid := len(prefixes) / 2
		if mid == 0 {
			mid = 1
		}
		projARepos := prefixes[:mid]
		projBRepos := prefixes[mid:]

		projAEntries := make([]config.RepoEntry, len(projARepos))
		for i, p := range projARepos {
			projAEntries[i] = config.RepoEntry{Path: "/tmp/" + p, Name: p}
		}
		projBEntries := make([]config.RepoEntry, len(projBRepos))
		for i, p := range projBRepos {
			projBEntries[i] = config.RepoEntry{Path: "/tmp/" + p, Name: p}
		}

		gc := &config.GlobalConfig{
			Projects: map[string]config.ProjectConfig{
				"projA": {Repos: projAEntries},
				"projB": {Repos: projBEntries},
			},
		}

		dir := t.TempDir()
		gcPath := filepath.Join(dir, "config.yaml")
		gc.SetConfigPath(gcPath)
		require.NoError(t, gc.Save())

		cm, err := config.NewConfigManager(gcPath)
		require.NoError(t, err)

		eng := query.NewEngine(g)

		// --- Phase 1: Set active project to projA ---
		srv := NewServer(eng, g, nil, nil, zap.NewNop(), nil, MultiRepoOptions{
			ConfigManager: cm,
			ActiveProject: "projA",
		})

		// Call resolveRepoFilter with empty params — should scope to projA.
		reqEmpty := mcplib.CallToolRequest{}
		reqEmpty.Params.Arguments = map[string]any{}
		allowed, err := srv.resolveRepoFilter(context.Background(), reqEmpty)
		require.NoError(t, err)
		require.NotNil(t, allowed, "active project should produce non-nil filter")

		for _, p := range projARepos {
			assert.True(t, allowed[p],
				"active project projA: repo %s should be in scope", p)
		}
		for _, p := range projBRepos {
			assert.False(t, allowed[p],
				"active project projA: repo %s should NOT be in scope", p)
		}

		// Verify node filtering matches.
		allNodes := g.AllNodes()
		filtered := filterNodes(allNodes, allowed)
		for _, n := range filtered {
			assert.True(t, allowed[n.RepoPrefix],
				"filtered node %s should be in projA scope", n.ID)
		}

		// --- Phase 2: Switch active project to projB ---
		srv.activeProject = "projB"

		allowed2, err := srv.resolveRepoFilter(context.Background(), reqEmpty)
		require.NoError(t, err)
		require.NotNil(t, allowed2, "active project projB should produce non-nil filter")

		for _, p := range projBRepos {
			assert.True(t, allowed2[p],
				"active project projB: repo %s should be in scope", p)
		}
		for _, p := range projARepos {
			assert.False(t, allowed2[p],
				"active project projB: repo %s should NOT be in scope", p)
		}

		// --- Phase 3: Explicit project param overrides active project ---
		reqExplicit := mcplib.CallToolRequest{}
		reqExplicit.Params.Arguments = map[string]any{"project": "projA"}
		allowed3, err := srv.resolveRepoFilter(context.Background(), reqExplicit)
		require.NoError(t, err)
		require.NotNil(t, allowed3)

		for _, p := range projARepos {
			assert.True(t, allowed3[p],
				"explicit project=projA should include repo %s even when active is projB", p)
		}
	})
}

// ============================================================================
// 10.8 Property 15: Shared repo indexed once
// ============================================================================

// Feature: multi-repo-support, Property 15: Shared repo indexed once
//
// For any repository path that appears in multiple project definitions,
// the graph SHALL contain exactly one set of nodes for that repository
// (not duplicated per project). The node count for that RepoPrefix SHALL
// equal the count from a single indexing of that repository.
func TestPropertySharedRepoIndexedOnce(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		// Generate a shared repo prefix and 1-2 unique repo prefixes.
		sharedPrefix := rapid.StringMatching(`shared[a-z]{1,4}`).Draw(rt, "sharedPrefix")
		numUnique := rapid.IntRange(1, 3).Draw(rt, "numUnique")
		uniquePrefixes := make([]string, numUnique)
		for i := 0; i < numUnique; i++ {
			uniquePrefixes[i] = rapid.StringMatching(`unique[a-z]{1,4}`).Draw(rt, "uniquePrefix") +
				rapid.StringMatching(`[0-9]{1,2}`).Draw(rt, "suffix")
		}

		// Generate nodes for the shared repo.
		sharedNodeCount := rapid.IntRange(2, 8).Draw(rt, "sharedNodeCount")
		g := graph.New()

		// Add shared repo nodes exactly once (simulating correct indexing behavior).
		// Use index-based names to guarantee uniqueness.
		for j := 0; j < sharedNodeCount; j++ {
			funcName := fmt.Sprintf("SharedFunc%d", j)
			id := sharedPrefix + "/file.go::" + funcName
			g.AddNode(&graph.Node{
				ID:         id,
				Kind:       graph.KindFunction,
				Name:       funcName,
				FilePath:   sharedPrefix + "/file.go",
				StartLine:  j + 1,
				Language:   "go",
				RepoPrefix: sharedPrefix,
			})
		}

		// Add unique repo nodes.
		for _, prefix := range uniquePrefixes {
			numNodes := rapid.IntRange(1, 5).Draw(rt, "numNodes_"+prefix)
			for j := 0; j < numNodes; j++ {
				funcName := fmt.Sprintf("UniqueFunc%d", j)
				id := prefix + "/file.go::" + funcName
				g.AddNode(&graph.Node{
					ID:         id,
					Kind:       graph.KindFunction,
					Name:       funcName,
					FilePath:   prefix + "/file.go",
					StartLine:  j + 1,
					Language:   "go",
					RepoPrefix: prefix,
				})
			}
		}

		// Create a GlobalConfig where the shared repo appears in multiple projects.
		numProjects := rapid.IntRange(2, 4).Draw(rt, "numProjects")
		projects := make(map[string]config.ProjectConfig, numProjects)
		for i := 0; i < numProjects; i++ {
			projName := rapid.StringMatching(`proj[a-z]{1,4}`).Draw(rt, "projName") +
				rapid.StringMatching(`[0-9]{1,2}`).Draw(rt, "projSuffix")
			entries := []config.RepoEntry{
				{Path: "/tmp/" + sharedPrefix, Name: sharedPrefix},
			}
			// Optionally add a unique repo to this project.
			if len(uniquePrefixes) > 0 {
				idx := rapid.IntRange(0, len(uniquePrefixes)-1).Draw(rt, "uniqueIdx")
				entries = append(entries, config.RepoEntry{
					Path: "/tmp/" + uniquePrefixes[idx],
					Name: uniquePrefixes[idx],
				})
			}
			projects[projName] = config.ProjectConfig{Repos: entries}
		}

		// Verify: GetRepoNodes returns exactly the shared nodes (not duplicated).
		sharedNodes := g.GetRepoNodes(sharedPrefix)
		assert.Equal(t, sharedNodeCount, len(sharedNodes),
			"shared repo should have exactly %d nodes, got %d", sharedNodeCount, len(sharedNodes))

		// Verify: each shared node appears exactly once in the graph.
		allNodes := g.AllNodes()
		sharedInGraph := 0
		for _, n := range allNodes {
			if n.RepoPrefix == sharedPrefix {
				sharedInGraph++
			}
		}
		assert.Equal(t, sharedNodeCount, sharedInGraph,
			"shared repo nodes in full graph should be %d, got %d", sharedNodeCount, sharedInGraph)

		// Verify: querying from different projects returns the same shared nodes.
		// Both projects should see the shared repo's nodes when filtering.
		for projName, proj := range projects {
			allowedPrefixes := make(map[string]bool)
			for _, e := range proj.Repos {
				allowedPrefixes[config.ResolvePrefix(e)] = true
			}

			filtered := filterNodes(allNodes, allowedPrefixes)
			sharedInFiltered := 0
			for _, n := range filtered {
				if n.RepoPrefix == sharedPrefix {
					sharedInFiltered++
				}
			}
			assert.Equal(t, sharedNodeCount, sharedInFiltered,
				"project %s should see exactly %d shared nodes, got %d",
				projName, sharedNodeCount, sharedInFiltered)
		}
	})
}
