package source

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

var (
	ErrFrozenRootPath    = errors.New("cannot resolve frozen build input path")
	ErrFrozenRootChanged = errors.New("frozen build input changed after observation")
)

const (
	maxFrozenRootLinks     = 40
	maxFrozenRootPathBytes = 32 << 10
)

// FrozenRootPath observes a file or directory and the links used to reach it.
// It borrows the caller's root and does not keep any other descriptor open.
// Only Path and derived facts, not the observation, belong in frozen inputs.
type FrozenRootPath struct {
	root        *os.Root
	name        string
	entry       frozenRootEntry
	links       []frozenRootLink
	literal     bool
	admitTarget func(string) (fs.FileMode, error)
}

func (p FrozenRootPath) Path() string      { return p.entry.path }
func (p FrozenRootPath) Info() fs.FileInfo { return p.entry.info }

type frozenRootEntry struct {
	path string
	info fs.FileInfo
}

type frozenRootLink struct {
	frozenRootEntry
	target string
}

type frozenRootWalk struct {
	root          *os.Root
	stack         []frozenRootEntry
	links         []frozenRootLink
	bytesLeft     int
	checkRootLink func(string) error
	literal       bool
	admitTarget   func(string) (fs.FileMode, error)
}

// ResolveFrozenRootPath resolves links in traversal order, without cleaning
// away any components. It returns a slash-relative target ("." for the root).
// An absolute spelling must start with the literal absolute name supplied to
// OpenRoot. That prefix denotes the held root, not a new host path lookup.
// Other spellings, including aliases, cannot be proven here and are refused.
// Only a missing requested component, not a dangling link target, is absence.
func ResolveFrozenRootPath(ctx context.Context, root *os.Root, name string) (FrozenRootPath, error) {
	return resolveFrozenRootPath(ctx, root, name, nil, false, nil)
}

// ResolveFrozenRootLink checks the requested literal root link's raw target
// before resolving it. The checker runs before any target lookup or read.
// If supplied, admitTarget verifies each target path before its lookup and
// returns the expected entry type. The same gate protects later rechecks.
func (s *FilesystemSource) ResolveFrozenRootLink(ctx context.Context, name string, check func(string) error,
	admitTarget func(string) (fs.FileMode, error),
) (FrozenRootPath, error) {
	if !filepath.IsLocal(name) || name == "." || path.Base(filepath.ToSlash(name)) != name || check == nil {
		return FrozenRootPath{}, fmt.Errorf("expected a literal root link and target check: %w", ErrFrozenRootPath)
	}
	return resolveFrozenRootPath(ctx, s.root, name, check, false, admitTarget)
}

func resolveFrozenRootPath(ctx context.Context, root *os.Root, name string, check func(string) error, literal bool,
	admitTarget func(string) (fs.FileMode, error),
) (FrozenRootPath, error) {
	if ctx == nil || root == nil {
		return FrozenRootPath{}, fmt.Errorf("nil context or root: %w", ErrFrozenRootPath)
	}
	if err := ctx.Err(); err != nil {
		return FrozenRootPath{}, err
	}
	if !regularFilesystemReadSupported {
		return FrozenRootPath{}, ErrRegularReadUnsupported
	}
	info, err := root.Lstat(".")
	if err != nil {
		return FrozenRootPath{}, fmt.Errorf("stat held root: %w: %v", ErrFrozenRootPath, err)
	}
	walk := frozenRootWalk{
		root:          root,
		stack:         []frozenRootEntry{{path: ".", info: info}},
		bytesLeft:     maxFrozenRootPathBytes,
		checkRootLink: check,
		literal:       literal,
		admitTarget:   admitTarget,
	}
	if err := walk.resolve(ctx, name, false); err != nil {
		return FrozenRootPath{}, err
	}
	if err := ctx.Err(); err != nil {
		return FrozenRootPath{}, err
	}
	return FrozenRootPath{root: root, name: name, entry: walk.last(), links: walk.links, literal: literal, admitTarget: admitTarget}, nil
}

func (w *frozenRootWalk) last() frozenRootEntry { return w.stack[len(w.stack)-1] }

func (w *frozenRootWalk) resolve(ctx context.Context, raw string, fromLink bool) error {
	if len(raw) > w.bytesLeft {
		return fmt.Errorf("expanded path exceeds %d bytes: %w", maxFrozenRootPathBytes, ErrFrozenRootPath)
	}
	w.bytesLeft -= len(raw)
	if raw == "" || strings.IndexByte(raw, 0) >= 0 {
		return fmt.Errorf("empty path or NUL byte: %w", ErrFrozenRootPath)
	}
	name := filepath.ToSlash(raw)
	if filepath.IsAbs(raw) {
		rel, ok := frozenRootAbsoluteSuffix(w.root.Name(), name)
		if !ok {
			return fmt.Errorf("absolute path lacks the literal checkout prefix: %w: %w", ErrFrozenRootPath, ErrOutsideRoot)
		}
		name = rel
		w.stack = w.stack[:1]
	} else if strings.HasPrefix(name, "/") || filepath.VolumeName(raw) != "" {
		return fmt.Errorf("path has no confined relative base: %w: %w", ErrFrozenRootPath, ErrOutsideRoot)
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		part, rest, more := strings.Cut(name, "/")
		parent := w.last()
		if !parent.info.IsDir() {
			return fmt.Errorf("%s: non-directory ancestor: %w: %w", parent.path, ErrFrozenRootPath, ErrNotRegularFile)
		}
		switch part {
		case "", ".":
		case "..":
			if len(w.stack) == 1 {
				return fmt.Errorf("parent traversal leaves checkout: %w: %w", ErrFrozenRootPath, ErrOutsideRoot)
			}
			w.stack = w.stack[:len(w.stack)-1]
		default:
			if !filepath.IsLocal(part) {
				return fmt.Errorf("invalid path component: %w", ErrFrozenRootPath)
			}
			if err := w.visit(ctx, path.Join(parent.path, part), fromLink); err != nil {
				return err
			}
		}
		if !more {
			return nil
		}
		name = rest
	}
}

func frozenRootAbsoluteSuffix(rootName, slashed string) (string, bool) {
	if !filepath.IsAbs(rootName) || filepath.Clean(rootName) != rootName {
		return "", false
	}
	base := filepath.ToSlash(rootName)
	if slashed == base {
		return ".", true
	}
	return strings.CutPrefix(slashed, strings.TrimSuffix(base, "/")+"/")
}

func (w *frozenRootWalk) visit(ctx context.Context, name string, fromLink bool) error {
	// Root walks each resolved prefix again; bound that work as well as
	// the original and expanded link texts.
	if len(name) > w.bytesLeft {
		return fmt.Errorf("resolved prefixes exceed the path budget: %w", ErrFrozenRootPath)
	}
	w.bytesLeft -= len(name)
	var expected fs.FileMode
	checked := fromLink && w.admitTarget != nil
	if checked {
		var err error
		expected, err = w.admitTarget(name)
		if err != nil {
			return err
		}
	}
	native := filepath.FromSlash(name)
	info, err := w.root.Lstat(native)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			if fromLink {
				return fmt.Errorf("%s: dangling link target: %w", name, ErrFrozenRootPath)
			}
			parent := w.last()
			current, statErr := w.root.Lstat(filepath.FromSlash(parent.path))
			if statErr != nil || !os.SameFile(parent.info, current) {
				return fmt.Errorf("%s: parent changed during lookup: %w", name, ErrFrozenRootChanged)
			}
			return regularFileAbsent(name, err)
		}
		return fmt.Errorf("%w: %w", ErrFrozenRootPath, mapRootError(name, err))
	}
	if checked && info.Mode().Type() != expected.Type() {
		return fmt.Errorf("%s: target type differs from admission: %w", name, ErrFrozenRootChanged)
	}
	entry := frozenRootEntry{path: name, info: info}
	if info.Mode()&fs.ModeSymlink == 0 {
		if !fromLink && w.checkRootLink != nil {
			return fmt.Errorf("%s: expected root symlink: %w", name, ErrFrozenRootChanged)
		}
		w.stack = append(w.stack, entry)
		return nil
	}
	if w.literal {
		return fmt.Errorf("%s: literal path crosses a symlink: %w", name, ErrFrozenRootPath)
	}
	if len(w.links) == maxFrozenRootLinks {
		return fmt.Errorf("%s: too many symlink hops: %w", name, ErrFrozenRootPath)
	}
	target, err := w.root.Readlink(native)
	if err != nil {
		return fmt.Errorf("%s: read link: %w: %v", name, ErrFrozenRootChanged, err)
	}
	current, err := w.root.Lstat(native)
	if err != nil || !sameFrozenRootState(info, current) {
		return fmt.Errorf("%s: link changed during resolution: %w", name, ErrFrozenRootChanged)
	}
	if !fromLink && w.checkRootLink != nil {
		if err := w.checkRootLink(target); err != nil {
			return err
		}
	}
	w.links = append(w.links, frozenRootLink{frozenRootEntry: entry, target: target})
	return w.resolve(ctx, target, true)
}

// Read returns bytes only from the observed regular descriptor within maxBytes.
// Cancellation and metadata drift checks are cooperative, not an atomic snapshot.
func (p FrozenRootPath) Read(ctx context.Context, maxBytes int64) ([]byte, error) {
	return p.readWithOpen(ctx, maxBytes, openRootRegularFile)
}

func (p FrozenRootPath) readWithOpen(ctx context.Context, maxBytes int64, openFile func(*os.Root, string) (*os.File, error)) ([]byte, error) {
	if maxBytes < 0 || maxBytes > maxRegularReadBytes || p.Info() == nil {
		return nil, fmt.Errorf("invalid read limit or empty observation: %w", ErrFrozenRootPath)
	}
	if err := regularMetadataError(regularDescriptorMeta(p.Path(), p.Info()), maxBytes); err != nil {
		return nil, err
	}
	f, err := p.openVerified(ctx, openFile)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	size := p.Info().Size()
	data, err := io.ReadAll(io.LimitReader(regularContextReader{ctx: ctx, reader: f}, size+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != size {
		return nil, fmt.Errorf("%s: read length differs from observed size: %w", p.Path(), ErrFrozenRootChanged)
	}
	if err := p.checkDescriptor(f); err != nil {
		return nil, err
	}
	if err := p.checkCurrent(ctx); err != nil {
		return nil, err
	}
	return data, nil
}

// ReadPrefix returns up to maxBytes (0–512) from an observed literal regular
// path. The caller must already have authorized reading the path. As with Read,
// these checks are not an atomic snapshot.
func (p FrozenRootPath) ReadPrefix(ctx context.Context, maxBytes int64) ([]byte, error) {
	return p.readPrefixWithOpen(ctx, maxBytes, openRootRegularFile)
}

func (p FrozenRootPath) readPrefixWithOpen(ctx context.Context, maxBytes int64, openFile func(*os.Root, string) (*os.File, error)) ([]byte, error) {
	info := p.Info()
	if maxBytes < 0 || maxBytes > 512 || !p.literal || info == nil {
		return nil, fmt.Errorf("invalid prefix limit or missing literal observation: %w", ErrFrozenRootPath)
	}
	if err := regularMetadataError(regularDescriptorMeta(p.Path(), info), math.MaxInt64); err != nil {
		return nil, err
	}
	f, err := p.openVerified(ctx, openFile)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	data := make([]byte, min(maxBytes, info.Size()))
	if _, err := io.ReadFull(regularContextReader{ctx: ctx, reader: f}, data); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, fmt.Errorf("%s: prefix shorter than observed: %w", p.Path(), ErrFrozenRootChanged)
		}
		return nil, err
	}
	if err := p.checkDescriptor(f); err != nil {
		return nil, err
	}
	if err := p.checkCurrent(ctx); err != nil {
		return nil, err
	}
	return data, nil
}

// OpenDir opens the observed directory without blocking on a substituted
// special file. The caller owns the returned descriptor. On platforms without
// a supported directory-only open it returns ErrRegularReadUnsupported.
func (p FrozenRootPath) OpenDir(ctx context.Context) (*os.File, error) {
	return p.openDirWithOpen(ctx, openRootFrozenDir)
}

func (p FrozenRootPath) openDirWithOpen(ctx context.Context, openDir func(*os.Root, string) (*os.File, error)) (*os.File, error) {
	if p.Info() == nil || !p.Info().IsDir() {
		return nil, fmt.Errorf("%s: expected a directory: %w", p.Path(), ErrFrozenRootPath)
	}
	f, err := p.openVerified(ctx, openDir)
	if err != nil {
		return nil, err
	}
	if err := p.checkCurrent(ctx); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func (p FrozenRootPath) openVerified(ctx context.Context, open func(*os.Root, string) (*os.File, error)) (*os.File, error) {
	if err := p.checkCurrent(ctx); err != nil {
		return nil, err
	}
	f, err := open(p.root, filepath.FromSlash(p.Path()))
	if errors.Is(err, ErrRegularReadUnsupported) {
		return nil, err
	}
	if err != nil {
		return nil, fmt.Errorf("%s: open observed entry: %w: %v", p.Path(), ErrFrozenRootChanged, err)
	}
	if err := p.checkDescriptor(f); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func (p FrozenRootPath) checkDescriptor(f *os.File) error {
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("%s: stat opened entry: %w: %v", p.Path(), ErrFrozenRootChanged, err)
	}
	if !sameFrozenRootState(p.Info(), info) {
		return fmt.Errorf("%s: descriptor does not match observation: %w", p.Path(), ErrFrozenRootChanged)
	}
	return nil
}

func (p FrozenRootPath) checkCurrent(ctx context.Context) error {
	current, err := resolveFrozenRootPath(ctx, p.root, p.name, nil, p.literal, p.admitTarget)
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if err != nil {
		// Missing now is a changed witness, not a negative frozen input.
		if p.admitTarget != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%s: recheck observed path: %w: %w", p.Path(), ErrFrozenRootChanged, err)
		}
		return fmt.Errorf("%s: recheck observed path: %w: %v", p.Path(), ErrFrozenRootChanged, err)
	}
	if !sameFrozenRootEntry(p.entry, current.entry) || !slices.EqualFunc(p.links, current.links, func(a, b frozenRootLink) bool {
		return a.target == b.target && sameFrozenRootEntry(a.frozenRootEntry, b.frozenRootEntry)
	}) {
		return fmt.Errorf("%s: path does not match observation: %w", p.Path(), ErrFrozenRootChanged)
	}
	return nil
}

func sameFrozenRootEntry(a, b frozenRootEntry) bool {
	return a.path == b.path && sameFrozenRootState(a.info, b.info)
}

func sameFrozenRootState(a, b fs.FileInfo) bool {
	return os.SameFile(a, b) && a.Mode() == b.Mode() && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}
