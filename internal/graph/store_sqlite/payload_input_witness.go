package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"

	"github.com/zzet/gortex/internal/storage"
)

// ErrPayloadInputChanged refuses publication when a selected input changed.
var ErrPayloadInputChanged = fmt.Errorf("%w: payload input changed", ErrCatalogStaleGuard)

// PayloadInputWitness binds selected ancestor rows to one live store core.
// It is process-local proof, not persisted cache authority.
type PayloadInputWitness = storage.PayloadInputWitness

// PayloadInputWitnessRevision is one opaque captured generation row.
type PayloadInputWitnessRevision = storage.PayloadInputWitnessRevision

// PayloadInputRevision is an opaque comparable revision of one selected handle.
// It describes cached input ownership, not catalog lifetime or publication authority.
type PayloadInputRevision = storage.PayloadInputRevision

// PayloadInputRevision samples only this handle's raw generation clocks. It
// includes sidecar/ownership changes and rare administration without implicitly
// adding base zero. The monotonic tuple is a conservative before/after check, not a
// coherent committed snapshot. The locked publication witness remains authority;
// no database read, writer gate or connection acquisition occurs.
func (s *Store) PayloadInputRevision() PayloadInputRevision {
	revision, _ := s.PayloadInputRevisionContext(context.Background())
	return revision
}

// PayloadInputRevisionContext permits checked readers to fail promptly after cancellation.
func (s *Store) PayloadInputRevisionContext(ctx context.Context) (PayloadInputRevision, error) {
	if ctx == nil || s == nil || s.coreless() {
		return PayloadInputRevision{}, ErrCatalogInvalidValue
	}
	if err := ctx.Err(); err != nil {
		return PayloadInputRevision{}, err
	}
	return NewPayloadInputRevision(&s.payloadInputIdentity, s.viewGen, s.analysisViewCounter(s.viewGen).Load(), s.constantInputCounter(s.viewGen).Load(), s.payloadInputAdminRevision.Load()), nil
}

// CapturePayloadInputWitness must precede the checked projection. It includes
// exactly the supplied generations, holds no gate after return, and excludes no supplied
// ancestor. Callers must omit the generation they are about to build.
func (s *Store) CapturePayloadInputWitness(ctx context.Context, generationIDs []int64) (*PayloadInputWitness, error) {
	if ctx == nil || s == nil || s.coreless() {
		return nil, fmt.Errorf("%w: invalid input witness", ErrCatalogInvalidValue)
	}
	if len(generationIDs) == 0 {
		return nil, fmt.Errorf("%w: empty input witness", ErrCatalogInvalidValue)
	}
	ids := map[int64]bool{}
	for _, g := range generationIDs {
		if g < 0 {
			return nil, fmt.Errorf("%w: negative input generation", ErrCatalogInvalidValue)
		}
		ids[g] = true
	}
	if err := s.writeMu.LockContext(ctx); err != nil {
		return nil, err
	}
	defer s.writeMu.Unlock()
	conn, release, err := s.activeWriteConnLocked(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	if err := conn.PingContext(ctx); err != nil {
		return nil, err
	}
	source := &s.payloadInputIdentity
	admin := s.payloadInputAdminRevision.Load()
	var revisions []PayloadInputWitnessRevision
	for g := range ids {
		var state string
		if g > 0 {
			if err := conn.QueryRowContext(ctx, "SELECT state FROM view_generations WHERE generation_id=?", g).Scan(&state); err != nil {
				return nil, err
			}
		}
		revisions = append(revisions, NewPayloadInputWitnessRevision(g, s.analysisViewCounter(g).Load(), s.constantInputCounter(g).Load(), s.semanticBindingInputCounter(g).Load(), state))
	}
	sort.Slice(revisions, func(i, j int) bool { return revisions[i].GenerationID() < revisions[j].GenerationID() })
	return NewPayloadInputWitness(source, admin, revisions), nil
}

func validatePayloadInputWitnessLocked(w *PayloadInputWitness, s *Store, ctx context.Context) error {
	if !w.MatchesSource(&s.payloadInputIdentity) || w.RevisionCount() == 0 || w.AdminRevision() != s.payloadInputAdminRevision.Load() {
		return ErrPayloadInputChanged
	}
	conn, release, err := s.activeWriteConnLocked(ctx)
	if err != nil {
		return err
	}
	defer release()
	for i := 0; i < w.RevisionCount(); i++ {
		generation, analysis, constants, bindings, capturedState := w.Revision(i).Values()
		if generation > 0 {
			var state string
			err := conn.QueryRowContext(ctx, "SELECT state FROM view_generations WHERE generation_id=?", generation).Scan(&state)
			if errors.Is(err, sql.ErrNoRows) {
				return ErrPayloadInputChanged
			}
			if err != nil {
				return err
			}
			if state != capturedState {
				return ErrPayloadInputChanged
			}
		}
		if analysis != s.analysisViewCounter(generation).Load() || constants != s.constantInputCounter(generation).Load() || bindings != s.semanticBindingInputCounter(generation).Load() {
			return ErrPayloadInputChanged
		}
	}
	return nil
}

// PublishPayloadGenerationWithInputWitness leaves existing publication callers
// unchanged. Ancestor validation and the ready transition share one write gate.
func (s *Store) PublishPayloadGenerationWithInputWitness(ctx context.Context, generationID, publishedAt int64, witness *PayloadInputWitness) error {
	if witness == nil {
		return ErrPayloadInputChanged
	}
	p, err := s.PreparePayloadGenerationPublication(ctx, generationID)
	if err != nil {
		return err
	}
	return p.PublishWithInputWitness(ctx, publishedAt, witness)
}

func (p *PreparedPayloadGenerationPublication) PublishWithInputWitness(ctx context.Context, publishedAt int64, witness *PayloadInputWitness) error {
	if witness == nil {
		return ErrPayloadInputChanged
	}
	return p.publishWithInputWitness(ctx, publishedAt, true, witness)
}

func (c *Catalog) execGuardedWithInputWitness(ctx context.Context, w *PayloadInputWitness, subject, query string, args ...any) error {
	if w == nil {
		return c.execGuarded(ctx, subject, query, args...)
	}
	if ctx == nil {
		return errors.New("nil publication context")
	}
	if err := c.store.writeMu.LockContext(ctx); err != nil {
		return err
	}
	defer c.store.writeMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validatePayloadInputWitnessLocked(w, c.store, ctx); err != nil {
		return err
	}
	result, err := c.execLocked(ctx, query, args...)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: %s", ErrCatalogStaleGuard, subject)
	}
	return nil
}
