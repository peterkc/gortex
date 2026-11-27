package store_pebble

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/cockroachdb/pebble/v2"
	"github.com/cockroachdb/pebble/v2/sstable"
	"github.com/zeebo/blake3"
	"github.com/zzet/gortex/internal/graph"
)

// FileGraphBase retains the worker's exact file-graph data SSTs. Its links
// are private to the originating BuildStore and removed when it closes.
type FileGraphBase struct {
	build       *BuildStore
	paths       []string
	witness     Witness
	digest      HeadDigest
	incarnation string
}

// RetainFileGraph links a PrepareFileGraph result before the owner can ingest
// and consume its outward names. The stage and the working store must share
// a filesystem. No copy, private ingest, or reconstruction is attempted.
func (b *BuildStore) RetainFileGraph(ctx context.Context, stage StageTables) (base FileGraphBase, err error) {
	if err := ctx.Err(); err != nil {
		return FileGraphBase{}, err
	}
	w := stage.Witness
	if w.Stage != StageFileGraph || w.Profile != b.profile || stage.Incarnation == "" || !filepath.IsAbs(stage.Dir) {
		return FileGraphBase{}, invalid("relations needs its file-graph stage")
	}
	if len(stage.Files) >= goldFileCap {
		return FileGraphBase{}, ErrStageTableLimit
	}
	raw, err := encodeWitness(w)
	if err != nil {
		return FileGraphBase{}, err
	}
	w.Families = slices.Clone(w.Families)
	w.Dependencies = slices.Clone(w.Dependencies)
	if !slices.ContainsFunc(w.Families, func(f FamilyWitness) bool { return f.Family == familyManifest }) {
		return FileGraphBase{}, invalid("file-graph witness lacks the manifest family")
	}
	dir, err := os.MkdirTemp(filepath.Join(b.scratchParent, "pending"), "file-graph-base-")
	if err != nil {
		return FileGraphBase{}, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, os.RemoveAll(dir))
		}
	}()
	paths := make([]string, 0, len(stage.Files))
	for _, name := range stage.Files {
		if err := ctx.Err(); err != nil {
			return FileGraphBase{}, err
		}
		if name != filepath.Base(name) || filepath.Ext(name) != ".sst" {
			return FileGraphBase{}, invalid("file-graph table name")
		}
		from, to := filepath.Join(stage.Dir, name), filepath.Join(dir, name)
		info, err := os.Lstat(from)
		if err != nil {
			return FileGraphBase{}, err
		}
		if !info.Mode().IsRegular() {
			return FileGraphBase{}, invalid("file-graph table is not a regular file")
		}
		if err := os.Link(from, to); err != nil {
			return FileGraphBase{}, err
		}
		paths = append(paths, to)
	}
	if err := ctx.Err(); err != nil {
		return FileGraphBase{}, err
	}
	return FileGraphBase{build: b, paths: paths, witness: w, digest: blake3.Sum256(raw), incarnation: stage.Incarnation}, nil
}

// ErrRelationsSeal marks a refused relations stage. Causes such as the table
// cap, an invalid record, or context cancellation are preserved.
var ErrRelationsSeal = errors.New("pebble relations stage seal failed")

// PrepareRelations seals the final private node and edge projections. Call
// only after all link mutations finish, including replacing/removing raw
// parse edges as dictated by the linker. It does not run or certify linking.
// dir is a fresh, owned stage directory. The lease owner cleans it on error.
// extractorOf returns the frozen parse receipt ID for an exact graph file
// key, or empty for no receipt. It must not read or mutate the store.
func (b *BuildStore) PrepareRelations(ctx context.Context, dir string, base FileGraphBase, extractorOf func(string) string) (tables StageTables, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	defer func() {
		if err != nil {
			tables = StageTables{}
			err = fmt.Errorf("%w: %w", ErrRelationsSeal, err)
		}
	}()
	if b.first != nil {
		return StageTables{}, b.first
	}
	if base.build != b || !filepath.IsAbs(dir) {
		return StageTables{}, invalid("relations needs a retained file graph from this build and an absolute stage directory")
	}
	endpoints, err := b.checkRelationsEndpoints(ctx, base.witness.PlatformCatalogRevision, extractorOf)
	if err != nil {
		return StageTables{}, err
	}
	format := b.db.FormatMajorVersion().MaxTableFormat()
	gold := sortedRuns{dir: dir, name: "relations", options: b.options, format: format, limit: goldBufferBytes}
	refs := sortedRuns{dir: dir, name: "relations-refs", options: b.options, format: format, limit: goldBufferBytes}
	seal := &goldSeal{refs: refs, owner: b.keptOwner, edgeOwner: b.rebuildEdgeSourceOwner}
	if err := b.writeRows(ctx, &gold, StageRelations); err != nil {
		return StageTables{}, err
	}
	if endpoints.externalImports != 0 {
		if err := gold.add(countKey("external import endpoints"), encodeCount(endpoints.externalImports)); err != nil {
			return StageTables{}, err
		}
	}
	if endpoints.goArguments != 0 {
		if err := gold.add(countKey("go dataflow source endpoints"), encodeCount(endpoints.goArguments)); err != nil {
			return StageTables{}, err
		}
	}
	w := base.witness
	w.Stage = StageRelations
	w.Families = nil
	old := &fileGraphCursor{ctx: ctx, options: b.options, paths: base.paths, witness: goldWitness{digest: blake3.New()}}
	for _, f := range base.witness.Families {
		if f.Family == familyManifest || f.Family == familyFileMeta {
			w.Families = append(w.Families, f)
		}
		if f.Family != familyManifest {
			old.expected = append(old.expected, f)
		}
	}
	stream := goldWitness{digest: blake3.New()}
	paths, err := writeGoldDiff(ctx, old, &gold, goldFileCap-1, func(r row) error {
		if err := seal.visit(r); err != nil {
			return err
		}
		stream.add(r)
		return nil
	}, seal.finish)
	if err != nil {
		return StageTables{}, err
	}
	stream.flush()
	w.Families = append(w.Families, stream.families...)
	slices.SortFunc(w.Families, func(a, b FamilyWitness) int { return int(a.Family) - int(b.Family) })
	for i, path := range paths {
		paths[i] = filepath.Base(path)
	}
	return StageTables{Dir: dir, Files: paths, Incarnation: base.incarnation, Base: base.digest, Witness: w}, nil
}

const rebuildExternalImportPrefix = "external::"

// RebuildExternalImportTarget spells a native terminal import key without a
// target node. Keep the effective specifier verbatim, including ::export.
func RebuildExternalImportTarget(specifier string) (string, bool) {
	if specifier == "" || strings.ContainsRune(specifier, 0) {
		return "", false
	}
	return rebuildExternalImportPrefix + specifier, true
}

type relationsEndpointCounts struct {
	externalImports, goArguments int
}

// Ordinary Go argument sources can be unresolved identifiers or selectors.
// The built-in Go extractor is the only producer of these AST arg_of rows.
// Match exact native name IDs, not the general unresolved namespace.
func rebuildGoArgumentSource(e *graph.Edge, extractorOf func(string) string) bool {
	if extractorOf == nil || e.Kind != graph.EdgeArgOf || e.Origin != graph.OriginASTInferred ||
		e.FilePath == "" || e.Line <= 0 || extractorOf(e.FilePath) != "go" {
		return false
	}
	name := strings.TrimPrefix(graph.UnresolvedName(e.From), "*.")
	return token.IsIdentifier(name) && name != "_" &&
		slices.Contains(graph.UnresolvedNameCandidateIDsForName(name, ""), e.From)
}

// rebuildExternalProved checks the emitted target against its installed bytes
// and the exact frozen receipt at the original recording site.
func (b *BuildStore) rebuildExternalProved(r row, e *graph.Edge) (bool, error) {
	digest := sha256.Sum256(r.value)
	if (e.To == "external::error" && (e.Kind == graph.EdgeReturns || e.Kind == graph.EdgeThrows)) || rebuildCodegenExternal(e) {
		proof, err := pendingGet(b.pending, rebuildExternalEvidenceKey('p', r.key), copyValue)
		if err != nil || bytes.Equal(proof, digest[:]) {
			return err == nil, err
		}
	}
	evidence, err := pendingGet(b.pending, rebuildExternalEvidenceKey('m', r.key), copyValue)
	if err != nil || evidence == nil {
		return false, err
	}
	spec, savedDigest, err := readKeyString(evidence)
	if err != nil || len(savedDigest) != sha256.Size {
		return false, invalid("terminal external evidence")
	}
	target, valid := RebuildExternalImportTarget(spec)
	if !valid || target != e.To || !bytes.Equal(savedDigest, digest[:]) {
		return false, nil
	}
	custody, err := readRebuildEdgeCustody(b.pending, r.key)
	if err != nil || custody.raw == nil {
		return false, err
	}
	lang, err := pendingGet(b.pending, rebuildExternalEvidenceKey('r', custody.raw), copyValue)
	if err != nil || len(lang) == 0 {
		return false, err
	}
	raw, err := decodeLinkRecorder(custody.raw)
	if err != nil {
		return false, err
	}
	if raw.From != e.From || raw.Kind != e.Kind || raw.FilePath != e.FilePath || raw.Line != e.Line {
		return false, nil
	}
	name := graph.UnresolvedName(raw.To)
	if original, ok := strings.CutPrefix(name, "extern::"); ok {
		return original == spec && !strings.Contains(original, "::"), nil
	}
	if original, ok := strings.CutPrefix(name, "pyrel::"); ok {
		return e.Kind == graph.EdgeImports && original == spec, nil
	}
	original, ok := strings.CutPrefix(name, "import::")
	return ok && original != "" &&
		(original == spec || string(lang) == "typescript" || string(lang) == "javascript"), nil
}

func (b *BuildStore) checkRelationsEndpoints(ctx context.Context, revision uint32, extractorOf func(string) string) (relationsEndpointCounts, error) {
	var counts relationsEndpointCounts
	// The same lock covers this private lookup and the final node projection.
	// A saved logical node may be omitted by the profile; an unknown ID may
	// not. Native unresolved targets carry no declared node by design.
	check := func(r row, id string, e *graph.Edge, target, kept bool) error {
		if graph.IsProxyID(id) {
			return invalid("proxy relations endpoint")
		}
		if graph.IsBuiltinStub(id) {
			return nil // Whole-edge validation already proved role and scope.
		}
		n, err := pendingGet(b.pending, pendingNodeKey(id), decodeNodeLight)
		if err != nil {
			return err
		}
		if n != nil {
			if graph.IsProxyNode(n) {
				return invalid("proxy relations endpoint")
			}
			if n.Kind == graph.KindBuiltin {
				return invalid("builtin node cannot supply a relations endpoint")
			}
			if id == "" || n.ID != id {
				return invalid("invalid relations endpoint identity")
			}
			return nil
		}
		if target {
			if graph.IsUnresolvedTarget(id) && graph.UnresolvedName(id) != "" {
				return nil
			}
			if specifier, ok := strings.CutPrefix(id, rebuildExternalImportPrefix); ok {
				key, valid := RebuildExternalImportTarget(specifier)
				if !valid || key != id {
					return invalid("malformed external import endpoint")
				}
				proved, err := b.rebuildExternalProved(r, e)
				if err != nil {
					return err
				}
				if !proved {
					return fmt.Errorf("%w: unproved external relations endpoint %q: %w", errInvalidRecord, id,
						&RebuildUnknownProvenanceError{Count: 1, First: graph.EdgeIdentityFor(e)})
				}
				if kept {
					counts.externalImports++
				}
				return nil
			}
		}
		if !target && rebuildGoArgumentSource(e, extractorOf) {
			if kept {
				counts.goArguments++
			}
			return nil
		}
		return fmt.Errorf("%w: dangling relations endpoint %q", errInvalidRecord, id)
	}
	err := pendingVisit(ctx, b.pending, []byte{familyEdge, edgeOut}, func(r row) error {
		e, err := decodeEdge(r.value)
		if err != nil {
			return err
		}
		if !bytes.Equal(r.key, pendingEdgeKey(e)) {
			return invalid("working edge identity does not match key")
		}
		if err := b.validateRebuildPlatformEdge(ctx, b.pending, e, revision, extractorOf); err != nil {
			return err
		}
		kept := true
		if b.profile != FullProfile {
			_, kept, err = b.projectedEdgeOwner(e)
			if err != nil {
				return err
			}
		}
		if err := check(r, e.From, e, false, kept); err != nil {
			return err
		}
		return check(r, e.To, e, true, kept)
	})
	return counts, err
}

// fileGraphCursor streams non-overlapping SSTs in their writer order, with
// one open reader at a time. Only mutable families enter the diff. All base
// data, including the immutable file receipts, is checked against its witness.
type fileGraphCursor struct {
	ctx      context.Context
	options  *pebble.Options
	paths    []string
	expected []FamilyWitness
	witness  goldWitness
	table    *sstable.Reader
	iter     sstable.Iterator
	row      row
	last     []byte
	err      error
}

func (c *fileGraphCursor) First() bool                  { return c.Next() }
func (c *fileGraphCursor) Key() []byte                  { return c.row.key }
func (c *fileGraphCursor) ValueAndErr() ([]byte, error) { return c.row.value, nil }
func (c *fileGraphCursor) Error() error                 { return c.err }

func (c *fileGraphCursor) Close() error {
	if c.table == nil {
		return nil
	}
	var err error
	if c.iter != nil {
		err = c.iter.Close()
		c.iter = nil
	}
	err = errors.Join(err, c.table.Close())
	c.table = nil
	return err
}

func (c *fileGraphCursor) Next() bool {
	for c.err == nil {
		if c.err = c.ctx.Err(); c.err != nil {
			return false
		}
		first := c.iter == nil
		if first {
			if len(c.paths) == 0 {
				c.witness.flush()
				if !slices.Equal(c.witness.families, c.expected) {
					c.err = invalid("retained file-graph rows differ from their witness")
				}
				return false
			}
			c.table, c.err = openGoldTable(c.ctx, c.paths[0], c.options)
			if c.err != nil {
				return false
			}
			c.paths = c.paths[1:]
			c.iter, c.err = c.table.NewIter(sstable.NoTransforms, nil, nil, sstable.TableBlobContext{})
			if c.err != nil {
				return false
			}
		}
		if c.readRow(first) {
			if c.row.key[0] != familyFileMeta {
				return true
			}
			continue
		}
		c.err = errors.Join(c.err, c.Close())
	}
	return false
}

func (c *fileGraphCursor) readRow(first bool) bool {
	move := c.iter.Next
	if first {
		move = c.iter.First
	}
	kv := move()
	if kv == nil {
		c.err = c.iter.Error()
		return false
	}
	key := kv.K.UserKey
	if len(key) == 0 || bytes.Compare(c.last, key) >= 0 {
		c.err = invalid("retained file-graph keys are not strictly ordered")
		return false
	}
	if key[0] == familyManifest || key[0] == familyWitness {
		c.err = invalid("retained file graph includes a non-data row")
		return false
	}
	value, _, err := kv.Value(nil)
	if err != nil {
		c.err = err
		return false
	}
	c.last = append(c.last[:0], key...)
	c.row = row{key, value}
	c.witness.add(c.row)
	return true
}
