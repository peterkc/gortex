package resolver

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestPythonModuleAttributionBatchWritesArePerPass(t *testing.T) {
	for _, count := range []int{3, 7} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			base := graph.New()
			frontier := incrementalFileFrontier{nodesByFile: map[string][]*graph.Node{}, outByNode: map[string][]*graph.Edge{}}
			var imports []*graph.Edge
			for i := 0; i < count; i++ {
				path := "app/f" + strconv.Itoa(i) + ".py"
				seedFile(base, path, "python")
				edge := seedExternalImport(base, path, "package_"+strconv.Itoa(i)+".mod")
				imports = append(imports, edge)
				frontier.paths = append(frontier.paths, path)
				frontier.nodesByFile[path] = base.GetFileNodes(path)
				frontier.outByNode[path] = []*graph.Edge{edge}
			}
			seedFile(base, "outside/main.py", "python")
			outside := seedExternalImport(base, "outside/main.py", "outside_package")
			store := &moduleAttributionBatchRecordingStore{Store: base}
			r := New(store)
			r.prepareIncrementalAttributionCache(frontier)
			defer r.clearIncrementalAttributionCache()
			// Given cached Python imports, when the save batch attributes them,
			// then writes cover the entire pass rather than each file.
			r.runFileAttributionPassesForFilesLocked(frontier)
			assert.Equal(t, []moduleAttributionBatchCall{{nodes: count}, {edges: count}}, store.batchCalls)
			assert.Equal(t, []int{count}, store.reindexSizes)
			for i, edge := range imports {
				spec := "package_" + strconv.Itoa(i)
				assert.Equal(t, "module::pypi:"+spec, edge.To)
				assert.Equal(t, spec+".mod", edge.Meta["python_import"])
				require.NotNil(t, base.GetNode(edge.To))
				assert.Len(t, outEdgesOfKind(base, edge.From, graph.EdgeDependsOnModule), 1)
			}
			assert.Equal(t, "external::outside_package", outside.To, "a save must not sweep files outside its frontier")
		})
	}
}

func TestPythonModuleAttributionScopedResolveAllWritesOnce(t *testing.T) {
	base := graph.New()
	const count = 3
	var imports []*graph.Edge
	for i := 0; i < count; i++ {
		path := "consumer/f" + strconv.Itoa(i) + ".py"
		base.AddNode(&graph.Node{ID: path, Kind: graph.KindFile, Name: path, FilePath: path, Language: "python", RepoPrefix: "consumer"})
		imports = append(imports, seedExternalImport(base, path, "package_"+strconv.Itoa(i)+".mod"))
	}
	// A pending reference makes ResolveAll reach its post-resolution tail.
	base.AddEdge(&graph.Edge{From: "consumer/f0.py", To: "unresolved::Missing", Kind: graph.EdgeCalls, FilePath: "consumer/f0.py", Line: 2})
	store := &moduleAttributionBatchRecordingStore{Store: base}
	r := New(store)
	r.SetScope(map[string]struct{}{"consumer": {}})
	// Given a small scoped warm restart, when ResolveAll runs its file tail,
	// then module attribution remains one later whole-scope pass.
	r.ResolveAll()
	assert.Equal(t, []moduleAttributionBatchCall{{nodes: count}, {edges: count}}, store.batchCalls)
	assert.Equal(t, []int{count}, store.reindexSizes)
	for i, edge := range imports {
		assert.Equal(t, "module::pypi:package_"+strconv.Itoa(i), edge.To)
		assert.Equal(t, "package_"+strconv.Itoa(i)+".mod", edge.Meta["python_import"])
	}
}
