package store_pebble

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/cockroachdb/pebble/v2"
)

// routeCatalog is the store-level switch. One owner opens it and pins both
// heads through the switch. Startup path checks and cleanup belong to the
// daemon lifecycle; this store is not wired to the live catalog.
type routeCatalog struct {
	db *pebble.DB
	mu sync.Mutex // serializes catalog read/check/write
}

type routeLocator struct {
	Lease, Directory string
}

const maxRoutePathBytes = 4096

func (r routeLocator) valid() bool {
	return ValidateIncarnation(r.Lease) == nil && len(r.Directory) <= maxRoutePathBytes &&
		utf8.ValidString(r.Directory) && !strings.ContainsRune(r.Directory, 0) &&
		filepath.IsAbs(r.Directory) && filepath.Clean(r.Directory) == r.Directory
}

type routeCheckout struct {
	Incarnation        string
	Serving, Candidate routeLocator
	Publication        LocalPublicationRecord
}

func openRouteCatalog(path string) (*routeCatalog, error) {
	db, err := pebble.Open(path, headOptions())
	if err != nil {
		return nil, err
	}
	return &routeCatalog{db: db}, nil
}

func (c *routeCatalog) Close() error { return c.db.Close() }

func routeCatalogKey(checkout string) []byte {
	return keyStrings([]byte{'r'}, checkout)
}

func (c *routeCatalog) lookup(checkout string) (routeCheckout, error) {
	if checkout == "" || len(checkout) > maxRoutePathBytes {
		return routeCheckout{}, invalid("catalog checkout key")
	}
	return pendingGet(c.db, routeCatalogKey(checkout), func(raw []byte) (routeCheckout, error) {
		var row routeCheckout
		if len(raw) > 16*maxRoutePathBytes {
			return row, invalid("catalog route row size")
		}
		if err := json.Unmarshal(raw, &row); err != nil {
			return routeCheckout{}, fmt.Errorf("decode catalog route: %w", err)
		}
		var zero routeLocator
		if ValidateIncarnation(row.Incarnation) != nil ||
			(row.Candidate != zero && !row.Candidate.valid()) ||
			(row.Serving != zero && (!row.Serving.valid() || row.Publication.Incarnation != row.Incarnation || row.Publication.WitnessDigest == (HeadDigest{}))) ||
			(row.Serving == zero && row.Publication != (LocalPublicationRecord{})) ||
			(row.Candidate != zero && (row.Candidate.Lease == row.Serving.Lease || row.Candidate.Directory == row.Serving.Directory)) {
			return routeCheckout{}, invalid("catalog route row")
		}
		return row, nil
	})
}

func (c *routeCatalog) save(ctx context.Context, checkout string, row routeCheckout) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	raw, err := json.Marshal(row)
	if err != nil {
		return err
	}
	return c.db.Set(routeCatalogKey(checkout), raw, pebble.Sync)
}

// initCheckout stands in for the future track transaction. It never replaces
// an existing checkout; registering a candidate cannot create one.
func (c *routeCatalog) initCheckout(ctx context.Context, checkout, incarnation string) error {
	if err := ValidateIncarnation(incarnation); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	row, err := c.lookup(checkout)
	if err != nil {
		return err
	}
	if row.Incarnation != "" {
		return ErrStaleBase
	}
	return c.save(ctx, checkout, routeCheckout{Incarnation: incarnation})
}

// registerCandidate records the owner-supplied locator before building. A
// replacement registration fences the earlier lease even at the same stage.
func (c *routeCatalog) registerCandidate(ctx context.Context, checkout, incarnation string, expected, candidate routeLocator) error {
	if !candidate.valid() {
		return invalid("catalog candidate locator")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	row, err := c.lookup(checkout)
	if err != nil {
		return err
	}
	if row.Incarnation == "" || row.Incarnation != incarnation || row.Serving != expected ||
		candidate.Lease == row.Serving.Lease || candidate.Directory == row.Serving.Directory ||
		candidate.Lease == row.Candidate.Lease || candidate.Directory == row.Candidate.Directory {
		return ErrStaleBase
	}
	row.Candidate = candidate
	return c.save(ctx, checkout, row)
}

// switchHead selects an already ingested, registered head in one synced write.
// The serving locator and exact publication commit together. A failed write
// needs readback before any further action.
func (c *routeCatalog) switchHead(ctx context.Context, checkout, incarnation string, expected, candidate routeLocator, prior, next *Head, publication LocalPublicationRecord) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	row, err := c.lookup(checkout)
	if err != nil {
		return err
	}
	if row.Incarnation == "" || row.Incarnation != incarnation || row.Serving != expected ||
		!candidate.valid() || row.Candidate != candidate || publication.Incarnation != incarnation ||
		!headMatchesLocator(next, incarnation, candidate) ||
		((prior == nil) != (expected == (routeLocator{}))) ||
		(prior != nil && !headMatchesLocator(prior, incarnation, expected)) {
		return ErrStaleBase
	}
	// No ingest may move the witnesses between these reads and the synced
	// catalog write. Head ingests never take the catalog lock.
	ingestMu.Lock()
	defer ingestMu.Unlock()
	w, digest, err := next.currentWitness()
	if err != nil {
		return err
	}
	if w.Stage == 0 {
		return ErrNotPublished
	}
	if w.Profile != next.profile || digest != publication.WitnessDigest || w.Version() != publication.Version {
		return ErrStaleBase
	}
	if prior != nil {
		old, _, err := prior.currentWitness()
		if err != nil {
			return err
		}
		if old.Stage == 0 {
			return ErrNotPublished
		}
		if old.Profile != prior.profile {
			return ErrStaleBase
		}
		if w.Stage.rank() < old.Stage.rank() {
			return ErrStageOrder
		}
		if !w.CoversRelationsAtLeast(old) || (old.Contracts != nil && w.Contracts == nil) {
			return ErrCoverageRegression
		}
	}
	row.Serving, row.Candidate, row.Publication = candidate, routeLocator{}, publication
	return c.save(ctx, checkout, row)
}

func headMatchesLocator(h *Head, incarnation string, locator routeLocator) bool {
	return h != nil && h.incarnation == incarnation && h.directory == locator.Directory
}
