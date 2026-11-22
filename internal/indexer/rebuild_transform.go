package indexer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/zzet/gortex/internal/platform"
)

var ErrBuildTransformBounds = errors.New("build transform exceeded its envelope")

// rebuildTransformBounds is allocated by the build owner. CaptureBytes is
// the pipe buffer; FileBytes bounds the final byte slice passed to the parser.
// DiskReserve is the space the owner must leave free on the spill volume.
type rebuildTransformBounds struct {
	Dir          string
	CaptureBytes int
	FileBytes    int
	DiskReserve  uint64
}

func (p *transformPipeline) runRebuild(ctx context.Context, path string, src []byte, bounds rebuildTransformBounds) ([]byte, int64, error) {
	var spilled int64
	data, err := p.runWith(path, src, func(t contentTransform, path string, in []byte) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrBuildTransformBounds, err)
		}
		if cmd, ok := t.(*commandTransform); ok {
			out, bytes, err := cmd.applyRebuild(ctx, in, bounds)
			spilled += bytes
			return out, err
		}
		return t.apply(path, in)
	})
	return data, spilled, err
}

func (c *commandTransform) applyRebuild(ctx context.Context, src []byte, bounds rebuildTransformBounds) (_ []byte, spilled int64, err error) {
	if bounds.Dir == "" || bounds.CaptureBytes < 1 || bounds.FileBytes < bounds.CaptureBytes {
		return nil, 0, fmt.Errorf("%w: invalid worker transform allocation", ErrBuildTransformBounds)
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	stdout := rebuildTransformOutput{bounds: bounds, cancel: cancel}
	stderr := rebuildTransformOutput{bounds: rebuildTransformBounds{
		CaptureBytes: bounds.CaptureBytes, FileBytes: bounds.CaptureBytes,
	}, cancel: cancel}
	defer func() {
		if stdout.file != nil {
			spilled = int64(stdout.size)
			if cleanupErr := errors.Join(stdout.file.Close(), os.Remove(stdout.file.Name())); cleanupErr != nil {
				err = errors.Join(err, fmt.Errorf("%w: clean up transform spill: %w", ErrBuildTransformBounds, cleanupErr))
			}
		}
	}()
	// The same operator-configured argv and stdin as commandTransform.apply.
	cmd := exec.CommandContext(ctx, c.argv[0], c.argv[1:]...) //nolint:gosec
	platform.ConfigureBackgroundCommand(cmd)
	cmd.Stdin = bytes.NewReader(src)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	cmd.WaitDelay = time.Second // bound orphaned stdout or stderr after exit
	runErr := cmd.Run()
	if err := errors.Join(stdout.err, stderr.err); err != nil {
		return nil, 0, err
	}
	if err := ctx.Err(); err != nil {
		return nil, 0, fmt.Errorf("%w: %w", ErrBuildTransformBounds, err)
	}
	if errors.Is(runErr, exec.ErrWaitDelay) {
		return nil, 0, fmt.Errorf("%w: %w", ErrBuildTransformBounds, runErr)
	}
	if runErr != nil {
		if detail := strings.TrimSpace(stderr.buffer.String()); detail != "" {
			return nil, 0, fmt.Errorf("%w: %s", runErr, detail)
		}
		return nil, 0, runErr
	}
	if stdout.file == nil {
		return stdout.buffer.Bytes(), 0, nil
	}
	if _, err := stdout.file.Seek(0, io.SeekStart); err != nil {
		return nil, 0, fmt.Errorf("%w: read transform spill: %w", ErrBuildTransformBounds, err)
	}
	out := make([]byte, stdout.size)
	if _, err := io.ReadFull(stdout.file, out); err != nil {
		return nil, 0, fmt.Errorf("%w: read transform spill: %w", ErrBuildTransformBounds, err)
	}
	return out, 0, nil
}

type rebuildTransformOutput struct {
	bounds rebuildTransformBounds
	cancel context.CancelFunc
	buffer bytes.Buffer
	file   *os.File
	size   int
	err    error
}

func (w *rebuildTransformOutput) refuse(err error) (int, error) {
	w.err = fmt.Errorf("%w: %w", ErrBuildTransformBounds, err)
	w.cancel()
	return 0, w.err
}

func (w *rebuildTransformOutput) Write(p []byte) (int, error) {
	if len(p) > w.bounds.FileBytes-w.size {
		return w.refuse(fmt.Errorf("received %d bytes, limit %d", int64(w.size)+int64(len(p)), w.bounds.FileBytes))
	}
	if w.file == nil && len(p) <= w.bounds.CaptureBytes-w.size {
		n, err := w.buffer.Write(p)
		w.size += n
		return n, err
	}
	free, err := platform.DiskAvailBytes(w.bounds.Dir)
	if err != nil {
		return w.refuse(err)
	}
	need := uint64(w.buffer.Len() + len(p))
	if free < w.bounds.DiskReserve || need > free-w.bounds.DiskReserve {
		return w.refuse(fmt.Errorf("spill needs %d available bytes above the disk reserve", need))
	}
	if w.file == nil {
		w.file, err = os.CreateTemp(w.bounds.Dir, ".transform-*")
		if err != nil {
			return w.refuse(err)
		}
		if _, err := w.buffer.WriteTo(w.file); err != nil {
			return w.refuse(err)
		}
		w.buffer = bytes.Buffer{}
	}
	n, err := w.file.Write(p)
	if err != nil {
		return w.refuse(err)
	}
	w.size += n
	return n, nil
}
