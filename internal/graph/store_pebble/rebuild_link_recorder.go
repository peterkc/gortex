package store_pebble

import (
	"bytes"
	"context"
	"encoding/binary"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/graph"
)

// This family lives only in the private build. Indexed by winning live key,
// it records the original complete key without changing the saved edge row.
var rebuildLinkRecorderPrefix = []byte{'g', 'Z'}

func rebuildLinkRecorderKey(live []byte) []byte {
	return append(bytes.Clone(rebuildLinkRecorderPrefix), live...)
}

func readLinkRecorder(reader pebble.Reader, live []byte) ([]byte, error) {
	return pendingGet(reader, rebuildLinkRecorderKey(live), func(raw []byte) ([]byte, error) {
		if len(raw) < 2 || raw[0] != familyEdge || raw[1] != edgeOut {
			return nil, invalid("link recorder identity")
		}
		return bytes.Clone(raw), nil
	})
}

// OriginalLinkRecorder returns the raw identity for an installed move. An
// unmoved row has no link; its exact current key can be checked in receipts.
func (v RebuildView) OriginalLinkRecorder(ctx context.Context, live graph.EdgeIdentity) (raw graph.EdgeIdentity, found bool, err error) {
	key := pendingEdgeKey(&graph.Edge{From: live.From, To: live.To, Kind: live.Kind, FilePath: live.FilePath, Line: live.Line})
	err = v.read(ctx, func(snap *pebble.Snapshot) error {
		encoded, err := readLinkRecorder(snap, key)
		if err != nil || encoded == nil {
			return err
		}
		raw, err = decodeLinkRecorder(encoded)
		found = err == nil
		return err
	})
	return raw, found, err
}

// RequireOriginalLinkRecorder refuses when a consumer already knows that
// the current identity is not raw and its move link is absent.
func (v RebuildView) RequireOriginalLinkRecorder(ctx context.Context, live graph.EdgeIdentity) (graph.EdgeIdentity, error) {
	raw, found, err := v.OriginalLinkRecorder(ctx, live)
	if err == nil && !found {
		err = &RebuildUnknownProvenanceError{Count: 1, First: live}
	}
	return raw, err
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
