package store_pebble

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cockroachdb/pebble/v2"
	"github.com/cockroachdb/pebble/v2/vfs"
	"github.com/stretchr/testify/require"
	"github.com/zeebo/blake3"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
)

const headCrashPhaseEnv = "GORTEX_TEST_HEAD_INGEST_CRASH_PHASE"
const headCrashDecl = "a.go::OnlyInFileGraph"
const headCrashAfterSelectedIngest = "after-selected-ingest"

// Given a committed manifest and a sealed file graph, kill the head owner
// before the second version edit reaches the MANIFEST, or immediately after
// PublishStage returns. Restart with the same stale local record in both cases.
func TestHeadPublicationCrash(t *testing.T) {
	for _, phase := range []string{"during", "after"} {
		t.Run(phase, func(t *testing.T) {
			lease := t.TempDir()
			headDir := filepath.Join(lease, "head")
			old, next := prepareHeadCrashFixture(t, headDir, lease)
			digest := func(w Witness) HeadDigest {
				raw, err := encodeWitness(w)
				require.NoError(t, err)
				return blake3.Sum256(raw)
			}
			event := uint64(5)
			saved := LocalPublicationRecord{Version: old.Version(), Incarnation: next.Incarnation, WitnessDigest: digest(old), EventID: &event}
			killHeadCrashChild(t, lease, phase)

			h, err := OpenHead(headDir, next.Incarnation, old.Profile)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, h.Close()) })
			w, record, err := h.ReconcilePublication(&saved)
			require.NoError(t, err)
			view := stageRead(t, h)
			manifest, err := view.ManifestFile("a.go")
			require.NoError(t, err)
			require.Equal(t, HeadDigest{3}, manifest.Content)
			if phase == "during" {
				require.Equal(t, old, w)
				require.Equal(t, saved, record)
				require.Nil(t, view.GetNode(headCrashDecl))
				require.ErrorAs(t, view.RequireStage(StageFileGraph), new(*ErrStagePending))
			} else {
				require.Equal(t, next.Witness, w)
				require.Equal(t, LocalPublicationRecord{Version: saved.Version, Incarnation: saved.Incarnation, WitnessDigest: digest(next.Witness)}, record)
				require.NoError(t, view.RequireStage(StageFileGraph))
				n := view.GetNode(headCrashDecl)
				require.NotNil(t, n)
				require.Equal(t, headCrashDecl, n.ID)
				meta, err := view.FileMetadata("", "a.go")
				require.NoError(t, err)
				require.Equal(t, 1, meta.NodeCount)
			}
			require.NoError(t, view.ReadError())
		})
	}
}

// Candidate ingest and route selection are separate durable commits. A kill
// at either boundary must leave an entire catalog row and both heads readable.
func TestRouteCatalogCrash(t *testing.T) {
	for _, phase := range []string{"before-switch", "after-switch"} {
		t.Run(phase, func(t *testing.T) {
			lease := t.TempDir()
			want, stage := prepareRouteCrashFixture(t, lease)
			old, next, oldRecord := want.Serving, want.Candidate, want.Publication
			raw, err := encodeWitness(stage.Witness)
			require.NoError(t, err)
			event := uint64(100) + uint64(stage.Witness.Stage)
			nextRecord := LocalPublicationRecord{
				Version: stage.Witness.Version(), Incarnation: stage.Incarnation,
				WitnessDigest: blake3.Sum256(raw), EventID: &event,
			}
			require.Equal(t, oldRecord.Version, nextRecord.Version)
			require.NotEqual(t, oldRecord.WitnessDigest, nextRecord.WitnessDigest)

			killHeadCrashChild(t, lease, phase)

			if phase == "after-switch" {
				want.Serving, want.Candidate, want.Publication = next, routeLocator{}, nextRecord
			}
			catalog, err := openRouteCatalog(filepath.Join(lease, "catalog"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, catalog.Close()) })
			got, err := catalog.lookup(lease)
			require.NoError(t, err)
			require.Equal(t, want, got)

			oldHead, err := OpenHead(old.Directory, got.Incarnation, FullProfile)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, oldHead.Close()) })
			oldWitness, record, err := oldHead.ReconcilePublication(&oldRecord)
			require.NoError(t, err)
			require.Equal(t, StageManifest, oldWitness.Stage)
			require.Equal(t, oldRecord, record)
			oldView := stageRead(t, oldHead)
			require.Nil(t, oldView.GetNode(headCrashDecl))
			require.NoError(t, oldView.ReadError())

			// Even before selection, the candidate's ingest survived loss of its owner.
			nextHead, err := OpenHead(next.Directory, got.Incarnation, FullProfile)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, nextHead.Close()) })
			live, record, err := nextHead.ReconcilePublication(&nextRecord)
			require.NoError(t, err)
			require.Equal(t, stage.Witness, live)
			require.Equal(t, nextRecord, record)
			nextView := stageRead(t, nextHead)
			require.NotNil(t, nextView.GetNode(headCrashDecl))
			require.NoError(t, nextView.ReadError())
		})
	}
}

func TestRouteCatalogRecoversSelectedStage(t *testing.T) {
	lease := t.TempDir()
	want, stage := prepareRouteCrashFixture(t, lease)
	require.NotNil(t, want.Publication.EventID)
	killHeadCrashChild(t, lease, headCrashAfterSelectedIngest)

	catalog, err := openRouteCatalog(filepath.Join(lease, "catalog"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, catalog.Close()) })
	row, err := catalog.lookup(lease)
	require.NoError(t, err)
	require.Equal(t, want, row) // The selected locator and its old record did not change.

	head, err := OpenHead(row.Serving.Directory, row.Incarnation, stage.Witness.Profile)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, head.Close()) })
	witness, record, err := head.ReconcilePublication(&row.Publication)
	require.NoError(t, err)
	require.Equal(t, stage.Witness, witness)
	raw, err := encodeWitness(stage.Witness)
	require.NoError(t, err)
	require.Equal(t, LocalPublicationRecord{
		Version: row.Publication.Version, Incarnation: row.Incarnation, WitnessDigest: blake3.Sum256(raw),
	}, record, "recovery must drop the old watcher mark")
	view := stageRead(t, head)
	require.NoError(t, view.RequireStage(StageFileGraph))
	require.NotNil(t, view.GetNode(headCrashDecl))
	require.NoError(t, view.ReadError())
}

func prepareRouteCrashFixture(t *testing.T, lease string) (routeCheckout, StageTables) {
	t.Helper()
	old := routeLocator{Lease: routeTestOldLease, Directory: filepath.Join(lease, "old")}
	next := routeLocator{Lease: routeTestNewLease, Directory: filepath.Join(lease, "next")}
	_, stage := prepareHeadCrashFixture(t, old.Directory, lease)
	inc := stage.Incarnation
	h, err := OpenHead(old.Directory, inc, FullProfile)
	require.NoError(t, err)
	defer func() { require.NoError(t, h.Close()) }()
	_, record, err := h.ReconcilePublication(nil)
	require.NoError(t, err)
	event := uint64(5)
	record.EventID = &event
	require.NoError(t, h.Checkpoint(next.Directory))

	ctx := t.Context()
	catalog, err := openRouteCatalog(filepath.Join(lease, "catalog"))
	require.NoError(t, err)
	defer func() { require.NoError(t, catalog.Close()) }()
	var unrouted routeLocator
	require.NoError(t, catalog.initCheckout(ctx, lease, inc))
	require.NoError(t, catalog.registerCandidate(ctx, lease, inc, unrouted, old))
	require.NoError(t, catalog.switchHead(ctx, lease, inc, unrouted, old, nil, h, record))
	require.NoError(t, catalog.registerCandidate(ctx, lease, inc, old, next))
	return routeCheckout{Incarnation: inc, Serving: old, Candidate: next, Publication: record}, stage
}

func prepareHeadCrashFixture(t *testing.T, headDir, lease string) (Witness, StageTables) {
	t.Helper()
	ctx := t.Context()
	const incarnation = testHeadIncarnation
	manifest, err := PrepareManifest(ctx, t.TempDir(), []ManifestFile{{
		Path: "a.go", Language: "go", Kind: 'f', Size: 64, Content: HeadDigest{3}, Extractor: HeadDigest{4},
	}}, Witness{Input: HeadDigest{1}, Recipe: HeadDigest{2}, Stage: StageManifest, Profile: FullProfile}, incarnation)
	require.NoError(t, err)
	h, err := OpenHead(headDir, incarnation, FullProfile)
	require.NoError(t, err)
	base, publishErr := h.PublishStage(ctx, manifest)
	require.NoError(t, h.Close())
	require.NoError(t, publishErr)

	b, err := OpenBuild(filepath.Join(t.TempDir(), "work"), FullProfile)
	require.NoError(t, err)
	defer func() { require.NoError(t, b.Close()) }()
	require.NoError(t, b.AddParsedFile("", ParseFacts{
		Nodes: []*graph.Node{{ID: headCrashDecl, Name: "OnlyInFileGraph", Kind: graph.KindFunction, FilePath: "a.go", Language: "go"}},
		File:  graph.FileMetaRow{FilePath: "a.go", ContentHash: "digest", Size: 64, NodeCount: 1},
	}))
	next, err := b.PrepareFileGraph(ctx, t.TempDir(), manifest.Witness, incarnation)
	require.NoError(t, err)
	require.NotEmpty(t, next.Files)
	require.Equal(t, base, next.Base)
	request, err := json.Marshal(next)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(lease, "stage.json"), request, 0o600))
	return manifest.Witness, next
}

func killHeadCrashChild(t *testing.T, lease, phase string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	exe, err := os.Executable()
	require.NoError(t, err)
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestHeadPublicationCrashChild$", "--", lease)
	cmd.Env = append(os.Environ(), headCrashPhaseEnv+"="+phase)
	cmd.Stderr = os.Stderr
	out, err := cmd.StdoutPipe()
	require.NoError(t, err)
	in, err := cmd.StdinPipe()
	require.NoError(t, err)
	defer in.Close()
	require.NoError(t, cmd.Start())
	reaped := false
	defer func() {
		if !reaped {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	line, err := bufio.NewReader(out).ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, phase+"\n", line, "child must reach the exact crash point")
	require.NoError(t, ctx.Err())
	require.NoError(t, cmd.Process.Kill())
	err = cmd.Wait()
	reaped = true
	var exit *exec.ExitError
	require.ErrorAs(t, err, &exit)
	require.False(t, exit.Success())
}

func TestHeadPublicationCrashChild(t *testing.T) {
	phase := os.Getenv(headCrashPhaseEnv)
	lease := os.Args[len(os.Args)-1]
	switch phase {
	case "during", "after", "before-switch", "after-switch", headCrashAfterSelectedIngest:
	default:
		t.Skip("isolated crash fixture child")
	}
	request, err := os.ReadFile(filepath.Join(lease, "stage.json"))
	require.NoError(t, err)
	var stage StageTables
	require.NoError(t, json.Unmarshal(request, &stage))
	switch phase {
	case "before-switch", "after-switch", headCrashAfterSelectedIngest:
		crashRouteCatalogChild(t, lease, phase, stage)
		return
	}
	options := headOptions()
	fs := &headCrashFS{FS: options.FS, beforeManifestWrite: func() { waitForHeadCrash(t, "during") }}
	options.FS = fs
	db, err := pebble.Open(filepath.Join(lease, "head"), options)
	require.NoError(t, err)
	h := &Head{db: db, options: options, incarnation: stage.Incarnation, profile: stage.Witness.Profile}
	defer func() { require.NoError(t, h.Close()) }()
	fs.armed.Store(phase == "during")
	_, err = h.PublishStage(t.Context(), stage)
	require.NoError(t, err)
	if phase == "after" {
		waitForHeadCrash(t, "after")
	}
	t.Fatal("ingest returned without reaching the armed MANIFEST write")
}

func crashRouteCatalogChild(t *testing.T, lease, phase string, stage StageTables) {
	t.Helper()
	ctx := t.Context()
	catalog, err := openRouteCatalog(filepath.Join(lease, "catalog"))
	require.NoError(t, err)
	defer func() { require.NoError(t, catalog.Close()) }()
	row, err := catalog.lookup(lease)
	require.NoError(t, err)
	old, err := OpenHead(row.Serving.Directory, row.Incarnation, FullProfile)
	require.NoError(t, err)
	defer func() { require.NoError(t, old.Close()) }()
	if phase == headCrashAfterSelectedIngest {
		publishRouteTestStage(t, old, stage)
		waitForHeadCrash(t, phase)
		return
	}
	next, err := OpenHead(row.Candidate.Directory, row.Incarnation, FullProfile)
	require.NoError(t, err)
	defer func() { require.NoError(t, next.Close()) }()
	publication := publishRouteTestStage(t, next, stage)
	if phase == "before-switch" {
		waitForHeadCrash(t, phase)
		return
	}
	require.NoError(t, catalog.switchHead(ctx, lease, row.Incarnation, row.Serving, row.Candidate, old, next, publication))
	waitForHeadCrash(t, phase)
}

func waitForHeadCrash(t *testing.T, point string) {
	t.Helper()
	_, err := fmt.Fprintln(os.Stdout, point)
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, os.Stdin) // held open until the parent kills this child
	t.Fatal("owner pipe closed before the crash fixture killed the child")
}

// Only new MANIFEST writes in this isolated child are armed. Pebble's
// record.Writer flushes each version edit through this descriptor.
type headCrashFS struct {
	vfs.FS
	armed               atomic.Bool
	beforeManifestWrite func()
}

func (fs *headCrashFS) Create(name string, category vfs.DiskWriteCategory) (vfs.File, error) {
	f, err := fs.FS.Create(name, category)
	if err == nil && strings.HasPrefix(filepath.Base(name), "MANIFEST-") {
		return &headCrashFile{File: f, fs: fs}, nil
	}
	return f, err
}

type headCrashFile struct {
	vfs.File
	fs *headCrashFS
}

func (f *headCrashFile) Write(p []byte) (int, error) {
	if f.fs.armed.Swap(false) {
		f.fs.beforeManifestWrite()
	}
	return f.File.Write(p)
}

// Kill the same head owner at both atomic ingest boundaries, this time
// with a graph delta and a split contract shape in the same offer.
func TestHeadContractsPublicationCrash(t *testing.T) {
	for _, phase := range []string{"during", "after"} {
		t.Run(phase, func(t *testing.T) {
			ctx := t.Context()
			lease := t.TempDir()
			headDir := filepath.Join(lease, "head")
			h, err := OpenHead(headDir, testHeadIncarnation, FullProfile)
			require.NoError(t, err)
			w := Witness{Input: HeadDigest{1}, Recipe: HeadDigest{2}, Stage: StageManifest, Profile: FullProfile}
			manifest, err := PrepareManifest(ctx, t.TempDir(), []ManifestFile{{Path: "a.go", Language: "go", Kind: 'f'}}, w, testHeadIncarnation)
			require.NoError(t, err)
			_, err = h.PublishStage(ctx, manifest)
			require.NoError(t, err)
			b, err := OpenBuild(filepath.Join(t.TempDir(), "build"), FullProfile)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, b.Close()) })
			n := &graph.Node{ID: "Type", Name: "Type", Kind: graph.KindType, FilePath: "a.go", Language: "go"}
			require.NoError(t, b.AddParsedFile("", ParseFacts{
				Nodes: []*graph.Node{n}, File: graph.FileMetaRow{FilePath: "a.go", NodeCount: 1},
			}))
			fg, err := b.PrepareFileGraph(ctx, t.TempDir(), manifest.Witness, manifest.Incarnation)
			require.NoError(t, err)
			files, err := b.RetainFileGraph(ctx, fg)
			require.NoError(t, err)
			_, err = h.PublishStage(ctx, fg)
			require.NoError(t, err)
			relations, err := b.PrepareRelations(ctx, t.TempDir(), files, nil)
			require.NoError(t, err)
			base, err := b.RetainRelations(ctx, files, relations)
			require.NoError(t, err)
			_, err = h.PublishStage(ctx, relations)
			require.NoError(t, err)
			_, old, err := h.ReconcilePublication(nil)
			require.NoError(t, err)
			contract := testContractRow()
			shape := &contracts.Shape{Kind: "struct"}
			edge := &graph.Edge{From: n.ID, To: contract.ID, Kind: graph.EdgeProvides, FilePath: "a.go", Line: 1}
			stage, err := b.PrepareContracts(ctx, t.TempDir(), base, contractBatches(RebuildContractBatch{
				Nodes:  []*graph.Node{contract},
				Edges:  []*graph.Edge{edge},
				Shapes: map[string]*contracts.Shape{n.ID: shape},
			}), ContractCoverage{})
			require.NoError(t, err)
			require.NoError(t, h.Close())
			request, err := json.Marshal(stage)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(lease, "stage.json"), request, 0o600))
			killHeadCrashChild(t, lease, phase)
			h, err = OpenHead(headDir, testHeadIncarnation, FullProfile)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, h.Close()) })
			w, _, err = h.ReconcilePublication(&old)
			require.NoError(t, err)
			view := stageRead(t, h)
			gotShape, err := point(view, contractShapeKey(n.ID), decodeContractShape)
			require.NoError(t, err)
			require.Equal(t, n, view.GetNode(n.ID))
			if phase == "during" {
				require.Equal(t, relations.Witness, w)
				require.Nil(t, view.GetNode(contract.ID))
				require.Empty(t, view.GetInEdges(contract.ID))
				require.Nil(t, gotShape)
				require.ErrorAs(t, view.RequireStage(StageContracts), new(*ErrStagePending))
			} else {
				require.Equal(t, stage.Witness, w)
				require.Equal(t, contract, view.GetNode(contract.ID))
				require.Equal(t, []*graph.Edge{edge}, view.GetInEdges(contract.ID))
				require.Equal(t, shape, gotShape)
				require.NoError(t, view.RequireStage(StageContracts))
			}
			require.Equal(t, w.Families, relationsSavedWitness(t, view))
			require.NoError(t, view.ReadError())
		})
	}
}
