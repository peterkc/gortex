package store_pebble

import (
	"context"
	"errors"

	"github.com/zzet/gortex/internal/graph"
)

// CandidateBatchCharge is the exact order-stream row and byte charge for a
// scalar request on the stationary main precommit node view. The order key
// includes the 12 bytes of the child result key plus the native sort tuple,
// so the same bound also covers both child and merged result caps.
type CandidateBatchCharge struct {
	Rows, Bytes int
	Oversize    bool
}

func (v RebuildView) ChargeCandidateRequest(ctx context.Context, req LinkRequest) (charge CandidateBatchCharge, err error) {
	if v.build == nil {
		return charge, errors.New("candidate batch sizing requires a working view")
	}
	if !req.valid() {
		return CandidateBatchCharge{Oversize: true}, nil // original scalar path owns refusal
	}
	// The main frontier freezes VisitNodes before its first page, then mutates
	// only edge keys. These exact name/qualified indexes project the same
	// node rows. They are used solely for sizing, never selecting candidates.
	visit := func(n *graph.Node) error {
		if !req.match(n) {
			return nil
		}
		value, err := encodeNode(n)
		if err != nil {
			return err
		}
		size := len(candidateOrderKey(req, 0, n, 0)) + len(value)
		if size > linkRecordBytes || charge.Rows == linkResultRows || size > linkResultBytes-charge.Bytes {
			charge.Oversize = true
			return nil // keep validating the read; do not grow counters beyond the caps
		}
		charge.Rows++
		charge.Bytes += size
		return nil
	}
	if req.Kind == LinkName && !req.AllRepos {
		// The scratch index tuple is (name, repository, language, ID).
		// Do not read other repos just to discard them from the size.
		prefix := keyStrings([]byte{familyName, nameRebuildScoped}, req.Key, req.RepoPrefix)
		err = v.visitCandidatePrefix(ctx, nameRebuildScoped, prefix, nil, visit)
	} else if req.Kind == LinkName {
		err = v.VisitNameCandidates(ctx, req.Key, nil, visit)
	} else {
		err = v.VisitQualNameCandidates(ctx, req.Key, visit)
	}
	return charge, err
}

// CandidateBatchLimit admits an ordered sub-batch before dispatch. It uses the
// existing wire encoder and result caps. The greedy sort flush needs at most
// one run per row, and adjacent nonempty runs together exceed the spill limit.
// No scalar request is refused by these conservative aggregate limits: a
// request that cannot be grouped runs alone on the original path.
type CandidateBatchLimit struct {
	count, wire, rows, bytes int
}

func (b *CandidateBatchLimit) Add(req LinkRequest, charge CandidateBatchCharge, spill int) bool {
	if b.count == LinkPageRequests || charge.Oversize || !req.valid() || spill < 1 {
		return false
	}
	wire, err := appendLinkRequest(nil, req, b.count, linkFrameBytes-b.wire)
	if err != nil {
		return false
	}
	rows, bytes := b.rows+charge.Rows, b.bytes+charge.Bytes
	if rows > linkResultRows || bytes > linkResultBytes || min(rows, 2*(bytes+48*rows)/spill+1) > goldFileCap {
		return false
	}
	b.count++
	b.wire += len(wire)
	b.rows, b.bytes = rows, bytes
	return true
}
