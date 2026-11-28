package store_pebble

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"iter"
	"path/filepath"
	"slices"
	"strings"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zeebo/blake3"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
)

// RebuildContractBatch contains final elected native rows, not proposals.
// Shapes are keyed by full type ID. No ordinary type node is rewritten.
type RebuildContractBatch struct {
	Nodes   []*graph.Node
	Edges   []*graph.Edge
	Shapes  map[string]*contracts.Shape
	Omitted []RebuildContractOmission
}

// RebuildContractOmission describes one omitted D83 logical record. RepoPrefix
// is exact (empty is standalone). ID is empty only if the canonical ID is
// unknown. RecordKey is the producer's stable nonzero digest of the logical
// record identity, not its payload; siblings can share the same scope and ID.
type RebuildContractOmission struct {
	RepoPrefix string
	FilePath   string
	Type       contracts.ContractType
	ID         string
	RecordKey  HeadDigest
}

var (
	ErrContractsSeal       = errors.New("pebble contracts stage seal failed")
	ErrContractIDCollision = errors.New("contract ID collides with an existing node")
)

// ContractIDCollisionError identifies the one conflicting ID; the caller
// must discard the failed build. Native can replace the incumbent. B2 may
// not rewrite an ordinary node as a side effect of contract storage.
type ContractIDCollisionError struct {
	ID    string
	Count int
}

func (e *ContractIDCollisionError) Error() string {
	return fmt.Sprintf("%s: %q (count %d)", ErrContractIDCollision, e.ID, e.Count)
}

func (e *ContractIDCollisionError) Unwrap() error { return ErrContractIDCollision }

// Scratch membership for duplicate checks and the actual selected ID stream.
var contractBatchPrefix = []byte{'g', 'q', '!'}

func contractBatchKey(tag byte, key []byte) []byte {
	return append(append(bytes.Clone(contractBatchPrefix), tag), key...)
}

const contractRowMetaVersion byte = 2
const contractShapeFormat byte = 1

// Use the checked contract metadata codec for output rows as well as input
// facts. The legacy JSON-compatible leaf fallback can change a typed value;
// the contract layout preserves types and nil versus empty collections.
func appendContractRowMetadata(b []byte, m map[string]any) ([]byte, error) {
	b = appendBool(b, m != nil)
	if m == nil {
		return b, nil
	}
	e := contractFactsEncoder{buf: []byte{metaFlatMagic0, contractRowMetaVersion}}
	writeFactMeta(&e, m, 0)
	if e.err != nil {
		return nil, e.err
	}
	return appendBytes(b, e.buf), nil
}

func contractShapeKey(id string) []byte { return keyStrings([]byte{familyShape}, id) }

func encodeContractShape(shape *contracts.Shape) ([]byte, error) {
	if shape == nil {
		return nil, invalid("nil contract shape")
	}
	e := contractFactsEncoder{buf: []byte{contractShapeFormat}}
	writeFactShape(&e, shape)
	return e.buf, e.err
}

func decodeContractShape(raw []byte) (*contracts.Shape, error) {
	if len(raw) == 0 || raw[0] != contractShapeFormat {
		return nil, invalid("contract shape format")
	}
	d := recordCursor{buf: raw[1:]}
	shape := readFactShape(&d)
	if err := d.finish(); err != nil {
		return nil, err
	}
	if shape == nil {
		return nil, invalid("nil stored contract shape")
	}
	return shape, nil
}

const (
	contractOmissionFormat      byte = 1
	maxContractOmissionKeyBytes      = 64 << 10
)

func contractOmissionKey(o RebuildContractOmission) ([]byte, error) {
	if o.FilePath == "" || o.Type == "" || o.RecordKey == (HeadDigest{}) ||
		len(o.RepoPrefix)+len(o.FilePath)+len(o.Type)+len(o.ID) > maxContractOmissionKeyBytes {
		return nil, invalid("contract omission scope")
	}
	key := append(keyStrings([]byte{familyOmission}, o.RepoPrefix, o.FilePath, string(o.Type), o.ID), o.RecordKey[:]...)
	if len(key) > maxContractOmissionKeyBytes {
		return nil, invalid("contract omission key too large")
	}
	return key, nil
}

// Native uses http IDs for OpenAPI operations and dep IDs for Go modules.
func contractOmissionIDMatchesType(o RebuildContractOmission) bool {
	var prefix string
	switch o.Type {
	case contracts.ContractHTTP, contracts.ContractGRPC, contracts.ContractThrift, contracts.ContractGraphQL,
		contracts.ContractTopic, contracts.ContractWS, contracts.ContractEnv, contracts.ContractDI, contracts.ContractTRPC:
		prefix = string(o.Type)
	case contracts.ContractOpenAPI:
		prefix = "http"
	case contracts.ContractDependency:
		prefix = "dep"
	default:
		return false
	}
	if o.ID == "" {
		return true
	}
	kind, id, found := strings.Cut(o.ID, "::")
	return found && kind == prefix && id != ""
}

func decodeContractOmission(key, value []byte) (RebuildContractOmission, error) {
	if len(key) == 0 || key[0] != familyOmission || len(key) > maxContractOmissionKeyBytes ||
		len(value) != 1 || value[0] != contractOmissionFormat {
		return RebuildContractOmission{}, invalid("contract omission row format")
	}
	var fields [4]string
	var err error
	rest := key[1:]
	for i := range fields {
		fields[i], rest, err = readKeyString(rest)
		if err != nil {
			return RebuildContractOmission{}, err
		}
	}
	if len(rest) != len(HeadDigest{}) || fields[1] == "" || fields[2] == "" || HeadDigest(rest) == (HeadDigest{}) {
		return RebuildContractOmission{}, invalid("contract omission scope")
	}
	return RebuildContractOmission{RepoPrefix: fields[0], FilePath: fields[1], Type: contracts.ContractType(fields[2]), ID: fields[3], RecordKey: HeadDigest(rest)}, nil
}

// PrepareContracts writes the exact atomic diff against a retained Relations
// witness. C3 owns native election and coverage selection; it must supply
// unique, bounded batches and no other graph writes may interleave. The
// producer may read private inputs between batches, but must not assume that
// the working graph still represents the frozen Relations cut. An error
// leaves no offer and this private build must be discarded.
func (b *BuildStore) PrepareContracts(ctx context.Context, dir string, base RelationsBase,
	batches iter.Seq2[RebuildContractBatch, error], coverage ContractCoverage,
) (tables StageTables, err error) {
	defer func() {
		if err != nil {
			tables = StageTables{}
			err = fmt.Errorf("%w: %w", ErrContractsSeal, err)
		}
	}()
	if base.files.build != b || !filepath.IsAbs(dir) || batches == nil {
		return StageTables{}, invalid("contracts needs its retained predecessor, stage directory and batches")
	}
	if err := b.withWrites(func() error {
		if b.contractOverlay {
			return ErrRebuildContractOverlay
		}
		if b.contractsStarted {
			return invalid("contracts already attempted for this build")
		}
		b.contractsStarted = true
		return ctx.Err()
	}); err != nil {
		return StageTables{}, err
	}
	defer func() {
		if err != nil {
			_ = b.withWrites(func() error { return err })
		}
	}()
	for batch, err := range batches {
		if err != nil {
			return StageTables{}, err
		}
		if err := b.withWrites(func() error { return b.writeContractBatch(ctx, batch) }); err != nil {
			return StageTables{}, err
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.first != nil {
		return StageTables{}, b.first
	}
	if err := b.checkContractEndpoints(ctx, base.witness.PlatformCatalogRevision); err != nil {
		return StageTables{}, err
	}
	format := b.db.FormatMajorVersion().MaxTableFormat()
	gold := sortedRuns{dir: dir, name: "contracts", options: b.options, format: format, limit: goldBufferBytes}
	refs := sortedRuns{dir: dir, name: "contracts-refs", options: b.options, format: format, limit: goldBufferBytes}
	seal := &goldSeal{refs: refs, owner: b.keptOwner, edgeOwner: b.rebuildEdgeSourceOwner}
	if err := b.writeRows(ctx, &gold, StageContracts); err != nil {
		return StageTables{}, err
	}
	if err := pendingVisit(ctx, b.pending, []byte{familyOmission}, func(r row) error {
		return gold.add(bytes.Clone(r.key), bytes.Clone(r.value))
	}); err != nil {
		return StageTables{}, err
	}
	if err := base.addEndpointCounts(ctx, &gold); err != nil {
		return StageTables{}, err
	}
	stream := goldWitness{digest: blake3.New()}
	paths, err := writeCheckedGoldDiff(ctx, base.cursor(ctx), &gold, goldFileCap-1, func(r row) error {
		switch r.key[0] {
		case familyShape:
			if _, err := decodeContractShape(r.value); err != nil {
				return err
			}
		case familyOmission:
			if _, err := decodeContractOmission(r.key, r.value); err != nil {
				return err
			}
		default:
			if err := seal.visit(r); err != nil {
				return err
			}
		}
		if r.key[0] == familyNode {
			n, err := decodeNodeLight(r.value)
			if err != nil {
				return err
			}
			// The GoFrame parse-only KindContract row has language go;
			// native registry contract nodes and dependency seeds use contract.
			if n.Kind == graph.KindContract && n.Language == "contract" {
				if err := b.pending.Set(contractBatchKey('w', keyStrings(nil, n.ID)), nil, pebble.NoSync); err != nil {
					return err
				}
			}
		}
		stream.add(r)
		return nil
	}, seal.finish, b.checkContractChange)
	if err != nil {
		return StageTables{}, err
	}
	stream.flush()
	w := base.witness
	w.Stage, w.Contracts = StageContracts, &coverage
	w.Families = nil
	for _, f := range base.witness.Families {
		if f.Family == familyManifest || f.Family == familyFileMeta {
			w.Families = append(w.Families, f)
		}
	}
	w.Families = append(w.Families, stream.families...)
	slices.SortFunc(w.Families, func(a, b FamilyWitness) int { return int(a.Family) - int(b.Family) })
	raw, err := encodeWitness(w)
	if err != nil {
		return StageTables{}, err
	}
	b.contractsSealed = blake3.Sum256(raw)
	for i, path := range paths {
		paths[i] = filepath.Base(path)
	}
	return StageTables{Dir: dir, Files: paths, Incarnation: base.files.incarnation, Base: base.digest, Witness: w}, nil
}

// VisitPreparedContractIDs streams registry and dependency IDs from the
// actual selected sealed N stream, not the producer's proposals. The owner
// calls this before offering Contracts and compares against its D83 set.
func (b *BuildStore) VisitPreparedContractIDs(ctx context.Context, tables StageTables, yield func(string) error) error {
	raw, err := encodeWitness(tables.Witness)
	if err != nil {
		return err
	}
	b.mu.Lock()
	sealed, mutationErr := b.contractsSealed, b.first
	b.mu.Unlock()
	if mutationErr != nil {
		return mutationErr
	}
	if sealed == (HeadDigest{}) || sealed != blake3.Sum256(raw) || tables.Witness.Stage != StageContracts || yield == nil {
		return invalid("contract ID visit does not name the prepared stage")
	}
	prefix := contractBatchKey('w', nil)
	return pendingVisit(ctx, b.pending, prefix, func(r row) error {
		id, rest, err := readKeyString(r.key[len(prefix):])
		if err != nil || id == "" || len(rest) != 0 {
			return invalid("prepared contract ID key")
		}
		return yield(id)
	})
}

// The normal graph writer emits full rows. Only claimed primary changes are
// authorized after Relations; secondary and count rows follow that graph.
func (b *BuildStore) checkContractChange(r row) error {
	var tag byte
	var key []byte
	switch r.key[0] {
	case familyNode:
		tag, key = 'n', r.key
	case familyEdge:
		if len(r.key) < 2 || r.key[1] != edgeOut {
			return nil
		}
		e, err := decodeEdge(r.value)
		if err != nil {
			return err
		}
		tag, key = 'e', pendingEdgeKey(e)
	case familyShape:
		tag, key = 't', r.key
	case familyOmission:
		tag, key = 'o', r.key
	default:
		return nil
	}
	claimed, err := pendingGet(b.pending, contractBatchKey(tag, key), func([]byte) (bool, error) { return true, nil })
	if err != nil {
		return err
	}
	if !claimed {
		return invalid("unclaimed primary row changed after relations")
	}
	return nil
}

func (b *BuildStore) claimContractRow(tag byte, key []byte) error {
	marker := contractBatchKey(tag, key)
	found, err := pendingGet(b.pending, marker, func([]byte) (bool, error) { return true, nil })
	if err != nil {
		return err
	}
	if found {
		return invalid("duplicate contract stage row")
	}
	return b.pending.Set(marker, nil, pebble.NoSync)
}

func (b *BuildStore) writeContractBatch(ctx context.Context, batch RebuildContractBatch) error {
	for _, n := range batch.Nodes {
		if err := ctx.Err(); err != nil {
			return err
		}
		if n == nil || n.ID == "" || graph.IsProxyNode(n) || graph.IsProxyID(n.ID) || graph.IsBuiltinStub(n.ID) || graph.IsUnresolvedTarget(n.ID) ||
			strings.HasPrefix(n.ID, rebuildExternalImportPrefix) || (n.Kind != graph.KindContract && n.Kind != graph.KindConfigKey) {
			return invalid("invalid contract stage node")
		}
		if n.Kind == graph.KindContract && n.Language != "contract" {
			return invalid("contract registry node language")
		}
		if _, exists := n.Meta["shape"]; exists {
			return invalid("contract batch must supply split shape rows")
		}
		if err := b.claimContractRow('n', pendingNodeKey(n.ID)); err != nil {
			return err
		}
		prev, err := b.pendingNode(n.ID)
		if err != nil {
			return err
		}
		if prev != nil && (n.Kind != graph.KindContract || n.Meta["type"] != string(contracts.ContractDependency) || !rebuildDependencySeed(prev)) {
			return &ContractIDCollisionError{ID: n.ID, Count: 1}
		}
		value, err := encodeNodeRecord(n, appendContractRowMetadata)
		if err != nil {
			return err
		}
		if err := b.writeNodeRecord(n, prev, value); err != nil {
			return err
		}
	}
	for _, e := range batch.Edges {
		if err := ctx.Err(); err != nil {
			return err
		}
		if e == nil || e.From == "" || e.To == "" || graph.StructuralEdgeTargetInvalid(e.Kind, e.To) {
			return invalid("invalid contract stage edge")
		}
		switch e.Kind {
		case graph.EdgeProvides, graph.EdgeConsumes, graph.EdgeHandlesRoute, graph.EdgeReadsConfig:
		case graph.EdgeCalls:
			if e.Meta["via"] != "spring.Bean" {
				return invalid("unproved contract Call emitter")
			}
		default:
			return invalid("unexpected contract stage edge kind")
		}
		key := pendingEdgeKey(e)
		if err := b.claimContractRow('c', key); err != nil {
			return err
		}
		old, err := b.pendingEdge(key)
		if err != nil {
			return err
		}
		if old != nil {
			continue // native INSERT OR IGNORE keeps the incumbent payload
		}
		val, err := encodeEdgeRecord(e, appendContractRowMetadata)
		if err != nil {
			return err
		}
		if err := b.writeEdgeRecord(e, key, val, rebuildEdgeCustody{}, false); err != nil {
			return err
		}
		// Only inserted edges can authorize a primary-row change.
		if err := b.pending.Set(contractBatchKey('e', key), nil, pebble.NoSync); err != nil {
			return err
		}
	}
	for id, shape := range batch.Shapes {
		if err := ctx.Err(); err != nil {
			return err
		}
		if id == "" {
			return invalid("empty contract shape ID")
		}
		key := contractShapeKey(id)
		if err := b.claimContractRow('t', key); err != nil {
			return err
		}
		val, err := encodeContractShape(shape)
		if err != nil {
			return err
		}
		if err := b.pending.Set(key, val, pebble.NoSync); err != nil {
			return err
		}
	}
	for _, omitted := range batch.Omitted {
		if err := ctx.Err(); err != nil {
			return err
		}
		key, err := contractOmissionKey(omitted)
		if err != nil {
			return err
		}
		if !contractOmissionIDMatchesType(omitted) {
			return invalid("contract omission type or ID")
		}
		// The file-graph receipt binds the admitted graph path to its exact
		// repository. An arbitrary node with that path is not file authority.
		file, err := pendingGet(b.pending, fileMetaKey(omitted.RepoPrefix, omitted.FilePath), func(raw []byte) (graph.FileMetaRow, error) {
			return decodeFileMeta(omitted.FilePath, raw)
		})
		if err != nil {
			return err
		}
		if file.FilePath == "" {
			return invalid("contract omission lacks admitted file scope")
		}
		if err := b.claimContractRow('o', key); err != nil {
			return err
		}
		if err := b.pending.Set(key, []byte{contractOmissionFormat}, pebble.NoSync); err != nil {
			return err
		}
	}
	return nil
}

func rebuildDependencySeed(n *graph.Node) bool {
	return n.Kind == graph.KindContract && strings.HasPrefix(n.ID, "dep::") && n.Name == n.ID && n.FilePath == "go.mod" &&
		n.Language == "contract" && len(n.Meta) == 2 && n.Meta["type"] == string(contracts.ContractDependency) && n.Meta["role"] == string(contracts.RoleConsumer)
}

func (b *BuildStore) checkContractEndpoints(ctx context.Context, revision uint32) error {
	prefix := contractBatchKey('e', nil)
	return pendingVisit(ctx, b.pending, prefix, func(r row) error {
		e, err := b.pendingEdge(r.key[len(prefix):])
		if err != nil {
			return err
		}
		if e == nil || !bytes.Equal(pendingEdgeKey(e), r.key[len(prefix):]) {
			return invalid("missing contract edge")
		}
		if err := b.validateRebuildPlatformEdge(b.pending, e, revision, nil); err != nil {
			return err
		}
		for _, id := range []string{e.From, e.To} {
			n, err := b.pendingNode(id)
			if err != nil {
				return err
			}
			if n == nil || n.ID != id || n.Kind == graph.KindBuiltin || graph.IsProxyNode(n) || graph.IsProxyID(id) {
				return fmt.Errorf("%w: invalid contract stage endpoint %q", errInvalidRecord, id)
			}
		}
		return nil
	})
}

func (b *BuildStore) writeContractShapes(ctx context.Context, gold *sortedRuns) error {
	return pendingVisit(ctx, b.pending, []byte{familyShape}, func(r row) error {
		id, rest, err := readKeyString(r.key[1:])
		if err != nil || id == "" || len(rest) != 0 {
			return invalid("contract shape key")
		}
		n, err := b.pendingNode(id)
		if err != nil {
			return err
		}
		if n == nil || n.ID != id || (n.Kind != graph.KindType && n.Kind != graph.KindInterface) || graph.IsProxyNode(n) {
			return invalid("contract shape needs its declared type")
		}
		if err := gold.add(bytes.Clone(r.key), bytes.Clone(r.value)); err != nil {
			return err
		}
		return addPendingCount(b.pending, countKey("repo node bytes", n.RepoPrefix), len(r.value))
	})
}
