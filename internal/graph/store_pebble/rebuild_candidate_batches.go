package store_pebble

import (
	"bytes"
	"context"
	"errors"
	"slices"

	"github.com/cockroachdb/pebble/v2"
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

// Both immutable table construction and live sorting encode the decoded
// scratch node. A fallback JSON value can normalize to a different encoding.
// Check the second immutable coordinator encoding when normalization changes
// the raw bytes. An unproved size keeps the original scalar path.
func rebuildCandidateEncodedSize(raw []byte) uint64 {
	n, err := decodeNode(raw)
	if err != nil {
		return 0
	}
	encoded, err := encodeNode(n)
	if err != nil {
		return 0
	}
	if !bytes.Equal(raw, encoded) {
		n, err = decodeNode(encoded)
		if err != nil {
			return 0
		}
		again, err := encodeNode(n)
		if err != nil || len(again) != len(encoded) {
			return 0
		}
	}
	return uint64(len(encoded))
}

func (v RebuildView) ChargeCandidateRequest(ctx context.Context, req LinkRequest) (charge CandidateBatchCharge, err error) {
	if v.build == nil {
		return charge, errors.New("candidate batch sizing requires a working view")
	}
	// Qualified keys carry no repo or language; the main all-repo qualified
	// import is exact. Any narrower qualified query stays scalar.
	if !req.valid() || req.Kind == LinkQualName && (!req.AllRepos || len(req.Languages) != 0) {
		return CandidateBatchCharge{Oversize: true}, nil
	}
	column := nameRebuildScoped
	if req.Kind == LinkQualName {
		column = nameQualified
	}
	prefix := keyStrings([]byte{familyName, column}, req.Key)
	if req.Kind == LinkName && !req.AllRepos {
		prefix = appendKeyString(prefix, req.RepoPrefix)
	}
	err = v.visit(ctx, prefix, func(_ *pebble.Snapshot, r row) error {
		repo, lang, id, encoded, err := readRebuildCandidateIndex(column, r.key, r.value)
		if err != nil {
			return err
		}
		if req.Kind == LinkName && len(req.Languages) != 0 && !slices.Contains(req.Languages, lang) {
			return nil
		}
		if encoded == 0 || encoded > linkRecordBytes {
			charge.Oversize = true
			return nil
		}
		key := candidateOrderKey(req, 0, &graph.Node{ID: id, RepoPrefix: repo, Language: lang}, 0)
		size := len(key) + int(encoded)
		if size > linkRecordBytes || charge.Rows == linkResultRows || size > linkResultBytes-charge.Bytes {
			charge.Oversize = true
			return nil
		}
		charge.Rows++
		charge.Bytes += size
		return nil
	})
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
