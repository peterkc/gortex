package indexer

import (
	"bytes"
	"context"
	"encoding/gob"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/clones"
	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/excludes"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/resolver"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

const rebuildTestIncarnation = "0198abcd-0123-7abc-8def-0123456789ab"

func TestRebuildBuildChildStages(t *testing.T) {
	for _, tc := range []struct {
		name        string
		edit        bool
		badRetained bool
	}{
		{name: "sealed prefix is not complete"},
		{name: "uncached edit crosses process boundary", edit: true},
		{name: "sidecar collision prevents manifest publication", badRetained: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			root, src := rebuildInputFixture(t)
			const rel = "sample.go"
			require.NoError(t, os.WriteFile(filepath.Join(root, rel), []byte("package p\nfunc Frozen() {}\n"), 0o600))
			in, _, err := readRebuildInput(ctx, src, rel, "go", 1024)
			require.NoError(t, err)
			inputs := []rebuildInput{in}
			digest, err := rebuildInputRoot(inputs)
			require.NoError(t, err)
			raw, err := hex.DecodeString(digest)
			require.NoError(t, err)
			require.Len(t, raw, 32)
			cfg := config.Default().Index
			req := rebuildBuildRequest{
				Root: root, ParseDir: t.TempDir(), ParseLimit: 1 << 20, Incarnation: rebuildTestIncarnation,
				Recipes: map[string]rebuildDigest{"go": {1}}, Policy: freezeRebuildParsePolicy(root, cfg),
				Bounds: rebuildTransformBounds{CaptureBytes: 1024, FileBytes: 64 << 10},
				Witness: store_pebble.Witness{Input: store_pebble.HeadDigest(raw), Recipe: store_pebble.HeadDigest{2},
					Profile: store_pebble.FullProfile, Stage: store_pebble.StageManifest},
			}
			manifests := map[string]*rebuildInput{"go.mod": nil, "package.json": nil}
			if !tc.edit {
				require.NoError(t, os.WriteFile(filepath.Join(root, "go-target"), []byte("module example.org/widgets\n"), 0o600))
				require.NoError(t, os.Symlink("go-target", filepath.Join(root, "go.mod")))
				link, _, err := readRebuildInput(ctx, src, "go.mod", "manifest", 1024)
				require.NoError(t, err)
				manifests["go.mod"] = &link
			}
			leases := t.TempDir()
			other := filepath.Join(leases, "unowned")
			require.NoError(t, os.WriteFile(other, []byte("keep"), 0o600))
			exe, err := os.Executable()
			require.NoError(t, err)
			t.Setenv("GORTEX_TEST_REBUILD_BUILD_CHILD", "1")
			headDir := filepath.Join(t.TempDir(), "head")
			h, err := store_pebble.OpenHead(headDir, req.Incarnation, req.Witness.Profile)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, h.Close()) })
			var pinned *store_pebble.Snapshot
			var ref store_pebble.HeadDigest
			err = withRebuildBuildInputs(ctx, leases, src, manifests, req, nil,
				func(lease string, bound rebuildBuildRequest) error {
					ref = bound.Witness.FrozenInputs
					if !tc.edit {
						require.NotEmpty(t, ref)
						// The child must not need the followed manifest's bytes.
						require.NoError(t, os.Remove(filepath.Join(root, "go-target")))
					}
					if tc.badRetained {
						require.NoError(t, os.Mkdir(retainedRebuildInputPath(headDir, rebuildDigest(ref)), 0o700))
					}
					return runRebuildBuild(ctx, exe, []string{"-test.run=^TestRebuildBuildChildEntry$", "--"}, lease, headDir, bound, cfg, inputs,
						func(ctx context.Context, tables store_pebble.StageTables) error {
							require.Equal(t, req.Incarnation, tables.Incarnation)
							if _, err := h.PublishStage(ctx, tables); err != nil {
								return err
							}
							if tables.Witness.Stage == store_pebble.StageManifest {
								pinned = h.OpenSnapshot()
								t.Cleanup(func() { require.NoError(t, pinned.Close()) })
								if tc.edit {
									return os.WriteFile(filepath.Join(root, rel), []byte("package p\nfunc Other() {}\n"), 0o600)
								}
							}
							return nil
						})
				})
			require.ErrorIs(t, err, ErrRebuildWorker)
			entries, leaseErr := os.ReadDir(leases)
			require.NoError(t, leaseErr)
			require.Len(t, entries, 1, "the owner must remove only its build lease after reaping")
			require.Equal(t, "unowned", entries[0].Name())
			if tc.badRetained {
				require.ErrorIs(t, err, errRebuildInputSidecar)
				require.Nil(t, pinned, "the owner must not offer the manifest to the publisher")
				latest := h.OpenSnapshot()
				t.Cleanup(func() { require.NoError(t, latest.Close()) })
				require.ErrorIs(t, latest.RequireStage(store_pebble.StageManifest), store_pebble.ErrNotPublished)
				collision, statErr := os.Lstat(retainedRebuildInputPath(headDir, rebuildDigest(ref)))
				require.NoError(t, statErr)
				require.True(t, collision.IsDir())
				return
			}
			require.NotNil(t, pinned, "the earlier stage must really publish")
			published, publicationErr := pinned.Publication()
			require.NoError(t, publicationErr)
			require.Equal(t, ref, published.FrozenInputs)
			nManifest := 0
			require.NoError(t, pinned.ManifestFiles(ctx, func(f store_pebble.ManifestFile) error {
				nManifest++
				require.Equal(t, in.Path, f.Path)
				require.Equal(t, store_pebble.HeadDigest(in.Content), f.Content)
				return nil
			}))
			require.Equal(t, 1, nManifest)
			var pending *store_pebble.ErrStagePending
			require.ErrorAs(t, pinned.RequireStage(store_pebble.StageFileGraph), &pending)
			latest := h.OpenSnapshot()
			t.Cleanup(func() { require.NoError(t, latest.Close()) })
			saved, e := os.ReadDir(req.ParseDir)
			require.NoError(t, e)
			if tc.edit {
				require.ErrorIs(t, err, ErrBuildInputChanged)
				require.Empty(t, saved)
				require.ErrorAs(t, latest.RequireStage(store_pebble.StageFileGraph), &pending)
			} else {
				require.ErrorIs(t, err, errRebuildBuildIncomplete)
				require.Len(t, saved, 1)
				require.NoError(t, latest.RequireStage(store_pebble.StageFileGraph))
				require.Len(t, latest.FindNodesByName("Frozen"), 1)
				require.ErrorAs(t, latest.RequireStage(store_pebble.StageRelations), &pending)
			}
			if ref != (store_pebble.HeadDigest{}) {
				fresh := t.TempDir()
				require.NoError(t, restoreRebuildInputSidecar(ctx, headDir, fresh, rebuildDigest(published.FrozenInputs)))
				require.NoError(t, visitRebuildInputSidecar(ctx, fresh, rebuildDigest(published.FrozenInputs), func(row rebuildSidecarRow) error {
					require.Equal(t, rebuildFollowedManifest, row.Kind)
					require.Equal(t, "go.mod", string(row.Path))
					return nil
				}))
			}
		})
	}
}

func TestRebuildBuildPrivateCloneTail(t *testing.T) {
	for _, tc := range []struct {
		name            string
		exact, shadowed bool
	}{
		{name: "signed_only_with_extraction_off", exact: true},
		{name: "refuses_shadow_before_apply", exact: true, shadowed: true},
		{name: "legacy_capture_stops_before_clones", shadowed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			root, src := rebuildInputFixture(t)
			lease := t.TempDir()
			cfg := config.Default().Index
			off := false
			cfg.Coverage.Modules.Enabled, cfg.Coverage.Clones.Enabled = &off, &off
			req := rebuildBuildRequest{
				Root: root, ParseDir: t.TempDir(), ParseLimit: 1 << 20, Incarnation: rebuildTestIncarnation,
				BaseRecipe: store_pebble.HeadDigest{1}, ProvidersOff: true, Recipes: map[string]rebuildDigest{"go": {1}},
				Policy: freezeRebuildParsePolicy(root, cfg), Bounds: rebuildTransformBounds{CaptureBytes: 1024, FileBytes: 1024},
				Witness: store_pebble.Witness{Stage: store_pebble.StageManifest, Profile: store_pebble.FullProfile},
			}
			// Use checked cached receipts so this test exercises the build
			// worker rather than tree-sitter or an external extractor process.
			idx := &Indexer{config: cfg}
			idx.installRebuildParsePolicy(req.Policy)
			store := rebuildParseStore{dir: req.ParseDir, limit: req.ParseLimit}
			sig := clones.EncodeSignature(clones.Signature{1})
			var inputs []rebuildInput
			for _, path := range []string{"a.go", "b.go"} {
				require.NoError(t, os.WriteFile(filepath.Join(root, path), []byte("package p\n"), 0o600))
				input, _, err := readRebuildInput(ctx, src, path, "go", 1024)
				require.NoError(t, err)
				inputs = append(inputs, input)
				n := &graph.Node{ID: path + "::F", Kind: graph.KindFunction, Name: "F", FilePath: path, Language: "go",
					StartLine: 1, EndLine: 2, Meta: map[string]any{cloneSigMetaKey: sig, cloneTokensMetaKey: 80}}
				if tc.shadowed && path == "a.go" {
					n.ID, n.FilePath, n.Kind, n.Meta = "b.go::F", "b.go", graph.KindVariable, nil
				}
				payload := encodeRebuildTestParseFacts(t, rebuildCloneClaimFixture(path, n))
				require.NoError(t, store.write(input.parseKey(idx.rebuildExtractorRecipe(req.Recipes["go"], input)), payload))
			}
			inputRoot, err := rebuildInputRoot(inputs)
			require.NoError(t, err)
			bytes, err := hex.DecodeString(inputRoot)
			require.NoError(t, err)
			req.Witness.Input = store_pebble.HeadDigest(bytes)
			if tc.exact {
				digest, err := writeRebuildInputSidecar(ctx, lease, func(emit func(rebuildSidecarRow) error) error {
					for _, in := range inputs {
						if err := emit(rebuildSidecarRow{Kind: rebuildNamespaceRegularFact, Path: []byte(in.Path)}); err != nil {
							return err
						}
					}
					return emit(rebuildSidecarRow{Kind: rebuildNamespaceCloseExactFact, Path: []byte(".")})
				})
				require.NoError(t, err)
				req.Witness.FrozenInputs = store_pebble.HeadDigest(digest)
			}
			req.Witness.Recipe = req.projectRecipe(cfg.Coverage.ClonesThreshold())
			require.NoError(t, writeRebuildBuildRequest(ctx, lease, req, cfg, inputs))
			core, logs := observer.New(zap.InfoLevel)
			var sent []store_pebble.Stage
			err = buildRebuildStages(ctx, lease, zap.New(core), store_pebble.OpenBuild, rebuildTestLinkOptions(t), func(tables store_pebble.StageTables) error {
				sent = append(sent, tables.Witness.Stage)
				return nil
			})
			const capabilityMessage = "rebuild capabilities applied to private build"
			const cloneMessage = "rebuild clone relations applied to private build"
			capabilities := logs.FilterMessage(capabilityMessage).All()
			applied := logs.FilterMessage(cloneMessage).All()
			if tc.shadowed {
				// Receipt validation precedes file_graph for both checkout modes.
				require.Equal(t, []store_pebble.Stage{store_pebble.StageManifest}, sent)
				require.ErrorIs(t, err, store_pebble.ErrRebuildUnknownProvenance)
				reply := buildRefusal(err)
				require.Equal(t, uint8(36), reply.Refusal)
				require.EqualValues(t, 1, reply.UnknownProvenance)
				require.Contains(t, reply.Detail, "receipt shadowed_id=1 unexplained_id=1 shadowed_edge=0:")
				require.Empty(t, capabilities)
				require.Empty(t, applied)
				return
			}
			require.Equal(t, []store_pebble.Stage{store_pebble.StageManifest, store_pebble.StageFileGraph}, sent)
			require.ErrorIs(t, err, errRebuildBuildIncomplete)
			if !tc.exact {
				require.Empty(t, applied)
				return
			}
			require.Len(t, capabilities, 1)
			require.Len(t, applied, 1)
			entries := logs.All()
			capabilityAt := slices.IndexFunc(entries, func(e observer.LoggedEntry) bool { return e.Message == capabilityMessage })
			cloneAt := slices.IndexFunc(entries, func(e observer.LoggedEntry) bool { return e.Message == cloneMessage })
			require.Less(t, capabilityAt, cloneAt)
			capabilityFields := capabilities[0].ContextMap()
			require.EqualValues(t, 1, capabilityFields["receiver_rounds"])
			require.EqualValues(t, 1, capabilityFields["receiver_round_bound"])
			require.EqualValues(t, 0, capabilityFields["receiver_matches"])
			fields := applied[0].ContextMap()
			require.EqualValues(t, 0, fields["raw_bodies"])
			require.EqualValues(t, 2, fields["signed_only"])
			require.EqualValues(t, 1, fields["clone_pairs"])
			require.EqualValues(t, 38, fields["claim_scratch_bytes"])
		})
	}
}

func TestRebuildBuildChildEntry(t *testing.T) {
	if os.Getenv("GORTEX_TEST_REBUILD_BUILD_CHILD") != "1" {
		t.Skip("worker entry point")
	}
	if err := RunRebuildBuildWorker(t.Context(), os.Args[len(os.Args)-1], os.Stdin, os.Stdout, zap.NewNop(), rebuildTestLinkOptions(t)); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

func rebuildTestLinkOptions(t *testing.T) store_pebble.RebuildLinkOptions {
	t.Helper()
	return store_pebble.RebuildLinkOptions{SpillBytes: 1 << 20}
}

func TestRebuildBuildFatalChildDiagnostic(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	root, src := rebuildInputFixture(t)
	parent := t.TempDir()
	unowned := filepath.Join(parent, "unowned")
	require.NoError(t, os.WriteFile(unowned, []byte("keep"), 0o600))
	exe, err := os.Executable()
	require.NoError(t, err)
	t.Setenv("GORTEX_TEST_REBUILD_BUILD_FATAL", "1")
	req := rebuildBuildRequest{Root: root, Witness: store_pebble.Witness{Recipe: store_pebble.HeadDigest{1}}}
	err = withRebuildBuildInputs(ctx, parent, src, map[string]*rebuildInput{"go.mod": nil, "package.json": nil}, req, nil,
		func(lease string, bound rebuildBuildRequest) error {
			return runRebuildBuild(ctx, exe, []string{"-test.run=^TestRebuildBuildFatalChildEntry$", "--"}, lease, parent, bound, config.Default().Index, nil,
				func(context.Context, store_pebble.StageTables) error {
					t.Fatal("failed child did not complete a stage")
					return nil
				})
		})
	require.ErrorIs(t, err, ErrRebuildWorker)
	var exit *exec.ExitError
	require.ErrorAs(t, err, &exit)
	require.Equal(t, 79, exit.ExitCode())
	require.ErrorContains(t, err, "MANIFEST sync failed: planted scratch error")
	require.NotContains(t, err.Error(), "earlier diagnostic")
	entries, err := os.ReadDir(parent)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, "unowned", entries[0].Name())
}

func TestRebuildBuildFatalChildEntry(t *testing.T) {
	if os.Getenv("GORTEX_TEST_REBUILD_BUILD_FATAL") != "1" {
		t.Skip("isolated fatal worker fixture")
	}
	_, err := fmt.Fprint(os.Stderr, "earlier diagnostic\nMANIFEST sync failed: planted scratch error\n\n")
	require.NoError(t, err)
	os.Exit(79)
}

func TestRebuildWorkerDiagnosticBound(t *testing.T) {
	var diagnostic rebuildWorkerDiagnostic
	for _, chunk := range []string{"old\nnew ", "cause ", strings.Repeat("x", 2*rebuildDiagnosticBytes), "\n \n"} {
		n, err := diagnostic.Write([]byte(chunk))
		require.NoError(t, err)
		require.Len(t, chunk, n)
	}
	line := diagnostic.lastLine()
	require.Len(t, line, rebuildDiagnosticBytes)
	require.True(t, strings.HasPrefix(line, "new cause "))
	_, err := diagnostic.Write([]byte("unterminated"))
	require.NoError(t, err)
	require.Equal(t, "unterminated", diagnostic.lastLine())
}

func TestRebuildBuildRequestPreservesPathBytesAndSetting(t *testing.T) {
	dir, src := rebuildInputFixture(t)
	req := rebuildBuildRequest{Root: dir, ParseDir: dir, ParseLimit: 4096,
		Bounds: rebuildTransformBounds{CaptureBytes: 10, FileBytes: 1024}, Incarnation: rebuildTestIncarnation,
		Witness: store_pebble.Witness{Stage: store_pebble.StageManifest, Recipe: store_pebble.HeadDigest{2}},
	}
	req, err := captureProjectNameTest(t, src, map[string]*rebuildInput{"go.mod": nil, "package.json": nil}, req)
	require.NoError(t, err)
	disabled := false
	cfg := config.Default().Index
	cfg.SynthesizeExternalCalls = &disabled
	inputs := []rebuildInput{{Path: "source\xff.go", Language: "go"}}
	require.NoError(t, writeRebuildBuildRequest(t.Context(), dir, req, cfg, inputs))
	got, gotInputs, err := readRebuildBuildRequest(t.Context(), dir)
	require.NoError(t, err)
	require.Equal(t, inputs, gotInputs)
	require.Equal(t, rebuildTestIncarnation, got.Incarnation)
	require.True(t, got.ProvidersOff)
	var cfgGot config.IndexConfig
	require.NoError(t, json.Unmarshal(got.Config, &cfgGot))
	require.NotNil(t, cfgGot.SynthesizeExternalCalls)
	require.False(t, *cfgGot.SynthesizeExternalCalls)
}

func TestRebuildBuildRequestRequiresProvidersOff(t *testing.T) {
	dir, src := rebuildInputFixture(t)
	req := rebuildBuildRequest{Root: dir, ParseDir: dir, ParseLimit: 4096,
		Bounds: rebuildTransformBounds{CaptureBytes: 10, FileBytes: 1024}, Incarnation: rebuildTestIncarnation,
		Witness: store_pebble.Witness{Stage: store_pebble.StageManifest, Recipe: store_pebble.HeadDigest{2}},
	}
	bound, err := captureProjectNameTest(t, src, map[string]*rebuildInput{"go.mod": nil, "package.json": nil}, req)
	require.NoError(t, err)
	require.True(t, bound.ProvidersOff)
	bound.ProvidersOff = false
	require.NotEqual(t, bound.Witness.Recipe, bound.projectRecipe(0))
	// Missing policy must refuse even when the recipe matches the header.
	// Neither request boundary should rely on opening the store.
	bound.Witness.Recipe = bound.projectRecipe(0)
	lease := t.TempDir()
	err = writeRebuildBuildRequest(t.Context(), lease, bound, config.Default().Index, nil)
	require.ErrorIs(t, err, errRebuildBuildWire)
	require.ErrorContains(t, err, "resolver LSP off policy")
	var encoded bytes.Buffer
	require.NoError(t, gob.NewEncoder(&encoded).Encode(bound))
	require.NoError(t, os.WriteFile(filepath.Join(lease, rebuildRequestName), encoded.Bytes(), 0o600))
	_, inputs, err := readRebuildBuildRequest(t.Context(), lease)
	require.ErrorIs(t, err, errRebuildBuildWire)
	require.ErrorContains(t, err, "resolver LSP off policy")
	require.Empty(t, inputs)
}

func TestRebuildProvidersOffRejectsInstalledLSP(t *testing.T) {
	idx := newTestIndexer(graph.New())
	t.Cleanup(idx.Close)
	require.NoError(t, idx.checkRebuildProvidersOff())
	helper := &preEnrichLSP{}
	idx.SetResolverLSPHelper(helper)
	err := idx.checkRebuildProvidersOff()
	require.ErrorIs(t, err, errRebuildBuildWire)
	require.ErrorContains(t, err, "no LSP helper")
	require.Zero(t, helper.calls.Load())
}

func TestRebuildBuildRequestRejectsInvalidIncarnation(t *testing.T) {
	dir, src := rebuildInputFixture(t)
	req := rebuildBuildRequest{Root: dir, ParseDir: dir, ParseLimit: 4096,
		Bounds: rebuildTransformBounds{CaptureBytes: 10, FileBytes: 1024}, Incarnation: strings.ToUpper(rebuildTestIncarnation),
		Witness: store_pebble.Witness{Stage: store_pebble.StageManifest, Recipe: store_pebble.HeadDigest{2}},
	}
	req, err := captureProjectNameTest(t, src, map[string]*rebuildInput{"go.mod": nil, "package.json": nil}, req)
	require.NoError(t, err)
	req.Files = 1 // The worker must refuse the identity before trying to decode the missing record.
	for _, tc := range []struct {
		name   string
		header any
		want   error
	}{
		{"noncanonical", req, store_pebble.ErrInvalidIncarnation},
		{"old numeric field", struct{ Incarnation uint64 }{7}, errRebuildBuildWire},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lease := t.TempDir()
			var request, reply, acks bytes.Buffer
			require.NoError(t, gob.NewEncoder(&request).Encode(tc.header))
			require.NoError(t, os.WriteFile(filepath.Join(lease, rebuildRequestName), request.Bytes(), 0o600))
			in, owner, err := os.Pipe()
			require.NoError(t, err)
			defer owner.Close()
			err = RunRebuildBuildWorker(t.Context(), lease, in, &reply, zap.NewNop(), store_pebble.RebuildLinkOptions{})
			require.ErrorIs(t, err, tc.want)
			err = receiveRebuildStages(t.Context(), lease, req, &reply, &acks, func(context.Context, store_pebble.StageTables) error {
				t.Fatal("an invalid request must not publish")
				return nil
			})
			require.ErrorIs(t, err, ErrRebuildWorker)
			require.ErrorIs(t, err, tc.want)
			require.Empty(t, acks.Bytes())
			entries, err := os.ReadDir(lease)
			require.NoError(t, err)
			require.Len(t, entries, 1, "the worker must not create its private store or stages")
		})
	}
}

func TestRebuildBuildMalformedStageDoesNotPublish(t *testing.T) {
	lease := t.TempDir()
	for name, expected := range map[string]error{
		"skip manifest":      store_pebble.ErrStageOrder,
		"wrong input digest": store_pebble.ErrStaleBase,
		"invalid frame":      errRebuildBuildWire,
	} {
		t.Run(name, func(t *testing.T) {
			req := rebuildBuildRequest{Witness: store_pebble.Witness{
				Input: store_pebble.HeadDigest{1}, Stage: store_pebble.StageManifest, Profile: store_pebble.FullProfile,
			}}
			var in, acks bytes.Buffer
			w := req.Witness
			switch name {
			case "skip manifest":
				w.Stage = store_pebble.StageFileGraph
			case "wrong input digest":
				w.Input = store_pebble.HeadDigest{2}
			case "invalid frame":
				in.Write([]byte{0xff, 0xff, 0xff, 0xff})
			}
			if in.Len() == 0 {
				require.NoError(t, writeRebuildBuildReply(&in, rebuildBuildReply{Witness: w}))
			}
			calls := 0
			err := receiveRebuildStages(t.Context(), lease, req, &in, &acks, func(context.Context, store_pebble.StageTables) error {
				calls++
				return nil
			})
			require.ErrorIs(t, err, expected)
			require.Zero(t, calls)
			require.Zero(t, acks.Len())
		})
	}
}

func TestRebuildBuildLostAckKeepsIngestedStage(t *testing.T) {
	ctx := t.Context()
	req := rebuildBuildRequest{Incarnation: rebuildTestIncarnation, Witness: store_pebble.Witness{
		Input: store_pebble.HeadDigest{1}, Recipe: store_pebble.HeadDigest{2},
		Profile: store_pebble.FullProfile, Stage: store_pebble.StageManifest,
	}}
	headDir := filepath.Join(t.TempDir(), "head")
	leases := t.TempDir()
	file := store_pebble.ManifestFile{Path: "a.go", Language: "go", Kind: 'f', Size: 10,
		Content: store_pebble.HeadDigest{3}, Extractor: store_pebble.HeadDigest{4}}
	node := &graph.Node{ID: "a.go::F", Name: "F", Kind: graph.KindFunction, FilePath: file.Path, Language: "go"}
	var saved store_pebble.LocalPublicationRecord
	var committed store_pebble.Witness
	var committedDigest store_pebble.HeadDigest

	func() {
		h, err := store_pebble.OpenHead(headDir, req.Incarnation, req.Witness.Profile)
		require.NoError(t, err)
		defer func() { require.NoError(t, h.Close()) }()
		readAck, writeAck, err := os.Pipe()
		require.NoError(t, err)
		defer readAck.Close()
		defer writeAck.Close()

		err = withRebuildInputLease(leases, func(lease string) error {
			build, err := store_pebble.OpenBuild(filepath.Join(lease, "working"), req.Witness.Profile)
			require.NoError(t, err)
			defer func() { require.NoError(t, build.Close()) }()
			require.NoError(t, build.AddParsedFile("", store_pebble.ParseFacts{
				Nodes: []*graph.Node{node}, File: graph.FileMetaRow{FilePath: file.Path, ContentHash: "content", Size: 10, NodeCount: 1},
			}))
			manifestDir := filepath.Join(lease, store_pebble.StageManifest.String())
			fileGraphDir := filepath.Join(lease, store_pebble.StageFileGraph.String())
			require.NoError(t, os.Mkdir(manifestDir, 0o700))
			require.NoError(t, os.Mkdir(fileGraphDir, 0o700))
			manifest, err := store_pebble.PrepareManifest(ctx, manifestDir, []store_pebble.ManifestFile{file}, req.Witness, req.Incarnation)
			require.NoError(t, err)
			fileGraph, err := build.PrepareFileGraph(ctx, fileGraphDir, manifest.Witness, req.Incarnation)
			require.NoError(t, err)
			var offers bytes.Buffer
			for _, tables := range []store_pebble.StageTables{manifest, fileGraph} {
				require.NoError(t, writeRebuildBuildReply(&offers, rebuildBuildReply{Witness: tables.Witness, Base: tables.Base, Files: tables.Files}))
			}
			var pinned *store_pebble.Snapshot
			defer func() {
				if pinned != nil {
					require.NoError(t, pinned.Close())
				}
			}()
			return receiveRebuildStages(ctx, lease, req, &offers, writeAck, func(ctx context.Context, tables store_pebble.StageTables) error {
				digest, err := h.PublishStage(ctx, tables)
				if err != nil {
					return err
				}
				if tables.Witness.Stage == store_pebble.StageManifest {
					pinned = h.OpenSnapshot()
					event := uint64(5)
					saved = store_pebble.LocalPublicationRecord{Version: tables.Witness.Version(), Incarnation: req.Incarnation, WitnessDigest: digest, EventID: &event}
					return nil
				}
				committed, committedDigest = fileGraph.Witness, digest
				require.ErrorAs(t, pinned.RequireStage(store_pebble.StageFileGraph), new(*store_pebble.ErrStagePending))
				require.Nil(t, pinned.GetNode(node.ID))
				// The commit has landed. Only the next ACK write fails.
				return writeAck.Close()
			})
		})
		require.ErrorIs(t, err, ErrRebuildWorker)
		require.ErrorIs(t, err, os.ErrClosed)
		require.ErrorContains(t, err, "acknowledge file_graph")
		acks, err := io.ReadAll(readAck)
		require.NoError(t, err)
		require.Equal(t, []byte{rebuildStageAck}, acks)
	}()
	entries, err := os.ReadDir(leases)
	require.NoError(t, err)
	require.Empty(t, entries)

	h, err := store_pebble.OpenHead(headDir, req.Incarnation, req.Witness.Profile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, h.Close()) })
	w, recovered, err := h.ReconcilePublication(&saved)
	require.NoError(t, err)
	require.Equal(t, store_pebble.StageFileGraph, w.Stage)
	require.Equal(t, committed, w)
	require.Equal(t, store_pebble.LocalPublicationRecord{Version: saved.Version, Incarnation: saved.Incarnation, WitnessDigest: committedDigest}, recovered)
	latest := h.OpenSnapshot()
	t.Cleanup(func() { require.NoError(t, latest.Close()) })
	require.Equal(t, node, latest.GetNode(node.ID))
	require.ErrorAs(t, latest.RequireStage(store_pebble.StageRelations), new(*store_pebble.ErrStagePending))
	require.NoError(t, latest.ReadError())
}

func TestRebuildBuildReplyRequiresCoverage(t *testing.T) {
	for _, tc := range []struct {
		name     string
		stage    store_pebble.Stage
		coverage *store_pebble.RelationCoverage
		wantAcks int
		wantErr  error
	}{
		{"manifest claim", store_pebble.StageManifest, &store_pebble.RelationCoverage{}, 0, errRebuildBuildWire},
		{"file_graph claim", store_pebble.StageFileGraph, &store_pebble.RelationCoverage{}, 1, errRebuildBuildWire},
		{"relations without claim", store_pebble.StageRelations, nil, 2, errRebuildBuildWire},
		{"relations with empty claim", store_pebble.StageRelations, &store_pebble.RelationCoverage{}, 3, errRebuildBuildIncomplete},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lease := t.TempDir()
			w := store_pebble.Witness{
				Input: store_pebble.HeadDigest{1}, Recipe: store_pebble.HeadDigest{2},
				Profile: store_pebble.FullProfile, Stage: store_pebble.StageManifest,
			}
			req := rebuildBuildRequest{Witness: w}
			var replies, acks bytes.Buffer
			for stage := store_pebble.StageManifest; stage <= tc.stage; stage++ {
				require.NoError(t, os.Mkdir(filepath.Join(lease, stage.String()), 0o700))
				w.Stage = stage
				if stage == tc.stage {
					w.Relations = tc.coverage
				}
				require.NoError(t, writeRebuildBuildReply(&replies, rebuildBuildReply{Witness: w}))
			}
			var offered []store_pebble.Stage
			err := receiveRebuildStages(t.Context(), lease, req, &replies, &acks, func(_ context.Context, got store_pebble.StageTables) error {
				offered = append(offered, got.Witness.Stage)
				if got.Witness.Stage == store_pebble.StageRelations {
					require.Equal(t, tc.coverage, got.Witness.Relations)
				}
				return nil
			})
			require.ErrorIs(t, err, tc.wantErr)
			want := []store_pebble.Stage{store_pebble.StageManifest, store_pebble.StageFileGraph, store_pebble.StageRelations}
			require.Len(t, offered, tc.wantAcks)
			for i, stage := range offered {
				require.Equal(t, want[i], stage)
			}
			require.True(t, bytes.Equal(bytes.Repeat([]byte{rebuildStageAck}, tc.wantAcks), acks.Bytes()))
		})
	}
}

func TestRebuildBuildViewRefusal(t *testing.T) {
	for _, tc := range []struct {
		name        string
		cause, want error
		code        uint8
	}{
		{"view error alone", store_pebble.ErrRebuildView, store_pebble.ErrRebuildView, 21},
		{"denied", fmt.Errorf("%w: %w: %w", store_pebble.ErrRebuildView, errRebuildResolverNamespace, errRebuildResolverInputDenied), errRebuildResolverInputDenied, 29},
		{"unproved namespace", fmt.Errorf("%w: %w", store_pebble.ErrRebuildView, errRebuildResolverNamespace), errRebuildResolverNamespace, 30},
		{"external recording path", fmt.Errorf("%w: %w", store_pebble.ErrRebuildView, resolver.ErrRebuildGoExternalRecordingPath), resolver.ErrRebuildGoExternalRecordingPath, 35},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var reply, acks bytes.Buffer
			r := buildRefusal(tc.cause)
			require.Equal(t, tc.code, r.Refusal)
			require.NoError(t, writeRebuildBuildReply(&reply, r))
			err := receiveRebuildStages(t.Context(), t.TempDir(), rebuildBuildRequest{}, &reply, &acks,
				func(context.Context, store_pebble.StageTables) error {
					t.Fatal("a refusal must not publish")
					return nil
				})
			require.ErrorIs(t, err, ErrRebuildWorker)
			require.ErrorIs(t, err, tc.want)
			require.Empty(t, acks.Bytes())
		})
	}
}

func TestRebuildBuildReceiverRefusals(t *testing.T) {
	for _, tc := range []struct {
		cause  error
		code   uint8
		detail string
	}{
		{store_pebble.ErrReceiverMutationNonconvergent, 39, ""},
		{store_pebble.ErrReceiverMutationInput, 40, "receiver_matches=73: merge claims: pebble claims exceeds 1024 sorted runs"},
		{io.ErrUnexpectedEOF, 41, ""},
	} {
		t.Run(tc.cause.Error(), func(t *testing.T) {
			cause := tc.cause
			if tc.detail != "" {
				cause = fmt.Errorf("%w: %s", cause, tc.detail)
			}
			cause = fmt.Errorf("%w: %w", store_pebble.ErrRebuildReceiverMutation, cause)
			want := tc.cause
			if tc.code == 41 {
				want = store_pebble.ErrRebuildReceiverMutation
			}
			var reply, acks bytes.Buffer
			r := buildRefusal(cause)
			require.Equal(t, tc.code, r.Refusal)
			require.NoError(t, writeRebuildBuildReply(&reply, r))
			err := receiveRebuildStages(t.Context(), t.TempDir(), rebuildBuildRequest{}, &reply, &acks,
				func(context.Context, store_pebble.StageTables) error {
					t.Fatal("a receiver refusal must not publish")
					return nil
				})
			require.ErrorIs(t, err, ErrRebuildWorker)
			require.ErrorIs(t, err, want)
			require.ErrorContains(t, err, tc.cause.Error())
			if tc.detail != "" {
				require.ErrorContains(t, err, tc.detail)
			}
			require.Empty(t, acks.Bytes())
		})
	}
}

func TestRebuildBuildAttributionChainRefusal(t *testing.T) {
	old := graph.EdgeIdentity{From: "p.go::Caller", Kind: graph.EdgeReferences, To: "unresolved::Old", FilePath: "p.go", Line: 8}
	dst := old
	dst.To = "p.go::Caller#local:old::unresolved::New"
	cause := &store_pebble.RebuildAttributionChainError{Count: 2, Old: old, Destination: dst}
	valid := buildRefusal(fmt.Errorf("%w: %w", store_pebble.ErrRebuildView, cause))
	require.Equal(t, uint64(2), valid.AttributionChains)
	require.Equal(t, store_pebble.ErrRebuildAttributionChain, rebuildFailureKinds[valid.Refusal-1])
	missing := valid
	missing.AttributionChains = 0
	unrelated := buildRefusal(errRebuildBuildIncomplete)
	unrelated.AttributionChains = 2
	for _, tc := range []struct {
		name  string
		reply rebuildBuildReply
		want  error
	}{
		{"exact refused count", valid, store_pebble.ErrRebuildAttributionChain},
		{"missing count", missing, errRebuildBuildWire},
		{"count on unrelated refusal", unrelated, errRebuildBuildWire},
		{"count on stage offer", rebuildBuildReply{AttributionChains: 2}, errRebuildBuildWire},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var reply, acks bytes.Buffer
			require.NoError(t, writeRebuildBuildReply(&reply, tc.reply))
			err := receiveRebuildStages(t.Context(), t.TempDir(), rebuildBuildRequest{}, &reply, &acks,
				func(context.Context, store_pebble.StageTables) error {
					t.Fatal("chain refusal must not publish")
					return nil
				})
			require.ErrorIs(t, err, tc.want)
			require.Empty(t, acks.Bytes())
			if tc.want == store_pebble.ErrRebuildAttributionChain {
				require.ErrorContains(t, err, "attribution_chains=2")
				require.ErrorContains(t, err, old.To)
				require.ErrorContains(t, err, dst.To)
			}
		})
	}
}

func TestRebuildBuildUnknownProvenanceRefusal(t *testing.T) {
	cause := &store_pebble.RebuildUnknownProvenanceError{Count: 2}
	valid := buildRefusal(errors.Join(store_pebble.ErrRebuildView, cause))
	require.Equal(t, uint8(36), valid.Refusal)
	require.Equal(t, uint64(2), valid.UnknownProvenance)
	missing := valid
	missing.UnknownProvenance = 0
	unrelated := buildRefusal(errRebuildBuildIncomplete)
	unrelated.UnknownProvenance = 2
	for _, tc := range []struct {
		name  string
		reply rebuildBuildReply
		want  error
	}{
		{"exact refused count", valid, store_pebble.ErrRebuildUnknownProvenance},
		{"missing count", missing, errRebuildBuildWire},
		{"count on unrelated refusal", unrelated, errRebuildBuildWire},
		{"count on stage offer", rebuildBuildReply{UnknownProvenance: 2}, errRebuildBuildWire},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var reply, acks bytes.Buffer
			require.NoError(t, writeRebuildBuildReply(&reply, tc.reply))
			err := receiveRebuildStages(t.Context(), t.TempDir(), rebuildBuildRequest{}, &reply, &acks,
				func(context.Context, store_pebble.StageTables) error {
					t.Fatal("unknown provenance must not publish")
					return nil
				})
			require.ErrorIs(t, err, tc.want)
			require.Empty(t, acks.Bytes())
		})
	}
}

func TestRebuildBuildCppIncludeRefusal(t *testing.T) {
	var reply bytes.Buffer
	cause := errors.Join(store_pebble.ErrRebuildFileImports, resolver.ErrRebuildCppIncludeInput)
	require.NoError(t, writeRebuildBuildReply(&reply, buildRefusal(cause)))
	var acks bytes.Buffer
	err := receiveRebuildStages(t.Context(), t.TempDir(), rebuildBuildRequest{}, &reply, &acks,
		func(context.Context, store_pebble.StageTables) error {
			t.Fatal("an include refusal must not publish")
			return nil
		})
	require.ErrorIs(t, err, ErrRebuildWorker)
	require.ErrorIs(t, err, resolver.ErrRebuildCppIncludeInput)
	require.Empty(t, acks.Bytes())
}

func TestRunRebuildCheckout(t *testing.T) {
	for _, tc := range []struct {
		name                             string
		mismatch, denied, omitJSONRecipe bool
	}{
		{name: "verified checkout builds exact prefix"},
		{name: "supplied input root differs", mismatch: true},
		{name: "denied link input keeps file graph", denied: true},
		{name: "missing JSON recipe refuses before manifest", denied: true, omitJSONRecipe: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			root, src := rebuildInputFixture(t)
			const mod = "module example.org/widgets\n"
			require.NoError(t, os.WriteFile(filepath.Join(root, "go.mod"), []byte(mod), 0o600))
			require.NoError(t, os.WriteFile(filepath.Join(root, "sample.go"), []byte("package p\nfunc Frozen() {}\n"), 0o600))
			require.NoError(t, os.WriteFile(filepath.Join(root, "large.go"), []byte(strings.Repeat("x", 129)), 0o600))
			require.NoError(t, os.Mkdir(filepath.Join(root, "excluded"), 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(root, "excluded", "secret.go"), []byte("package hidden"), 0o600))
			if tc.denied {
				writeRebuildTSAliasFixtureFile(t, root, "caller.ts", `import "@lib/x"`)
				writeRebuildTSAliasFixtureFile(t, root, "tsconfig.json", `{"compilerOptions":{"paths":{"@lib/*":["./excluded/*"]}}}`)
				writeRebuildTSAliasFixtureFile(t, root, "excluded/x.ts", "export {}")
			}
			cfg := config.Default().Index
			cfg.MaxFileSize = 128
			cfg.Coverage.Clones.Threshold = 0.95 // the owner must seal this before dispatch
			req := rebuildBuildRequest{
				Root: root, ParseDir: t.TempDir(), ParseLimit: 1 << 20, Incarnation: rebuildTestIncarnation,
				Policy: freezeRebuildParsePolicy(root, cfg), Recipes: map[string]rebuildDigest{"go": {1}, "typescript": {2}, "json": {3}},
				Bounds:  rebuildTransformBounds{CaptureBytes: 1024, FileBytes: 64 << 10},
				Witness: store_pebble.Witness{Recipe: store_pebble.HeadDigest{2}, Profile: store_pebble.FullProfile, Stage: store_pebble.StageManifest},
			}
			if tc.omitJSONRecipe {
				delete(req.Recipes, "json")
			}
			reg := parser.NewRegistry()
			languages.RegisterAll(reg)
			idx := New(graph.New(), reg, cfg, zap.NewNop())
			t.Cleanup(idx.Close)
			exe, err := os.Executable()
			require.NoError(t, err)
			t.Setenv("GORTEX_TEST_REBUILD_BUILD_CHILD", "1")
			leases := t.TempDir()
			unowned := filepath.Join(leases, "unowned")
			require.NoError(t, os.WriteFile(unowned, []byte("keep"), 0o600))
			headDir := filepath.Join(t.TempDir(), "head")
			h, err := store_pebble.OpenHead(headDir, req.Incarnation, req.Witness.Profile)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, h.Close()) })
			if tc.mismatch {
				req.Witness.Input[0] = 1
				exe = filepath.Join(root, "not-a-binary")
			}
			var stages []store_pebble.Stage
			err = idx.runRebuildCheckout(ctx, src, req, excludes.New([]string{"excluded/"}), false,
				exe, []string{"-test.run=^TestRebuildBuildChildEntry$", "--"}, leases, headDir,
				func(ctx context.Context, offer store_pebble.StageTables) error {
					require.False(t, tc.mismatch, "a mismatched epoch must not reach the child")
					stages = append(stages, offer.Witness.Stage)
					if offer.Witness.Stage == store_pebble.StageManifest {
						ref := rebuildDigest(offer.Witness.FrozenInputs)
						require.NotZero(t, ref)
						require.NoError(t, streamRebuildInputSidecar(retainedRebuildInputPath(headDir, ref), io.Discard, ref))
					}
					_, err := h.PublishStage(ctx, offer)
					return err
				})
			entries, leaseErr := os.ReadDir(leases)
			require.NoError(t, leaseErr)
			require.Len(t, entries, 1)
			require.Equal(t, "unowned", entries[0].Name())
			s := h.OpenSnapshot()
			defer func() { require.NoError(t, s.Close()) }()
			if tc.mismatch {
				require.ErrorIs(t, err, ErrBuildInputChanged)
				require.Empty(t, stages)
				require.ErrorIs(t, s.RequireStage(store_pebble.StageManifest), store_pebble.ErrNotPublished)
				return
			}
			if tc.omitJSONRecipe {
				require.ErrorIs(t, err, ErrRebuildWorker)
				require.ErrorContains(t, err, "extractor recipe missing for json")
				require.Empty(t, stages)
				require.ErrorIs(t, s.RequireStage(store_pebble.StageManifest), store_pebble.ErrNotPublished)
				return
			}
			wantRefusal := errRebuildBuildIncomplete
			wantPaths := []string{"sample.go"}
			if tc.denied {
				wantRefusal = errRebuildResolverInputDenied
				wantPaths = []string{"caller.ts", "sample.go", "tsconfig.json"}
			}
			require.ErrorIs(t, err, wantRefusal)
			require.Equal(t, []store_pebble.Stage{store_pebble.StageManifest, store_pebble.StageFileGraph}, stages)
			var pending *store_pebble.ErrStagePending
			require.ErrorAs(t, s.RequireStage(store_pebble.StageRelations), &pending)
			var paths []string
			require.NoError(t, s.ManifestFiles(ctx, func(f store_pebble.ManifestFile) error {
				paths = append(paths, f.Path)
				return nil
			}))
			require.Equal(t, wantPaths, paths)
			require.NotNil(t, s.GetNode("sample.go::Frozen"))
			stub := s.GetNode("large.go")
			require.NotNil(t, stub)
			require.Equal(t, true, stub.Meta["skipped_due_to_size"])
			require.Nil(t, s.GetNode("excluded/secret.go"))
		})
	}
}
