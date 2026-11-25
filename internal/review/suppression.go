package review

import (
	"time"

	"github.com/zzet/gortex/internal/persistence"
)

// SuppressionStore is the durable false-positive filter for review findings. It
// wraps the SQLite sidecar's suppressions table: a finding whose IdentityKey is
// recorded for a repo is silently dropped from every subsequent review of that
// repo, permanently, until it is explicitly un-suppressed.
//
// This is distinct from a development memory or a feedback signal: it is not
// ranked, not surfaced, not decayed. It is a hard, finding-identity-keyed
// never-flag-again list, scoped per repo (the same repoKey the notes / memories
// managers use), and it survives daemon restarts because it lives in the sidecar.
//
// A nil *SuppressionStore — and a store wrapping a nil sidecar — is tolerated by
// every method: IsSuppressed returns false, the mutators are no-ops, and List
// returns nothing. So a review flow can hold a nil store with no guard.
type SuppressionStore struct {
	sidecar suppressionRecords
}

// NewSuppressionStore binds a suppression store to an already-open sidecar. A
// nil sidecar yields a store that tolerates every call as a no-op / false.
func NewSuppressionStore(sidecar *persistence.SidecarStore) *SuppressionStore {
	s := &SuppressionStore{}
	if sidecar != nil {
		s.sidecar = sidecar
	}
	return s
}

// SuppressionRow is the read-side projection of a stored suppression — the
// denormalised finding context plus the hit bookkeeping.
type SuppressionRow struct {
	IdentityKey string    `json:"identity_key"`
	Rule        string    `json:"rule"`
	Category    string    `json:"category"`
	File        string    `json:"file"`
	SymbolID    string    `json:"symbol_id"`
	Reason      string    `json:"reason,omitempty"`
	Author      string    `json:"author,omitempty"`
	HitCount    int64     `json:"hit_count"`
	Created     time.Time `json:"created_at"`
	LastHit     time.Time `json:"last_hit,omitempty"`
}

// usable reports whether the store has a live sidecar to read / write.
func (s *SuppressionStore) usable() bool {
	return s != nil && s.sidecar != nil
}

// Suppress records (or refreshes) a suppression for a finding in a repo. The
// IdentityKey is computed from the finding if it does not already carry one, so
// a caller can pass a bare finding. An existing row's hit bookkeeping is
// preserved (the finding's own counters are not trusted here); only the context
// and the suppress reason / author are refreshed. A nil store is a no-op.
func (s *SuppressionStore) Suppress(repoKey string, f Finding, reason, author string) error {
	if !s.usable() {
		return nil
	}
	key := f.IdentityKey
	if key == "" {
		key = IdentityKey(f)
	}

	now := time.Now().UTC()
	created := now
	var hits int64
	var lastHit time.Time
	if prior, ok := s.sidecar.LoadSuppression(repoKey, key); ok {
		// Preserve the original creation time and accumulated hit counters so a
		// re-suppress (e.g. with a corrected reason) does not reset history.
		if !prior.Created.IsZero() {
			created = prior.Created
		}
		hits = prior.HitCount
		lastHit = prior.LastHit
	}

	return s.sidecar.UpsertSuppression(repoKey, persistence.SuppressionEntry{
		IdentityKey: key,
		Rule:        f.Rule,
		Category:    f.Category,
		File:        f.File,
		SymbolID:    f.SymbolID,
		Reason:      reason,
		Author:      author,
		HitCount:    hits,
		Created:     created,
		LastHit:     lastHit,
	})
}

// IsSuppressed is the hot read inside the gate: it reports whether a finding
// identity is suppressed for a repo and, on a hit, bumps the row's hit_count /
// last_hit so the suppression's usefulness is observable. A nil store / sidecar,
// or an empty key, returns false. A failed bump never changes the answer — the
// finding is still suppressed.
func (s *SuppressionStore) IsSuppressed(repoKey, identityKey string) bool {
	if !s.usable() || identityKey == "" {
		return false
	}
	if _, ok := s.sidecar.LoadSuppression(repoKey, identityKey); !ok {
		return false
	}
	_ = s.sidecar.BumpSuppressionHit(repoKey, identityKey, time.Now().UTC())
	return true
}

// List returns every suppression for a repo, most-recently-hit first. A nil
// store / sidecar returns an empty slice and no error.
func (s *SuppressionStore) List(repoKey string) ([]SuppressionRow, error) {
	if !s.usable() {
		return nil, nil
	}
	entries, err := s.sidecar.LoadSuppressions(repoKey)
	if err != nil {
		return nil, err
	}
	out := make([]SuppressionRow, 0, len(entries))
	for _, e := range entries {
		out = append(out, SuppressionRow{
			IdentityKey: e.IdentityKey,
			Rule:        e.Rule,
			Category:    e.Category,
			File:        e.File,
			SymbolID:    e.SymbolID,
			Reason:      e.Reason,
			Author:      e.Author,
			HitCount:    e.HitCount,
			Created:     e.Created,
			LastHit:     e.LastHit,
		})
	}
	return out, nil
}

// Unsuppress removes a suppression so the finding can be flagged again. A nil
// store / sidecar, or a missing row, is a no-op (not an error).
func (s *SuppressionStore) Unsuppress(repoKey, identityKey string) error {
	if !s.usable() || identityKey == "" {
		return nil
	}
	return s.sidecar.DeleteSuppression(repoKey, identityKey)
}
