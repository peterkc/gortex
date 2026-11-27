package indexer

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/zeebo/blake3"
)

var (
	errParseEntryCorrupt  = errors.New("corrupt parse-store entry")
	errParseEntryConflict = errors.New("different parse-store entries share a key")
	errParseEntryTooLarge = errors.New("parse-store entry exceeds the worker limit")
)

const parseEntryFormat byte = 1
const parseEntryHeader = 1 + 32 + 32 // format, key, compact-payload digest

type rebuildParseStore struct {
	dir   string
	limit int
}

func (s rebuildParseStore) entry(key rebuildDigest) string {
	return filepath.Join(s.dir, hex.EncodeToString(key[:]))
}

func (s rebuildParseStore) read(key rebuildDigest) (payload []byte, err error) {
	name := s.entry(key)
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Size() <= parseEntryHeader {
		return nil, fmt.Errorf("%w: %x", errParseEntryCorrupt, key)
	}
	if st.Size()-parseEntryHeader > int64(s.limit) {
		return nil, fmt.Errorf("%w: %x", errParseEntryTooLarge, key)
	}
	var header [parseEntryHeader]byte
	if _, err := io.ReadFull(f, header[:]); err != nil {
		return nil, fmt.Errorf("%w: %x: %w", errParseEntryCorrupt, key, err)
	}
	size := int(st.Size() - parseEntryHeader)
	payload = make([]byte, size)
	if _, err := io.ReadFull(f, payload); err != nil {
		return nil, fmt.Errorf("%w: %x: %w", errParseEntryCorrupt, key, err)
	}
	var extra [1]byte
	if _, err := io.ReadFull(f, extra[:]); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: %x: trailing data or read error: %v", errParseEntryCorrupt, key, err)
	}
	if err := checkRebuildParseEntryHeader(key, header[:], payload); err != nil {
		return nil, err
	}
	return payload, nil
}

func (s rebuildParseStore) write(key rebuildDigest, payload []byte) (err error) {
	if len(payload) == 0 {
		return fmt.Errorf("%w: empty payload", errParseEntryCorrupt)
	}
	if len(payload) > s.limit {
		return fmt.Errorf("%w: %x", errParseEntryTooLarge, key)
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(s.dir, ".parse-*")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, os.Remove(f.Name())) }()
	header := makeRebuildParseEntryHeader(key, payload)
	_, err = f.Write(header[:])
	if err == nil {
		_, err = f.Write(payload)
	}
	if err == nil {
		err = f.Sync()
	}
	if err = errors.Join(err, f.Close()); err != nil {
		return err
	}
	// Rename replaces an existing file. A same-directory link publishes the
	// complete inode atomically and refuses to overwrite a concurrent winner.
	err = os.Link(f.Name(), s.entry(key))
	if !errors.Is(err, os.ErrExist) {
		return err
	}
	prior, err := s.read(key)
	if err != nil {
		return err
	}
	if !bytes.Equal(prior, payload) {
		return fmt.Errorf("%w: %x", errParseEntryConflict, key)
	}
	return nil
}

// The worker reply and the leased store use the same key and digest framing.
func makeRebuildParseEntryHeader(key rebuildDigest, payload []byte) [parseEntryHeader]byte {
	var header [parseEntryHeader]byte
	header[0] = parseEntryFormat
	copy(header[1:33], key[:])
	digest := blake3.Sum256(payload)
	copy(header[33:], digest[:])
	return header
}

func checkRebuildParseEntryHeader(key rebuildDigest, header, payload []byte) error {
	if len(header) != parseEntryHeader {
		return fmt.Errorf("%w: %x", errParseEntryCorrupt, key)
	}
	want := makeRebuildParseEntryHeader(key, payload)
	if !bytes.Equal(header, want[:]) {
		return fmt.Errorf("%w: %x", errParseEntryCorrupt, key)
	}
	return nil
}
