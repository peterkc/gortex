package indexer

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/search"
)

func TestTrackedPythonPackageReExport(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		for _, providerFirst := range []bool{false, true} {
			for _, tc := range []struct {
				name, exports string
				resolves      bool
			}{
				{"explicit_alias", "from .url_safe import URLSafeTimedSerializer as URLSafeTimedSerializer\n", true},
				{"plain", "from .url_safe import URLSafeTimedSerializer\n", true},
				{"ambiguous", "from .url_safe import URLSafeTimedSerializer\nfrom .other import URLSafeTimedSerializer\n", false},
				{"not_exported", "from .other import unrelated\n", false},
			} {
				t.Run(fmt.Sprintf("%s/provider_first=%v/%s", backend, providerFirst, tc.name), func(t *testing.T) {
					provider, consumer := t.TempDir(), t.TempDir()
					writeWorkspaceFile(t, provider, "pyproject.toml", "[project]\nname = 'itsdangerous'\n")
					writeWorkspaceFile(t, provider, "src/itsdangerous/__init__.py", tc.exports)
					writeWorkspaceFile(t, provider, "src/itsdangerous/url_safe.py", "class URLSafeTimedSerializer: pass\n")
					writeWorkspaceFile(t, provider, "src/itsdangerous/other.py", "class URLSafeTimedSerializer: pass\ndef unrelated(): pass\n")
					writeWorkspaceFile(t, consumer, "pyproject.toml", "[project]\ndependencies = ['itsdangerous']\n")
					writeWorkspaceFile(t, consumer, "app.py", "from itsdangerous import URLSafeTimedSerializer\ndef run(): return URLSafeTimedSerializer('secret')\n")
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
					g := pythonLocalStore(t, backend)
					reg := newTestRegistry()
					reg.Register(languages.NewPythonExtractor())
					mi := NewMultiIndexer(g, reg, search.NewNull(), cm, zap.NewNop())
					t.Cleanup(func() { require.NoError(t, mi.Close(context.Background())) })
					for _, entry := range entries {
						_, err = mi.TrackRepoCtx(context.Background(), entry)
						require.NoError(t, err)
					}
					var imports, calls []*graph.Edge
					for _, edge := range g.GetOutEdges("consumer/app.py") {
						if edge.Kind == graph.EdgeImports {
							imports = append(imports, edge)
						}
					}
					require.Len(t, imports, 1)
					require.Equal(t, "provider/src/itsdangerous/__init__.py", imports[0].To)
					require.True(t, imports[0].CrossRepo)
					assertCall := func(target string, linked bool) {
						t.Helper()
						calls = nil
						for _, edge := range g.GetOutEdges("consumer/app.py::run") {
							if edge.Kind == graph.EdgeCalls {
								calls = append(calls, edge)
							}
						}
						require.Len(t, calls, 1)
						require.Equal(t, linked, calls[0].CrossRepo, "%#v", calls[0])
						require.Equal(t, target, calls[0].To)
						require.NotEmpty(t, calls[0].Meta["python_call_stub"])
						require.NotContains(t, calls[0].Meta, "external_call")
					}
					stub := "unresolved::extern::itsdangerous.URLSafeTimedSerializer::URLSafeTimedSerializer"
					target := "provider/src/itsdangerous/url_safe.py::URLSafeTimedSerializer"
					if !tc.resolves {
						assertCall(stub, false)
						return
					}
					assertCall(target, true)
					if tc.name != "explicit_alias" {
						return
					}
					writeWorkspaceFile(t, provider, "src/itsdangerous/url_safe.py", "class URLSafeTimedSerializer:\n    def added(self): pass\n")
					_, err = mi.IncrementalReindexRepo("provider", []string{filepath.Join(provider, "src/itsdangerous/url_safe.py")})
					require.NoError(t, err)
					assertCall(target, true)
					_, err = mi.IndexRepo("provider")
					require.NoError(t, err)
					assertCall(target, true)
					_, _, err = mi.UntrackRepoChecked(context.Background(), "provider")
					require.NoError(t, err)
					assertCall(stub, false)
					_, err = mi.TrackRepoCtx(context.Background(), config.RepoEntry{Path: provider, Name: "provider", Workspace: "shared"})
					require.NoError(t, err)
					assertCall(target, true)
					writeWorkspaceFile(t, provider, "src/itsdangerous/__init__.py", "from .other import URLSafeTimedSerializer\n")
					_, err = mi.IncrementalReindexRepo("provider", []string{filepath.Join(provider, "src/itsdangerous/__init__.py")})
					require.NoError(t, err)
					assertCall("provider/src/itsdangerous/other.py::URLSafeTimedSerializer", true)
					writeWorkspaceFile(t, provider, "src/itsdangerous/__init__.py", "from .url_safe import URLSafeTimedSerializer\nfrom .other import URLSafeTimedSerializer\n")
					_, err = mi.IncrementalReindexRepo("provider", []string{filepath.Join(provider, "src/itsdangerous/__init__.py")})
					require.NoError(t, err)
					assertCall(stub, false)
					writeWorkspaceFile(t, provider, "src/itsdangerous/__init__.py", "")
					_, err = mi.IncrementalReindexRepo("provider", []string{filepath.Join(provider, "src/itsdangerous/__init__.py")})
					require.NoError(t, err)
					assertCall(stub, false)
				})
			}
		}
	}
}
