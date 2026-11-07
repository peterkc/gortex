package store_sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

// The fault-point tests: one per declared point (FR-6). Each stops one
// operation at its point, checks that the operation stopped there, opens a new
// instance over the same file and checks that it holds the complete old or new
// state, then finishes the chain on that instance.
//
// The chain is the routed commit layer of one checkout: E0 is published and
// routed, E1 is a building generation over the same layer. The retire cases
// retire E0 after E1 has replaced it on the route.

// errInjected is what the hook returns at its armed point.
var errInjected = errors.New("injected stop")

// faultStop stops the operation that reaches point once it is armed. With
// skip set, the armed hook lets that many arrivals at point pass first.
type faultStop struct {
	point FaultPoint
	skip  atomic.Int32
	armed atomic.Bool
	stops atomic.Int32
	// observe, when set, sees every arrival at point.
	observe func()
}

func (f *faultStop) hook(point FaultPoint) error {
	if point == f.point && f.observe != nil {
		f.observe()
	}
	if point != f.point || !f.armed.Load() || f.skip.Add(-1) >= 0 {
		return nil
	}
	if !f.armed.CompareAndSwap(true, false) {
		return nil
	}
	f.stops.Add(1)
	return fmt.Errorf("%w at %s", errInjected, point)
}

// requireStopped checks that err is the injected stop and that it fired once.
func (f *faultStop) requireStopped(t *testing.T, op string, err error) {
	t.Helper()
	if !errors.Is(err, errInjected) || f.stops.Load() != 1 {
		t.Fatalf("%s returned %v after %d stops, want the injected stop at %s exactly once",
			op, err, f.stops.Load(), f.point)
	}
}

// faultChain is one store over one file with E0 routed and E1 building.
type faultChain struct {
	ctx      context.Context
	path     string
	store    *Store
	stop     *faultStop
	e0, e1   int64
	e1Handle *Store
}

const faultCheckout = payloadCheckoutID

var (
	faultNodeA   = &graph.Node{ID: "repo/a.go::A", Kind: graph.KindFunction, Name: "A", QualName: "pkg.A", FilePath: "repo/a.go", RepoPrefix: "repo", Language: "go"}
	faultNodeA1  = &graph.Node{ID: "repo/a.go::A", Kind: graph.KindFunction, Name: "A", QualName: "pkg.A(v1)", FilePath: "repo/a.go", RepoPrefix: "repo", Language: "go"}
	faultNodeB1  = &graph.Node{ID: "repo/b.go::B", Kind: graph.KindFunction, Name: "B", QualName: "pkg.B", FilePath: "repo/b.go", RepoPrefix: "repo", Language: "go"}
	faultEdgeBA1 = &graph.Edge{From: "repo/b.go::B", To: "repo/a.go::A", Kind: graph.EdgeCalls, FilePath: "repo/b.go", Line: 3}
)

func newFaultChain(t *testing.T, point FaultPoint) *faultChain {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	t.Cleanup(cancel)
	c := &faultChain{ctx: ctx, path: filepath.Join(t.TempDir(), "fault.sqlite"), stop: &faultStop{point: point}}
	store, err := Open(c.path, WithFaultHook(c.stop.hook))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	c.store = store
	seedPayloadControlPlane(t, store)

	var e0Handle *Store
	c.e0, e0Handle = c.begin(t, store, "tree-e0")
	if err := e0Handle.AddBatchCheckedContext(ctx, []*graph.Node{faultNodeA}, nil); err != nil {
		t.Fatalf("write E0: %v", err)
	}
	if err := store.PublishPayloadGeneration(ctx, c.e0, 1); err != nil {
		t.Fatalf("seal E0: %v", err)
	}
	c.flip(t, store, c.e0, 0)
	c.e1, c.e1Handle = c.begin(t, store, "tree-e1")
	return c
}

func (c *faultChain) begin(t *testing.T, s *Store, tree string) (int64, *Store) {
	t.Helper()
	req := payloadRequest()
	req.GenerationKind, req.TreeOID = "commit", tree
	id, handle, err := s.BeginPayloadGeneration(c.ctx, req)
	if err != nil {
		t.Fatalf("begin %s: %v", tree, err)
	}
	return id, handle
}

func (c *faultChain) flipRequest(id, epoch int64) FlipCheckoutRouteSlotRequest {
	return FlipCheckoutRouteSlotRequest{
		CheckoutID: faultCheckout, Slot: RouteSlotCommit, GenerationID: id,
		ExpectedRouteEpoch: epoch, State: RouteActive,
	}
}

func (c *faultChain) flip(t *testing.T, s *Store, id, epoch int64) {
	t.Helper()
	if err := s.Catalog().FlipCheckoutRouteSlot(c.ctx, c.flipRequest(id, epoch)); err != nil {
		t.Fatalf("route generation %d at epoch %d: %v", id, epoch, err)
	}
}

// writeE1 writes E1's batch through handle.
func (c *faultChain) writeE1(handle *Store) error {
	return handle.AddBatchCheckedContext(c.ctx, []*graph.Node{faultNodeA1, faultNodeB1}, []*graph.Edge{faultEdgeBA1})
}

// publishE1 seals E1 and routes it, the steps an interrupted E1 build still owes.
func (c *faultChain) publishE1(t *testing.T, s *Store) {
	t.Helper()
	if err := s.PublishPayloadGeneration(c.ctx, c.e1, 2); err != nil {
		t.Fatalf("seal E1: %v", err)
	}
	c.flip(t, s, c.e1, 1)
}

// reopen cleanly closes the stopped instance before opening its files. The
// chain never starts a bulk load, so Close does not seal or flush private
// work; the default policy leaves committed WAL for recovery. This is not
// a process-kill test.
func (c *faultChain) reopen(t *testing.T) *Store {
	t.Helper()
	if err := c.store.Close(); err != nil {
		t.Fatalf("close the stopped instance: %v", err)
	}
	s, err := Open(c.path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// requireRoute checks which generation the checkout serves and at which epoch.
func (c *faultChain) requireRoute(t *testing.T, s *Store, id, epoch int64) {
	t.Helper()
	route, found, err := s.Catalog().GetCheckoutRoute(c.ctx, faultCheckout)
	if err != nil || !found {
		t.Fatalf("read route: found=%v err=%v", found, err)
	}
	if route.CommitGenerationID != id || route.RouteEpoch != epoch {
		t.Fatalf("route serves generation %d at epoch %d, want %d at %d",
			route.CommitGenerationID, route.RouteEpoch, id, epoch)
	}
}

// requireState checks a generation's catalog state.
func (c *faultChain) requireState(t *testing.T, s *Store, id int64, want ViewGenerationState) {
	t.Helper()
	row, found, err := s.Catalog().GetViewGeneration(c.ctx, id)
	if err != nil || !found || row.State != want {
		t.Fatalf("generation %d: state %q found=%v err=%v, want %q", id, row.State, found, err, want)
	}
}

// requirePayload checks a generation's complete node and edge sets.
func requirePayload(t *testing.T, s *Store, id int64, nodes []*graph.Node, edges []*graph.Edge) {
	t.Helper()
	h := s.AtGeneration(id)
	got, want := renderAll(h.AllNodes()), renderAll(nodes)
	if !slices.Equal(got, want) {
		t.Fatalf("generation %d nodes:\n got  %v\n want %v", id, got, want)
	}
	if got, want := renderAll(h.AllEdges()), renderAll(edges); !slices.Equal(got, want) {
		t.Fatalf("generation %d edges:\n got  %v\n want %v", id, got, want)
	}
}

func renderAll[T any](values []*T) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		out = append(out, fmt.Sprintf("%#v", *v))
	}
	slices.Sort(out)
	return out
}

// The payload writer families the contract suite drives. Each family's write
// commits through commitPayloadTx, ownedWriteTx or execActiveWriteLocked;
// every other payload writer is held to those helpers by
// TestStorageFaultPayloadCommitsUseTheHelpers.
//
// A family names the generation its write changes. A stop before the commit
// leaves that generation's durable rows as they were, and the write then
// succeeds on a new instance; a stop after the commit leaves them changed.
type payloadFamily struct {
	name string
	// setup prepares the chain before the hook is armed and returns the
	// generation the write changes.
	setup func(t *testing.T, c *faultChain) int64
	// write is the interrupted operation, run on instance s.
	write func(c *faultChain, s *Store) error
	// skip lets that many payload commits of the write pass before the stop.
	skip int32
	// skipToChange stops instead at the first commit that changes the
	// generation's rows, found by a dry run of the same write.
	skipToChange bool
}

// stopSkip is how many payload commits of the family's write pass before the
// stop. A skipToChange family counts them on a dry run over its own chain.
func (f payloadFamily) stopSkip(t *testing.T) int32 {
	t.Helper()
	if !f.skipToChange {
		return f.skip
	}
	c := newFaultChain(t, FaultPayloadWriteAfterCommit)
	g := f.setup(t, c)
	before := payloadRows(t, c.store, g)
	commits, changedAt := int32(0), int32(-1)
	c.stop.observe = func() {
		if changedAt < 0 && payloadRows(t, c.store, g) != before {
			changedAt = commits
		}
		commits++
	}
	if err := f.write(c, c.store); err != nil {
		t.Fatalf("%s dry run: %v", f.name, err)
	}
	if changedAt < 0 {
		t.Fatalf("%s dry run: no payload commit changed generation %d", f.name, g)
	}
	return changedAt
}

// onE1 runs a write on a managed handle over the building E1.
func onE1(write func(c *faultChain, h *Store) error) func(c *faultChain, s *Store) error {
	return func(c *faultChain, s *Store) error {
		h, err := s.AtManagedGeneration(c.e1)
		if err != nil {
			return err
		}
		return write(c, h)
	}
}

func e1Setup(_ *testing.T, c *faultChain) int64 { return c.e1 }

// baseSetup writes a base-corpus batch, the source of the copies and the
// target of the purge.
func baseSetup(t *testing.T, c *faultChain) int64 {
	t.Helper()
	if err := c.store.AddBatchCheckedContext(c.ctx, []*graph.Node{faultNodeA, faultNodeB1}, []*graph.Edge{faultEdgeBA1}); err != nil {
		t.Fatalf("write the base corpus: %v", err)
	}
	return baseViewGeneration
}

func payloadFamilies() []payloadFamily {
	vector := []float32{1, 0, 0}
	return []payloadFamily{
		{name: "FileMetas", setup: e1Setup, write: onE1(func(_ *faultChain, h *Store) error {
			return h.SetFileMetas("repo", []graph.FileMetaRow{{FilePath: "repo/a.go", ContentHash: "hash-a", Size: 10, NodeCount: 1}})
		})},
		{name: "SymbolFTS", setup: e1Setup, write: onE1(func(_ *faultChain, h *Store) error {
			return h.BatchUpsertSymbolFTS([]graph.SymbolFTSItem{{NodeID: "repo/a.go::A", Tokens: "a"}})
		})},
		{name: "ContentFTS", setup: e1Setup, write: onE1(func(_ *faultChain, h *Store) error {
			return h.AppendContent("repo", []graph.ContentFTSItem{{NodeID: "repo/a.go::A", FilePath: "repo/a.go", Body: "func A() {}"}})
		})},
		{name: "VectorsBatch", setup: e1Setup, write: onE1(func(_ *faultChain, h *Store) error {
			return h.BulkUpsertEmbeddings([]graph.VectorItem{{NodeID: "repo/a.go::A", Vec: vector}})
		})},
		{name: "VectorManagedStatement", setup: e1Setup, write: onE1(func(_ *faultChain, h *Store) error {
			return h.UpsertEmbedding("repo/a.go::A", vector)
		})},
		{name: "VectorAutocommitStatement", setup: e1Setup, write: func(c *faultChain, s *Store) error {
			return s.AtGeneration(c.e1).UpsertEmbedding("repo/a.go::A", vector)
		}},
		{name: "FileMasks", setup: func(t *testing.T, c *faultChain) int64 {
			if err := c.writeE1(c.e1Handle); err != nil {
				t.Fatal(err)
			}
			return c.e1
		}, write: onE1(func(_ *faultChain, h *Store) error {
			return h.SetFileMasks([]FileMask{{RepoPrefix: "repo", FilePath: "repo/a.go", Mode: OwnershipReplace}})
		})},
		{name: "ProducerStates", setup: e1Setup, write: onE1(func(_ *faultChain, h *Store) error {
			return h.SetProducerState(ProducerCompleteness{Producer: "extractor", State: ProducerStateComplete})
		})},
		{name: "Copy", setup: func(t *testing.T, c *faultChain) int64 { baseSetup(t, c); return c.e1 }, write: func(c *faultChain, s *Store) error {
			_, err := s.CopyPayloadGeneration(c.ctx, baseViewGeneration, c.e1, "repo")
			return err
		}},
		{name: "InitialCopy", setup: func(t *testing.T, c *faultChain) int64 { baseSetup(t, c); return c.e1 }, write: func(c *faultChain, s *Store) error {
			_, err := s.CopyPayloadGenerationCooperatively(c.ctx, baseViewGeneration, c.e1, "repo")
			return err
		}},
		{name: "CopyWhole", setup: e1Setup, write: func(c *faultChain, s *Store) error {
			_, err := s.CopyGenerationPayloadWhole(c.ctx, c.e0, c.e1)
			return err
		}},
		{name: "Flatten", setup: e1Setup, write: func(c *faultChain, s *Store) error {
			_, err := s.FlattenGenerationChain(c.ctx, []int64{c.e0}, c.e1)
			return err
		}},
		{name: "ChainFold", setup: e1Setup, skip: 1, write: func(c *faultChain, s *Store) error {
			// A planner-statistics pass changes the schema version, which
			// makes a fold preparation give way; wait the lane out first.
			if err := s.waitMaintenanceIdle(c.ctx); err != nil {
				return err
			}
			fold, err := s.BeginChainFold(c.ctx, ChainFoldRequest{Chain: []int64{c.e0}, To: c.e1, Owner: "fault-test"})
			if err != nil {
				return err
			}
			for {
				done, err := fold.Step(c.ctx)
				if errors.Is(err, ErrChainFoldYielded) || errors.Is(err, ErrChainFoldWALMark) {
					continue
				}
				if err != nil {
					return err
				}
				if done {
					return fold.Release(c.ctx)
				}
			}
		}},
		{name: "RetireSweep", skipToChange: true, setup: func(t *testing.T, c *faultChain) int64 { c.routeE1(t); return c.e0 }, write: func(c *faultChain, s *Store) error {
			return s.RetirePayloadGeneration(c.ctx, c.e0, nil)
		}},
		{name: "Purge", setup: baseSetup, write: func(c *faultChain, s *Store) error {
			return s.PurgeRepoContext(c.ctx, "repo")
		}},
	}
}

// payloadRows counts a generation's rows in every generation-keyed table.
func payloadRows(t *testing.T, s *Store, generationID int64) int {
	t.Helper()
	total := 0
	for _, table := range payloadGenerationTables() {
		total += countAtGeneration(t, s, table, generationID)
	}
	return total
}

// completedPayloadRows is the uninterrupted reference for the same declared
// fixture operation. Comparing full rows (not just a changed count) catches a
// resumed multi-commit writer that stops after only its first durable step.
func completedPayloadRows(t *testing.T, family payloadFamily) []string {
	t.Helper()
	c := newFaultChain(t, FaultPayloadWriteAfterCommit)
	g := family.setup(t, c)
	if err := family.write(c, c.store); err != nil {
		t.Fatalf("%s uninterrupted reference: %v", family.name, err)
	}
	return payloadRowValues(t, c.store, g)
}

func payloadRowValues(t *testing.T, s *Store, generationID int64) []string {
	t.Helper()
	var out []string
	for _, table := range payloadGenerationTables() {
		rows, err := s.db.Query(`SELECT * FROM `+table+` WHERE view_gen = ?`, generationID)
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			rows.Close()
			t.Fatal(err)
		}
		for rows.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for i := range values {
				pointers[i] = &values[i]
			}
			if err := rows.Scan(pointers...); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			encoded, err := json.Marshal(values)
			if err != nil {
				rows.Close()
				t.Fatal(err)
			}
			out = append(out, table+":"+string(encoded))
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		rows.Close()
	}
	slices.Sort(out)
	return out
}

func TestStorageFaultPayloadWriteBeforeCommit(t *testing.T) {
	t.Run("AddBatch", testPayloadWriteBeforeCommitAddBatch)
	for _, family := range payloadFamilies() {
		t.Run(family.name, func(t *testing.T) {
			c := newFaultChain(t, FaultPayloadWriteBeforeCommit)
			g := family.setup(t, c)
			before := payloadRows(t, c.store, g)
			c.stop.skip.Store(family.stopSkip(t))
			c.stop.armed.Store(true)
			c.stop.requireStopped(t, family.name, family.write(c, c.store))

			s := c.reopen(t)
			if got := payloadRows(t, s, g); got != before {
				t.Fatalf("generation %d holds %d rows after the stopped write, want the %d it had", g, got, before)
			}
			if err := family.write(c, s); err != nil {
				t.Fatalf("%s on the new instance: %v", family.name, err)
			}
			if got, want := payloadRowValues(t, s, g), completedPayloadRows(t, family); !slices.Equal(got, want) {
				t.Fatalf("%s resumed rows:\n got %v\nwant %v", family.name, got, want)
			}
		})
	}
}

func TestStorageFaultPayloadWriteAfterCommit(t *testing.T) {
	t.Run("AddBatch", testPayloadWriteAfterCommitAddBatch)
	for _, family := range payloadFamilies() {
		t.Run(family.name, func(t *testing.T) {
			c := newFaultChain(t, FaultPayloadWriteAfterCommit)
			g := family.setup(t, c)
			before := payloadRows(t, c.store, g)
			c.stop.skip.Store(family.stopSkip(t))
			c.stop.armed.Store(true)
			c.stop.requireStopped(t, family.name, family.write(c, c.store))

			s := c.reopen(t)
			if got := payloadRows(t, s, g); got == before {
				t.Fatalf("generation %d holds its %d rows after the committed write; the commit did not last", g, got)
			}
			// Cleanup is resumable. Append and copy are not replay APIs:
			// their durable write must already be complete at this point.
			if family.name == "RetireSweep" || family.name == "Purge" {
				if err := family.write(c, s); err != nil {
					t.Fatalf("%s finish on the recovered owner: %v", family.name, err)
				}
			}
			if got, want := payloadRowValues(t, s, g), completedPayloadRows(t, family); !slices.Equal(got, want) {
				t.Fatalf("%s completed rows:\n got %v\nwant %v", family.name, got, want)
			}
			if family.name == "RetireSweep" {
				if _, found, err := s.Catalog().GetViewGeneration(c.ctx, g); err != nil || found {
					t.Fatalf("finished retirement left generation %d: found=%v, err=%v", g, found, err)
				}
			} else {
				c.publishE1(t, s)
				c.requireState(t, s, c.e1, ViewGenerationReady)
			}
			c.requireRoute(t, s, c.e1, 2)
		})
	}
}

// testPayloadWriteBeforeCommitAddBatch is the AddBatch family, checked to
// the end of the chain.
func testPayloadWriteBeforeCommitAddBatch(t *testing.T) {
	c := newFaultChain(t, FaultPayloadWriteBeforeCommit)
	c.stop.armed.Store(true)
	c.stop.requireStopped(t, "E1 payload write", c.writeE1(c.e1Handle))

	// The batch never committed: E0 is served and E1 is an empty build that
	// the next instance can write.
	s := c.reopen(t)
	c.requireRoute(t, s, c.e0, 1)
	c.requireState(t, s, c.e1, ViewGenerationBuilding)
	requirePayload(t, s, c.e1, nil, nil)
	handle, err := s.AtManagedGeneration(c.e1)
	if err != nil {
		t.Fatalf("managed handle on E1: %v", err)
	}
	if err := c.writeE1(handle); err != nil {
		t.Fatalf("rewrite E1 on the new instance: %v", err)
	}
	c.publishE1(t, s)
	c.requireRoute(t, s, c.e1, 2)
	requirePayload(t, s, c.e1, []*graph.Node{faultNodeA1, faultNodeB1}, []*graph.Edge{faultEdgeBA1})
}

// testPayloadWriteAfterCommitAddBatch is the AddBatch family, checked to the
// end of the chain.
func testPayloadWriteAfterCommitAddBatch(t *testing.T) {
	c := newFaultChain(t, FaultPayloadWriteAfterCommit)
	c.stop.armed.Store(true)
	c.stop.requireStopped(t, "E1 payload write", c.writeE1(c.e1Handle))

	// The batch is durable in the private build, and E0 is still served.
	s := c.reopen(t)
	c.requireRoute(t, s, c.e0, 1)
	c.requireState(t, s, c.e1, ViewGenerationBuilding)
	requirePayload(t, s, c.e1, []*graph.Node{faultNodeA1, faultNodeB1}, []*graph.Edge{faultEdgeBA1})

	// Repeating the begin with the same inputs rejoins the stopped attempt
	// instead of allocating a second one.
	req := payloadRequest()
	req.GenerationKind, req.TreeOID = "commit", "tree-e1"
	id, _, adopted, err := s.BeginPayloadGenerationWithStatus(c.ctx, req)
	if err != nil || id != c.e1 || !adopted {
		t.Fatalf("repeat begin = generation %d adopted=%v err=%v, want the stopped attempt %d", id, adopted, err, c.e1)
	}
	c.publishE1(t, s)
	c.requireRoute(t, s, c.e1, 2)
	requirePayload(t, s, c.e1, []*graph.Node{faultNodeA1, faultNodeB1}, []*graph.Edge{faultEdgeBA1})
}

func TestStorageFaultSealBeforeCommit(t *testing.T) {
	c := newFaultChain(t, FaultSealBeforeCommit)
	if err := c.writeE1(c.e1Handle); err != nil {
		t.Fatalf("write E1: %v", err)
	}
	c.stop.armed.Store(true)
	c.stop.requireStopped(t, "E1 seal", c.store.PublishPayloadGeneration(c.ctx, c.e1, 2))

	// The seal never committed, so the builder's own handle still writes.
	extra := &graph.Node{ID: "repo/c.go::C", Kind: graph.KindFunction, Name: "C", FilePath: "repo/c.go", RepoPrefix: "repo", Language: "go"}
	if err := c.e1Handle.AddBatchCheckedContext(c.ctx, []*graph.Node{extra}, nil); err != nil {
		t.Fatalf("write E1 after the stopped seal: %v", err)
	}

	s := c.reopen(t)
	c.requireRoute(t, s, c.e0, 1)
	c.requireState(t, s, c.e1, ViewGenerationBuilding)
	c.publishE1(t, s)
	c.requireRoute(t, s, c.e1, 2)
	requirePayload(t, s, c.e1, []*graph.Node{faultNodeA1, faultNodeB1, extra}, []*graph.Edge{faultEdgeBA1})
}

func TestStorageFaultSealAfterCommit(t *testing.T) {
	c := newFaultChain(t, FaultSealAfterCommit)
	if err := c.writeE1(c.e1Handle); err != nil {
		t.Fatalf("write E1: %v", err)
	}
	c.stop.armed.Store(true)
	c.stop.requireStopped(t, "E1 seal", c.store.PublishPayloadGeneration(c.ctx, c.e1, 2))

	late := []*graph.Node{{ID: "repo/late.go::Late", Kind: graph.KindFunction, Name: "Late", FilePath: "repo/late.go", RepoPrefix: "repo"}}
	if err := c.e1Handle.AddBatchCheckedContext(c.ctx, late, nil); !errors.Is(err, ErrPayloadGenerationSealed) {
		t.Fatalf("write through the builder's handle after the seal committed = %v, want %v", err, ErrPayloadGenerationSealed)
	}

	// The seal is durable: E1 is ready but unrouted, refuses writes on the new
	// instance too, and can still be routed.
	s := c.reopen(t)
	c.requireRoute(t, s, c.e0, 1)
	c.requireState(t, s, c.e1, ViewGenerationReady)
	if err := s.AtGeneration(c.e1).AddBatchCheckedContext(c.ctx, late, nil); !errors.Is(err, ErrPayloadGenerationSealed) {
		t.Fatalf("write into the sealed E1 on the new instance = %v, want %v", err, ErrPayloadGenerationSealed)
	}
	c.flip(t, s, c.e1, 1)
	c.requireRoute(t, s, c.e1, 2)
	requirePayload(t, s, c.e1, []*graph.Node{faultNodeA1, faultNodeB1}, []*graph.Edge{faultEdgeBA1})
}

func TestStorageFaultPublishBeforeCommit(t *testing.T) {
	c := newFaultChain(t, FaultPublishBeforeCommit)
	if err := c.writeE1(c.e1Handle); err != nil {
		t.Fatalf("write E1: %v", err)
	}
	if err := c.store.PublishPayloadGeneration(c.ctx, c.e1, 2); err != nil {
		t.Fatalf("seal E1: %v", err)
	}
	c.stop.armed.Store(true)
	c.stop.requireStopped(t, "E1 route flip", c.store.Catalog().FlipCheckoutRouteSlot(c.ctx, c.flipRequest(c.e1, 1)))

	// The flip rolled back: E0 at its old epoch, and the same guarded flip
	// succeeds on the new instance.
	s := c.reopen(t)
	c.requireRoute(t, s, c.e0, 1)
	c.requireState(t, s, c.e1, ViewGenerationReady)
	c.flip(t, s, c.e1, 1)
	c.requireRoute(t, s, c.e1, 2)
}

func TestStorageFaultPublishAfterCommit(t *testing.T) {
	c := newFaultChain(t, FaultPublishAfterCommit)
	if err := c.writeE1(c.e1Handle); err != nil {
		t.Fatalf("write E1: %v", err)
	}
	if err := c.store.PublishPayloadGeneration(c.ctx, c.e1, 2); err != nil {
		t.Fatalf("seal E1: %v", err)
	}
	c.stop.armed.Store(true)
	c.stop.requireStopped(t, "E1 route flip", c.store.Catalog().FlipCheckoutRouteSlot(c.ctx, c.flipRequest(c.e1, 1)))

	// The flip is durable although its caller saw an error. Repeating it with
	// the guard the caller still holds refuses, so E1 is published once.
	s := c.reopen(t)
	c.requireRoute(t, s, c.e1, 2)
	if err := s.Catalog().FlipCheckoutRouteSlot(c.ctx, c.flipRequest(c.e1, 1)); !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("repeated flip with the old guard = %v, want %v", err, ErrCatalogStaleGuard)
	}
	c.requireRoute(t, s, c.e1, 2)
	requirePayload(t, s, c.e1, []*graph.Node{faultNodeA1, faultNodeB1}, []*graph.Edge{faultEdgeBA1})
}

// TestStorageFaultPublishOtherSites covers the route installation, full-route
// flip and ref-view adoption, in addition to the slot flip above.
func TestStorageFaultPublishOtherSites(t *testing.T) {
	for _, point := range []FaultPoint{FaultPublishBeforeCommit, FaultPublishAfterCommit} {
		t.Run(string(point), func(t *testing.T) {
			for _, refView := range []bool{false, true} {
				name := "FullRoute"
				if refView {
					name = "RefViewAdoption"
				}
				t.Run(name, func(t *testing.T) { testPublishOtherSite(t, point, refView) })
			}
			t.Run("DirtyClearWithCommit", func(t *testing.T) { testPublishDirtyClear(t, point) })
			t.Run("FullRouteInstall", func(t *testing.T) {
				for _, slot := range []RouteSlot{RouteSlotCommit, RouteSlotDirty} {
					t.Run(string(slot), func(t *testing.T) { testPublishRouteInstall(t, point, slot) })
				}
			})
		})
	}
}

func testPublishOtherSite(t *testing.T, point FaultPoint, refView bool) {
	c := newFaultChain(t, point)
	if err := c.writeE1(c.e1Handle); err != nil {
		t.Fatalf("write E1: %v", err)
	}
	if err := c.store.PublishPayloadGeneration(c.ctx, c.e1, 2); err != nil {
		t.Fatalf("seal E1: %v", err)
	}
	const viewID = "fault-ref"
	if refView {
		if err := c.store.Catalog().UpsertRefView(c.ctx, RefView{
			RefViewID: viewID, GraphID: payloadGraphID, SelectorKind: "git_ref", SelectorValue: "refs/heads/main",
			DesiredTree: "tree-e1", DesiredBuildFingerprint: "fingerprint-e1",
			ActiveGenerationID: c.e0, ActiveTree: "tree-e0", State: RefViewReady, ExactView: true, EnrichmentProfile: "structural",
		}); err != nil {
			t.Fatalf("seed the ref view: %v", err)
		}
	}
	publish := func(s *Store) error {
		if refView {
			return s.Catalog().AdoptRefViewGeneration(c.ctx, AdoptRefViewGenerationRequest{
				RefViewID: viewID, ExpectedRouteEpoch: 0, ExpectedDesiredTree: "tree-e1",
				ExpectedDesiredBuildFingerprint: "fingerprint-e1", GenerationID: c.e1,
				ActiveTree: "tree-e1", ActiveBuildFingerprint: "fingerprint-e1", ExactView: true,
			})
		}
		return s.Catalog().FlipCheckoutRoute(c.ctx, FlipCheckoutRouteRequest{
			CheckoutID: faultCheckout, ExpectedRouteEpoch: 1, GraphID: payloadGraphID,
			CommitGenerationID: c.e1, State: RouteActive,
		})
	}
	c.stop.armed.Store(true)
	c.stop.requireStopped(t, "publish", publish(c.store))
	s := c.reopen(t)
	want, epoch := c.e0, int64(1)
	if point == FaultPublishAfterCommit {
		want, epoch = c.e1, 2
	}
	if refView {
		view, found, err := s.Catalog().GetRefView(c.ctx, viewID)
		if err != nil || !found || view.ActiveGenerationID != want || view.RouteEpoch != epoch-1 {
			t.Fatalf("recovered ref view = %+v, found %v, error %v; want generation %d at epoch %d", view, found, err, want, epoch-1)
		}
	} else {
		c.requireRoute(t, s, want, epoch)
	}
	requirePayload(t, s, c.e0, []*graph.Node{faultNodeA}, nil)
	requirePayload(t, s, c.e1, []*graph.Node{faultNodeA1, faultNodeB1}, []*graph.Edge{faultEdgeBA1})
	if point == FaultPublishBeforeCommit {
		if err := publish(s); err != nil {
			t.Fatalf("publish on the recovered instance: %v", err)
		}
	}
	if err := publish(s); !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("repeat the published operation with the old guard = %v, want %v", err, ErrCatalogStaleGuard)
	}
	if refView {
		view, found, err := s.Catalog().GetRefView(c.ctx, viewID)
		if err != nil || !found || view.ActiveGenerationID != c.e1 || view.RouteEpoch != 1 {
			t.Fatalf("final ref view = %+v, found %v, error %v; want E1 once at epoch 1", view, found, err)
		}
	} else {
		c.requireRoute(t, s, c.e1, 2)
	}
}

func testPublishRouteInstall(t *testing.T, point FaultPoint, slot RouteSlot) {
	c := newFaultChain(t, point)
	if err := c.writeE1(c.e1Handle); err != nil {
		t.Fatalf("write E1: %v", err)
	}
	if err := c.store.PublishPayloadGeneration(c.ctx, c.e1, 2); err != nil {
		t.Fatalf("seal E1: %v", err)
	}
	if err := c.store.Catalog().DeleteCheckoutRoute(c.ctx, faultCheckout); err != nil {
		t.Fatalf("remove the seed route: %v", err)
	}
	route := CheckoutRoute{CheckoutID: faultCheckout, GraphID: payloadGraphID, RouteEpoch: 7, State: RouteActive}
	if slot == RouteSlotCommit {
		route.CommitGenerationID = c.e1
	} else {
		route.DirtyGenerationID = c.e1
	}
	c.stop.armed.Store(true)
	c.stop.requireStopped(t, "install the first route", c.store.Catalog().UpsertCheckoutRoute(c.ctx, route))
	s := c.reopen(t)
	got, found, err := s.Catalog().GetCheckoutRoute(c.ctx, faultCheckout)
	if err != nil {
		t.Fatalf("read the recovered route: %v", err)
	}
	if point == FaultPublishBeforeCommit {
		if found {
			t.Fatalf("the stopped installation left a route: %+v", got)
		}
		if err := s.Catalog().UpsertCheckoutRoute(c.ctx, route); err != nil {
			t.Fatalf("install on the recovered instance: %v", err)
		}
	} else if !found || got != route {
		t.Fatalf("recovered route = %+v, found %v; want %+v", got, found, route)
	}
	if err := s.Catalog().UpsertCheckoutRoute(c.ctx, route); err != nil {
		t.Fatalf("repeat the installation: %v", err)
	}
	if got, found, err := s.Catalog().GetCheckoutRoute(c.ctx, faultCheckout); err != nil || !found || got != route {
		t.Fatalf("final route = %+v, found %v, error %v; want %+v", got, found, err, route)
	}
	requirePayload(t, s, c.e0, []*graph.Node{faultNodeA}, nil)
	requirePayload(t, s, c.e1, []*graph.Node{faultNodeA1, faultNodeB1}, []*graph.Edge{faultEdgeBA1})
}

func testPublishDirtyClear(t *testing.T, point FaultPoint) {
	c := newFaultChain(t, point)
	if err := c.writeE1(c.e1Handle); err != nil {
		t.Fatalf("write E1: %v", err)
	}
	if err := c.store.PublishPayloadGeneration(c.ctx, c.e1, 2); err != nil {
		t.Fatalf("seal E1: %v", err)
	}
	dirty := c.flipRequest(c.e1, 1)
	dirty.Slot = RouteSlotDirty
	if err := c.store.Catalog().FlipCheckoutRouteSlot(c.ctx, dirty); err != nil {
		t.Fatalf("seed the dirty route: %v", err)
	}
	clear := c.flipRequest(0, 2)
	clear.Slot = RouteSlotDirty
	c.stop.armed.Store(true)
	c.stop.requireStopped(t, "clear dirty to serve commit", c.store.Catalog().FlipCheckoutRouteSlot(c.ctx, clear))
	s := c.reopen(t)
	want := CheckoutRoute{CheckoutID: faultCheckout, GraphID: payloadGraphID, CommitGenerationID: c.e0, RouteEpoch: 3, State: RouteActive}
	if point == FaultPublishBeforeCommit {
		want.DirtyGenerationID, want.RouteEpoch = c.e1, 2
	}
	if got, found, err := s.Catalog().GetCheckoutRoute(c.ctx, faultCheckout); err != nil || !found || got != want {
		t.Fatalf("recovered route = %+v, found %v, error %v; want %+v", got, found, err, want)
	}
	if point == FaultPublishBeforeCommit {
		if err := s.Catalog().FlipCheckoutRouteSlot(c.ctx, clear); err != nil {
			t.Fatalf("clear dirty on the recovered instance: %v", err)
		}
	}
	if err := s.Catalog().FlipCheckoutRouteSlot(c.ctx, clear); !errors.Is(err, ErrCatalogStaleGuard) {
		t.Fatalf("repeat clear with the old guard = %v, want %v", err, ErrCatalogStaleGuard)
	}
	want.DirtyGenerationID, want.RouteEpoch = 0, 3
	if got, found, err := s.Catalog().GetCheckoutRoute(c.ctx, faultCheckout); err != nil || !found || got != want {
		t.Fatalf("final route = %+v, found %v, error %v; want %+v", got, found, err, want)
	}
	requirePayload(t, s, c.e0, []*graph.Node{faultNodeA}, nil)
	requirePayload(t, s, c.e1, []*graph.Node{faultNodeA1, faultNodeB1}, []*graph.Edge{faultEdgeBA1})
}

func TestStorageFaultUnchangedServedGenerationDoesNotPublish(t *testing.T) {
	for _, point := range []FaultPoint{FaultPublishBeforeCommit, FaultPublishAfterCommit} {
		t.Run(string(point), func(t *testing.T) {
			for _, site := range []string{"SameCommit", "SameDirty"} {
				t.Run(site, func(t *testing.T) {
					c := newFaultChain(t, point)
					req := c.flipRequest(c.e0, 1)
					want := CheckoutRoute{CheckoutID: faultCheckout, GraphID: payloadGraphID, CommitGenerationID: c.e0, RouteEpoch: 2, State: RouteActive}
					if site != "SameCommit" {
						if err := c.writeE1(c.e1Handle); err != nil {
							t.Fatalf("write E1: %v", err)
						}
						if err := c.store.PublishPayloadGeneration(c.ctx, c.e1, 2); err != nil {
							t.Fatalf("seal E1: %v", err)
						}
						dirty := c.flipRequest(c.e1, 1)
						dirty.Slot = RouteSlotDirty
						if err := c.store.Catalog().FlipCheckoutRouteSlot(c.ctx, dirty); err != nil {
							t.Fatalf("seed dirty: %v", err)
						}
						req.ExpectedRouteEpoch = 2
						want.DirtyGenerationID, want.RouteEpoch = c.e1, 3
					}
					if site == "SameDirty" {
						req.Slot, req.GenerationID = RouteSlotDirty, c.e1
					}
					c.stop.armed.Store(true)
					if err := c.store.Catalog().FlipCheckoutRouteSlot(c.ctx, req); err != nil || c.stop.stops.Load() != 0 {
						t.Fatalf("an unchanged served generation returned %v after %d publish stops", err, c.stop.stops.Load())
					}
					s := c.reopen(t)
					if got, found, err := s.Catalog().GetCheckoutRoute(c.ctx, faultCheckout); err != nil || !found || got != want {
						t.Fatalf("recovered route = %+v, found %v, error %v; want %+v", got, found, err, want)
					}
				})
			}
		})
	}
}

// TestStorageFaultCommitSlotUnderDirtyPublishes changes the commit slot of a
// route whose dirty slot names a generation. The checkout serves the stack of
// both slots, so the change publishes: a stop before commit recovers the old
// route and a stop after commit recovers the new one.
func TestStorageFaultCommitSlotUnderDirtyPublishes(t *testing.T) {
	for _, point := range []FaultPoint{FaultPublishBeforeCommit, FaultPublishAfterCommit} {
		t.Run(string(point), func(t *testing.T) {
			for _, site := range []string{"CommitUnderDirty", "ClearCommitUnderDirty"} {
				t.Run(site, func(t *testing.T) {
					c := newFaultChain(t, point)
					if err := c.writeE1(c.e1Handle); err != nil {
						t.Fatalf("write E1: %v", err)
					}
					if err := c.store.PublishPayloadGeneration(c.ctx, c.e1, 2); err != nil {
						t.Fatalf("seal E1: %v", err)
					}
					dirty := c.flipRequest(c.e1, 1)
					dirty.Slot = RouteSlotDirty
					if err := c.store.Catalog().FlipCheckoutRouteSlot(c.ctx, dirty); err != nil {
						t.Fatalf("seed dirty: %v", err)
					}
					req := c.flipRequest(c.e1, 2)
					if site == "ClearCommitUnderDirty" {
						req.GenerationID = 0
					}
					want := CheckoutRoute{CheckoutID: faultCheckout, GraphID: payloadGraphID, CommitGenerationID: c.e0, DirtyGenerationID: c.e1, RouteEpoch: 2, State: RouteActive}
					if point == FaultPublishAfterCommit {
						want.CommitGenerationID, want.RouteEpoch = req.GenerationID, 3
					}
					c.stop.armed.Store(true)
					c.stop.requireStopped(t, "flip the commit slot under a dirty slot", c.store.Catalog().FlipCheckoutRouteSlot(c.ctx, req))
					s := c.reopen(t)
					if got, found, err := s.Catalog().GetCheckoutRoute(c.ctx, faultCheckout); err != nil || !found || got != want {
						t.Fatalf("recovered route = %+v, found %v, error %v; want %+v", got, found, err, want)
					}
				})
			}
		})
	}
}

func TestStorageFaultRouteWithoutGenerationDoesNotPublish(t *testing.T) {
	for _, point := range []FaultPoint{FaultPublishBeforeCommit, FaultPublishAfterCommit} {
		t.Run(string(point), func(t *testing.T) {
			for _, site := range []string{"Install", "FullRoute", "SlotClear"} {
				t.Run(site, func(t *testing.T) {
					c := newFaultChain(t, point)
					if site == "Install" {
						if err := c.store.Catalog().DeleteCheckoutRoute(c.ctx, faultCheckout); err != nil {
							t.Fatalf("remove the seed route: %v", err)
						}
					}
					c.stop.armed.Store(true)
					var err error
					switch site {
					case "Install":
						err = c.store.Catalog().UpsertCheckoutRoute(c.ctx, CheckoutRoute{
							CheckoutID: faultCheckout, GraphID: payloadGraphID, RouteEpoch: 2, State: RoutePending,
						})
					case "FullRoute":
						err = c.store.Catalog().FlipCheckoutRoute(c.ctx, FlipCheckoutRouteRequest{
							CheckoutID: faultCheckout, GraphID: payloadGraphID, ExpectedRouteEpoch: 1, State: RoutePending,
						})
					case "SlotClear":
						req := c.flipRequest(0, 1)
						req.State = RoutePending
						err = c.store.Catalog().FlipCheckoutRouteSlot(c.ctx, req)
					}
					if err != nil || c.stop.stops.Load() != 0 {
						t.Fatalf("a route change naming no generation returned %v after %d publish stops", err, c.stop.stops.Load())
					}
					s := c.reopen(t)
					want := CheckoutRoute{CheckoutID: faultCheckout, GraphID: payloadGraphID, RouteEpoch: 2, State: RoutePending}
					if got, found, err := s.Catalog().GetCheckoutRoute(c.ctx, faultCheckout); err != nil || !found || got != want {
						t.Fatalf("recovered route = %+v, found %v, error %v; want %+v", got, found, err, want)
					}
				})
			}
		})
	}
}

// routeE1 publishes E1 over E0 on the stopped instance, leaving E0 unrouted
// and ready to retire.
func (c *faultChain) routeE1(t *testing.T) {
	t.Helper()
	if err := c.writeE1(c.e1Handle); err != nil {
		t.Fatalf("write E1: %v", err)
	}
	c.publishE1(t, c.store)
}

func TestStorageFaultCatalogBeforeCommit(t *testing.T) {
	c := newFaultChain(t, FaultCatalogBeforeCommit)
	c.routeE1(t)
	c.stop.armed.Store(true)
	c.stop.requireStopped(t, "E0 retirement", c.store.RetirePayloadGeneration(c.ctx, c.e0, nil))

	// The retirement fence rolled back: E0 is intact and not fenced, and the
	// new instance retires it completely.
	s := c.reopen(t)
	c.requireRoute(t, s, c.e1, 2)
	c.requireState(t, s, c.e0, ViewGenerationReady)
	requirePayload(t, s, c.e0, []*graph.Node{faultNodeA}, nil)
	if err := s.RetirePayloadGeneration(c.ctx, c.e0, nil); err != nil {
		t.Fatalf("retire E0 on the new instance: %v", err)
	}
	requireRetired(t, c, s, c.e0)
}

func TestStorageFaultCatalogAfterCommit(t *testing.T) {
	c := newFaultChain(t, FaultCatalogAfterCommit)
	c.routeE1(t)
	c.stop.armed.Store(true)
	c.stop.requireStopped(t, "E0 retirement", c.store.RetirePayloadGeneration(c.ctx, c.e0, nil))

	// The fence is durable: a new reference to E0 refuses on the new
	// instance, and the retirement resumes from the fence to the end.
	s := c.reopen(t)
	c.requireState(t, s, c.e0, ViewGenerationRetiring)
	if err := s.Catalog().FlipCheckoutRouteSlot(c.ctx, c.flipRequest(c.e0, 2)); !errors.Is(err, ErrCatalogGenerationRetiring) {
		t.Fatalf("route the fenced E0 = %v, want %v", err, ErrCatalogGenerationRetiring)
	}
	c.requireRoute(t, s, c.e1, 2)
	if err := s.RetirePayloadGeneration(c.ctx, c.e0, nil); err != nil {
		t.Fatalf("resume E0's retirement on the new instance: %v", err)
	}
	requireRetired(t, c, s, c.e0)
}

// The single-statement catalog writes: Catalog.exec, behind every
// execGuarded, deleteOne and upsert, and the witnessed seal. A witnessed seal
// first writes the rollup, so its seal statement is the second catalog
// statement after the hook is armed.

func TestStorageFaultCatalogStatementBeforeCommit(t *testing.T) {
	t.Run("Supersede", func(t *testing.T) {
		c := newFaultChain(t, FaultCatalogBeforeCommit)
		c.routeE1(t)
		c.stop.armed.Store(true)
		c.stop.requireStopped(t, "supersede E0",
			c.store.Catalog().SetViewGenerationState(c.ctx, c.e0, ViewGenerationSuperseded, ViewGenerationReady))

		s := c.reopen(t)
		c.requireState(t, s, c.e0, ViewGenerationReady)
		if err := s.Catalog().SetViewGenerationState(c.ctx, c.e0, ViewGenerationSuperseded, ViewGenerationReady); err != nil {
			t.Fatalf("supersede E0 on the new instance: %v", err)
		}
		c.requireState(t, s, c.e0, ViewGenerationSuperseded)
	})
	t.Run("WitnessedSeal", func(t *testing.T) {
		c := newFaultChain(t, FaultCatalogBeforeCommit)
		witness := c.writeE1Witnessed(t)
		c.stop.skip.Store(1)
		c.stop.armed.Store(true)
		c.stop.requireStopped(t, "witnessed E1 seal", c.store.PublishPayloadGenerationWithInputWitness(c.ctx, c.e1, 2, witness))

		s := c.reopen(t)
		c.requireState(t, s, c.e1, ViewGenerationBuilding)
		c.requireRoute(t, s, c.e0, 1)
		c.publishE1(t, s)
		c.requireRoute(t, s, c.e1, 2)
	})
}

func TestStorageFaultCatalogStatementAfterCommit(t *testing.T) {
	t.Run("Supersede", func(t *testing.T) {
		c := newFaultChain(t, FaultCatalogAfterCommit)
		c.routeE1(t)
		c.stop.armed.Store(true)
		c.stop.requireStopped(t, "supersede E0",
			c.store.Catalog().SetViewGenerationState(c.ctx, c.e0, ViewGenerationSuperseded, ViewGenerationReady))

		// The transition is durable; repeating it with its old guard refuses.
		s := c.reopen(t)
		c.requireState(t, s, c.e0, ViewGenerationSuperseded)
		err := s.Catalog().SetViewGenerationState(c.ctx, c.e0, ViewGenerationSuperseded, ViewGenerationReady)
		if !errors.Is(err, ErrCatalogStaleGuard) {
			t.Fatalf("repeat the supersede with the ready guard = %v, want %v", err, ErrCatalogStaleGuard)
		}
	})
	t.Run("WitnessedSeal", func(t *testing.T) {
		c := newFaultChain(t, FaultCatalogAfterCommit)
		witness := c.writeE1Witnessed(t)
		c.stop.skip.Store(1)
		c.stop.armed.Store(true)
		c.stop.requireStopped(t, "witnessed E1 seal", c.store.PublishPayloadGenerationWithInputWitness(c.ctx, c.e1, 2, witness))

		// The seal is durable: E1 is ready, unrouted and closed to writes.
		s := c.reopen(t)
		c.requireState(t, s, c.e1, ViewGenerationReady)
		c.requireRoute(t, s, c.e0, 1)
		late := []*graph.Node{{ID: "repo/late.go::Late", Kind: graph.KindFunction, Name: "Late", FilePath: "repo/late.go", RepoPrefix: "repo"}}
		if err := s.AtGeneration(c.e1).AddBatchCheckedContext(c.ctx, late, nil); !errors.Is(err, ErrPayloadGenerationSealed) {
			t.Fatalf("write into the sealed E1 on the new instance = %v, want %v", err, ErrPayloadGenerationSealed)
		}
	})
}

// writeE1Witnessed writes E1 and captures the base-corpus input witness its
// seal checks.
func (c *faultChain) writeE1Witnessed(t *testing.T) *PayloadInputWitness {
	t.Helper()
	if err := c.writeE1(c.e1Handle); err != nil {
		t.Fatalf("write E1: %v", err)
	}
	witness, err := c.store.CapturePayloadInputWitness(c.ctx, []int64{baseViewGeneration})
	if err != nil {
		t.Fatalf("capture the input witness: %v", err)
	}
	return witness
}

// requireRetired checks that a generation's catalog row and payload are gone.
func requireRetired(t *testing.T, c *faultChain, s *Store, id int64) {
	t.Helper()
	if _, found, err := s.Catalog().GetViewGeneration(c.ctx, id); err != nil || found {
		t.Fatalf("generation %d after retirement: found=%v err=%v", id, found, err)
	}
	for _, table := range payloadGenerationTables() {
		if n := countAtGeneration(t, s, table, id); n != 0 {
			t.Fatalf("%s still holds %d rows of retired generation %d", table, n, id)
		}
	}
}

// TestStorageFaultOpenLeavesHookNil shows that Open without the option opens
// the instance with no fault hook. storetest's
// TestStorageFaultDefaultOpenPathsSetNoHook shows that no production caller
// passes the option.
func TestStorageFaultOpenLeavesHookNil(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "default.sqlite"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if s.faultHook != nil {
		t.Fatal("Open without WithFaultHook installed a fault hook")
	}
	for _, point := range FaultPoints() {
		if err := s.fault(point); err != nil {
			t.Fatalf("fault point %s on a store opened without a hook = %v, want nil", point, err)
		}
	}
}

// Given a cohort that seals payloads and binds contract attachments in one
// transaction, each stop recovers either both bindings and seals or neither.
func TestStorageFaultAttachmentSeal(t *testing.T) {
	for _, point := range []FaultPoint{
		FaultPayloadWriteBeforeCommit, FaultSealBeforeCommit, FaultCatalogBeforeCommit,
		FaultCatalogAfterCommit, FaultSealAfterCommit, FaultPayloadWriteAfterCommit,
	} {
		t.Run(string(point), func(t *testing.T) {
			c := newFaultChain(t, point)
			targets := []ContractAttachmentPublishTarget{attachmentBatchTarget(t, c.store, "one"), attachmentBatchTarget(t, c.store, "two")}
			assertAttachmentBatchDistinct(t, targets)
			c.stop.armed.Store(true)
			result, err := c.store.PublishContractAttachmentsWithInputsContext(c.ctx, targets, 2)
			c.stop.requireStopped(t, "attachment cohort seal", err)
			if result != nil {
				t.Fatalf("stopped publication returned results: %+v", result)
			}
			s := c.reopen(t)
			before := point == FaultPayloadWriteBeforeCommit || point == FaultSealBeforeCommit || point == FaultCatalogBeforeCommit
			for i := range targets {
				targets[i].Store = s.AtGeneration(targets[i].Store.viewGen)
			}
			if before {
				assertAttachmentBatchUnpublished(t, s, targets)
			} else {
				requireAttachmentCohort(t, c, s, targets)
			}
			// Retry on the recovered owner. A lost reply returns the recorded
			// binding, and a rolled-back transaction now commits completely.
			result, err = s.PublishContractAttachmentsWithInputsContext(c.ctx, targets, 3)
			if err != nil || len(result) != len(targets) {
				t.Fatalf("finish the recovered cohort: %+v, %v", result, err)
			}
			for i, target := range targets {
				if result[i].PayloadGeneration != target.Attachment.PayloadGeneration || result[i].UnusedPayloadGeneration != 0 {
					t.Fatalf("recovered result %d = %+v, want payload %d and no loser", i, result[i], target.Attachment.PayloadGeneration)
				}
			}
			requireAttachmentCohort(t, c, s, targets)
			c.requireRoute(t, s, c.e0, 1)
		})
	}
}

func requireAttachmentCohort(t *testing.T, c *faultChain, s *Store, targets []ContractAttachmentPublishTarget) {
	t.Helper()
	for _, target := range targets {
		got, err := s.GetContractAttachmentContext(c.ctx, attachmentKey(target.Expected))
		if err != nil || got == nil || got.PayloadGeneration != target.Attachment.PayloadGeneration || !slices.Equal(got.CompletedTokens, target.Attachment.CompletedTokens) {
			t.Fatalf("recovered attachment = %+v, %v; want %+v", got, err, target.Attachment)
		}
		c.requireState(t, s, target.Attachment.PayloadGeneration, ViewGenerationReady)
	}
}
