package indexer

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zeebo/blake3"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/indexer/source"
	"github.com/zzet/gortex/internal/resolver"
	"go.uber.org/zap"
)

// The C capture and settle are deferred by D71. This fixture exercises the
// preserved helpers explicitly without enabling them in a build worker.
func TestRebuildBuildFrozenInputs(t *testing.T) {
	ctx := t.Context()
	const mod = "module example.org/widgets\n"
	db := []byte(`[{"file":"src/a.c","arguments":["cc","-Iz","-Ia"]}]`)
	var want store_pebble.Witness
	var wantRows []*graph.Edge
	for _, name := range []string{"plain", "repo[1]"} {
		root := filepath.Join(t.TempDir(), name)
		putRebuildCppFixture(t, root, "manifests/go.txt", []byte(mod))
		putRebuildCppFixture(t, root, "real/"+compileDBName, db)
		require.NoError(t, os.Symlink("manifests/go.txt", filepath.Join(root, "go.mod")))
		require.NoError(t, os.Symlink(filepath.Join(root, "real"), filepath.Join(root, "build")))
		src, err := source.NewFilesystemSource(root)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, src.Close()) })
		link, _, err := readRebuildInput(ctx, src, "go.mod", "manifest", 1024)
		require.NoError(t, err)
		req := rebuildBuildRequest{Root: root, Witness: store_pebble.Witness{Recipe: store_pebble.HeadDigest{1}, Input: store_pebble.HeadDigest{2}}}
		parent := t.TempDir()
		captureC := func(remaining *int64, emit func(rebuildSidecarRow) error) error {
			return captureRebuildCppInputs(ctx, src.FrozenRoot(), root, remaining, source.FrozenRootPath.Read, func(fact rebuildCppFact) error {
				return emit(rebuildSidecarCppRow(fact))
			})
		}
		err = withRebuildBuildInputs(ctx, parent, src, map[string]*rebuildInput{"go.mod": &link}, req, captureC,
			func(lease string, bound rebuildBuildRequest) error {
				require.Equal(t, "widgets", bound.ProjectName)
				require.NotEmpty(t, bound.Witness.FrozenInputs)
				require.NoError(t, bound.checkProjectRecipe())
				var rows []rebuildSidecarRow
				require.NoError(t, visitRebuildInputSidecar(ctx, lease, rebuildDigest(bound.Witness.FrozenInputs), func(row rebuildSidecarRow) error {
					rows = append(rows, row)
					return nil
				}))
				require.Equal(t, []rebuildSidecarRow{
					{Kind: rebuildFollowedManifest, Path: []byte("go.mod"), Present: true, Digest: blake3.Sum256([]byte(mod))},
					rebuildSidecarCppRow(rebuildCppFact{Kind: rebuildCppDatabase, Path: compileDBName}),
					rebuildSidecarCppRow(rebuildCppFact{Kind: rebuildCppDatabase, Path: "build/" + compileDBName, Present: true, Digest: blake3.Sum256(db)}),
					rebuildSidecarCppRow(rebuildCppFact{Kind: rebuildCppTU, Path: "src/a.c", IncludeDirs: []string{"z", "a"}}),
				}, rows)
				saved := settleFrozenCppFixture(t, lease, rebuildDigest(bound.Witness.FrozenInputs))
				if name == "plain" {
					want, wantRows = bound.Witness, saved
				} else {
					require.Equal(t, want.FrozenInputs, bound.Witness.FrozenInputs)
					require.Equal(t, want.Version(), bound.Witness.Version())
					require.Equal(t, wantRows, saved)
				}
				return nil
			})
		require.NoError(t, err)
		entries, err := os.ReadDir(parent)
		require.NoError(t, err)
		require.Empty(t, entries)
	}
}

func settleFrozenCppFixture(t *testing.T, lease string, digest rebuildDigest) []*graph.Edge {
	t.Helper()
	ctx := t.Context()
	replay, err := openRebuildCppReplay(ctx, lease, digest)
	require.NoError(t, err)
	defer func() { require.NoError(t, replay.Close()) }()
	build, err := store_pebble.OpenBuild(filepath.Join(lease, "fixture"), store_pebble.FullProfile)
	require.NoError(t, err)
	defer func() { require.NoError(t, build.Close()) }()
	var nodes []*graph.Node
	for _, file := range []string{"src/a.c", "src/missing.c", "a/dup.h", "z/dup.h"} {
		nodes = append(nodes, &graph.Node{ID: file, Name: filepath.Base(file), FilePath: file, Kind: graph.KindFile, Language: "c"})
	}
	var edges, want []*graph.Edge
	for i, dir := range []string{"z", "a"} {
		from := nodes[i].ID
		edge := &graph.Edge{From: from, To: "unresolved::import::dup.h", Kind: graph.EdgeImports, FilePath: from, Line: i + 1,
			Origin: graph.OriginASTInferred, Confidence: 0.5, ConfidenceLabel: "medium",
			Meta: map[string]any{"include_kind": "quoted", "payload": "dup.h"}}
		resolved := *edge
		resolved.To, resolved.Origin = dir+"/dup.h", graph.OriginASTResolved
		resolved.Meta = map[string]any{"include_kind": "quoted", "payload": "dup.h", "include_dir": dir, "resolved_via": "compile_db"}
		edges, want = append(edges, edge), append(want, &resolved)
	}
	build.AddBatch(nodes, edges)
	_, err = resolver.RunRebuildFileImports(ctx, build, 1, replay.Visit)
	require.NoError(t, err)
	var saved []*graph.Edge
	require.NoError(t, build.RebuildView().VisitEdges(ctx, func(edge *graph.Edge) error {
		saved = append(saved, edge)
		return nil
	}))
	require.Equal(t, want, saved)
	return saved
}

func TestRebuildBuildFrozenInputsRefuseLink(t *testing.T) {
	ctx := t.Context()
	root, src := rebuildInputFixture(t)
	parent := t.TempDir()
	outside := filepath.Join(t.TempDir(), "go.mod")
	require.NoError(t, os.WriteFile(outside, []byte("module example.org/outside\n"), 0o600))
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "go.mod")))
	link, _, err := readRebuildInput(ctx, src, "go.mod", "manifest", 4096)
	require.NoError(t, err)
	req := rebuildBuildRequest{Root: root, Witness: store_pebble.Witness{Recipe: store_pebble.HeadDigest{1}}}
	capture := func() error {
		return withRebuildBuildInputs(ctx, parent, src, map[string]*rebuildInput{"go.mod": &link}, req, nil, func(string, rebuildBuildRequest) error {
			t.Fatal("refused capture must not reach lookup or worker launch")
			return nil
		})
	}
	require.ErrorIs(t, capture(), source.ErrOutsideRoot)
	link.Content[0] ^= 1
	err = capture()
	require.ErrorIs(t, err, ErrBuildInputChanged)
	require.NotErrorIs(t, err, source.ErrOutsideRoot, "changed raw text refuses before following")
	entries, err := os.ReadDir(parent)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestRebuildBuildFrozenInputsRejectBeforeStage(t *testing.T) {
	ctx := t.Context()
	root, src := rebuildInputFixture(t)
	req := rebuildBuildRequest{
		Root: root, ParseDir: t.TempDir(), ParseLimit: 1024, Incarnation: rebuildTestIncarnation,
		Bounds:  rebuildTransformBounds{CaptureBytes: 10, FileBytes: 1024},
		Witness: store_pebble.Witness{Recipe: store_pebble.HeadDigest{1}, Stage: store_pebble.StageManifest, Profile: store_pebble.FullProfile},
	}
	for _, mode := range []string{"missing", "corrupt", "wrong digest"} {
		t.Run(mode, func(t *testing.T) {
			lease := t.TempDir()
			bound, err := captureRebuildBuildInputs(ctx, lease, src, map[string]*rebuildInput{"go.mod": nil, "package.json": nil}, req,
				func(_ *int64, emit func(rebuildSidecarRow) error) error {
					return writeRebuildNamespaceFact(rebuildNamespaceRow{key: ".", kind: rebuildNamespaceClose}, emit)
				})
			require.NoError(t, err)
			path := filepath.Join(lease, rebuildInputSidecarName)
			switch mode {
			case "missing":
				require.NoError(t, os.Remove(path))
			case "corrupt":
				require.NoError(t, os.Truncate(path, int64(len(rebuildInputSidecarMagic))))
			case "wrong digest":
				bound.Witness.FrozenInputs = store_pebble.HeadDigest{99}
				bound.Witness.Recipe = bound.projectRecipe(0)
			}
			require.NoError(t, writeRebuildBuildRequest(ctx, lease, bound, config.Default().Index, nil))
			err = buildRebuildStages(ctx, lease, zap.NewNop(), store_pebble.OpenBuild, store_pebble.RebuildLinkOptions{}, func(store_pebble.StageTables) error {
				t.Fatal("worker sent a stage without verified inputs")
				return nil
			})
			require.ErrorIs(t, err, errRebuildInputSidecar)
			_, err = os.Stat(filepath.Join(lease, "working"))
			require.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}

func TestRebuildCheckoutGitRequiredRefusesBeforeConsumer(t *testing.T) {
	dir, src := rebuildInputFixture(t)
	leases := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "source.go"), nil, 0o600))
	req := rebuildBuildRequest{Root: dir, Witness: store_pebble.Witness{Recipe: store_pebble.HeadDigest{1}}}
	err := withRebuildCheckoutBuildInputs(t.Context(), leases, src, req, nil, true,
		func(string, fs.FileMode, rebuildGitMember, *int64, func(rebuildSidecarRow) error) (rebuildCheckoutLeaf, error) {
			t.Fatal("Git refusal must not fall back to leaf capture")
			return rebuildCheckoutLeaf{}, nil
		}, func(rebuildCheckoutInputs, *int64, func(rebuildSidecarRow) error) (string, error) {
			t.Fatal("Git refusal reached resolver capture")
			return "", nil
		}, func(string, rebuildBuildRequest, []rebuildInput) error {
			t.Fatal("Git refusal reached the build consumer")
			return nil
		})
	require.ErrorIs(t, err, errRebuildGitMembership)
	entries, err := os.ReadDir(leases)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestRebuildBuildInputsRawAllowanceRefusal(t *testing.T) {
	root, src := rebuildInputFixture(t)
	leases := t.TempDir()
	req := rebuildBuildRequest{Root: root, Witness: store_pebble.Witness{Recipe: store_pebble.HeadDigest{1}}}
	for _, left := range []int64{0, -1} {
		called := false
		err := withRebuildBuildInputs(t.Context(), leases, src, map[string]*rebuildInput{"go.mod": nil, "package.json": nil}, req,
			func(remaining *int64, emit func(rebuildSidecarRow) error) error {
				*remaining -= int64(rebuildCppRawLimit) - left
				return emit(rebuildSidecarRow{Kind: rebuildNamespaceCloseExactFact, Path: []byte(".")})
			}, func(_ string, bound rebuildBuildRequest) error {
				called = true
				require.NotZero(t, bound.Witness.FrozenInputs)
				return nil
			})
		if left == 0 {
			require.NoError(t, err)
			require.True(t, called)
		} else {
			require.ErrorIs(t, err, errRebuildInputSidecar)
			require.ErrorIs(t, err, errRebuildCppCaptureLimit)
			require.False(t, called)
		}
		entries, err := os.ReadDir(leases)
		require.NoError(t, err)
		require.Empty(t, entries)
	}
}
