package indexer

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/search"
	"go.uber.org/zap"
)

func TestMultiIndexer_CargoCrateImport(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, mode := range []string{"item", "module", "directory", "custom lib", "rename", "dev", "build", "target", "workspace", "undeclared", "sibling declaration", "ambiguous", "disallowed workspace", "same named repo", "ambiguous module", "missing inherited", "reserved", "escaped lib", "nested nonmember", "excluded member", "wrong lib name", "renamed policy allowed", "renamed policy refused", "incremental frontier", "incremental file"} {
			t.Run(backend+"/"+mode, func(t *testing.T) {
				provider, consumer := t.TempDir(), t.TempDir()
				pkgDir, entry, spec := "", "src/lib.rs", "other_crate::Item"
				providerManifest := "[package]\nname = \"other-crate\"\nversion = \"0.1.0\"\n"
				dep := "[dependencies]\nother-crate = \"1\"\n"
				switch mode {
				case "module", "directory", "ambiguous module":
					spec = "other_crate::a::b::Item"
					entry = "src/a/b.rs"
					if mode == "directory" {
						entry = "src/a/b/mod.rs"
					}
				case "wrong lib name":
					providerManifest += "[lib]\nname = \"engine\"\n"
				case "custom lib":
					providerManifest += "[lib]\nname = \"engine\"\npath = \"code/root.rs\"\n"
					entry, spec = "code/root.rs", "engine::Item"
				case "escaped lib":
					providerManifest += "[lib]\npath = \"../src/lib.rs\"\n"
				case "reserved":
					dep, spec = "[dependencies]\ncore = { package = \"other-crate\", version = \"1\" }\n", "core::Item"
				case "missing inherited":
					dep = "[dependencies]\nother-crate = { workspace = true }\n"
				case "rename", "renamed policy allowed", "renamed policy refused":
					dep, spec = "[dependencies]\nalias = { package = \"other-crate\", version = \"1\" }\n", "alias::Item"
				case "dev", "build":
					dep = "[" + mode + "-dependencies]\nother-crate = \"1\"\n"
				case "target":
					dep = "[target.'cfg(unix)'.dependencies]\nother-crate = \"1\"\n"
				case "workspace":
					pkgDir = "crates/engine"
					writeWorkspaceFile(t, provider, "Cargo.toml", "[workspace]\nmembers = [\"crates/*\"]\n")
					writeWorkspaceFile(t, consumer, "Cargo.toml", "[workspace]\nmembers = [\"apps/*\"]\n[workspace.dependencies]\nalias = { package = \"other-crate\", version = \"1\" }\n")
					dep, spec = "[dependencies]\nalias = { workspace = true }\n", "alias::Item"
				case "undeclared", "same named repo", "sibling declaration":
					dep = ""
				}
				writeWorkspaceFile(t, provider, filepath.Join(pkgDir, "Cargo.toml"), providerManifest)
				writeWorkspaceFile(t, provider, filepath.Join(pkgDir, "src/lib.rs"), "pub struct Item;\n")
				writeWorkspaceFile(t, provider, filepath.Join(pkgDir, entry), "pub struct Item;\n")
				if mode == "module" || mode == "directory" || mode == "ambiguous module" {
					writeWorkspaceFile(t, provider, "src/lib.rs", "pub mod a;\n")
					writeWorkspaceFile(t, provider, "src/a.rs", "pub mod b;\n")
				}
				if mode == "ambiguous module" {
					writeWorkspaceFile(t, provider, "src/a/b/mod.rs", "pub struct Item;\n")
				}
				consumerDir := ""
				if mode == "nested nonmember" || mode == "excluded member" {
					consumerDir = "tools/child"
					rootManifest := "[package]\nname = \"root\"\n" + dep
					if mode == "excluded member" {
						rootManifest += "[workspace]\nmembers = [\"tools/*\"]\nexclude = [\"tools/child\"]\n"
					}
					writeWorkspaceFile(t, consumer, "Cargo.toml", rootManifest)
					dep = ""
				}
				if mode == "workspace" || mode == "sibling declaration" {
					consumerDir = "apps/app"
				}
				if mode == "sibling declaration" {
					writeWorkspaceFile(t, consumer, "Cargo.toml", "[workspace]\nmembers = [\"apps/*\"]\n")
					writeWorkspaceFile(t, consumer, "apps/sibling/Cargo.toml", "[package]\nname = \"sibling\"\n[dependencies]\nother-crate = \"1\"\n")
				}
				writeWorkspaceFile(t, consumer, filepath.Join(consumerDir, "Cargo.toml"), "[package]\nname = \"app\"\nversion = \"0.1.0\"\n"+dep)
				caller := filepath.Join(consumerDir, "src/lib.rs")
				writeWorkspaceFile(t, consumer, caller, "use "+spec+";\n")
				providerWS, providerName := "shared", "provider"
				if mode == "disallowed workspace" || strings.HasPrefix(mode, "renamed policy") {
					providerWS = "private"
				}
				if mode == "same named repo" {
					providerName = "other_crate"
				}
				entries := []config.RepoEntry{{Path: provider, Name: providerName, Workspace: providerWS}, {Path: consumer, Name: "consumer", Workspace: "shared"}}
				if mode == "ambiguous" {
					duplicate := t.TempDir()
					writeWorkspaceFile(t, duplicate, "Cargo.toml", providerManifest)
					writeWorkspaceFile(t, duplicate, "src/lib.rs", "pub struct Item;\n")
					entries = append(entries, config.RepoEntry{Path: duplicate, Name: "duplicate", Workspace: "shared"})
				}
				if strings.HasPrefix(mode, "renamed policy") {
					module := "other_crate"
					if mode == "renamed policy refused" {
						module = "alias"
					}
					writeWorkspaceFile(t, consumer, ".gortex.yaml", "cross_workspace_deps:\n  - workspace: private\n    modules: ["+module+"]\n    mode: read-only\n")
				}
				cfgPath := filepath.Join(t.TempDir(), "config.yaml")
				gc := &config.GlobalConfig{Repos: entries}
				gc.SetConfigPath(cfgPath)
				require.NoError(t, gc.Save())
				cm, err := config.NewConfigManager(cfgPath)
				require.NoError(t, err)
				var g graph.Store = graph.New()
				if backend == "sqlite" {
					db, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
					require.NoError(t, err)
					t.Cleanup(func() { require.NoError(t, db.Close()) })
					g = db
				}
				reg := newTestRegistry()
				reg.Register(languages.NewRustExtractor())
				mi := NewMultiIndexer(g, reg, search.NewNull(), cm, zap.NewNop())
				_, err = mi.IndexAll()
				require.NoError(t, err)
				want := filepath.ToSlash(filepath.Join(providerName, pkgDir, entry))
				require.NotNil(t, g.GetNode(want))
				resolves := mode != "nested nonmember" && mode != "excluded member" && mode != "wrong lib name" && mode != "renamed policy refused" && mode != "undeclared" && mode != "same named repo" && mode != "sibling declaration" && mode != "ambiguous" && mode != "disallowed workspace" && mode != "ambiguous module" && mode != "missing inherited" && mode != "reserved" && mode != "escaped lib"
				if strings.HasPrefix(mode, "incremental") {
					var initial []string
					for e := range g.EdgesByKind(graph.EdgeImports) {
						if e.FilePath == "consumer/src/lib.rs" {
							initial = append(initial, e.To)
						}
					}
					require.Equal(t, []string{want}, initial, "initial binding")
					writeWorkspaceFile(t, consumer, caller, "use "+spec+";\npub fn edited() {}\n")
					require.NoError(t, mi.GetIndexer("consumer").IndexFileNoResolve(caller))
					if mode == "incremental frontier" {
						require.NoError(t, mi.RunPreEnrichResolveFiles(context.Background(), []string{"consumer/src/lib.rs"}, nil))
					} else {
						mi.runMasterResolveFiles([]string{"consumer/src/lib.rs"}, true)
						mi.newCrossRepoResolver().ResolveForFile("consumer", "src/lib.rs")
					}
				}
				var imports int
				for e := range g.EdgesByKind(graph.EdgeImports) {
					if e.FilePath != filepath.ToSlash(filepath.Join("consumer", caller)) {
						continue
					}
					imports++
					t.Logf("import %s -> %s crossRepo=%v", e.From, e.To, e.CrossRepo)
					assert.Equal(t, resolves, e.CrossRepo)
					if resolves {
						assert.Equal(t, want, e.To)
					} else {
						assert.Equal(t, "external::"+strings.ReplaceAll(spec, "::", "/"), e.To)
					}
				}
				assert.Equal(t, 1, imports)
			})
		}
	}
}

func TestCargoCrateLookupNoManifest(t *testing.T) {
	mi := &MultiIndexer{repos: map[string]*RepoMetadata{"plain": {RootPath: t.TempDir()}}}
	assert.Nil(t, mi.cargoCrateLookup(), "no Cargo manifest must leave the retry disabled")
}
