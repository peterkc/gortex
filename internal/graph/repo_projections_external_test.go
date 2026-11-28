package graph_test

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestSpringRepoFileProjectionMatchesSQLite(t *testing.T) {
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	nodes := []*graph.Node{
		{ID: "upper", Kind: graph.KindFile, FilePath: "application.YAML", Language: "yaml"},
		{ID: "application.properties", Kind: graph.KindFile, Language: "text"},
		{ID: "reader", Kind: graph.KindFile, FilePath: "Bean.java.txt", Language: "java"},
		{ID: "other", Kind: graph.KindFile, FilePath: "values.txt", Language: "text"},
	}
	require.NoError(t, store.AddBatchChecked(nodes, nil))
	languages, extensions := contracts.SpringConfigFileProjection()
	var paths []string
	for _, node := range nodes {
		path := node.FilePath
		if path == "" {
			path = node.ID
		}
		if graph.ProjectionMatches(path, node.Language, languages, extensions) {
			paths = append(paths, path)
		}
	}
	slices.Sort(paths)
	require.Equal(t, []string{"Bean.java.txt", "application.YAML", "application.properties"}, paths)
	require.Equal(t, paths, graph.ReadRepoFilePaths(store, "", "", languages, extensions))
}
