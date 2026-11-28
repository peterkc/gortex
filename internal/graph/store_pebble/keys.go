package store_pebble

import (
	"encoding/binary"

	"github.com/zeebo/blake3"
	"github.com/zzet/gortex/internal/graph"
)

const (
	familyNode     byte = 'N'
	familyFile     byte = 'F'
	familyName     byte = 'M'
	familyRepo     byte = 'R'
	familyNodeKind byte = 'K'
	familyEdge     byte = 'E'
	familyCount    byte = 'C'
	familySymbol   byte = 'S'
	familyShape    byte = 'T'
	familyOmission byte = 'O'
	familyFileMeta byte = 'A'
	familyWitness  byte = 'W'
)

const (
	edgeOut byte = iota + 1
	edgeIn
	edgeInRepo
	edgeOutKind
	edgeKind
)

func appendKeyString(b []byte, s string) []byte {
	for i := range len(s) {
		b = append(b, s[i])
		if s[i] == 0 {
			b = append(b, 0xff)
		}
	}
	return append(b, 0, 0)
}

func keyStrings(b []byte, fields ...string) []byte {
	for _, field := range fields {
		b = appendKeyString(b, field)
	}
	return b
}

func appendKeyLine(b []byte, line int) []byte {
	return binary.BigEndian.AppendUint64(b, uint64(int64(line))^(1<<63))
}

func prefixEnd(prefix []byte) []byte {
	for i := len(prefix) - 1; i >= 0; i-- {
		if prefix[i] != 0xff {
			end := append([]byte(nil), prefix[:i+1]...)
			end[i]++
			return end
		}
	}
	return nil
}

// The same provenance-bearing fields as graph.Edge.IdentityHash, with an
// unambiguous byte encoding and BLAKE3 as the on-disk tie breaker.
func edgeIdentity(e *graph.Edge) [16]byte {
	b := keyStrings(nil, e.From, string(e.Kind), e.To, e.FilePath)
	b = appendKeyLine(b, e.Line)
	b = appendKeyString(b, e.Origin)
	hash := blake3.Sum256(b)
	return [16]byte(hash[:16])
}

func outEdgeKey(e *graph.Edge, id [16]byte) []byte {
	key := appendKeyString([]byte{familyEdge, edgeOut}, e.From)
	return append(appendKeyLine(key, e.Line), id[:]...)
}

func readKeyString(b []byte) (string, []byte, error) {
	var out []byte
	start := 0
	for i := 0; i < len(b); i++ {
		if b[i] != 0 {
			continue
		}
		if i+1 == len(b) {
			break
		}
		switch b[i+1] {
		case 0:
			if out == nil {
				return string(b[:i]), b[i+2:], nil
			}
			return string(append(out, b[start:i]...)), b[i+2:], nil
		case 0xff:
			out = append(out, b[start:i+1]...)
			i++
			start = i + 1
		default:
			return "", nil, invalid("key escape")
		}
	}
	return "", nil, invalid("key terminator")
}
