package resolver

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func TestRebuildJSTSImportNativeOrder(t *testing.T) {
	const main = "repo/app/main.ts"
	const aliasFile = "repo/app/alias.ts"
	const rewrittenFile = "repo/pkg/index.js"
	const exported = rewrittenFile + "::Run"
	rows := map[string]*graph.Node{
		aliasFile:     {ID: aliasFile, Kind: graph.KindFile, Language: "typescript"},
		rewrittenFile: {ID: rewrittenFile, Kind: graph.KindFile, Language: "javascript"},
		exported:      {ID: exported, Kind: graph.KindFunction, Language: "go"},
	}
	for _, tt := range []struct {
		name, from, file, spec string
		alias                  map[string]string
		rewrite                string
		wantTarget, effective  string
		wantCalls              []string
	}{
		{
			name: "AliasWinsBeforeNpm", from: main, file: main, spec: "pkg",
			alias: map[string]string{"pkg": "repo/app/alias"}, rewrite: "../pkg",
			wantTarget: aliasFile, effective: "pkg", wantCalls: []string{"alias:pkg"},
		},
		{
			name: "RelativeFromFallback", from: main, spec: "./alias",
			wantTarget: aliasFile, effective: "./alias",
		},
		{
			name: "RelativeExactMiss", from: main, file: main, spec: "./missing.js",
			effective: "./missing.js",
		},
		{
			name: "WorkspaceRelativeAfterGraphMiss", from: main, file: main, spec: "pkg",
			alias: map[string]string{"pkg": "repo/app/missing"}, rewrite: "../pkg",
			wantTarget: rewrittenFile, effective: "../pkg", wantCalls: []string{"alias:pkg", "npm:pkg"},
		},
		{
			name: "SecondBareAliasSymbolHit", from: "repo/other/main.ts", file: main, spec: "pkg/util::Run",
			alias: map[string]string{"@scope/pkg/util": "repo/pkg/index"}, rewrite: "@scope/pkg/util::Run",
			wantTarget: exported, effective: "@scope/pkg/util::Run",
			wantCalls: []string{"alias:pkg/util", "npm:pkg/util::Run", "alias:@scope/pkg/util"},
		},
		{
			name: "SecondBareAliasSymbolMiss", from: main, file: main, spec: "pkg/util::Missing",
			alias: map[string]string{"@scope/pkg/util": "repo/pkg/index"}, rewrite: "@scope/pkg/util::Missing",
			wantTarget: rewrittenFile, effective: "@scope/pkg/util::Missing",
			wantCalls: []string{"alias:pkg/util", "npm:pkg/util::Missing", "alias:@scope/pkg/util"},
		},
		{
			name: "BareRewriteRemainsEffectiveOnMiss", from: main, file: main, spec: "pkg::Run",
			alias: map[string]string{"real": "repo/app/missing"}, rewrite: "real::Run",
			effective: "real::Run", wantCalls: []string{"alias:pkg", "npm:pkg::Run", "alias:real"},
		},
		{
			name: "VerbatimNpmPayloadWithoutModule", from: main, file: main, spec: "::Run",
			rewrite: "../pkg::Run", wantTarget: exported, effective: "../pkg::Run", wantCalls: []string{"npm:::Run"},
		},
		{
			name: "EqualRewriteDoesNotReprobe", from: main, file: main, spec: "pkg",
			alias: map[string]string{"pkg": "repo/app/missing.js"}, rewrite: "pkg",
			effective: "pkg", wantCalls: []string{"alias:pkg", "npm:pkg"},
		},
		{
			name: "RawFilePathNotFromForNpm", from: main, spec: "pkg",
			effective: "pkg", wantCalls: []string{"alias:pkg"},
		},
		{
			name: "NpmExtensionIsCaseSensitive", from: "repo/app/main.TS", file: "repo/app/main.TS", spec: "pkg",
			effective: "pkg", wantCalls: []string{"alias:pkg"},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			edge := graph.Edge{From: tt.from, FilePath: tt.file, Kind: graph.EdgeImports, To: "unresolved::import::" + tt.spec}
			before := edge
			source := graph.Node{ID: tt.from, Language: "typescript"}
			var nativeIDs []string
			nativeNode := func(id string) *graph.Node {
				nativeIDs = append(nativeIDs, id)
				return rows[id]
			}
			nativeAlias := func(_, module string) string { return tt.alias[module] }
			caller := jsTSImportCallerFile(&edge)
			to, effective := resolveJSTSImportTarget(nativeNode, nativeAlias, caller, tt.spec), tt.spec
			if to == "" {
				var rewritten bool
				effective, rewritten = rewriteNpmAliasImport(func(_, _ string) string { return tt.rewrite }, edge.FilePath, tt.spec)
				if rewritten {
					to = resolveJSTSImportTarget(nativeNode, nativeAlias, caller, effective)
				}
			}
			require.Equal(t, tt.wantTarget, to)
			require.Equal(t, tt.effective, effective)

			var gotIDs, calls []string
			node := func(id string) (*graph.Node, error) {
				gotIDs = append(gotIDs, id)
				return rows[id], nil
			}
			in := rebuildJSTSImportInputs{
				pathAlias: func(file, module string) (string, error) {
					require.Equal(t, caller, file)
					calls = append(calls, "alias:"+module)
					return tt.alias[module], nil
				},
				rewriteNpm: func(file, raw string) (string, error) {
					require.Equal(t, tt.file, file)
					require.Equal(t, tt.spec, raw)
					calls = append(calls, "npm:"+raw)
					return tt.rewrite, nil
				},
			}
			if tt.wantCalls == nil {
				in = rebuildJSTSImportInputs{} // relative hit needs neither reader
			}
			got, spec, err := resolveRebuildJSTSImport(&edge, &source, tt.spec, node, in)
			require.NoError(t, err)
			require.Equal(t, rows[to], got)
			require.Equal(t, effective, spec)
			require.Equal(t, nativeIDs, gotIDs)
			require.Equal(t, tt.wantCalls, calls)
			require.Equal(t, before, edge)
		})
	}
}

func TestRebuildJSTSImportRefusesInputs(t *testing.T) {
	const main = "repo/app/main.ts"
	edge := graph.Edge{From: main, FilePath: main, To: "unresolved::import::pkg", Kind: graph.EdgeImports}
	source := graph.Node{ID: main, Language: "typescript"}
	unknown := errors.New("unproved alias input")
	dangling := errors.New("dangling npm input")
	readErr := errors.New("graph point read failed")

	t.Run("AliasBeforeNpm", func(t *testing.T) {
		in := rebuildJSTSImportInputs{
			pathAlias: func(_, _ string) (string, error) { return "repo/app/winner", unknown },
			rewriteNpm: func(_, _ string) (string, error) {
				t.Fatal("npm consulted after unknown TS alias")
				return "./winner", nil
			},
		}
		got, spec, err := resolveRebuildJSTSImport(&edge, &source, "pkg", nil, in)
		require.ErrorIs(t, err, unknown)
		require.Nil(t, got)
		require.Empty(t, spec)
	})
	t.Run("NpmBeforeSecondProbe", func(t *testing.T) {
		in := rebuildJSTSImportInputs{
			pathAlias:  func(_, _ string) (string, error) { return "", nil },
			rewriteNpm: func(_, _ string) (string, error) { return "./winner", dangling },
		}
		got, spec, err := resolveRebuildJSTSImport(&edge, &source, "pkg", nil, in)
		require.ErrorIs(t, err, dangling)
		require.Nil(t, got)
		require.Empty(t, spec)
	})
	t.Run("SecondAlias", func(t *testing.T) {
		in := rebuildJSTSImportInputs{
			pathAlias: func(_, module string) (string, error) {
				if module == "pkg" {
					return "", nil
				}
				return "repo/app/winner", unknown
			},
			rewriteNpm: func(_, _ string) (string, error) { return "real", nil },
		}
		got, spec, err := resolveRebuildJSTSImport(&edge, &source, "pkg", nil, in)
		require.ErrorIs(t, err, unknown)
		require.Nil(t, got)
		require.Empty(t, spec)
	})
	t.Run("FirstGraphErrorCannotUseLaterExtension", func(t *testing.T) {
		var reads []string
		node := func(id string) (*graph.Node, error) {
			reads = append(reads, id)
			if id == "repo/app/lib.js" {
				return nil, readErr
			}
			return &graph.Node{ID: id, Kind: graph.KindFile}, nil
		}
		got, spec, err := resolveRebuildJSTSImport(&edge, &source, "./lib.js", node, rebuildJSTSImportInputs{})
		require.ErrorIs(t, err, readErr)
		require.Equal(t, []string{"repo/app/lib.js"}, reads)
		require.Nil(t, got)
		require.Empty(t, spec)
	})
	t.Run("SecondGraphError", func(t *testing.T) {
		in := rebuildJSTSImportInputs{
			pathAlias:  func(_, _ string) (string, error) { return "", nil },
			rewriteNpm: func(_, _ string) (string, error) { return "./winner.js", nil },
		}
		node := func(id string) (*graph.Node, error) {
			require.Equal(t, "repo/app/winner.js", id)
			return &graph.Node{ID: id, Kind: graph.KindFile}, readErr
		}
		got, spec, err := resolveRebuildJSTSImport(&edge, &source, "pkg", node, in)
		require.ErrorIs(t, err, readErr)
		require.Nil(t, got)
		require.Empty(t, spec)
	})
	t.Run("SymbolReadErrorIsNotAFileHit", func(t *testing.T) {
		node := func(id string) (*graph.Node, error) {
			if id == "repo/app/lib.ts::Run" {
				return nil, readErr
			}
			return &graph.Node{ID: id, Kind: graph.KindFile}, nil
		}
		got, spec, err := resolveRebuildJSTSImport(&edge, &source, "./lib.ts::Run", node, rebuildJSTSImportInputs{})
		require.ErrorIs(t, err, readErr)
		require.Nil(t, got)
		require.Empty(t, spec)
	})
	t.Run("MissingExactReaders", func(t *testing.T) {
		in := rebuildJSTSImportInputs{}
		got, spec, err := resolveRebuildJSTSImport(&edge, &source, "pkg", nil, in)
		require.ErrorIs(t, err, errRebuildMainRule)
		require.ErrorContains(t, err, "checked TS path-alias/baseUrl result")
		require.Nil(t, got)
		require.Empty(t, spec)

		in.pathAlias = func(_, _ string) (string, error) { return "", nil }
		got, spec, err = resolveRebuildJSTSImport(&edge, &source, "pkg", nil, in)
		require.ErrorIs(t, err, errRebuildMainRule)
		require.ErrorContains(t, err, "checked npm alias/workspace/exports rewrite result")
		require.Nil(t, got)
		require.Empty(t, spec)
	})
	t.Run("MismatchedPointIdentity", func(t *testing.T) {
		node := func(string) (*graph.Node, error) {
			return &graph.Node{ID: "repo/app/other.ts", Kind: graph.KindFile}, nil
		}
		got, spec, err := resolveRebuildJSTSImport(&edge, &source, "./lib.ts", node, rebuildJSTSImportInputs{})
		require.ErrorContains(t, err, "matching graph point identity")
		require.Nil(t, got)
		require.Empty(t, spec)
	})
	t.Run("GoSourceNeedsOwnership", func(t *testing.T) {
		goSource := graph.Node{ID: main, Language: "go"}
		got, spec, err := resolveRebuildJSTSImport(&edge, &goSource, "./lib.ts", nil, rebuildJSTSImportInputs{})
		require.ErrorIs(t, err, errRebuildMainRule)
		require.ErrorContains(t, err, "Go package ownership")
		require.Nil(t, got)
		require.Empty(t, spec)
	})
}

func TestRebuildJSTSImportNoEffect(t *testing.T) {
	for _, tt := range []struct{ language, caller, spec string }{
		{"go", "repo/main.go", "pkg"},
		{"vue", "repo/main.vue", "./lib"},
		{"typescript", "repo/main.ts", ""},
	} {
		edge := graph.Edge{From: tt.caller, FilePath: tt.caller}
		source := graph.Node{ID: tt.caller, Language: tt.language}
		got, spec, err := resolveRebuildJSTSImport(&edge, &source, tt.spec, nil, rebuildJSTSImportInputs{})
		require.NoError(t, err)
		require.Nil(t, got)
		require.Equal(t, tt.spec, spec)
	}
}
