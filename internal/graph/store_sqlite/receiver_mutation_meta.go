package store_sqlite

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/zzet/gortex/internal/contracts"
)

var (
	receiverMutationReceiverKey  = []byte("receiver")
	receiverMutationScopePkgKey  = []byte("scope_pkg")
	receiverMutationRecvFieldKey = []byte("recv_field")
	receiverMutationRecvSelfKey  = []byte("recv_self")
)

// decodeReceiverMutationMeta validates every persisted metadata value while
// retaining only the scalar fields used by receiver-mutation synthesis.
// Current flat blobs are traversed without materializing a map; legacy JSON and
// gob rows retain the authoritative decodeMeta behavior.
func decodeReceiverMutationMeta(blob []byte) (receiver, recvField string, recvSelf bool, scopePkg string, err error) {
	if len(blob) == 0 {
		return "", "", false, "", nil
	}
	if !isFlatMeta(blob) {
		var meta map[string]any
		meta, err = decodeMeta(blob)
		if err != nil {
			return "", "", false, "", err
		}
		receiver, _ = meta["receiver"].(string)
		scopePkg, _ = meta["scope_pkg"].(string)
		recvField, _ = meta["recv_field"].(string)
		recvSelf, _ = meta["recv_self"].(bool)
		return receiver, recvField, recvSelf, scopePkg, nil
	}

	d := &metaDecoder{buf: blob[2:]}
	count, err := d.readCount()
	if err != nil {
		return "", "", false, "", err
	}
	for i := 0; i < count; i++ {
		key, readErr := d.readRawString()
		if readErr != nil {
			return "", "", false, "", readErr
		}
		switch {
		case bytes.Equal(key, receiverMutationReceiverKey):
			value, readErr := d.readValue()
			if readErr != nil {
				return "", "", false, "", readErr
			}
			receiver, _ = value.(string)
		case bytes.Equal(key, receiverMutationScopePkgKey):
			value, readErr := d.readValue()
			if readErr != nil {
				return "", "", false, "", readErr
			}
			scopePkg, _ = value.(string)
		case bytes.Equal(key, receiverMutationRecvFieldKey):
			value, readErr := d.readValue()
			if readErr != nil {
				return "", "", false, "", readErr
			}
			recvField, _ = value.(string)
		case bytes.Equal(key, receiverMutationRecvSelfKey):
			value, readErr := d.readValue()
			if readErr != nil {
				return "", "", false, "", readErr
			}
			recvSelf, _ = value.(bool)
		default:
			if readErr := d.skipValue(); readErr != nil {
				return "", "", false, "", readErr
			}
		}
	}
	return receiver, recvField, recvSelf, scopePkg, nil
}

func validateReceiverMutationMeta(blob []byte) error {
	_, _, _, _, err := decodeReceiverMutationMeta(blob)
	return err
}

func (d *metaDecoder) readRawString() ([]byte, error) {
	n, err := d.uvarint()
	if err != nil {
		return nil, err
	}
	if n > uint64(len(d.buf)-d.pos) {
		return nil, errMetaTruncated
	}
	return d.readBytes(int(n))
}

// skipValue validates and advances over one flat-codec value without retaining
// it. Shape payloads still receive the same JSON validation as readValue.
func (d *metaDecoder) skipValue() error {
	tag, err := d.readByte()
	if err != nil {
		return err
	}
	switch tag {
	case metaTagNil:
		return nil
	case metaTagString:
		_, err = d.readRawString()
		return err
	case metaTagBool:
		_, err = d.readByte()
		return err
	case metaTagInt, metaTagInt64:
		_, err = d.varint()
		return err
	case metaTagUint64:
		_, err = d.uvarint()
		return err
	case metaTagFloat64:
		_, err = d.readBytes(8)
		return err
	case metaTagF64Slice:
		count, countErr := d.readCount()
		if countErr != nil {
			return countErr
		}
		if count > (len(d.buf)-d.pos)/8 {
			return errMetaTruncated
		}
		_, err = d.readBytes(count * 8)
		return err
	case metaTagStrSlice:
		count, countErr := d.readCount()
		if countErr != nil {
			return countErr
		}
		for i := 0; i < count; i++ {
			if _, err = d.readRawString(); err != nil {
				return err
			}
		}
		return nil
	case metaTagMap:
		return d.skipMap()
	case metaTagMapSlice:
		count, countErr := d.readCount()
		if countErr != nil {
			return countErr
		}
		for i := 0; i < count; i++ {
			if err = d.skipMap(); err != nil {
				return err
			}
		}
		return nil
	case metaTagAnySlice:
		count, countErr := d.readCount()
		if countErr != nil {
			return countErr
		}
		for i := 0; i < count; i++ {
			if err = d.skipValue(); err != nil {
				return err
			}
		}
		return nil
	case metaTagShape:
		payload, readErr := d.readRawString()
		if readErr != nil {
			return readErr
		}
		var shape *contracts.Shape
		return json.Unmarshal(payload, &shape)
	default:
		return fmt.Errorf("store_sqlite: unknown meta value tag 0x%02x", tag)
	}
}

func (d *metaDecoder) skipMap() error {
	count, err := d.readCount()
	if err != nil {
		return err
	}
	for i := 0; i < count; i++ {
		if _, err = d.readRawString(); err != nil {
			return err
		}
		if err = d.skipValue(); err != nil {
			return err
		}
	}
	return nil
}
