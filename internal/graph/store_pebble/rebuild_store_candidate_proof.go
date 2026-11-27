package store_pebble

import (
	"bytes"
	"context"

	"github.com/zzet/gortex/internal/graph"
)

// Candidate proof needs the selected singleton and any higher-priority
// ambiguous file that fell through. With no winner, two scoped candidates
// also rule out a singleton before the file checks. Empty buckets use the
// live census, but must not gain a candidate from a conflicting receipt.
type rebuildStoreCandidatePick struct {
	target       string
	buckets      []rebuildStoreCandidateWitness
	nonempty     bool // the scoped pool is not empty
	getterWinner bool
}

type rebuildStoreCandidateWitness struct {
	bucket []byte
	exact  byte
}

func pickRebuildStoreCandidate(unique func([]byte) (string, bool, error), scoped []byte, singleton string, seen bool,
	getter func() (string, error), callerFile string, inFile func(string) []byte,
) (pick rebuildStoreCandidatePick, err error) {
	if !seen {
		return rebuildStoreCandidatePick{buckets: []rebuildStoreCandidateWitness{{scoped, 0}}}, nil
	}
	pick.nonempty = true
	if singleton != "" {
		return rebuildStoreCandidatePick{
			target: singleton, buckets: []rebuildStoreCandidateWitness{{scoped, 1}}, nonempty: true,
		}, nil
	}
	getterFile, err := getter()
	if err != nil {
		return pick, err
	}
	for i, file := range [...]string{getterFile, callerFile} {
		if file == "" {
			continue
		}
		key := inFile(file)
		id, seen, err := unique(key)
		if err != nil {
			return pick, err
		}
		if id != "" {
			pick.buckets = append(pick.buckets, rebuildStoreCandidateWitness{key, 1})
			pick.target = id
			pick.getterWinner = i == 0 && file != callerFile
			return pick, nil
		}
		var need byte
		if seen {
			need = 2
		}
		pick.buckets = append(pick.buckets, rebuildStoreCandidateWitness{key, need})
	}
	pick.buckets = append(pick.buckets, rebuildStoreCandidateWitness{scoped, 2})
	return pick, nil
}

// Proof rows are allocated for moving decisions only. The exact count is
// capped at two distinct live IDs. Fold one row per ID after receipt recording,
// so a repeated or contradictory receipt cannot inflate the count.
const rebuildStoreCandidateExtra = 0x80

func (p *rebuildCallMoves) candidateProofKey(bucket []byte) []byte {
	return append(p.column('p'), bucket[len(p.prefix):]...)
}

func (p *rebuildCallMoves) wantCandidateBuckets(buckets []rebuildStoreCandidateWitness) error {
	for _, witness := range buckets {
		if err := p.put(p.candidateProofKey(witness.bucket), []byte{1}); err != nil {
			return err
		}
	}
	return nil
}

func (p *rebuildCallMoves) countCandidateBuckets(buckets [][]byte, col int, proof byte) error {
	for _, bucket := range buckets {
		scope := bucket[col]
		if (scope == 'w' || scope == 's') && proof&rebuildStoreOtherWorkspace != 0 ||
			(scope == 'f' || scope == 's') && proof&rebuildStoreOtherFile != 0 {
			continue
		}
		key := p.candidateProofKey(bucket)
		count, err := rebuildScratchState(p.b.pending, key, p.label+" candidate bucket proof")
		if err != nil {
			return err
		}
		if n := count &^ rebuildStoreCandidateExtra; n == 1 || n == 2 {
			if err := p.put(key, []byte{count + 1}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *rebuildCallMoves) candidateBucketsProved(buckets []rebuildStoreCandidateWitness) (bool, error) {
	for _, witness := range buckets {
		count, err := rebuildScratchState(p.b.pending, p.candidateProofKey(witness.bucket), p.label+" candidate bucket proof")
		if witness.exact == 2 {
			count &^= rebuildStoreCandidateExtra
		}
		if err != nil || count != witness.exact+1 {
			return false, err
		}
	}
	return true, nil
}

// A receipt for a live ID can enter a deciding cohort without a live claim
// for that role. Only live claims can supply exact or activation witnesses.
func (p *rebuildCallMoves) recordStoreCandidate(key, after []byte, column int, global byte, anchor bool, raw *graph.Node, file, lang string) error {
	claim, err := p.marked(key)
	if err != nil {
		return err
	}
	var before []byte
	if len(claim) > 1 {
		before = claim[1:]
	}
	same := after != nil && bytes.Equal(before, after)
	var wanted bool
	if before != nil {
		state, err := rebuildScratchState(p.b.pending, before, p.label+" receipt proof")
		if err != nil {
			return err
		}
		wanted = state != 0
	}
	var entered bool
	if after != nil && !same {
		state, err := rebuildScratchState(p.b.pending, after, p.label+" receipt proof")
		if err != nil {
			return err
		}
		entered = state != 0
	}
	if !wanted && !entered && (!anchor || before == nil) {
		return nil
	}
	live, err := p.storeReceiptNode(raw.ID)
	if err != nil {
		return err
	}
	receipt := rebuildStoreNodeReceipt(raw, live, file, lang)
	if receipt && (entered || same && wanted && (raw.WorkspaceID != live.WorkspaceID || raw.FilePath != live.FilePath)) {
		if err := p.noteCandidateCohortChange(before, after, column, global, raw, live); err != nil {
			return err
		}
	}
	if before == nil {
		return nil
	}
	proof := rebuildStoreCandidateProof(receipt && same, raw, live)
	if anchor {
		proof |= rebuildStoreAnchorProof(receipt && after != nil)
	}
	return p.noteStoreNodeProof(key, claim, proof)
}

// Getter claims prove live files. A new role or a move can add a file
// without supplying an exact claim for it.
func (p *rebuildCallMoves) recordStoreGetter(key, wanted, files []byte, raw *graph.Node, file, lang string) error {
	claim, err := p.marked(key)
	if err != nil || len(claim) <= 1 && wanted == nil {
		return err
	}
	var before []byte
	if len(claim) > 1 {
		before = claim[1:]
	}
	var beforeState, afterState byte
	if before != nil {
		beforeState, err = rebuildScratchState(p.b.pending, before, p.label+" receipt proof")
		if err != nil {
			return err
		}
	}
	same := wanted != nil && bytes.Equal(before, wanted)
	if same {
		afterState = beforeState
	} else if wanted != nil {
		afterState, err = rebuildScratchState(p.b.pending, wanted, p.label+" receipt proof")
		if err != nil {
			return err
		}
	}
	if beforeState == 0 && afterState == 0 {
		return nil
	}
	live, err := p.storeReceiptNode(raw.ID)
	if err != nil {
		return err
	}
	receipt := rebuildStoreNodeReceipt(raw, live, file, lang)
	exact := receipt && same && file == live.FilePath
	if receipt && !exact && afterState != 0 {
		liveFile, _, err := p.unique(files)
		if err != nil {
			return err
		}
		if file != liveFile {
			if err := noteRebuildScratchState(p.put, wanted, afterState, rebuildCapabilityUnproved); err != nil {
				return err
			}
		}
	}
	if beforeState == 0 {
		return nil
	}
	return p.noteStoreNodeProof(key, claim, rebuildScratchProofBit(exact))
}

func (p *rebuildCallMoves) storeReceiptNode(id string) (*graph.Node, error) {
	key := pendingNodeKey(id)
	return pendingGet(p.b.pending, key, func(val []byte) (*graph.Node, error) {
		return checkedRebuildLightNode(row{key, val}, p.label)
	})
}

// A changed receipt for a known candidate may enter deciding buckets.
// Record only the buckets it enters, without disturbing other scopes.
func (p *rebuildCallMoves) noteCandidateScopeChange(cohort []byte, column int, global byte, raw, live *graph.Node) error {
	return p.noteCandidateCohortChange(cohort, cohort, column, global, raw, live)
}

func (p *rebuildCallMoves) noteCandidateCohortChange(beforeCohort, afterCohort []byte, column int, global byte, raw, live *graph.Node) error {
	var before [][]byte
	if len(beforeCohort) != 0 {
		before = rebuildStoreScopeKeys(beforeCohort, column, global, live)
	}
	for i, bucket := range rebuildStoreScopeKeys(afterCohort, column, global, raw) {
		if before != nil && bytes.Equal(bucket, before[i]) {
			continue
		}
		key := p.candidateProofKey(bucket)
		count, err := rebuildScratchState(p.b.pending, key, p.label+" candidate bucket proof")
		if err != nil {
			return err
		}
		if count != 0 {
			if err := noteRebuildScratchState(p.put, key, count, rebuildStoreCandidateExtra); err != nil {
				return err
			}
		}
	}
	return nil
}

// A receipt proves the role separately from the workspace and file. A
// mismatch bit persists across duplicate receipts and affects only buckets
// that read that field.
const (
	rebuildStoreOtherWorkspace byte = 1 << (iota + 3)
	rebuildStoreOtherFile
	rebuildStoreAnchorEligible
	rebuildStoreAnchorIneligible
)

func rebuildStoreCandidateExact(state byte) bool {
	return state&^(rebuildStoreOtherWorkspace|rebuildStoreOtherFile|rebuildStoreAnchorEligible|rebuildStoreAnchorIneligible) == rebuildScratchExact
}

// The anchor only proves that native enters the Call scan. Conflicting
// receipts for one declaration cannot establish eligibility.
func rebuildStoreAnchorProof(eligible bool) byte {
	if eligible {
		return rebuildStoreAnchorEligible
	}
	return rebuildStoreAnchorIneligible
}

func rebuildStoreAnchorExact(state byte) bool {
	return state&(rebuildStoreAnchorEligible|rebuildStoreAnchorIneligible) == rebuildStoreAnchorEligible
}

func rebuildStoreCandidateProof(ok bool, raw, live *graph.Node) byte {
	bits := rebuildScratchProofBit(ok)
	if ok && raw.WorkspaceID != live.WorkspaceID {
		bits |= rebuildStoreOtherWorkspace
	}
	if ok && raw.FilePath != live.FilePath {
		bits |= rebuildStoreOtherFile
	}
	return bits
}

func (p *rebuildCallMoves) noteStoreNodeProof(key, claim []byte, proof byte) error {
	if proof&^claim[0] == 0 {
		return nil
	}
	claim[0] |= proof
	return p.put(key, claim)
}

func rebuildStoreNodeReceipt(raw, live *graph.Node, file, lang string) bool {
	return live != nil && lang != "" && raw.FilePath == file
}

func rebuildStoreCallerReceipt(raw, live *graph.Node, file, lang string) bool {
	return rebuildStoreNodeReceipt(raw, live, file, lang) && raw.FilePath == live.FilePath && raw.WorkspaceID == live.WorkspaceID
}

// Recover the candidate's scope from its live declaration.
func (p *rebuildCallMoves) foldExactCandidate(r row, prefix []byte, column int, global byte,
	node func(string) (*graph.Node, error),
) error {
	n, err := p.candidateProofNode(r.key[len(prefix):], node)
	if err != nil {
		return err
	}
	return p.countCandidateBuckets(rebuildStoreScopeKeys(r.value[1:], column, global, n), column, r.value[0])
}

func (p *rebuildCallMoves) candidateProofNode(key []byte, node func(string) (*graph.Node, error)) (*graph.Node, error) {
	id, rest, err := readKeyString(key)
	if err != nil || len(rest) != 0 || id == "" {
		return nil, invalid(p.label + " candidate proof key")
	}
	n, err := node(id)
	if err != nil {
		return nil, err
	}
	if n == nil {
		return nil, invalid(p.label + " candidate proof has no live node")
	}
	return n, nil
}

func (p *rebuildCallMoves) getterProofKey(wanted []byte) []byte {
	return append(p.column('x'), wanted[len(p.prefix):]...)
}

// Note a known eligible getter that moved to another file. Unlike an omitted
// duplicate in the same file, it can defeat a unique getter file.
func (p *rebuildCallMoves) noteGetterFileChange(wanted []byte, state byte, raw, live *graph.Node, file, lang string) error {
	if rebuildStoreNodeReceipt(raw, live, file, lang) && file != live.FilePath {
		return noteRebuildScratchState(p.put, wanted, state, rebuildCapabilityUnproved)
	}
	return nil
}

// Fold one exact claim per ID. addUnique counts a file once and stops at two
// distinct files, even when more getter declarations are live.
func (p *rebuildCallMoves) foldGetterFiles(ctx context.Context, prefix []byte,
	node func(string) (*graph.Node, error), addUnique func([]byte, string) error,
) error {
	return pendingVisit(ctx, p.b.pending, prefix, func(r row) error {
		if len(r.value) <= len(p.prefix)+1 {
			return invalid(p.label + " getter proof")
		}
		if r.value[0] != rebuildScratchExact {
			return nil
		}
		n, err := p.candidateProofNode(r.key[len(prefix):], node)
		if err != nil {
			return err
		}
		return addUnique(p.getterProofKey(r.value[1:]), n.FilePath)
	})
}

// A unique getter that did not change the pick can be omitted, but a known
// getter moving to another file could still redirect the Call.
func (p *rebuildCallMoves) getterFileUnchanged(wanted []byte) (bool, error) {
	state, err := rebuildScratchState(p.b.pending, wanted, p.label+" getter proof")
	return state == rebuildCapabilityWanted, err
}

func (p *rebuildCallMoves) getterFilesProved(live, wanted []byte, unique func([]byte) (string, bool, error)) (bool, error) {
	file, seen, err := unique(live)
	if err != nil {
		return false, err
	}
	exactFile, exactSeen, err := unique(p.getterProofKey(wanted))
	if err != nil || seen != exactSeen || file != exactFile {
		return false, err
	}
	if seen && file == "" {
		return true, nil // two exact files already exclude a unique file
	}
	return p.getterFileUnchanged(wanted)
}

// rebuildStoreScopeKeys changes the cohort column and extends its encoded member key.
// Both the live census and the exact-receipt fold use these same keys.
func rebuildStoreScopeKeys(cohort []byte, col int, global byte, n *graph.Node) [][]byte {
	key := func(kind byte, scope ...string) []byte {
		k := bytes.Clone(cohort)
		k[col] = kind
		return keyStrings(k, scope...)
	}
	return [][]byte{
		key(global),
		key('w', n.WorkspaceID),
		key('f', n.FilePath),
		key('s', n.WorkspaceID, n.FilePath),
	}
}
