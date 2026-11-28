package store_pebble

import (
	"crypto/sha256"
	"errors"
	"iter"
	"strings"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/contracts"
)

// RebuildContractSelection holds separately elected logical IDs, selected IDs,
// and shape references. Its disk keys are independent of the output batches.
// The selection is serial and valid only inside WithContractSelection.
type RebuildContractSelection struct {
	records *RebuildContractRecords
	frozen  bool
}

// WithContractSelection lends the independent ID and shape-reference scratch.
// FreezeCompleteIDs must run after the final native ID rewrite, before selection.
func (r *RebuildContractRecords) WithContractSelection(use func(*RebuildContractSelection) error) (err error) {
	if err := r.ready(); err != nil {
		return err
	}
	prefix := rebuildContractRecordKey('l')
	occupied, err := rebuildGuardHasPrefix(r.ctx, r.build.pending, prefix)
	if err != nil {
		return err
	}
	if occupied || use == nil {
		return invalid("contract selection scratch is occupied or missing owner")
	}
	if err := r.build.putRebuildScratch(append(prefix, 'a'), nil, goldBufferBytes, "contract selection"); err != nil {
		return err
	}
	s := &RebuildContractSelection{records: r}
	defer func() {
		s.records = nil
		err = errors.Join(err, r.build.pending.DeleteRange(prefix, prefixEnd(prefix), pebble.NoSync))
	}()
	return use(s)
}

// FreezeCompleteIDs captures native representatives, including omitted records
// but excluding empty registry buckets. A frozen empty set is built evidence.
func (s *RebuildContractSelection) FreezeCompleteIDs() error {
	if err := s.ready(); err != nil {
		return err
	}
	if s.frozen {
		return invalid("complete contract IDs already frozen")
	}
	if err := s.records.VisitRepresentatives(func(c RebuildContractRecord) error {
		return s.put('c', c.ID)
	}); err != nil {
		return err
	}
	s.frozen = true
	return nil
}

func (s *RebuildContractSelection) ready() error {
	if s == nil || s.records == nil {
		return invalid("inactive contract selection")
	}
	return s.records.ready()
}

func rebuildContractSelectionKey(tag byte, id string) []byte {
	return appendKeyString(append(rebuildContractRecordKey('l'), tag), id)
}

func (s *RebuildContractSelection) put(tag byte, id string) error {
	if err := s.ready(); err != nil {
		return err
	}
	if id == "" {
		return invalid("empty contract selection key")
	}
	return s.records.build.putRebuildScratch(rebuildContractSelectionKey(tag, id), nil, goldBufferBytes, "contract selection key")
}

func (s *RebuildContractSelection) contains(tag byte, id string) (bool, error) {
	if err := s.ready(); err != nil {
		return false, err
	}
	return pendingGet(s.records.build.pending, rebuildContractSelectionKey(tag, id), func(raw []byte) (bool, error) {
		if len(raw) != 0 {
			return false, invalid("contract selection key value")
		}
		return true, nil
	})
}

// SelectID records the positive D83 decision for the elected native winner,
// before encoding any batch. Never use it for a later replacement winner.
func (s *RebuildContractSelection) SelectID(id string) error {
	if err := s.checkFrozen(); err != nil {
		return err
	}
	known, err := s.contains('c', id)
	if err != nil {
		return err
	}
	if !known {
		return invalid("contract selection contains an unknown logical ID")
	}
	return s.put('s', id)
}

func (s *RebuildContractSelection) Selected(id string) (bool, error) {
	if err := s.checkFrozen(); err != nil {
		return false, err
	}
	return s.contains('s', id)
}

// MarkSelectedDI records the exact selected DI representative, including its
// factory target and payload. A selected ID alone cannot prove a sibling.
func (s *RebuildContractSelection) MarkSelectedDI(c contracts.Contract) error {
	selected, err := s.Selected(c.ID)
	if err != nil {
		return err
	}
	if !selected {
		return invalid("DI representative without selected contract ID")
	}
	id, err := rebuildSelectedDIKey(c)
	if err != nil {
		return err
	}
	return s.put('b', id)
}

func (s *RebuildContractSelection) SelectedDI(c contracts.Contract) (bool, error) {
	if err := s.checkFrozen(); err != nil {
		return false, err
	}
	id, err := rebuildSelectedDIKey(c)
	if err != nil {
		return false, err
	}
	return s.contains('b', id)
}

func rebuildSelectedDIKey(c contracts.Contract) (string, error) {
	if c.Type != contracts.ContractDI {
		return "", invalid("selected DI key requires a DI record")
	}
	e := contractFactsEncoder{}
	writeFactRecord(&e, c)
	digest := sha256.Sum256(e.buf)
	return string(digest[:]), e.err
}

// RememberShapeRef records one checked full type reference at the shape cut.
// Duplicate references share a key; bare names cannot mint shape rows.
func (s *RebuildContractSelection) RememberShapeRef(id string) error {
	if err := s.checkFrozen(); err != nil {
		return err
	}
	if !strings.Contains(id, "::") {
		return invalid("contract shape reference is not a full ID")
	}
	return s.put('t', id)
}

func (s *RebuildContractSelection) HasShapeRef(id string) (bool, error) {
	if err := s.checkFrozen(); err != nil {
		return false, err
	}
	return s.contains('t', id)
}

func (s *RebuildContractSelection) CompleteIDs() iter.Seq2[string, error] { return s.stream('c') }
func (s *RebuildContractSelection) SelectedIDs() iter.Seq2[string, error] { return s.stream('s') }
func (s *RebuildContractSelection) ShapeRefs() iter.Seq2[string, error]   { return s.stream('t') }

func (s *RebuildContractSelection) checkFrozen() error {
	if err := s.ready(); err != nil {
		return err
	}
	if !s.frozen {
		return invalid("complete contract IDs not frozen")
	}
	return nil
}

func (s *RebuildContractSelection) stream(tag byte) iter.Seq2[string, error] {
	return func(yield func(string, error) bool) {
		if err := s.checkFrozen(); err != nil {
			yield("", err)
			return
		}
		stop := errors.New("contract selection visitor stopped")
		prefix := append(rebuildContractRecordKey('l'), tag)
		err := pendingVisit(s.records.ctx, s.records.build.pending, prefix, func(entry row) error {
			id, rest, err := readKeyString(entry.key[len(prefix):])
			if err != nil || id == "" || len(rest) != 0 || len(entry.value) != 0 {
				return invalid("malformed contract selection key")
			}
			if !yield(id, nil) {
				return stop
			}
			return nil
		})
		if err != nil && !errors.Is(err, stop) {
			yield("", err)
		}
	}
}
