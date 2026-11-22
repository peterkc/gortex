package store_pebble

import (
	"errors"
	"fmt"

	"github.com/zeebo/blake3"
)

// ErrPublicationWitness marks a witness that could not be read or decoded.
var ErrPublicationWitness = errors.New("pebble publication witness refused")

// LocalPublicationRecord is the local identity and watcher mark for a head.
// The first three fields bind it to the exact publication. EventID is nil
// when there is no safe start point and the checkout needs a full rehash.
// This value does not define a catalog format.
type LocalPublicationRecord struct {
	Version       HeadDigest
	Incarnation   uint64
	WitnessDigest HeadDigest
	EventID       *uint64
}

// ReconcilePublication reads one snapshot of the already-open head. A missing
// or stale local record supplies no event ID; the witness supplies the stage
// regardless of the record. It writes neither the head nor the catalog and
// does not replace startup integrity checks or determine which head to serve.
func (h *Head) ReconcilePublication(saved *LocalPublicationRecord) (Witness, LocalPublicationRecord, error) {
	s := h.OpenSnapshot()
	w, record, err := s.reconcilePublication(h.incarnation, saved)
	if err := errors.Join(err, s.Close()); err != nil {
		return Witness{}, LocalPublicationRecord{}, err
	}
	return w, record, nil
}

func (s *Snapshot) reconcilePublication(incarnation uint64, saved *LocalPublicationRecord) (Witness, LocalPublicationRecord, error) {
	var digest HeadDigest
	w, err := point(s, publicationWitnessKey, func(raw []byte) (Witness, error) {
		digest = blake3.Sum256(raw)
		return decodeWitness(raw)
	})
	if err != nil {
		return Witness{}, LocalPublicationRecord{}, fmt.Errorf("%w: %w", ErrPublicationWitness, err)
	}
	if w.Stage == 0 {
		return Witness{}, LocalPublicationRecord{}, ErrNotPublished
	}

	record := LocalPublicationRecord{Version: w.Version(), Incarnation: incarnation, WitnessDigest: digest}
	if saved != nil && saved.Version == record.Version && saved.Incarnation == record.Incarnation &&
		saved.WitnessDigest == record.WitnessDigest && saved.EventID != nil {
		id := *saved.EventID
		record.EventID = &id
	}
	return w, record, nil
}
