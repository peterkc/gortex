package indexer

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

type configuredRepositoryNilCatalogStore struct{ graph.Store }

func (configuredRepositoryNilCatalogStore) Catalog() *store_sqlite.Catalog { return nil }

func TestConfiguredRepositoryCatalogCapabilityKeepsOwnerSelection(t *testing.T) {
	for _, shape := range []string{"native", "nil-catalog-wrapper", "non-sqlite"} {
		t.Run(shape, func(t *testing.T) {
			f := newLifecycleFixture(t)
			t.Cleanup(f.close)
			owner, first := f.gitRepo("owner"), f.gitRepo("first")
			ownerEntry := config.RepoEntry{Path: owner, Name: "src"}
			_, err := f.lc.Register(t.Context(), ownerEntry, TrackSourceCLI)
			require.NoError(t, err)
			guardConfiguredOwner(t, f, "src")
			f.cm.Global().Repos = []config.RepoEntry{{Path: first, Name: "src"}, ownerEntry}
			require.NoError(t, f.cm.Global().Save())
			f.restart()
			switch shape {
			case "nil-catalog-wrapper":
				f.mi.graph = configuredRepositoryNilCatalogStore{f.store}
			case "non-sqlite":
				f.mi.graph = graph.New()
			}
			selected := f.mi.ResolveConfiguredRepos(t.Context(), f.cm.Global().ReposSnapshot())
			require.Len(t, selected, 1)
			err = f.mi.configuredPrefixCollisionBefore(t.Context(), "src", first, time.Now().Add(prefixOwnershipTimeout))
			if shape == "native" {
				require.Equal(t, owner, selected[0].Path)
				var collision *PrefixCollisionError
				require.ErrorAs(t, err, &collision)
				require.Equal(t, owner, collision.ExistingPath)
			} else {
				require.Equal(t, first, selected[0].Path)
				require.NoError(t, err)
			}
		})
	}
}
