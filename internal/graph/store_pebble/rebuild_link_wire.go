package store_pebble

import (
	"encoding/binary"
	"errors"
)

const linkFrameBytes = 2 << 20 // historical logical aggregate request bound
const linkLanguageLimit = 64

func (r LinkRequest) valid() bool {
	return (r.Kind == LinkName || r.Kind == LinkQualName) && r.Key != "" && len(r.Languages) <= linkLanguageLimit
}

func appendLinkRequest(b []byte, r LinkRequest, position int, remaining int) ([]byte, error) {
	// Include fixed fields and a worst-case length prefix for each string
	// before appending anything from the caller to the bounded frame.
	remaining -= 2 + 2*binary.MaxVarintLen64
	for _, s := range [...]string{r.Key, r.RepoPrefix} {
		remaining -= binary.MaxVarintLen64 + len(s)
		if remaining < 0 {
			return nil, errors.New("link request page exceeds wire byte bound")
		}
	}
	for _, s := range r.Languages {
		remaining -= binary.MaxVarintLen64 + len(s)
		if remaining < 0 {
			return nil, errors.New("link request page exceeds wire byte bound")
		}
	}
	b = binary.AppendVarint(b, int64(position))
	b = append(b, byte(r.Kind))
	b = appendMetaString(b, r.Key)
	b = appendMetaString(b, r.RepoPrefix)
	b = appendBool(b, r.AllRepos)
	b = binary.AppendVarint(b, int64(len(r.Languages)))
	for _, s := range r.Languages {
		b = appendMetaString(b, s)
	}
	return b, nil
}
