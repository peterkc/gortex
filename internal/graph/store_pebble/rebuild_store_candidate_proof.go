package store_pebble

import (
	"bytes"

	"github.com/zzet/gortex/internal/graph"
)

// Candidate proof needs the selected singleton and any higher-priority
// ambiguous file that fell through. With no winner, two scoped candidates
// also rule out a singleton before the file checks. Empty buckets use the
// live census, but must not gain a candidate from a conflicting receipt.
type rebuildStoreCandidatePick struct {
	target   string
	buckets  []rebuildStoreCandidateWitness
	nonempty bool // the scoped pool is not empty
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
	for _, file := range [...]string{getterFile, callerFile} {
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

func (p *rebuildCallMoves) countCandidateBuckets(buckets [][]byte) error {
	for _, bucket := range buckets {
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

// An inexact known candidate may move into a deciding scope. Record only
// the buckets it enters; changing an irrelevant loser's workspace does not
// invalidate a different file's winner.
func (p *rebuildCallMoves) noteCandidateScopeChange(cohort []byte, column int, global byte, raw, live *graph.Node) error {
	before := rebuildStoreScopeKeys(cohort, column, global, live)
	for i, bucket := range rebuildStoreScopeKeys(cohort, column, global, raw) {
		if bytes.Equal(bucket, before[i]) {
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

// Recover the candidate's scope from its live declaration.
func (p *rebuildCallMoves) foldExactCandidate(r row, prefix []byte, column int, global byte,
	node func(string) (*graph.Node, error),
) error {
	id, rest, err := readKeyString(r.key[len(prefix):])
	if err != nil || len(rest) != 0 || id == "" {
		return invalid(p.label + " candidate proof key")
	}
	n, err := node(id)
	if err != nil {
		return err
	}
	if n == nil {
		return invalid(p.label + " candidate proof has no live node")
	}
	return p.countCandidateBuckets(rebuildStoreScopeKeys(r.value[1:], column, global, n))
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
