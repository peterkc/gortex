package storetest_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/graph/storetest"
	"github.com/zzet/gortex/internal/graphview"
	"github.com/zzet/gortex/internal/persistence"
)

// SQLite is the sqlite backend's adapter. It maps the suite's neutral
// operations onto the store's own lifecycle: a build is a managed payload
// generation, the seal is its publish to ready, and the publication is the
// checkout route's commit slot. An incremental build is a dirty-slot
// generation over its parent, read through graphview's Materializer, which
// composes it as production does. SQLite's pins are graphview leases, above
// the store; the adapter holds one lease manager per instance and passes its
// InUse to retirement, as the daemon's cleanup paths do. Authored records
// live in the persistence sidecar beside the store.
func SQLite() storetest.Backend {
	return storetest.Backend{Name: "sqlite", Open: openSQLite, TryOpen: trySQLite, Damage: damageSQLite, DamageSealedFile: damageSQLiteSealedFile}
}

const (
	sqliteStoreFile   = "store.sqlite"
	sqliteSidecarFile = "sidecar.sqlite"
)

func openSQLite(t *testing.T, dir string, hook storetest.FaultHook) storetest.Instance {
	t.Helper()
	inst, err := trySQLite(t, dir, hook)
	if err != nil {
		t.Fatalf("open the SQLite store in %s: %v", dir, err)
	}
	return inst
}

func trySQLite(t *testing.T, dir string, hook storetest.FaultHook) (storetest.Instance, error) {
	t.Helper()
	var opts []store_sqlite.Option
	if hook != nil {
		opts = append(opts, store_sqlite.WithFaultHook(func(point store_sqlite.FaultPoint) error {
			return hook(string(point))
		}))
	}
	store, err := store_sqlite.Open(filepath.Join(dir, sqliteStoreFile), opts...)
	if err != nil {
		return nil, err
	}
	leases := graphview.NewLeaseManager()
	inst := &contractInstance{
		store: store, leases: leases, sidecarPath: filepath.Join(dir, sqliteSidecarFile),
		materializer: &graphview.Materializer{Store: store, Catalog: store.Catalog(), Leases: leases},
	}
	t.Cleanup(func() { _ = inst.Close() })
	return inst, nil
}

type contractInstance struct {
	store        *store_sqlite.Store
	leases       *graphview.LeaseManager
	materializer *graphview.Materializer
	sidecarPath  string
	closeOnce    sync.Once
	closeErr     error
}

// Declaration is SQLite's inventory: its fault points, no sealed files, and
// none of the optional cutover or pack capabilities.
func (c *contractInstance) Declaration() storetest.Declaration {
	var points []string
	for _, point := range store_sqlite.FaultPoints() {
		points = append(points, string(point))
	}
	return storetest.Declaration{
		FaultPoints: points,
		Unsupported: []storetest.Capability{
			storetest.CapabilityPackProtocol, storetest.CapabilityExport, storetest.CapabilityImport,
		},
	}
}

// SQLite has no sealed-file inventory; these adapter operations refuse.
func (c *contractInstance) PrepareSealedFile(context.Context, string, storetest.BuildRequest, storetest.Payload) (storetest.Build, error) {
	return nil, fmt.Errorf("%w: sqlite writes no sealed payload files", storetest.ErrUnsupported)
}

func damageSQLiteSealedFile(context.Context, string, storetest.Version, string, storetest.Damage) error {
	return fmt.Errorf("%w: sqlite writes no sealed payload files", storetest.ErrUnsupported)
}

// Track records the checkout's family, checkout row and pending route from
// its git working tree.
func (c *contractInstance) Track(ctx context.Context, checkout storetest.Checkout) error {
	commonDir, err := gitOutput(ctx, checkout.Root, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	gitDir, err := gitOutput(ctx, checkout.Root, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return err
	}
	headRef, err := gitOutput(ctx, checkout.Root, "symbolic-ref", "HEAD")
	if err != nil {
		return err
	}
	headCommit, err := gitOutput(ctx, checkout.Root, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	headTree, err := gitOutput(ctx, checkout.Root, "rev-parse", "HEAD^{tree}")
	if err != nil {
		return err
	}
	catalog := c.store.Catalog()
	if err := catalog.UpsertRepositoryFamily(ctx, store_sqlite.RepositoryFamily{
		FamilyID: checkout.Repository, CommonDirIdentity: commonDir, State: "family_ready",
	}); err != nil {
		return err
	}
	if err := catalog.UpsertCheckout(ctx, store_sqlite.Checkout{
		CheckoutID: checkout.ID, Incarnation: "incarnation-" + checkout.ID, FamilyID: checkout.Repository,
		RootPath: checkout.Root, GitDir: gitDir, AdminName: filepath.Base(checkout.Root),
		State: store_sqlite.CheckoutStateReady, DesiredMode: store_sqlite.CheckoutModeAutomatic,
		EffectiveMode: store_sqlite.CheckoutModeAutomatic,
		HeadRef:       headRef, HeadCommit: headCommit, HeadTree: headTree,
	}); err != nil {
		return err
	}
	return catalog.UpsertCheckoutRoute(ctx, store_sqlite.CheckoutRoute{
		CheckoutID: checkout.ID, GraphID: contractGraphID(checkout.ID), State: store_sqlite.RoutePending,
	})
}

// Begin opens a managed payload generation. SQLite's build record has no
// operation ID or owner token, so req.Operation is not passed on.
func (c *contractInstance) Begin(ctx context.Context, req storetest.BuildRequest) (storetest.Build, error) {
	checkout, found, err := c.store.Catalog().GetCheckout(ctx, req.Checkout)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("checkout %s is not tracked", req.Checkout)
	}
	kind, parent := "commit", int64(0)
	if req.Parent != "" {
		if parent, err = strconv.ParseInt(string(req.Parent), 10, 64); err != nil {
			return nil, fmt.Errorf("parent version %q: %w", req.Parent, err)
		}
		kind = "dirty"
		// The Materializer names a composed view by its dedicated graph's
		// repository prefix. That prefix is unique per graph, so only the
		// checkout that builds incrementally gets the row.
		if err := c.store.Catalog().UpsertDedicatedGraph(ctx, store_sqlite.DedicatedGraph{
			GraphID: contractGraphID(req.Checkout), OwnerCheckoutID: req.Checkout, RepoPrefix: checkout.FamilyID,
			FamilyID: checkout.FamilyID, State: "graph_ready",
		}); err != nil {
			return nil, err
		}
	}
	id, handle, err := c.store.BeginPayloadGeneration(ctx, store_sqlite.PayloadGenerationRequest{
		OwnerKind:           "dedicated_graph",
		GraphID:             contractGraphID(req.Checkout),
		LayerID:             kind + "-" + req.Checkout,
		CheckoutID:          req.Checkout,
		GenerationKind:      kind,
		BaseGenerationID:    parent,
		TreeOID:             req.Inputs.Tree,
		ProvenanceCommitOID: req.Inputs.Commit,
		ConfigHash:          req.Inputs.Config,
		ExtractorVersions:   req.Inputs.Extractors,
		ResolverVersion:     req.Inputs.Resolver,
		DependencyRevision:  req.Inputs.Dependencies,
		CreatedAt:           1,
	})
	if errors.Is(err, store_sqlite.ErrCatalogGenerationRetiring) {
		return nil, fmt.Errorf("%w: %w", storetest.ErrFenced, err)
	}
	if err != nil {
		return nil, err
	}
	return &contractBuild{store: c.store, handle: handle, id: id, checkout: req.Checkout, repo: checkout.FamilyID, incremental: parent > 0}, nil
}

func (c *contractInstance) Current(ctx context.Context, checkout string) (storetest.Publication, error) {
	route, found, err := c.store.Catalog().GetCheckoutRoute(ctx, checkout)
	if err != nil {
		return storetest.Publication{}, err
	}
	if !found {
		return storetest.Publication{}, fmt.Errorf("%w: checkout %s has no route", storetest.ErrNotTracked, checkout)
	}
	return storetest.Publication{
		Version: contractVersion(servedGeneration(route)),
		Guard:   strconv.FormatInt(route.RouteEpoch, 10),
	}, nil
}

// servedGeneration is the top of a route's stack: the dirty slot over the
// commit slot.
func servedGeneration(route store_sqlite.CheckoutRoute) int64 {
	if route.DirtyGenerationID > 0 {
		return route.DirtyGenerationID
	}
	return route.CommitGenerationID
}

func (c *contractInstance) Open(ctx context.Context, checkout string) (storetest.View, error) {
	route, found, err := c.store.Catalog().GetCheckoutRoute(ctx, checkout)
	if err != nil {
		return nil, err
	}
	if !found || route.CommitGenerationID == 0 {
		return nil, fmt.Errorf("checkout %s serves no version", checkout)
	}
	if route.DirtyGenerationID > 0 {
		view, err := c.materializer.MaterializeCheckout(ctx, checkout)
		if err != nil {
			return nil, err
		}
		return &contractView{store: c.store, id: route.DirtyGenerationID, reader: view.Reader, release: view.Close}, nil
	}
	lease := c.leases.Acquire(route.CommitGenerationID)
	return &contractView{store: c.store, id: route.CommitGenerationID, release: lease.Release}, nil
}

// Retire is the store's retirement of one generation, with the instance's
// leases as its reader check.
func (c *contractInstance) Retire(ctx context.Context, version storetest.Version) error {
	id, err := strconv.ParseInt(string(version), 10, 64)
	if err != nil {
		return fmt.Errorf("version %q: %w", version, err)
	}
	return c.store.RetirePayloadGeneration(ctx, id, c.leases.InUse)
}

// Inventory lists the catalog's view generations: ready and superseded are
// sealed, failed is incomplete.
func (c *contractInstance) Inventory(ctx context.Context) ([]storetest.Stored, error) {
	rows, err := c.store.Catalog().ListViewGenerations(ctx, store_sqlite.ViewGenerationFilter{})
	if err != nil {
		return nil, err
	}
	states := map[store_sqlite.ViewGenerationState]storetest.State{
		store_sqlite.ViewGenerationBuilding:   storetest.StateBuilding,
		store_sqlite.ViewGenerationFailed:     storetest.StateIncomplete,
		store_sqlite.ViewGenerationReady:      storetest.StateSealed,
		store_sqlite.ViewGenerationSuperseded: storetest.StateSealed,
		store_sqlite.ViewGenerationRetiring:   storetest.StateRetiring,
	}
	out := make([]storetest.Stored, 0, len(rows))
	for _, row := range rows {
		state, ok := states[row.State]
		if !ok {
			return nil, fmt.Errorf("generation %d has unknown state %q", row.GenerationID, row.State)
		}
		out = append(out, storetest.Stored{Version: contractVersion(row.GenerationID), State: state})
	}
	return out, nil
}

// Operation reports that SQLite records no publish operation witnesses: its
// route flip carries no operation ID.
func (c *contractInstance) Operation(_ context.Context, id string) (storetest.Version, error) {
	return "", fmt.Errorf("sqlite records no publish operation witness; operation %q is unknown", id)
}

// errUntrackNotExpressed is the adapter gap for untrack. SQLite's untrack is
// CheckoutLifecycle.Untrack: it needs a MultiIndexer whose Register indexes
// the repository itself, and its cleanup hooks are the lifecycle's own
// unexported type. Driving the reconciler with hooks written here would copy
// those side effects, so the adapter does not express untrack.
var errUntrackNotExpressed = errors.New("not expressed by the sqlite adapter: untrack runs only through indexer.CheckoutLifecycle, which needs a MultiIndexer and its own cleanup hooks")

func (c *contractInstance) Untrack(context.Context, string, storetest.Publication) error {
	return errUntrackNotExpressed
}

// Link reports SQLite's absence of a catalog link transition: cross-repository
// links are graph edges written by the resolver, not a guarded catalog record.
func (c *contractInstance) Link(_ context.Context, from, to string) error {
	return fmt.Errorf("sqlite has no catalog link transition (link %s to %s)", from, to)
}

// sidecar is the instance's authored-data store, opened on first use.
func (c *contractInstance) sidecar() (*persistence.SidecarStore, error) {
	return persistence.OpenSidecar(c.sidecarPath)
}

// authoredTime is the fixed timestamp of authored records written here, so
// a record read back compares equal.
var authoredTime = time.Unix(1_700_000_000, 0).UTC()

func (c *contractInstance) PutAuthored(_ context.Context, repository string, record storetest.AuthoredRecord) error {
	sidecar, err := c.sidecar()
	if err != nil {
		return err
	}
	switch record.Kind {
	case "note":
		return sidecar.UpsertNote(repository, persistence.NoteEntry{
			ID: record.ID, Timestamp: authoredTime, UpdatedAt: authoredTime,
			Body: record.Text, SymbolID: record.Anchor, RepoPrefix: repository,
		})
	case "memory":
		return sidecar.UpsertMemory(repository, persistence.MemoryEntry{
			ID: record.ID, Timestamp: authoredTime, UpdatedAt: authoredTime, LastAccessed: authoredTime,
			Body: record.Text, Kind: "reference", Source: "manual", SymbolIDs: []string{record.Anchor}, RepoPrefix: repository,
		})
	}
	return fmt.Errorf("authored record %q has unknown kind %q", record.ID, record.Kind)
}

func (c *contractInstance) Authored(_ context.Context, repository string) ([]storetest.AuthoredRecord, error) {
	sidecar, err := c.sidecar()
	if err != nil {
		return nil, err
	}
	notes, err := sidecar.LoadNotesRows(repository)
	if err != nil {
		return nil, err
	}
	memories, err := sidecar.LoadMemoriesRows(repository)
	if err != nil {
		return nil, err
	}
	var out []storetest.AuthoredRecord
	for _, n := range notes {
		out = append(out, storetest.AuthoredRecord{ID: n.ID, Kind: "note", Anchor: n.SymbolID, Text: n.Body})
	}
	for _, m := range memories {
		anchor := ""
		if len(m.SymbolIDs) > 0 {
			anchor = m.SymbolIDs[0]
		}
		out = append(out, storetest.AuthoredRecord{ID: m.ID, Kind: "memory", Anchor: anchor, Text: m.Body})
	}
	slices.SortFunc(out, func(a, b storetest.AuthoredRecord) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

func (c *contractInstance) Export(context.Context) ([]byte, error) {
	return nil, fmt.Errorf("%w: sqlite has no authored-data export", storetest.ErrUnsupported)
}

func (c *contractInstance) Import(context.Context, []byte) error {
	return fmt.Errorf("%w: sqlite has no authored-data import", storetest.ErrUnsupported)
}

// Settle waits out the maintenance lane. A store Open had to wipe and
// recreate is the one rebuild SQLite starts: the daemon's warm-restart path
// reads NeedsRebuild and forces a full re-index.
func (c *contractInstance) Settle(ctx context.Context) (int, error) {
	if err := c.store.AwaitMaintenanceIdle(ctx); err != nil {
		return 0, err
	}
	if c.store.NeedsRebuild() {
		return 1, nil
	}
	return 0, nil
}

// Crash uses SQLite's clean Close, not SIGKILL: it joins maintenance and
// checkpoint owners, closes prepared statements, pools and the sidecar, and
// leaves committed WAL for reopen under the default skip-checkpoint policy.
// Close can seal/flush an active bulk load, but this adapter never starts one.
// It does not seal or publish private generations. Old Builds refuse through
// their closed database handles, not through a recovered owner-token check.
func (c *contractInstance) Crash() error { return c.Close() }

func (c *contractInstance) Close() error {
	c.closeOnce.Do(func() {
		c.closeErr = errors.Join(c.store.Close(), persistence.CloseSidecar(c.sidecarPath))
	})
	return c.closeErr
}

type contractBuild struct {
	store       *store_sqlite.Store
	handle      *store_sqlite.Store
	id          int64
	checkout    string
	repo        string
	incremental bool
}

func (b *contractBuild) Version() storetest.Version { return contractVersion(b.id) }
func (b *contractBuild) Store() graph.Store         { return b.handle }

func (b *contractBuild) Write(ctx context.Context, p storetest.Payload) error {
	if err := b.handle.AddBatchCheckedContext(ctx, p.Nodes, p.Edges); err != nil {
		return err
	}
	if err := b.handle.SetFileMetas(b.repo, p.Files); err != nil {
		return err
	}
	if err := b.handle.BatchUpsertSymbolFTS(p.Symbols); err != nil {
		return err
	}
	if err := b.handle.BulkUpsertEmbeddings(p.Vectors); err != nil {
		return err
	}
	for _, producer := range p.Producers {
		if err := b.handle.SetProducerState(store_sqlite.ProducerCompleteness{
			Producer: producer, State: store_sqlite.ProducerStateComplete,
		}); err != nil {
			return err
		}
	}
	masks := fileMasks(b.repo, p)
	if len(masks) == 0 {
		return nil
	}
	return b.handle.SetFileMasks(masks)
}

// fileMasks is what a contribution claims over the layers below it, as a
// production generation claims every path it writes: every file it carries
// replaces theirs, and every removed file is deleted.
func fileMasks(repo string, p storetest.Payload) []store_sqlite.FileMask {
	claimed := map[string]store_sqlite.OwnershipMode{}
	for _, f := range p.Files {
		claimed[f.FilePath] = store_sqlite.OwnershipReplace
	}
	for _, n := range p.Nodes {
		claimed[n.FilePath] = store_sqlite.OwnershipReplace
	}
	for _, e := range p.Edges {
		claimed[e.FilePath] = store_sqlite.OwnershipReplace
	}
	for _, path := range p.RemovedFiles {
		claimed[path] = store_sqlite.OwnershipDelete
	}
	var masks []store_sqlite.FileMask
	for _, path := range slices.Sorted(maps.Keys(claimed)) {
		masks = append(masks, store_sqlite.FileMask{RepoPrefix: repo, FilePath: path, Mode: claimed[path]})
	}
	return masks
}

func (b *contractBuild) Seal(ctx context.Context) error {
	return b.store.PublishPayloadGeneration(ctx, b.id, 1)
}

// Publish flips the checkout route: the dirty slot for an incremental build,
// otherwise the commit slot, clearing a dirty slot the new commit replaces.
// The flip carries no operation ID, so operation is not passed on.
func (b *contractBuild) Publish(ctx context.Context, expected storetest.Publication, _ string) error {
	epoch, err := strconv.ParseInt(expected.Guard, 10, 64)
	if err != nil {
		return fmt.Errorf("publication guard %q: %w", expected.Guard, err)
	}
	catalog := b.store.Catalog()
	slot := store_sqlite.RouteSlotCommit
	if b.incremental {
		slot = store_sqlite.RouteSlotDirty
	} else if route, found, routeErr := catalog.GetCheckoutRoute(ctx, b.checkout); routeErr != nil {
		return routeErr
	} else if found && route.DirtyGenerationID > 0 {
		err = catalog.FlipCheckoutRoute(ctx, store_sqlite.FlipCheckoutRouteRequest{
			CheckoutID: b.checkout, ExpectedRouteEpoch: epoch, GraphID: route.GraphID,
			CommitGenerationID: b.id, State: store_sqlite.RouteActive,
		})
		return wrapStaleGuard(err)
	}
	err = catalog.FlipCheckoutRouteSlot(ctx, store_sqlite.FlipCheckoutRouteSlotRequest{
		CheckoutID:         b.checkout,
		Slot:               slot,
		GenerationID:       b.id,
		ExpectedRouteEpoch: epoch,
		State:              store_sqlite.RouteActive,
	})
	return wrapStaleGuard(err)
}

func wrapStaleGuard(err error) error {
	if errors.Is(err, store_sqlite.ErrCatalogStaleGuard) {
		return fmt.Errorf("%w: %w", storetest.ErrStaleGuard, err)
	}
	return err
}

// contractView reads one served version, pinned by a lease of the
// instance's lease manager. A commit version reads through its own handle; an
// incremental one reads graphview's composed reader.
type contractView struct {
	store   *store_sqlite.Store
	id      int64
	reader  graph.Reader
	release func()
}

func (v *contractView) Version() storetest.Version { return contractVersion(v.id) }
func (v *contractView) Release()                   { v.release() }

func (v *contractView) Reader() graph.Reader {
	if v.reader != nil {
		return v.reader
	}
	return v.store.AtGeneration(v.id)
}

func (v *contractView) Provenance(ctx context.Context) (storetest.Provenance, error) {
	row, found, err := v.store.Catalog().GetViewGeneration(ctx, v.id)
	if err != nil {
		return storetest.Provenance{}, err
	}
	if !found {
		return storetest.Provenance{}, errors.New("served generation has no catalog row")
	}
	states, err := v.store.AtGeneration(v.id).ProducerStates()
	if err != nil {
		return storetest.Provenance{}, err
	}
	var producers []string
	for _, state := range states {
		if state.State == store_sqlite.ProducerStateComplete {
			producers = append(producers, state.Producer)
		}
	}
	slices.Sort(producers)
	return storetest.Provenance{
		Inputs: storetest.Inputs{
			Tree: row.TreeOID, Commit: row.ProvenanceCommitOID, Config: row.ConfigHash,
			Extractors: row.ExtractorVersions, Resolver: row.ResolverVersion, Dependencies: row.DependencyRevision,
		},
		Producers: producers,
	}, nil
}

// damageSQLite changes the store a closed instance left in dir.
func damageSQLite(t *testing.T, dir string, kind storetest.Damage, version storetest.Version) {
	t.Helper()
	path := filepath.Join(dir, sqliteStoreFile)
	switch kind {
	case storetest.DamageTooNewFormat:
		execRaw(t, path, "PRAGMA user_version = 100000")
	case storetest.DamageCorrupt:
		// Fold the WAL into the main file first, so the header written over
		// is the one SQLite reads.
		execRaw(t, path, "PRAGMA wal_checkpoint(TRUNCATE)")
		file, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err != nil {
			t.Fatalf("open %s: %v", path, err)
		}
		if _, err := file.WriteAt([]byte("not a sqlite database header"), 0); err != nil {
			t.Fatalf("corrupt %s: %v", path, err)
		}
		if err := file.Close(); err != nil {
			t.Fatalf("close %s: %v", path, err)
		}
	case storetest.DamageSymlinkEscape:
		outside := filepath.Join(t.TempDir(), sqliteStoreFile)
		for _, suffix := range []string{"", "-wal", "-shm"} {
			if err := os.Rename(path+suffix, outside+suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("move %s outside: %v", path+suffix, err)
			}
		}
		if err := os.Symlink(outside, path); err != nil {
			t.Fatalf("symlink %s: %v", path, err)
		}
	case storetest.DamageMissingPayload:
		id, err := strconv.ParseInt(string(version), 10, 64)
		if err != nil {
			t.Fatalf("version %q: %v", version, err)
		}
		deletePayloadRows(t, path, id)
	default:
		t.Fatalf("sqlite cannot apply damage %q", kind)
	}
}

// execRaw runs statement on the store file outside the store.
func execRaw(t *testing.T, path, statement string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open %s raw: %v", path, err)
	}
	defer db.Close()
	if _, err := db.Exec(statement); err != nil {
		t.Fatalf("%s on %s: %v", statement, path, err)
	}
}

// deletePayloadRows deletes generation's rows from every ordinary table keyed
// by view_gen: its payload, with its catalog row left in place.
func deletePayloadRows(t *testing.T, path string, generation int64) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open %s raw: %v", path, err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT m.name FROM sqlite_master m, pragma_table_info(m.name) c
		WHERE m.type = 'table' AND m.sql NOT LIKE 'CREATE VIRTUAL%' AND c.name = 'view_gen'`)
	if err != nil {
		t.Fatalf("list payload tables: %v", err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan payload table: %v", err)
		}
		tables = append(tables, name)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		t.Fatalf("list payload tables: %v", err)
	}
	if len(tables) == 0 {
		t.Fatal("no table carries a view_gen column")
	}
	for _, table := range tables {
		if _, err := db.Exec(`DELETE FROM "`+table+`" WHERE view_gen = ?`, generation); err != nil {
			t.Fatalf("delete generation %d from %s: %v", generation, table, err)
		}
	}
}

func contractGraphID(checkout string) string { return "graph-" + checkout }

func contractVersion(generationID int64) storetest.Version {
	if generationID == 0 {
		return ""
	}
	return storetest.Version(strconv.FormatInt(generationID, 10))
}

func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...).Output()
	if err != nil {
		return "", fmt.Errorf("git %s in %s: %w", strings.Join(args, " "), dir, err)
	}
	return strings.TrimSpace(string(out)), nil
}
