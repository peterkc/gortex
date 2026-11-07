package serverstack

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graph/storetest"
	"github.com/zzet/gortex/internal/reconcile"
	"github.com/zzet/gortex/internal/search"
)

// TestStorageContract runs the K12 open-path cases of the storage contract
// suite (design-c.md 1.8) for NewSharedServer on every backend in the suite's
// backend list. Each case builds the stack through the real constructor with
// the backend selected by name, so the stack opens the store itself.
func TestStorageContract(t *testing.T) {
	for _, backend := range storetest.Backends() {
		t.Run("K12/SharedServerUsesOneLeaseDomain/"+backend, func(t *testing.T) {
			testSharedServerUsesOneLeaseDomain(t, backend)
		})
		t.Run("K12/RetireWaitsForTheMaterializedView/"+backend, func(t *testing.T) {
			testRetireWaitsForTheMaterializedView(t, backend)
		})
		t.Run("K12/OpenedStoreIsTheGraphStore/"+backend, func(t *testing.T) {
			testOpenedStoreIsTheGraphStore(t, backend)
		})
	}
}

// k12CaseDeadline bounds one case, so a wedged stack fails the case instead
// of hanging until the package timeout.
const k12CaseDeadline = 2 * time.Minute

// contractStore lists the store operations the K12 cases drive on the store
// the stack opened: the catalog and payload-generation lifecycle a routed view
// is read from, and the retirement the lifecycle's cleanup runs.
type contractStore interface {
	graph.Store
	Catalog() *store_sqlite.Catalog
	BeginPayloadGeneration(ctx context.Context, req store_sqlite.PayloadGenerationRequest) (int64, *store_sqlite.Store, error)
	PublishPayloadGeneration(ctx context.Context, generationID, publishedAt int64) error
	RetirePayloadGeneration(ctx context.Context, generationID int64, inUse func(int64) bool) error
}

// newContractStack builds the stack through NewSharedServer on backend, with
// the per-user directories in temp paths so nothing of the developer's is read
// or written.
func newContractStack(t *testing.T, backend string, f *storetest.FixtureF) *SharedServer {
	t.Helper()
	base := t.TempDir()
	home := filepath.Join(base, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatalf("mkdir home: %v", err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, "data"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	conf := config.Default()
	conf.Semantic.Enabled = false

	stack, err := NewSharedServer(SharedServerConfig{
		Lifecycle:         LifecycleOneshot,
		Index:             f.R.Root,
		Backend:           backend,
		BackendPath:       filepath.Join(base, "store"),
		Config:            conf,
		Logger:            zap.NewNop(),
		SavingsPath:       filepath.Join(base, "savings.sqlite"),
		SavingsLegacyJSON: filepath.Join(base, "savings.json"),
	})
	if err != nil {
		t.Fatalf("NewSharedServer(%s): %v", backend, err)
	}
	t.Cleanup(func() { _ = stack.Close() })
	return stack
}

func k12Context(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), k12CaseDeadline)
	t.Cleanup(cancel)
	return ctx
}

// testSharedServerUsesOneLeaseDomain: the Materializer, the checkout
// lifecycle, the dedicated-base cleanup runtime and the output-generation
// authority all hold the lifecycle's one lease manager.
func testSharedServerUsesOneLeaseDomain(t *testing.T, backend string) {
	stack := newContractStack(t, backend, storetest.NewFixtureF(t))
	if stack.CheckoutLifecycle == nil {
		t.Fatal("the stack built no checkout lifecycle")
	}
	leases := stack.CheckoutLifecycle.ViewLeases()
	if leases == nil {
		t.Fatal("the checkout lifecycle has no lease manager")
	}
	materializer := stack.MCP.Materializer()
	if materializer == nil {
		t.Fatal("the stack installed no Materializer on the MCP server")
	}
	if materializer.Leases != leases {
		t.Error("the Materializer leases through a manager the checkout lifecycle does not retire through")
	}
	if got := stack.DedicatedBaseRuntime.ViewLeases(); got != leases {
		t.Error("the dedicated-base cleanup runtime holds a lease manager the checkout lifecycle does not share")
	}
	if got := stack.OutputGenerationAuthority.ViewLeases(); got != leases {
		t.Error("the output-generation authority holds a lease manager the checkout lifecycle does not share")
	}
}

// testRetireWaitsForTheMaterializedView: a view the stack's Materializer
// opened serves fixture F's R at E0, and a retire through the predicate the
// lifecycle's cleanup uses is refused while the view is open and allowed once
// it closes.
func testRetireWaitsForTheMaterializedView(t *testing.T, backend string) {
	f := storetest.NewFixtureF(t)
	stack := newContractStack(t, backend, f)
	ctx := k12Context(t)
	store := openedContractStore(t, stack)
	generation := routeFixtureRAtE0(ctx, t, store, f)

	view, err := stack.MCP.Materializer().MaterializeCheckout(ctx, storetest.CheckoutR2)
	if err != nil {
		t.Fatalf("materialize %s: %v", storetest.CheckoutR2, err)
	}
	assertServesPayload(t, view.Reader, f.Payload("r", storetest.E0))
	// A route still naming the generation also refuses retirement, so the
	// route goes first: what is under test is the open view alone.
	if err := store.Catalog().DeleteCheckoutRoute(ctx, storetest.CheckoutR2); err != nil {
		t.Fatalf("delete the route of %s: %v", storetest.CheckoutR2, err)
	}

	inUse := stack.CheckoutLifecycle.ViewLeases().InUse
	err = store.RetirePayloadGeneration(ctx, generation, inUse)
	if !errors.Is(err, store_sqlite.ErrPayloadGenerationInUse) {
		t.Fatalf("retire while the view is open = %v, want %v", err, store_sqlite.ErrPayloadGenerationInUse)
	}
	view.Close()
	if err := store.RetirePayloadGeneration(ctx, generation, inUse); err != nil {
		t.Fatalf("retire after the view closed: %v", err)
	}
}

// testOpenedStoreIsTheGraphStore: the value the stack publishes as
// graph.Store is the store it opened, the one its Materializer reads, and the
// FTS rank adapter accepts it as a ranker source.
func testOpenedStoreIsTheGraphStore(t *testing.T, backend string) {
	f := storetest.NewFixtureF(t)
	stack := newContractStack(t, backend, f)
	ctx := k12Context(t)
	store := openedContractStore(t, stack)
	generation := routeFixtureRAtE0(ctx, t, store, f)

	var materialized graph.Store = stack.MCP.Materializer().Store
	if materialized != stack.Graph {
		t.Error("the stack's graph.Store is not the store its Materializer reads")
	}
	warmed, err := search.WarmFTSGenerations(ctx, stack.Graph, []int64{generation})
	if err != nil {
		t.Fatalf("warm the FTS ranker over the stack's graph.Store: %v", err)
	}
	if warmed != 1 {
		t.Fatalf("the FTS rank adapter read %d generations of the stack's graph.Store, want 1", warmed)
	}
}

// openedContractStore returns the store the stack opened, through the
// operations the K12 cases drive. A backend without them fails the case.
func openedContractStore(t *testing.T, stack *SharedServer) contractStore {
	t.Helper()
	store, ok := stack.Graph.(contractStore)
	if !ok {
		t.Fatalf("the stack's graph.Store %T lacks the catalog and payload-generation operations", stack.Graph)
	}
	return store
}

// routeFixtureRAtE0 records R's family, both checkouts and R's dedicated graph
// from F's git repositories, publishes F's R at E0 as a commit generation and
// routes checkout R2 to it. It returns the generation.
func routeFixtureRAtE0(ctx context.Context, t *testing.T, store contractStore, f *storetest.FixtureF) int64 {
	t.Helper()
	const (
		familyID = "family-r"
		graphID  = "graph-r"
	)
	catalog := store.Catalog()
	if err := catalog.UpsertRepositoryFamily(ctx, store_sqlite.RepositoryFamily{
		FamilyID: familyID, CommonDirIdentity: filepath.Join(f.R.Root, ".git"), State: reconcile.FamilyStateReady,
	}); err != nil {
		t.Fatalf("record R's family: %v", err)
	}
	gitDirs := map[string]string{
		storetest.CheckoutR1: filepath.Join(f.R.Root, ".git"),
		storetest.CheckoutR2: filepath.Join(f.R.Root, ".git", "worktrees", "r-feature"),
	}
	modes := map[string]store_sqlite.CheckoutMode{
		storetest.CheckoutR1: store_sqlite.CheckoutModeDedicated,
		storetest.CheckoutR2: store_sqlite.CheckoutModeAutomatic,
	}
	for _, checkout := range f.Checkouts {
		if checkout.Repository != "r" {
			continue
		}
		if err := catalog.UpsertCheckout(ctx, store_sqlite.Checkout{
			CheckoutID: checkout.ID, Incarnation: "incarnation-" + checkout.ID, FamilyID: familyID,
			RootPath: checkout.Root, GitDir: gitDirs[checkout.ID], AdminName: filepath.Base(checkout.Root),
			State: store_sqlite.CheckoutStateReady, DesiredMode: modes[checkout.ID], EffectiveMode: modes[checkout.ID],
		}); err != nil {
			t.Fatalf("record checkout %s: %v", checkout.ID, err)
		}
	}
	if err := catalog.UpsertDedicatedGraph(ctx, store_sqlite.DedicatedGraph{
		GraphID: graphID, OwnerCheckoutID: storetest.CheckoutR1, RepoPrefix: f.R.Prefix, FamilyID: familyID,
		IsPrimaryBase: true, State: reconcile.GraphStateReady,
	}); err != nil {
		t.Fatalf("record R's dedicated graph: %v", err)
	}

	payload := f.Payload("r", storetest.E0)
	generation, handle, err := store.BeginPayloadGeneration(ctx, store_sqlite.PayloadGenerationRequest{
		OwnerKind: "dedicated_graph", GraphID: graphID, LayerID: "commit-" + storetest.CheckoutR2,
		CheckoutID: storetest.CheckoutR2, GenerationKind: "commit",
		TreeOID: f.R.Tree[storetest.E0], ProvenanceCommitOID: f.R.Commit[storetest.E0], CreatedAt: 1,
	})
	if err != nil {
		t.Fatalf("begin R's E0 generation: %v", err)
	}
	if err := handle.AddBatchCheckedContext(ctx, payload.Nodes, payload.Edges); err != nil {
		t.Fatalf("write R's E0 graph: %v", err)
	}
	if err := handle.BatchUpsertSymbolFTS(payload.Symbols); err != nil {
		t.Fatalf("write R's E0 symbol corpus: %v", err)
	}
	// The generation owns each of its files outright, as a commit build does.
	var masks []store_sqlite.FileMask
	for _, file := range payload.Files {
		masks = append(masks, store_sqlite.FileMask{
			RepoPrefix: f.R.Prefix, FilePath: file.FilePath, Mode: store_sqlite.OwnershipReplace,
		})
	}
	if err := handle.SetFileMasks(masks); err != nil {
		t.Fatalf("write R's E0 file ownership: %v", err)
	}
	if err := store.PublishPayloadGeneration(ctx, generation, 2); err != nil {
		t.Fatalf("publish R's E0 generation: %v", err)
	}
	if err := catalog.UpsertCheckoutRoute(ctx, store_sqlite.CheckoutRoute{
		CheckoutID: storetest.CheckoutR2, GraphID: graphID, CommitGenerationID: generation, State: store_sqlite.RouteActive,
	}); err != nil {
		t.Fatalf("route %s: %v", storetest.CheckoutR2, err)
	}
	return generation
}

// assertServesPayload compares the complete multisets of every node and edge
// field, including multiplicity, and fails on extra served nodes or edges.
func assertServesPayload(t *testing.T, r graph.Reader, payload storetest.Payload) {
	t.Helper()
	if got, want := payloadMultiset(r.AllNodes()), payloadMultiset(payload.Nodes); !slices.Equal(got, want) {
		t.Errorf("served nodes:\n got %v\nwant %v", got, want)
	}
	if got, want := payloadMultiset(r.AllEdges()), payloadMultiset(payload.Edges); !slices.Equal(got, want) {
		t.Errorf("served edges:\n got %v\nwant %v", got, want)
	}
}

func payloadMultiset[T any](values []*T) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, fmt.Sprintf("%#v", *value))
	}
	slices.Sort(out)
	return out
}
