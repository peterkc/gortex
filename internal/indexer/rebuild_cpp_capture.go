package indexer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/zeebo/blake3"
	"github.com/zzet/gortex/internal/indexer/source"
)

const (
	rebuildCppRawLimit   = 64 << 20
	rebuildCppEntryLimit = 1 << 20
	// The sidecar also caps encoded facts at 64 MiB. Even before yielding,
	// the admitted names alone must fit; unrelated directory names do not count.
	rebuildCppNameLimit = 64 << 20
)

var (
	errRebuildCppCaptureLimit = errors.New("C-family input capture exceeds limit")
	errRebuildCppCapturePath  = errors.New("cannot capture C-family input path")
)

type rebuildCppFactKind uint8

const (
	rebuildCppDatabase rebuildCppFactKind = iota + 1
	rebuildCppTU
	rebuildCppHeuristicDir
)

// rebuildCppFact contains only portable input facts. Database facts include
// negative locations. TU facts are last-wins, even for empty IncludeDirs.
// Heuristic facts are the complete ordered positive roots, when consulted.
type rebuildCppFact struct {
	Kind        rebuildCppFactKind
	Path        string
	Present     bool
	Digest      rebuildDigest
	IncludeDirs []string
}

// captureRebuildCppInputs freezes the disk loader's inputs for an admitted
// C-family build. readDB must safely open only a root-confined regular file
// (following a contained link), verify the bytes it returns, and honor its
// size limit. The caller must discard its tentative encoding on any failure.
// rawRemaining is one budget shared across the capture owner, not per file.
func captureRebuildCppInputs(ctx context.Context, root *os.Root, repoRoot string, rawRemaining *int64,
	readDB func(source.FrozenRootPath, context.Context, int64) ([]byte, error), yield func(rebuildCppFact) error,
) error {
	if *rawRemaining < 0 || *rawRemaining > rebuildCppRawLimit {
		return errRebuildCppCaptureLimit
	}
	// Native compileDBLocations probes the root first, then sorts FULL paths
	// matching build*/compile_commands.json (build-fast sorts before build).
	hasTU, err := captureRebuildCppDB(ctx, root, repoRoot, compileDBName, rawRemaining, readDB, yield)
	if err != nil {
		return err
	}
	var locations []string
	var namesBytes int
	err = visitRebuildCppDir(ctx, root, ".", func(e fs.DirEntry) (bool, error) {
		name := e.Name()
		if !strings.HasPrefix(name, "build") {
			return false, nil
		}
		if !e.IsDir() {
			if e.Type()&fs.ModeSymlink == 0 {
				return false, nil
			}
			dir, err := resolveRebuildCppPath(ctx, root, name)
			if err != nil || dir.Info() == nil || !dir.Info().IsDir() {
				return false, err
			}
		}
		loc := name + "/" + compileDBName
		namesBytes += len(loc)
		if namesBytes > rebuildCppNameLimit {
			return false, errRebuildCppCaptureLimit
		}
		locations = append(locations, loc)
		return false, nil
	})
	if err != nil {
		return err
	}
	slices.Sort(locations)
	for _, loc := range locations {
		hasRows, err := captureRebuildCppDB(ctx, root, repoRoot, loc, rawRemaining, readDB, yield)
		if err != nil {
			return err
		}
		hasTU = hasTU || hasRows
	}
	if hasTU {
		return nil
	}
	return captureRebuildCppHeuristics(ctx, root, yield)
}

func captureRebuildCppDB(ctx context.Context, root *os.Root, repoRoot, loc string, rawRemaining *int64,
	readDB func(source.FrozenRootPath, context.Context, int64) ([]byte, error), yield func(rebuildCppFact) error,
) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	fact := rebuildCppFact{Kind: rebuildCppDatabase, Path: loc}
	observed, err := resolveRebuildCppPath(ctx, root, loc)
	if err != nil {
		return false, err
	}
	before := observed.Info()
	if before == nil || before.IsDir() {
		return false, yield(fact)
	}
	if !before.Mode().IsRegular() {
		return false, fmt.Errorf("%s is not a regular file: %w", loc, errRebuildCppCapturePath)
	}
	if before.Size() > *rawRemaining {
		return false, fmt.Errorf("%s: %w", loc, errRebuildCppCaptureLimit)
	}
	data, err := readDB(observed, ctx, *rawRemaining)
	if err != nil {
		return false, fmt.Errorf("read %s: %w: %w", loc, errRebuildCppCapturePath, err)
	}
	if int64(len(data)) > *rawRemaining {
		return false, fmt.Errorf("%s: %w", loc, errRebuildCppCaptureLimit)
	}
	*rawRemaining -= int64(len(data))
	fact.Present = true
	fact.Digest = blake3.Sum256(data)
	if err := yield(fact); err != nil {
		return false, err
	}
	// json.Unmarshal into []compileCommand rejects a whole DB on any bad
	// entry or trailing bytes. Validate the same immutable bytes before
	// emitting anything from the array; retain no per-DB map or command text.
	// The whole-document check also counts the outer array in JSON nesting.
	valid, err := scanRebuildCppDB(ctx, data, nil)
	if err != nil {
		return false, fmt.Errorf("%s: %w", loc, err)
	}
	if !valid || !json.Valid(data) {
		return false, nil
	}
	hasTU := false
	_, err = scanRebuildCppDB(ctx, data, func(cc compileCommand) error {
		rel := repoRelPath(repoRoot, cc.Directory, cc.File)
		if rel == "" {
			return nil
		}
		dirs, err := boundedRebuildCppIncludeDirs(cc, repoRoot, len(rel))
		if err != nil {
			return err
		}
		hasTU = true
		return yield(rebuildCppFact{Kind: rebuildCppTU, Path: rel, IncludeDirs: dirs})
	})
	if err != nil {
		return false, fmt.Errorf("%s: %w", loc, err)
	}
	return hasTU, nil
}

// Keep the flag order of extractIncludeDirs. Each normalized directory can
// repeat a long command Directory, so the raw entry limit alone does not
// bound how large this fact gets before the encoder sees it.
func boundedRebuildCppIncludeDirs(cc compileCommand, repoRoot string, used int) ([]string, error) {
	toks := cc.Arguments
	if len(toks) == 0 && cc.Command != "" {
		toks = shlexSplit(cc.Command)
	}
	var dirs []string
	seen := map[string]bool{}
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		var raw string
		switch {
		case strings.HasPrefix(t, "-I") && len(t) > 2:
			raw = t[2:]
		case (t == "-I" || t == "-isystem" || t == "-iquote" || t == "-idirafter") && i+1 < len(toks):
			i++
			raw = toks[i]
		default:
			continue
		}
		rel := repoRelPath(repoRoot, cc.Directory, raw)
		if rel == "" || seen[rel] {
			continue
		}
		used += len(rel)
		if used > rebuildCppEntryLimit {
			return nil, errRebuildCppCaptureLimit
		}
		seen[rel] = true
		dirs = append(dirs, rel)
	}
	return dirs, nil
}

// scanRebuildCppDB uses the same JSON decoder and row type as the native
// loader. It delimits the outer array so decoding each raw row can see at
// most 1 MiB plus one byte. Even a wrong-typed row does not hide a later
// oversized row in an otherwise valid array.
func scanRebuildCppDB(ctx context.Context, data []byte, visit func(compileCommand) error) (bool, error) {
	const space = " \t\r\n"
	data = bytes.Trim(data, space)
	if bytes.Equal(data, []byte("null")) {
		return true, nil
	}
	if len(data) == 0 || data[0] != '[' {
		return false, nil
	}
	data = bytes.TrimLeft(data[1:], space)
	if bytes.Equal(data, []byte("]")) {
		return true, nil
	}
	valid := true
	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		dec := json.NewDecoder(bytes.NewReader(data[:min(len(data), rebuildCppEntryLimit+1)]))
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			var syntax *json.SyntaxError
			if len(data) > rebuildCppEntryLimit && (errors.Is(err, io.ErrUnexpectedEOF) ||
				(errors.As(err, &syntax) && syntax.Offset > rebuildCppEntryLimit)) {
				return false, errRebuildCppCaptureLimit
			}
			return false, nil
		}
		end := dec.InputOffset()
		if end > rebuildCppEntryLimit {
			return false, errRebuildCppCaptureLimit
		}
		var cc compileCommand
		if err := json.Unmarshal(raw, &cc); err != nil {
			valid = false
		} else if visit != nil {
			if err := visit(cc); err != nil {
				return false, err
			}
		}
		data = bytes.TrimLeft(data[end:], space)
		if bytes.Equal(data, []byte("]")) {
			return valid, nil
		}
		if len(data) == 0 || data[0] != ',' {
			return false, nil
		}
		data = bytes.TrimLeft(data[1:], space)
	}
}

func captureRebuildCppHeuristics(ctx context.Context, root *os.Root, yield func(rebuildCppFact) error) error {
	// Native heuristicIncludeDirs follows conventional names but only
	// considers real listed directories for its other header roots.
	for _, name := range conventionalCppIncludeRoots {
		dir, err := resolveRebuildCppPath(ctx, root, name)
		if err != nil {
			return err
		}
		if dir.Info() != nil && dir.Info().IsDir() {
			if err := yield(rebuildCppFact{Kind: rebuildCppHeuristicDir, Path: name}); err != nil {
				return err
			}
		}
	}
	var extra []string
	var namesBytes int
	err := visitRebuildCppDir(ctx, root, ".", func(e fs.DirEntry) (bool, error) {
		name := e.Name()
		if !e.IsDir() || strings.HasPrefix(name, ".") || slices.Contains(conventionalCppIncludeRoots, name) {
			return false, nil
		}
		holdsHeader := false
		if err := visitRebuildCppDir(ctx, root, name, func(child fs.DirEntry) (bool, error) {
			// Native dirHoldsExt only consults the entry's type and suffix,
			// not whether a symlink header target exists or is readable.
			holdsHeader = !child.IsDir() && slices.Contains(cppHeaderExts, filepath.Ext(child.Name()))
			return holdsHeader, nil
		}); err != nil {
			return false, fmt.Errorf("%s: %w", name, err)
		}
		if holdsHeader {
			namesBytes += len(name)
			if namesBytes > rebuildCppNameLimit {
				return false, errRebuildCppCaptureLimit
			}
			extra = append(extra, name)
		}
		return false, nil
	})
	if err != nil {
		return err
	}
	slices.Sort(extra)
	for _, name := range extra {
		if err := yield(rebuildCppFact{Kind: rebuildCppHeuristicDir, Path: name}); err != nil {
			return err
		}
	}
	return nil
}

func resolveRebuildCppPath(ctx context.Context, root *os.Root, name string) (source.FrozenRootPath, error) {
	entry, err := source.ResolveFrozenRootPath(ctx, root, name)
	if errors.Is(err, fs.ErrNotExist) {
		return source.FrozenRootPath{}, nil
	}
	if err != nil {
		return source.FrozenRootPath{}, fmt.Errorf("%s: %w: %w", name, errRebuildCppCapturePath, err)
	}
	return entry, nil
}

// visitRebuildCppDir retains only one small listing batch. Its visitor can
// stop once the native existence question has been answered.
func visitRebuildCppDir(ctx context.Context, root *os.Root, name string, visit func(fs.DirEntry) (bool, error)) (err error) {
	entry, err := source.ResolveFrozenRootPath(ctx, root, name)
	if err != nil {
		return fmt.Errorf("%w: %w", errRebuildCppCapturePath, err)
	}
	f, err := entry.OpenDir(ctx)
	if err != nil {
		return fmt.Errorf("%w: %w", errRebuildCppCapturePath, err)
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, err := f.ReadDir(128)
		if err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("%w: %w", errRebuildCppCapturePath, err)
		}
		for _, e := range entries {
			if done, err := visit(e); done || err != nil {
				return err
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
	}
}
