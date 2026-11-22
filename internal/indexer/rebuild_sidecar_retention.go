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
)

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
