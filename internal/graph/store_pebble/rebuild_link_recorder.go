package store_pebble

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/graph"
)

// This family lives only in the private build. Indexed by winning live key,
// it records the original complete receipt key or the producer proof without
// changing the saved edge row.
var rebuildLinkRecorderPrefix = []byte{'g', 'Z'}

func rebuildLinkRecorderKey(live []byte) []byte {
	return append(bytes.Clone(rebuildLinkRecorderPrefix), live...)
}

// Each code identifies a producer and the proof that admitted its row.
type rebuildEdgeProof byte

const (
	// Exact source recorder and endpoint declarations.
	rebuildProofDirect rebuildEdgeProof = iota + 1
	// Checked receiver Call and field ownership.
	rebuildProofReceiver
	// Wrapper trigger or cross-language role proof.
	rebuildProofTemporalSynthetic
	// Emits/ListensOn, transport, and declaration proof.
	rebuildProofEventChannel
	// Declarations and selector/protocol metadata.
	rebuildProofSwiftObjC
	// First source recorder and declaration proof.
	rebuildProofRNBridge
	// Declarations and export metadata.
	rebuildProofRNPair
	// A move without a recorder is not a stationary receipt or a proof.
	rebuildProofUntrackedMove
	// Checked field accesses, registrar calls, callback references and declarations.
	rebuildProofObserver
	// Both callable declarations and collection-role metadata.
	rebuildProofClosureCollection
)

type rebuildEdgeCustody struct {
	raw   []byte
	proof rebuildEdgeProof
}

const rebuildEdgeProofFormat byte = 1

func rebuildObserverInputKind(kind graph.EdgeKind) bool {
	switch kind {
	case graph.EdgeAccessesField, graph.EdgeCalls, graph.EdgeReads, graph.EdgeReferences:
		return true
	default:
		return false
	}
}

func (c rebuildEdgeCustody) encode(edge []byte) ([]byte, error) {
	if c.proof == 0 {
		return c.raw, nil
	}
	if c.proof > rebuildProofClosureCollection || len(c.raw) != 0 {
		return nil, invalid("edge custody proof")
	}
	digest := sha256.Sum256(edge)
	return append([]byte{rebuildEdgeProofFormat, byte(c.proof)}, digest[:]...), nil
}

func readRebuildEdgeCustody(reader pebble.Reader, live []byte) (rebuildEdgeCustody, error) {
	return pendingGet(reader, rebuildLinkRecorderKey(live), func(raw []byte) (rebuildEdgeCustody, error) {
		if len(raw) >= 2 && raw[0] == familyEdge && raw[1] == edgeOut {
			return rebuildEdgeCustody{raw: bytes.Clone(raw)}, nil
		}
		if len(raw) != 2+sha256.Size || raw[0] != rebuildEdgeProofFormat || raw[1] == 0 || rebuildEdgeProof(raw[1]) > rebuildProofClosureCollection {
			return rebuildEdgeCustody{}, invalid("edge custody format")
		}
		match, err := pendingGet(reader, live, func(edge []byte) (bool, error) {
			digest := sha256.Sum256(edge)
			return bytes.Equal(digest[:], raw[2:]), nil
		})
		if err != nil {
			return rebuildEdgeCustody{}, err
		}
		if !match {
			id, err := decodeLinkRecorder(live)
			if err != nil {
				return rebuildEdgeCustody{}, err
			}
			return rebuildEdgeCustody{}, &RebuildUnknownProvenanceError{Count: 1, First: id}
		}
		return rebuildEdgeCustody{proof: rebuildEdgeProof(raw[1])}, nil
	})
}

// OriginalLinkRecorder returns the raw identity for an installed move. An
// synthetic or stationary row has no original move link. This method does not
// distinguish those cases; use custody when an absent link is not enough.
func (v RebuildView) OriginalLinkRecorder(ctx context.Context, live graph.EdgeIdentity) (raw graph.EdgeIdentity, found bool, err error) {
	key := pendingEdgeKey(&graph.Edge{From: live.From, To: live.To, Kind: live.Kind, FilePath: live.FilePath, Line: live.Line})
	err = v.read(ctx, func(snap *pebble.Snapshot) error {
		custody, err := readRebuildEdgeCustody(snap, key)
		if err != nil || custody.raw == nil {
			return err
		}
		raw, err = decodeLinkRecorder(custody.raw)
		found = err == nil
		return err
	})
	return raw, found, err
}

func decodeLinkRecorder(key []byte) (graph.EdgeIdentity, error) {
	var out graph.EdgeIdentity
	var kind string
	tail := key[2:]
	for _, field := range []*string{&out.From, &kind, &out.To, &out.FilePath} {
		var err error
		*field, tail, err = readKeyString(tail)
		if err != nil {
			return graph.EdgeIdentity{}, err
		}
	}
	if len(tail) != 8 {
		return graph.EdgeIdentity{}, invalid("link recorder line")
	}
	out.Kind = graph.EdgeKind(kind)
	out.Line = int(int64(binary.BigEndian.Uint64(tail) ^ (1 << 63)))
	return out, nil
}
