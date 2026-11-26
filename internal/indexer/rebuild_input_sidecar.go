package indexer

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/zeebo/blake3"
)

const (
	rebuildInputSidecarName  = "build-inputs"
	rebuildInputSidecarMagic = "gortex/build-inputs/v1\n"
	rebuildInputEncodedLimit = 64 << 20
	rebuildFollowedManifest  = rebuildCppHeuristicDir + 1
)

const (
	rebuildNamespaceRegularFact rebuildCppFactKind = rebuildFollowedManifest + 1 + iota
	rebuildNamespaceDirectoryFact
	rebuildNamespaceDeniedFact
	rebuildNamespaceUnsupportedFact
	rebuildNamespaceCloseFact
	rebuildNamespaceCloseExactFact
	rebuildGoManifestFact
	rebuildTSAliasConfigFact
	rebuildNpmManifestRow
	rebuildClassificationRow
	rebuildGitIndexStampRow
	rebuildGitIgnoreVerdictRow
	rebuildWorkspaceConfigRow
	rebuildModuleFact
)

var errRebuildInputSidecar = errors.New("invalid frozen build input sidecar")

// Paths are byte strings in the encoding, so a non-UTF-8 directory name does
// not turn into a different name on replay. Followed manifests carry only the
// logical root name and the verified bytes' digest, never the target path.
type rebuildSidecarRow struct {
	Kind      rebuildCppFactKind            `json:"kind"`
	Path      []byte                        `json:"path"`
	Present   bool                          `json:"present,omitempty"`
	Digest    rebuildDigest                 `json:"digest,omitzero"`
	Dirs      [][]byte                      `json:"dirs,omitempty"`
	Config    *rebuildResolverConfigPayload `json:"config,omitempty"`
	NPM       *rebuildNpmManifestPayload    `json:"npm,omitempty"`
	Workspace *rebuildWorkspacePayload      `json:"workspace,omitempty"`
	Module    *rebuildModulePayload         `json:"module,omitempty"`

	Classification *rebuildClassificationFact `json:"classification,omitempty"`
	GitVersion     string                     `json:"git_version,omitempty"`
	GitVerdict     rebuildGitIgnoreVerdict    `json:"git_verdict,omitempty"`
}

func rebuildSidecarCppRow(f rebuildCppFact) rebuildSidecarRow {
	r := rebuildSidecarRow{Kind: f.Kind, Path: []byte(f.Path), Present: f.Present, Digest: f.Digest}
	for _, dir := range f.IncludeDirs {
		r.Dirs = append(r.Dirs, []byte(dir))
	}
	return r
}

func (r rebuildSidecarRow) check() error {
	if r.Kind == rebuildModuleFact {
		return r.checkModuleFact()
	}
	if r.Module != nil {
		return fmt.Errorf("%w: unexpected module payload", errRebuildInputSidecar)
	}
	if r.Kind == rebuildWorkspaceConfigRow {
		return r.checkWorkspaceConfig()
	}
	if r.Workspace != nil {
		return fmt.Errorf("%w: unexpected workspace payload", errRebuildInputSidecar)
	}
	if r.Kind == rebuildGitIndexStampRow || r.Kind == rebuildGitIgnoreVerdictRow {
		return r.checkGitPolicy()
	}
	if r.GitVersion != "" || r.GitVerdict != 0 {
		return fmt.Errorf("%w: unexpected Git policy fields", errRebuildInputSidecar)
	}
	if r.Kind == rebuildClassificationRow {
		return r.checkClassification()
	}
	if r.Classification != nil {
		return fmt.Errorf("%w: unexpected classification payload", errRebuildInputSidecar)
	}
	if r.Kind == rebuildNpmManifestRow {
		return r.checkNpmManifest()
	}
	if r.NPM != nil {
		return fmt.Errorf("%w: unexpected npm manifest payload", errRebuildInputSidecar)
	}
	if r.isResolverConfig() {
		return r.checkResolverConfig()
	}
	if r.Config != nil {
		return fmt.Errorf("%w: unexpected resolver config payload", errRebuildInputSidecar)
	}
	if ns, ok := r.namespaceRow(); ok {
		closing := ns.kind == rebuildNamespaceClose || ns.kind == rebuildNamespaceCloseExact
		if (validRebuildNamespaceKey(ns.key) || closing && ns.key == ".") &&
			!r.Present && r.Digest == (rebuildDigest{}) && len(r.Dirs) == 0 {
			return nil
		}
		return fmt.Errorf("%w: invalid namespace row", errRebuildInputSidecar)
	}
	local := func(p []byte) bool {
		s := string(p)
		return s != "." && path.Clean(s) == s && filepath.IsLocal(filepath.FromSlash(s)) &&
			filepath.ToSlash(filepath.FromSlash(s)) == s && !strings.ContainsRune(s, 0)
	}
	if len(r.Path) > rebuildCppEntryLimit || !local(r.Path) {
		return fmt.Errorf("%w: nonportable fact path", errRebuildInputSidecar)
	}
	zero := r.Digest == (rebuildDigest{})
	switch r.Kind {
	case rebuildCppDatabase:
		if r.Present != zero && len(r.Dirs) == 0 {
			return nil
		}
	case rebuildCppTU:
		bytes := len(r.Path)
		for _, d := range r.Dirs {
			bytes += len(d)
			if bytes > rebuildCppEntryLimit || !local(d) {
				return fmt.Errorf("%w: invalid TU directory", errRebuildInputSidecar)
			}
		}
		if !r.Present && zero {
			return nil
		}
	case rebuildCppHeuristicDir:
		if !r.Present && zero && len(r.Dirs) == 0 && !strings.ContainsRune(string(r.Path), '/') {
			return nil
		}
	case rebuildFollowedManifest:
		if r.Present && !zero && len(r.Dirs) == 0 && (string(r.Path) == "go.mod" || string(r.Path) == "package.json") {
			return nil
		}
	}
	return fmt.Errorf("%w: invalid fact fields", errRebuildInputSidecar)
}

// writeRebuildInputSidecar owns one bounded, immutable file in the private
// lease. The hash covers every encoded byte. A failed capture has no digest;
// the owner discards the lease rather than keeping a prefix of the input.
func writeRebuildInputSidecar(ctx context.Context, lease string, capture func(func(rebuildSidecarRow) error) error) (_ rebuildDigest, err error) {
	if err := ctx.Err(); err != nil {
		return rebuildDigest{}, err
	}
	f, err := os.OpenFile(filepath.Join(lease, rebuildInputSidecarName), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return rebuildDigest{}, err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	h := blake3.New()
	w := bufio.NewWriter(io.MultiWriter(f, h))
	if _, err := w.WriteString(rebuildInputSidecarMagic); err != nil {
		return rebuildDigest{}, err
	}
	left := rebuildInputEncodedLimit - len(rebuildInputSidecarMagic)
	if err := capture(func(row rebuildSidecarRow) error {
		return writeRebuildSidecarRow(ctx, w, &left, row)
	}); err != nil {
		return rebuildDigest{}, err
	}
	if err := ctx.Err(); err != nil {
		return rebuildDigest{}, err
	}
	if err := w.Flush(); err != nil {
		return rebuildDigest{}, err
	}
	return rebuildDigest(h.Sum(nil)), nil
}

func writeRebuildSidecarRow(ctx context.Context, w *bufio.Writer, left *int, row rebuildSidecarRow) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := row.check(); err != nil {
		return err
	}
	b, err := json.Marshal(row)
	if err != nil {
		return err
	}
	lineSize := len(b) + 1
	if lineSize > rebuildCppEntryLimit || lineSize > *left {
		return fmt.Errorf("%w: encoded input cap for %q: %w", errRebuildInputSidecar, row.Path, errRebuildCppCaptureLimit)
	}
	if _, err := w.Write(b); err != nil {
		return err
	}
	if err := w.WriteByte('\n'); err != nil {
		return err
	}
	*left -= lineSize
	return nil
}

// visitRebuildInputSidecar visits one fact at a time. Visits are tentative
// until the entire byte stream and expected digest have been checked. No
// caller may publish or reuse facts from a failed visit.
func visitRebuildInputSidecar(ctx context.Context, lease string, expected rebuildDigest, visit func(rebuildSidecarRow) error) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	name := filepath.Join(lease, rebuildInputSidecarName)
	before, err := os.Lstat(name)
	if err != nil {
		return fmt.Errorf("%w: %w", errRebuildInputSidecar, err)
	}
	if !before.Mode().IsRegular() {
		return fmt.Errorf("%w: sidecar is not a private regular file", errRebuildInputSidecar)
	}
	f, err := os.Open(name)
	if err != nil {
		return fmt.Errorf("%w: %w", errRebuildInputSidecar, err)
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("%w: %w", errRebuildInputSidecar, err)
	}
	if !os.SameFile(before, info) || !info.Mode().IsRegular() || info.Size() < int64(len(rebuildInputSidecarMagic)) {
		return fmt.Errorf("%w: sidecar size or kind", errRebuildInputSidecar)
	}
	if info.Size() > rebuildInputEncodedLimit {
		return fmt.Errorf("%w: encoded input cap for %q: %w", errRebuildInputSidecar, rebuildInputSidecarName, errRebuildCppCaptureLimit)
	}
	h := blake3.New()
	limit := &io.LimitedReader{R: f, N: rebuildInputEncodedLimit + 1}
	r := bufio.NewReaderSize(io.TeeReader(limit, h), rebuildCppEntryLimit+1)
	magic, err := r.ReadSlice('\n')
	if err != nil || string(magic) != rebuildInputSidecarMagic {
		return fmt.Errorf("%w: sidecar format", errRebuildInputSidecar)
	}
	prefixLeft := int64(rebuildCppRawLimit)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		line, err := r.ReadSlice('\n')
		if err == io.EOF && len(line) == 0 {
			break
		}
		if errors.Is(err, bufio.ErrBufferFull) || len(line) > rebuildCppEntryLimit || limit.N == 0 {
			return fmt.Errorf("%w: oversized row or encoded input cap in %q: %w", errRebuildInputSidecar, rebuildInputSidecarName, errRebuildCppCaptureLimit)
		}
		if err != nil {
			return fmt.Errorf("%w: truncated or unreadable row: %w", errRebuildInputSidecar, err)
		}
		var row rebuildSidecarRow
		if err := json.Unmarshal(line, &row); err != nil {
			return fmt.Errorf("%w: decode row: %w", errRebuildInputSidecar, err)
		}
		if err := row.check(); err != nil {
			return err
		}
		_, namespace := row.namespaceRow()
		if namespace || row.Kind == rebuildFollowedManifest || row.isResolverConfig() || row.Kind == rebuildNpmManifestRow || row.Kind == rebuildClassificationRow || row.Kind == rebuildWorkspaceConfigRow ||
			row.Kind == rebuildGitIndexStampRow || row.Kind == rebuildGitIgnoreVerdictRow || row.Kind == rebuildModuleFact {
			// Refuse ambiguous fields (including duplicate JSON keys and null
			// payload members) on frozen resolver and owner facts.
			encoded, err := json.Marshal(row)
			if err != nil || !bytes.Equal(encoded, line[:len(line)-1]) {
				name := "resolver config"
				switch {
				case namespace:
					name = "namespace"
				case row.Kind == rebuildFollowedManifest:
					name = "followed manifest"
				case row.Kind == rebuildNpmManifestRow:
					name = "npm manifest"
				case row.Kind == rebuildWorkspaceConfigRow:
					name = "workspace declaration"
				case row.Kind == rebuildModuleFact:
					return fmt.Errorf("%w: noncanonical module row", errRebuildModuleInput)
				case row.Kind == rebuildClassificationRow:
					name = "classification"
				case row.Kind == rebuildGitIndexStampRow || row.Kind == rebuildGitIgnoreVerdictRow:
					name = "Git policy"
				}
				return fmt.Errorf("%w: noncanonical %s row %q", errRebuildInputSidecar, name, row.Path)
			}
		}
		if fact := row.Classification; fact != nil {
			// Other raw-input charges still belong to capture. Check the
			// portion replay can recover from the normalized payloads.
			prefixLeft -= fact.PrefixBytes
			if prefixLeft < 0 {
				return fmt.Errorf("%w: classification prefix replay cap: %w", errRebuildInputSidecar, errRebuildCppCaptureLimit)
			}
		}
		if err := visit(row); err != nil {
			return err
		}
	}
	if limit.N == 0 || rebuildDigest(h.Sum(nil)) != expected {
		return fmt.Errorf("%w: sidecar byte digest", errRebuildInputSidecar)
	}
	return ctx.Err()
}

// Namespace kinds have separate wire identifiers; old sidecar rows keep
// their exact encodings.
var rebuildNamespaceFactKinds = [...]rebuildNamespaceKind{
	rebuildNamespaceRegular,
	rebuildNamespaceDirectory,
	rebuildNamespaceDenied,
	rebuildNamespaceUnsupported,
	rebuildNamespaceClose,
	rebuildNamespaceCloseExact,
}

func (r rebuildSidecarRow) namespaceRow() (rebuildNamespaceRow, bool) {
	if r.Kind < rebuildNamespaceRegularFact || r.Kind > rebuildNamespaceCloseExactFact {
		return rebuildNamespaceRow{}, false
	}
	return rebuildNamespaceRow{key: string(r.Path), kind: rebuildNamespaceFactKinds[r.Kind-rebuildNamespaceRegularFact]}, true
}

func writeRebuildNamespaceFact(row rebuildNamespaceRow, emit func(rebuildSidecarRow) error) error {
	for i, local := range rebuildNamespaceFactKinds {
		if local == row.kind {
			return emit(rebuildSidecarRow{Kind: rebuildNamespaceRegularFact + rebuildCppFactKind(i), Path: []byte(row.key)})
		}
	}
	return fmt.Errorf("%w: unknown namespace kind", errRebuildInputSidecar)
}

// openRebuildResolverNamespace returns no view until the enclosing sidecar's
// entire digest has been checked and all namespace rows have passed replay.
func openRebuildResolverNamespace(ctx context.Context, lease string, expected rebuildDigest) (*rebuildResolverNamespace, error) {
	return visitRebuildResolverNamespace(ctx, lease, expected, nil)
}

// Visit other rows in the same stream as the namespace. They remain tentative
// until both the byte digest and the namespace have passed.
func visitRebuildResolverNamespace(ctx context.Context, lease string, expected rebuildDigest, visit func(rebuildSidecarRow) error) (*rebuildResolverNamespace, error) {
	var n rebuildResolverNamespace
	if err := visitRebuildInputSidecar(ctx, lease, expected, func(row rebuildSidecarRow) error {
		if ns, ok := row.namespaceRow(); ok {
			return n.add(ns)
		}
		if visit != nil {
			return visit(row)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if n.entries["."].close == rebuildNamespaceUnknown {
		return nil, fmt.Errorf("%w: checkout root listing not closed", errRebuildResolverNamespace)
	}
	if err := n.freeze(); err != nil {
		return nil, err
	}
	return &n, nil
}
