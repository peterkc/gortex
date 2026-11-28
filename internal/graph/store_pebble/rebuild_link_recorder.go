package store_pebble

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"strings"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/codegen"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/pathkey"
)

// This family lives only in the private build. For each winning live key it
// can hold a raw receipt key, a producer proof, or an untracked-move marker.
// Proofs and markers bind to the saved edge bytes. A stale digest returns
// ErrRebuildUnknownProvenance; it is not a missing record.
var rebuildLinkRecorderPrefix = []byte{'g', 'Z'}

func rebuildLinkRecorderKey(live []byte) []byte {
	return append(bytes.Clone(rebuildLinkRecorderPrefix), live...)
}

// Each code identifies a producer and the proof that admitted its row.
type rebuildEdgeProof byte

const (
	// Direct capability policy passed.
	rebuildProofDirect rebuildEdgeProof = iota + 1
	// Checked receiver Call and field ownership.
	rebuildProofReceiver
	// Wrapper trigger or cross-language role proof.
	rebuildProofTemporalSynthetic
	// Emits/ListensOn, transport, and declaration proof.
	rebuildProofEventChannel
	// Declarations and selector/protocol metadata.
	rebuildProofSwiftObjC
	// First source recorder and declaration proof.
	rebuildProofRNBridge
	// Declarations and export metadata.
	rebuildProofRNPair
	// A move without a recorder is not a stationary receipt or a proof.
	rebuildProofUntrackedMove
	// Checked field accesses, registrar calls, callback references and declarations.
	rebuildProofObserver
	// Both callable declarations and collection-role metadata.
	rebuildProofClosureCollection
	// Elected membership, lifecycle declarations and qualifying call.
	rebuildProofReactSetState
	// Elected membership, build declarations and qualifying call.
	rebuildProofFlutterSetState
	// Exact expect and actual declaration ownership.
	rebuildProofKMPExpectActual
	// First source recorder and Expo declaration ownership.
	rebuildProofExpoModules
	// Exact Fabric spec and manager declaration ownership.
	rebuildProofFabric
	// Exact notification Call and handler receipts for a fresh fanout row.
	rebuildProofMediatRFanout
	// Exact Laravel Call, handler, and listener route receipts for fanout.
	rebuildProofLaravelEventFanout
	// Exact function pointer inputs and elected slot member for fanout.
	rebuildProofFnPointerFanout
	// Exact macro declaration, body Call and use-site receipts.
	rebuildProofMacroExpansion
	// Exact EF Core entity, mapping and sibling receipts for ModelsTable.
	rebuildProofEFCoreModel
	// Exact SvelteKit route roles and workspace placement.
	rebuildProofSvelteKitLoad
	// Native external-call group and package election over proved live bytes.
	rebuildProofExternalCallSynthesis
	// A checked Go builtin repoint or return lift supplied this source at this site.
	rebuildProofGoBuiltinSource
	// Exact checked Go AST dataflow source, recording site and kind.
	rebuildProofGoDataflowSource
	rebuildProofLimit
)

type rebuildEdgeCustody struct {
	raw    []byte
	proof  rebuildEdgeProof
	source string // original checked Go dataflow source during a detour
}

const rebuildEdgeProofFormat byte = 1

// These kinds may be treated as original receipts by a later custody
// consumer. SetState also reads MemberOf to elect the first owner.
func rebuildReceiptInputKind(kind graph.EdgeKind) bool {
	switch kind {
	case graph.EdgeAccessesField, graph.EdgeCalls, graph.EdgeReads, graph.EdgeReferences, graph.EdgeMemberOf:
		return true
	default:
		return false
	}
}

func (c rebuildEdgeCustody) encode(edge []byte) ([]byte, error) {
	if c.proof == 0 {
		return c.raw, nil
	}
	if c.proof >= rebuildProofLimit || len(c.raw) != 0 || (c.source != "" && c.proof != rebuildProofGoDataflowSource) {
		return nil, invalid("edge custody proof")
	}
	digest := rebuildEdgeProofDigest(edge, c.source)
	return append(append([]byte{rebuildEdgeProofFormat, byte(c.proof)}, digest[:]...), c.source...), nil
}

func rebuildEdgeProofDigest(edge []byte, source string) [sha256.Size]byte {
	if source == "" {
		return sha256.Sum256(edge)
	}
	h := sha256.New()
	_, _ = h.Write(edge)
	_, _ = h.Write([]byte(source))
	var digest [sha256.Size]byte
	copy(digest[:], h.Sum(nil))
	return digest
}

func readRebuildEdgeCustody(reader pebble.Reader, live []byte) (rebuildEdgeCustody, error) {
	return pendingGet(reader, rebuildLinkRecorderKey(live), func(raw []byte) (rebuildEdgeCustody, error) {
		if len(raw) >= 2 && raw[0] == familyEdge && raw[1] == edgeOut {
			if _, err := decodeLinkRecorder(raw); err != nil {
				return rebuildEdgeCustody{}, err
			}
			return rebuildEdgeCustody{raw: bytes.Clone(raw)}, nil
		}
		if len(raw) < 2+sha256.Size || raw[0] != rebuildEdgeProofFormat || raw[1] == 0 || rebuildEdgeProof(raw[1]) >= rebuildProofLimit {
			return rebuildEdgeCustody{}, invalid("edge custody format")
		}
		proof, source := rebuildEdgeProof(raw[1]), string(raw[2+sha256.Size:])
		if source != "" && proof != rebuildProofGoDataflowSource {
			return rebuildEdgeCustody{}, invalid("edge custody source")
		}
		match, err := pendingGet(reader, live, func(edge []byte) (bool, error) {
			digest := rebuildEdgeProofDigest(edge, source)
			return bytes.Equal(digest[:], raw[2:2+sha256.Size]), nil
		})
		if err != nil {
			return rebuildEdgeCustody{}, err
		}
		if !match {
			id, err := decodeLinkRecorder(live)
			if err != nil {
				return rebuildEdgeCustody{}, err
			}
			return rebuildEdgeCustody{}, &RebuildUnknownProvenanceError{Count: 1, First: id}
		}
		return rebuildEdgeCustody{proof: proof, source: source}, nil
	})
}

// rebuildReceiptKey is for readers that require raw receipt custody. A
// genuinely stationary row can prove its exact live key. A synthetic or
// untracked row cannot use a receipt at that key instead of its own proof.
func rebuildReceiptKey(reader pebble.Reader, live []byte) ([]byte, error) {
	custody, err := readRebuildEdgeCustody(reader, live)
	if err != nil {
		return nil, err
	}
	if custody.proof != 0 {
		id, err := decodeLinkRecorder(live)
		if err != nil {
			return nil, err
		}
		return nil, &RebuildUnknownProvenanceError{Count: 1, First: id}
	}
	if custody.raw != nil {
		return custody.raw, nil
	}
	return live, nil
}

// Go builtin and dataflow source proofs certify the source, kind and site,
// not the target. Dataflow custody remembers the original source during a
// reversible repoint. Each winning move binds custody to the new saved bytes.
func (c rebuildEdgeCustody) reindexed(key []byte, old, e *graph.Edge, capture, goBuiltinRepoint bool) rebuildEdgeCustody {
	if old.Kind == e.Kind && old.FilePath == e.FilePath && old.Line == e.Line {
		if old.From == e.From && c.proof == rebuildProofGoBuiltinSource {
			return c
		}
		// A checked producer proves the new builtin source. The receipt's
		// dataflow proof only certifies the original unresolved source.
		if goBuiltinRepoint {
			if _, ok := rebuildGoBuiltinSource(e); ok {
				return rebuildEdgeCustody{proof: rebuildProofGoBuiltinSource}
			}
		}
		if c.proof == rebuildProofGoDataflowSource {
			original := c.source
			if original == "" {
				original = old.From
			}
			if e.From == original {
				c.source = ""
			} else {
				c.source = original
			}
			return c
		}
	}
	return c.moved(key, e.Kind, capture)
}

// A generic identity change may keep raw lineage. A producer proof does
// not certify a different identity. A tracked kind without raw lineage
// must not look like a stationary receipt at the new key.
func (c rebuildEdgeCustody) moved(old []byte, kind graph.EdgeKind, capture bool) rebuildEdgeCustody {
	if c.raw != nil {
		return c
	}
	if c.proof == 0 && capture && old != nil {
		return rebuildEdgeCustody{raw: old}
	}
	if c.proof != 0 || rebuildReceiptInputKind(kind) {
		return rebuildEdgeCustody{proof: rebuildProofUntrackedMove}
	}
	return rebuildEdgeCustody{}
}

func decodeLinkRecorder(key []byte) (graph.EdgeIdentity, error) {
	var out graph.EdgeIdentity
	var kind string
	tail := key[2:]
	for _, field := range []*string{&out.From, &kind, &out.To, &out.FilePath} {
		var err error
		*field, tail, err = readKeyString(tail)
		if err != nil {
			return graph.EdgeIdentity{}, err
		}
	}
	if len(tail) != 8 {
		return graph.EdgeIdentity{}, invalid("link recorder line")
	}
	out.Kind = graph.EdgeKind(kind)
	out.Line = int(int64(binary.BigEndian.Uint64(tail) ^ (1 << 63)))
	return out, nil
}

// The 'p' column binds direct parse producers (Go error and codegen edges)
// to their bytes. 'r' marks eligible raw receipt identities; 'm' binds a
// terminal move's effective input to installed bytes. All three are private.
func rebuildExternalEvidenceKey(col byte, edge []byte) []byte {
	return append([]byte{'g', 'q', '~', col}, edge...)
}

func (b *BuildStore) recordExternalReceipt(f ParseFacts, e *graph.Edge) error {
	if e == nil || f.EffectiveLanguage == "" || e.FilePath == "" || e.FilePath != pathkey.Normalize(f.File.FilePath) {
		return nil
	}
	goError := e.To == "external::error" && f.EffectiveLanguage == "go" && e.Origin == graph.OriginASTInferred &&
		(e.Kind == graph.EdgeReturns || e.Kind == graph.EdgeThrows)
	if goError || rebuildCodegenExternal(e) {
		val, err := encodeEdge(e)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(val)
		return b.pending.Set(rebuildExternalEvidenceKey('p', pendingEdgeKey(e)), digest[:], pebble.NoSync)
	}
	name := graph.UnresolvedName(e.To)
	if strings.HasPrefix(name, "extern::") || strings.HasPrefix(name, "import::") ||
		(e.Kind == graph.EdgeImports && strings.HasPrefix(name, "pyrel::")) {
		return b.pending.Set(rebuildExternalEvidenceKey('r', pendingEdgeKey(e)), []byte(f.EffectiveLanguage), pebble.NoSync)
	}
	return nil
}

// GeneratedBy is emitted in parse replay only by the native codegen marker,
// annotation and Lombok materialization producers. Check their exact target
// from the saved tool/source fields, not just the external namespace.
func rebuildCodegenExternal(e *graph.Edge) bool {
	if e.Kind != graph.EdgeGeneratedBy || e.Line != 0 {
		return false
	}
	tool, ok := e.Meta["tool"].(string)
	if !ok {
		return false
	}
	if source, present := e.Meta["source"]; present {
		path, ok := source.(string)
		if !ok || e.From != e.FilePath || e.Origin != graph.OriginASTResolved {
			return false
		}
		marker := codegen.Marker{Generated: true, Tool: tool, Source: path}
		return e.To == codegen.BuildGraphArtifacts(e.FilePath, marker)[0].To
	}
	marker := codegen.Marker{Generated: true, Tool: tool}
	return tool != "" && e.To == codegen.BuildGraphArtifacts(e.FilePath, marker)[0].To &&
		(e.Origin == graph.OriginASTResolved || (e.Origin == graph.OriginASTInferred && tool == "lombok"))
}

// RecordRebuildExternalMoves records exact terminal choices from the main
// resolver. A stationary incumbent or losing proposal receives no evidence.
func (b *BuildStore) RecordRebuildExternalMoves(ctx context.Context, moves []graph.EdgeReindex, effective map[*graph.Edge]string) error {
	if len(effective) == 0 {
		return nil
	}
	return b.withWrites(func() error { return b.recordRebuildExternalMoves(ctx, moves, effective) })
}

// The file-import emitter already holds the mutation lock.
func (b *BuildStore) recordRebuildExternalMoves(ctx context.Context, moves []graph.EdgeReindex, effective map[*graph.Edge]string) error {
	for _, move := range moves {
		if err := ctx.Err(); err != nil {
			return err
		}
		e := move.Edge
		spec, ok := effective[e]
		if !ok {
			continue
		}
		target, valid := RebuildExternalImportTarget(spec)
		if !valid || e.To != target {
			return invalid("terminal external emitter target")
		}
		old := *e
		old.To = move.OldTo
		if move.OldKind != "" {
			old.Kind = move.OldKind
		}
		live, raw := pendingEdgeKey(e), pendingEdgeKey(&old)
		custody, err := readRebuildEdgeCustody(b.pending, live)
		if err != nil {
			return err
		}
		if !bytes.Equal(custody.raw, raw) {
			continue
		}
		val, err := encodeEdge(e)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(val)
		evidence := keyStrings(nil, spec)
		evidence = append(evidence, digest[:]...)
		if err := b.pending.Set(rebuildExternalEvidenceKey('m', live), evidence, pebble.NoSync); err != nil {
			return err
		}
	}
	return nil
}
