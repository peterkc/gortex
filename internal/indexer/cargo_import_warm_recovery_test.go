package indexer

import (
	"context"
	"fmt"
	"iter"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/search"
)

func TestTrackedCargoWarmFileRecovery(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, providerFirst := range []bool{false, true} {
			for _, deferred := range []bool{false, true} {
				for _, full := range []bool{false, true} {
					for _, restore := range []bool{false, true} {
						t.Run(fmt.Sprintf("%s/provider_first=%v/deferred=%v/restore=%v/full=%v", backend, providerFirst, deferred, restore, full), func(t *testing.T) {
							provider, consumer := t.TempDir(), t.TempDir()
							writeWorkspaceFile(t, provider, "Cargo.toml", "[package]\nname = 'other-crate'\nversion = '0.1.0'\n")
							writeWorkspaceFile(t, provider, "src/lib.rs", cargoLifecycleProvider)
							writeWorkspaceFile(t, provider, "src/util.rs", "pub fn helper() {}\n")
							writeWorkspaceFile(t, consumer, "Cargo.toml", "[package]\nname = 'app'\nversion = '0.1.0'\n[dependencies]\nother-crate = '1'\n")
							consumerSource := cargoLifecycleConsumer
							if !restore {
								consumerSource += "\nuse other_crate::extra::*;\n"
							}
							writeWorkspaceFile(t, consumer, "src/lib.rs", consumerSource)
							for i := 0; i < 6; i++ {
								writeWorkspaceFile(t, provider, fmt.Sprintf("src/pad%d.rs", i), "pub fn pad() {}\n")
								writeWorkspaceFile(t, consumer, fmt.Sprintf("src/pad%d.rs", i), "pub fn pad() {}\n")
							}
							entries := []config.RepoEntry{{Path: consumer, Name: "consumer", Workspace: "shared"}, {Path: provider, Name: "provider", Workspace: "shared"}}
							if providerFirst {
								entries[0], entries[1] = entries[1], entries[0]
							}
							cfgPath := filepath.Join(t.TempDir(), "config.yaml")
							gc := &config.GlobalConfig{Repos: entries}
							gc.SetConfigPath(cfgPath)
							require.NoError(t, gc.Save())
							cm, err := config.NewConfigManager(cfgPath)
							require.NoError(t, err)
							reg := newTestRegistry()
							reg.Register(languages.NewRustExtractor())
							newMI := func(g graph.Store) *MultiIndexer {
								mi := NewMultiIndexer(g, reg, search.NewNull(), cm, zap.NewNop())
								t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
								return mi
							}
							g := cargoWarmStore(t, backend)
							mi := newMI(g)
							_, err = mi.IndexAll()
							require.NoError(t, err)
							if restore {
								file := filepath.Join(provider, "src/util.rs")
								require.NoError(t, os.Remove(file))
								_, err = mi.IncrementalReindexRepo("provider", []string{file})
								require.NoError(t, err)
							}
							prior := map[string]map[string]int64{"consumer": mi.FileMtimes("consumer"), "provider": mi.FileMtimes("provider")}
							require.NoError(t, mi.Close(context.Background()))
							if restore {
								writeWorkspaceFile(t, provider, "src/util.rs", "pub fn helper() {}\n")
							} else {
								writeWorkspaceFile(t, provider, "src/extra.rs", "")
							}
							restarted := newMI(g)
							if deferred {
								restarted.BeginParallelBatch()
							} else {
								restarted.BeginBatch()
							}
							var files []string
							for _, entry := range entries {
								result, err := restarted.ReconcileRepoCtx(context.Background(), entry, prior[entry.Name])
								require.NoError(t, err)
								require.False(t, result.FullRetrack, "must exercise scoped warm reconcile")
								if entry.Name == "provider" {
									require.True(t, result.CargoRetarget)
								}
								files = appendUniqueSorted(files, result.DerivedInvalidation.Files...)
								files = appendUniqueSorted(files, result.CrossRepoResolveFiles...)
							}
							if full {
								require.NoError(t, restarted.RunPreEnrichResolve(context.Background(), nil, nil))
							} else {
								require.NoError(t, restarted.RunPreEnrichResolveFiles(context.Background(), files, nil))
							}
							restarted.EndBatch()
							cold := cargoWarmStore(t, backend)
							_, err = newMI(cold).IndexAll()
							require.NoError(t, err)
							require.Equal(t, cargoLifecycleBindings(cold, "warm"), cargoLifecycleBindings(g, "warm"))
						})
					}
				}
			}
		}
	}
}

func cargoWarmStore(t *testing.T, backend string) graph.Store {
	t.Helper()
	if backend == "sqlite" {
		db, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, db.Close()) })
		return db
	}
	return graph.New()
}

func TestCargoWarmRetargetReceiptCancellation(t *testing.T) {
	a, b := &IndexResult{CargoRetarget: true}, &IndexResult{CargoRetarget: true}
	mi := &MultiIndexer{graph: graph.New(), repos: map[string]*RepoMetadata{
		"a": {cargoRetarget: a}, "b": {cargoRetarget: b},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, mi.runWarmCargoResolve(ctx, nil, false), context.Canceled)
	require.Same(t, a, mi.repos["a"].cargoRetarget)
	require.Same(t, b, mi.repos["b"].cargoRetarget)
	require.NoError(t, mi.runWarmCargoResolve(context.Background(), nil, false))
	require.Nil(t, mi.repos["a"].cargoRetarget)
	require.Nil(t, mi.repos["b"].cargoRetarget)
	require.True(t, a.CargoRetarget, "consumption must not mutate a returned result")
}

func TestCargoAddedFileFrontierOnlyMarkedProviderImports(t *testing.T) {
	provider, consumer := t.TempDir(), t.TempDir()
	writeWorkspaceFile(t, provider, "Cargo.toml", "[package]\nname = 'other-crate'\nversion = '0.1.0'\n")
	writeWorkspaceFile(t, provider, "src/lib.rs", "")
	writeWorkspaceFile(t, consumer, "Cargo.toml", "[package]\nname = 'app'\nversion = '0.1.0'\n[dependencies]\nother-crate = '1'\n")
	g := graph.New()
	for _, row := range []struct {
		file, target string
		marker       any
	}{
		{"consumer/marked.rs", "provider/src/lib.rs", "external::other_crate/util"},
		{"consumer/unmarked.rs", "provider/src/lib.rs", nil},
		{"consumer/invalid.rs", "provider/src/lib.rs", 1},
		{"consumer/other.rs", "other/src/lib.rs", "external::other_crate/util"},
		{"consumer/nonrust.py", "provider/src/lib.rs", "external::other_crate/util"},
	} {
		g.AddEdge(&graph.Edge{From: row.file, FilePath: row.file, To: row.target, Kind: graph.EdgeImports, CrossRepo: true,
			Meta: map[string]any{"cargo_import_stub": row.marker}})
	}
	mi := &MultiIndexer{graph: g, repos: map[string]*RepoMetadata{
		"provider": {RootPath: provider}, "consumer": {RootPath: consumer},
	}}
	require.Equal(t, []string{"consumer/marked.rs"}, mi.cargoConsumerFilesForRepo("provider", true))
	require.Empty(t, mi.cargoConsumerFilesForRepo("provider", false))
}

type cargoWarmScanStore struct {
	graph.Store
	fullScans, lightScans int
}

func (s *cargoWarmScanStore) EdgesByKind(kind graph.EdgeKind) iter.Seq[*graph.Edge] {
	if kind == graph.EdgeImports {
		s.fullScans++
	}
	return s.Store.EdgesByKind(kind)
}
func (s *cargoWarmScanStore) EdgesLightSeq(kinds ...graph.EdgeKind) iter.Seq[*graph.Edge] {
	if len(kinds) == 1 && kinds[0] == graph.EdgeImports {
		s.lightScans++
	}
	return graph.EdgesLightSeq(s.Store, kinds...)
}
func TestCargoWarmRecoveryScansImportsOnce(t *testing.T) {
	provider, consumer := t.TempDir(), t.TempDir()
	for _, root := range []string{provider, consumer} {
		writeWorkspaceFile(t, root, "Cargo.toml", "[package]\nname='library'\nversion='1.0.0'\n")
	}
	g := &cargoWarmScanStore{Store: graph.New()}
	g.AddEdge(&graph.Edge{From: "unrelated/lib.rs", FilePath: "unrelated/lib.rs", To: "external::unknown/f", Kind: graph.EdgeImports})
	mi := &MultiIndexer{graph: g, repos: map[string]*RepoMetadata{
		"provider": {RootPath: provider, cargoRetarget: &IndexResult{CargoRetarget: true}},
		"consumer": {RootPath: consumer, cargoRetarget: &IndexResult{CargoRetarget: true}},
	}}
	require.NoError(t, mi.runWarmCargoResolve(t.Context(), nil, false))
	require.Zero(t, g.fullScans, "warm recovery must not read every import's metadata")
	require.Equal(t, 1, g.lightScans, "all receipt repos must share one scan")
}

func TestCargoLookupDefersManifestReadsUntilUse(t *testing.T) {
	provider, consumer := t.TempDir(), t.TempDir()
	mi := &MultiIndexer{graph: graph.New(), repos: map[string]*RepoMetadata{"provider": {RootPath: provider}, "consumer": {RootPath: consumer}}}
	for _, root := range []string{provider, consumer} {
		writeWorkspaceFile(t, root, "Cargo.toml", "")
	}
	lookup := mi.cargoCrateLookup()
	require.NotNil(t, lookup, "construction may stat roots but must defer reading Cargo manifests")
	writeWorkspaceFile(t, provider, "Cargo.toml", "[package]\nname='library'\nversion='1.0.0'\n")
	writeWorkspaceFile(t, consumer, "Cargo.toml", "[package]\nname='app'\nversion='1.0.0'\n[dependencies]\nlibrary='1'\n")
	providers := lookup("consumer/src/lib.rs", "library")
	require.Len(t, providers, 1)
	require.Equal(t, "provider/src/lib.rs", providers[0].Root)
	writeWorkspaceFile(t, consumer, "Cargo.toml", "[package]\nname='app'\nversion='1.0.0'\n")
	require.Equal(t, providers, lookup("consumer/src/lib.rs", "library"), "one pass must retain one manifest snapshot")
}
