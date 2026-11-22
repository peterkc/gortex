package indexer

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/zeebo/blake3"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

const rebuildCheckpointName = "checkpoint"

// checkpointRebuildHead uses the owner's pinned head directory and exact head
// identity. The owner provides an existing, durable lease directory and holds
// it exclusively until the result is accepted or discarded. The owner handles
// cleanup of incomplete candidates and only registers a successful result.
func checkpointRebuildHead(ctx context.Context, head *store_pebble.Head, headDir, leaseDir, incarnation string, profile store_pebble.Profile) (store_pebble.LocalPublicationRecord, error) {
	var zero store_pebble.LocalPublicationRecord
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	if err := store_pebble.ValidateIncarnation(incarnation); err != nil {
		return zero, err
	}
	dest, err := freshRebuildCheckpointPath(headDir, leaseDir)
	if err != nil {
		return zero, err
	}
	if err := head.Checkpoint(dest); err != nil {
		return zero, fmt.Errorf("checkpoint rebuild head: %w", err)
	}
	// Only the destination can say which witness Pebble captured.
	checkpoint, err := store_pebble.OpenHead(dest, incarnation, profile)
	if err != nil {
		return zero, fmt.Errorf("open rebuild checkpoint: %w", err)
	}
	w, record, err := checkpoint.ReconcilePublication(nil)
	if err := errors.Join(err, checkpoint.Close()); err != nil {
		return zero, fmt.Errorf("read rebuild checkpoint witness: %w", err)
	}
	digest := rebuildDigest(w.FrozenInputs)
	if err := copyRebuildInputSidecar(ctx, retainedRebuildInputPath(headDir, digest), retainedRebuildInputPath(dest, digest), digest); err != nil {
		return zero, fmt.Errorf("retain rebuild checkpoint sidecar: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	return record, nil
}

func freshRebuildCheckpointPath(headDir, leaseDir string) (string, error) {
	if !filepath.IsAbs(headDir) || filepath.Clean(headDir) != headDir ||
		!filepath.IsAbs(leaseDir) || filepath.Clean(leaseDir) != leaseDir {
		return "", fmt.Errorf("checkpoint requires clean absolute owner paths: %w", os.ErrInvalid)
	}
	lease, err := os.Lstat(leaseDir)
	if err != nil {
		return "", fmt.Errorf("inspect checkpoint lease: %w", err)
	}
	if !lease.IsDir() || filepath.Dir(leaseDir) == leaseDir {
		return "", fmt.Errorf("checkpoint lease must be a private directory: %w", os.ErrInvalid)
	}
	head, err := os.Stat(headDir)
	if err != nil {
		return "", fmt.Errorf("inspect checkpoint source directory: %w", err)
	}
	if !head.IsDir() {
		return "", fmt.Errorf("checkpoint source is not a directory: %w", os.ErrInvalid)
	}
	// Check the physical ancestors, not a string prefix which can miss aliases.
	realLease, err := filepath.EvalSymlinks(leaseDir)
	if err != nil {
		return "", fmt.Errorf("resolve checkpoint lease: %w", err)
	}
	for dir := realLease; ; dir = filepath.Dir(dir) {
		info, err := os.Stat(dir)
		if err != nil {
			return "", fmt.Errorf("inspect checkpoint lease ancestor: %w", err)
		}
		if os.SameFile(info, head) {
			return "", fmt.Errorf("checkpoint lease is inside the source head: %w", os.ErrInvalid)
		}
		if dir == filepath.Dir(dir) {
			break
		}
	}
	dest := filepath.Join(leaseDir, rebuildCheckpointName)
	// Lstat also refuses dangling links, which Pebble's Stat misses. Its
	// error cleanup is safe only while the caller exclusively holds the name.
	_, err = os.Lstat(dest)
	if err == nil {
		return "", fmt.Errorf("checkpoint destination %q: %w", dest, os.ErrExist)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("inspect checkpoint destination: %w", err)
	}
	return dest, nil
}

// retainRebuildInputSidecar runs under the head owner's directory pin, before
// publishing a witness with this digest. The lease input has already passed
// capture and its codec limits. Only the owner's exact private paths are valid.
func retainRebuildInputSidecar(ctx context.Context, headDir, lease string, digest rebuildDigest) error {
	return copyRebuildInputSidecar(ctx, filepath.Join(lease, rebuildInputSidecarName), retainedRebuildInputPath(headDir, digest), digest)
}

// restoreRebuildInputSidecar reopens only the pinned witness's input. Copy it
// into a fresh owner lease so the existing worker can replay its usual file
// without opening the head. Zero means explicitly unbound, not a failed lookup.
func restoreRebuildInputSidecar(ctx context.Context, headDir, lease string, digest rebuildDigest) error {
	return copyRebuildInputSidecar(ctx, retainedRebuildInputPath(headDir, digest), filepath.Join(lease, rebuildInputSidecarName), digest)
}

func retainedRebuildInputPath(dir string, digest rebuildDigest) string {
	return filepath.Join(dir, rebuildInputSidecarName+"-"+hex.EncodeToString(digest[:]))
}

// Both paths are inside existing private directories pinned by their owner.
// Never link directly from worker scratch or remove an existing destination.
func copyRebuildInputSidecar(ctx context.Context, from, to string, digest rebuildDigest) (err error) {
	if digest == (rebuildDigest{}) {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !filepath.IsAbs(from) || !filepath.IsAbs(to) {
		return fmt.Errorf("%w: non-absolute owner path", errRebuildInputSidecar)
	}
	dir := filepath.Dir(to)
	parent, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("%w: open destination directory: %w", errRebuildInputSidecar, err)
	}
	defer func() { err = errors.Join(err, parent.Close()) }()

	tmp, err := os.CreateTemp(dir, ".build-inputs-*")
	if err != nil {
		return fmt.Errorf("%w: stage sidecar copy: %w", errRebuildInputSidecar, err)
	}
	defer func() { err = errors.Join(err, os.Remove(tmp.Name())) }()
	if err := streamRebuildInputSidecar(from, tmp, digest); err != nil {
		return errors.Join(err, tmp.Close())
	}
	if err := errors.Join(tmp.Sync(), tmp.Close()); err != nil {
		return fmt.Errorf("%w: sync sidecar copy: %w", errRebuildInputSidecar, err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	// Link the complete synced inode without replacing an existing binding.
	if err := os.Link(tmp.Name(), to); errors.Is(err, os.ErrExist) {
		if err := streamRebuildInputSidecar(to, io.Discard, digest); err != nil {
			return fmt.Errorf("retain existing sidecar: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("%w: link sidecar copy: %w", errRebuildInputSidecar, err)
	}
	// Sync the final name even on reuse: a prior owner may have crashed after
	// linking but before syncing the directory. Never undo a visible binding.
	if err := parent.Sync(); err != nil {
		return fmt.Errorf("%w: sync sidecar directory: %w", errRebuildInputSidecar, err)
	}
	return nil
}

// The owner keeps these directories stable. Match the opened file to the
// lstat observation, as in visitRebuildInputSidecar, and bound the actual read.
func streamRebuildInputSidecar(name string, dst io.Writer, digest rebuildDigest) (err error) {
	before, err := os.Lstat(name)
	if err != nil {
		return fmt.Errorf("%w: inspect sidecar: %w", errRebuildInputSidecar, err)
	}
	if !before.Mode().IsRegular() {
		return fmt.Errorf("%w: sidecar is not a private regular file", errRebuildInputSidecar)
	}
	f, err := os.Open(name)
	if err != nil {
		return fmt.Errorf("%w: open sidecar: %w", errRebuildInputSidecar, err)
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("%w: stat sidecar: %w", errRebuildInputSidecar, err)
	}
	if !os.SameFile(before, info) || !info.Mode().IsRegular() || info.Size() < int64(len(rebuildInputSidecarMagic)) {
		return fmt.Errorf("%w: sidecar size or kind", errRebuildInputSidecar)
	}
	if info.Size() > rebuildInputEncodedLimit {
		return fmt.Errorf("%w: encoded input cap: %w", errRebuildInputSidecar, errRebuildCppCaptureLimit)
	}
	hash := blake3.New()
	n, err := io.Copy(io.MultiWriter(dst, hash), io.LimitReader(f, rebuildInputEncodedLimit+1))
	if err != nil {
		return fmt.Errorf("%w: read or copy sidecar: %w", errRebuildInputSidecar, err)
	}
	if n > rebuildInputEncodedLimit {
		return fmt.Errorf("%w: encoded input cap: %w", errRebuildInputSidecar, errRebuildCppCaptureLimit)
	}
	if n != info.Size() || rebuildDigest(hash.Sum(nil)) != digest {
		return fmt.Errorf("%w: sidecar byte digest", errRebuildInputSidecar)
	}
	return nil
}
