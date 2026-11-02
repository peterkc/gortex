package runner

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// emptyBuildWAL recognizes only no bytes or Pebble v2.1.7's exact EOF trailer.
// LogWriter.emitEOFTrailer writes CRC=0, size=0, recyclable-full type=5 and
// low32(file number)+1. A real record, even an empty one, has a different header.
func emptyBuildWAL(name string, raw []byte) bool {
	number := strings.TrimSuffix(name, ".log")
	if number == name || number == "" {
		return false
	}
	for _, digit := range number {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	value, err := strconv.ParseUint(number, 10, 64)
	if err != nil {
		return false
	}
	if len(raw) == 0 {
		return true
	}
	expected := [11]byte{6: 5}
	binary.LittleEndian.PutUint32(expected[7:], uint32(value)+1)
	return bytes.Equal(raw, expected[:])
}
func sameBuildDirectory(path string, owned os.FileInfo) error {
	actual, err := os.Lstat(path)
	if err != nil {
		return err
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	if owned == nil || !owned.IsDir() || !actual.IsDir() || canonical != path || !os.SameFile(owned, actual) {
		return fmt.Errorf("build directory identity/alias changed: %s", path)
	}
	return nil
}
func readEmptyBuildWAL(root *os.Root, name string) (os.FileInfo, error) {
	identity, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !identity.Mode().IsRegular() {
		return nil, fmt.Errorf("nonregular build WAL: %s", name)
	}
	file, err := root.Open(name)
	if err != nil {
		return nil, err
	}
	opened, err := file.Stat()
	if err != nil {
		return nil, errors.Join(err, file.Close())
	}
	if !os.SameFile(identity, opened) {
		return nil, errors.Join(fmt.Errorf("build WAL identity changed: %s", name), file.Close())
	}
	// Twelve bytes distinguish the exact eleven-byte trailer from any suffix.
	raw, readErr := io.ReadAll(io.LimitReader(file, 12))
	after, statErr := file.Stat()
	closeErr := file.Close()
	if err := errors.Join(readErr, statErr, closeErr); err != nil {
		return nil, err
	}
	if !os.SameFile(identity, after) || identity.Size() != after.Size() || !identity.ModTime().Equal(after.ModTime()) {
		return nil, fmt.Errorf("build WAL changed while reading: %s", name)
	}
	if !emptyBuildWAL(name, raw) {
		return nil, fmt.Errorf("record-bearing or unrecognized build WAL: %s", name)
	}
	return identity, nil
}

// RemoveEmptyBuildWALs canonicalizes an unpublished, successfully flushed and
// closed Pebble build. The caller captures owned before writing. No writer may
// remain active. Every WAL is validated before any removal; non-WAL files stay.
// The enclosing seal syncs the directory after this returns. Directory-relative
// operations retain the checked directory across renames.
func RemoveEmptyBuildWALs(path string, owned os.FileInfo) (err error) {
	if err = sameBuildDirectory(path, owned); err != nil {
		return err
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, directory.Close()) }()
	actual, err := directory.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(owned, actual) {
		return fmt.Errorf("opened build directory identity changed")
	}
	entries, err := directory.ReadDir(-1)
	if err != nil {
		return err
	}
	var pending []struct {
		name     string
		identity os.FileInfo
	}
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".log" {
			continue
		}
		identity, err := readEmptyBuildWAL(root, entry.Name())
		if err != nil {
			return err
		}
		pending = append(pending, struct {
			name     string
			identity os.FileInfo
		}{entry.Name(), identity})
	}
	if err = sameBuildDirectory(path, owned); err != nil {
		return err
	}
	// Check all identities again before the first irreversible effect.
	for _, file := range pending {
		actual, err := root.Lstat(file.name)
		if err != nil {
			return err
		}
		if !actual.Mode().IsRegular() || !os.SameFile(file.identity, actual) || file.identity.Size() != actual.Size() || !file.identity.ModTime().Equal(actual.ModTime()) {
			return fmt.Errorf("build WAL identity/content changed before removal: %s", file.name)
		}
	}
	for _, file := range pending {
		if err = root.Remove(file.name); err != nil {
			return err
		}
	}
	return nil
}
