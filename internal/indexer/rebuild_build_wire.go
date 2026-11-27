package indexer

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/gob"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/parser/tsalias"
	"github.com/zzet/gortex/internal/resolver"
)

const (
	rebuildRequestName  = "request"
	rebuildRequestBytes = 64 << 20 // control data, not parsed file content
	rebuildReplyBytes   = 1 << 20  // witness, SST basenames, or one refusal
)

var (
	ErrRebuildWorker               = errors.New("rebuild worker failed")
	errRebuildBuildIncomplete      = errors.New("rebuild worker stopped before the complete head")
	errRebuildBuildWire            = errors.New("invalid rebuild worker transport")
	errRebuildBuildRequestSize     = errors.New("rebuild request exceeds its byte limit")
	errRebuildCrossRepoUnavailable = errors.New("cross-repository linking not available in b2")
)

type rebuildBuildRequest struct {
	Root, ParseDir         string
	ParseLimit             int
	Config                 []byte
	Policy                 rebuildParsePolicy
	Recipes                map[string]rebuildDigest
	Bounds                 rebuildTransformBounds
	Incarnation            string
	ProjectName            string
	BaseRecipe             store_pebble.HeadDigest
	Witness                store_pebble.Witness
	Files                  int
	TrackedRepos           []string // b2 accepts only the frozen empty set
	SkipPlaceholderSources bool
	ProvidersOff           bool // required D2 semantic and resolver-LSP policy
}

// New process, same effective settings. JSON keeps the difference between a
// nil *bool and a configured false, which gob would flatten to a zero value.
// Input path bytes and policy files instead travel in gob without UTF-8 folding.
func writeRebuildBuildRequest(ctx context.Context, dir string, req rebuildBuildRequest, cfg config.IndexConfig, inputs []rebuildInput) (err error) {
	req.Config, err = json.Marshal(cfg)
	if err != nil {
		return err
	}
	if err := req.checkProjectRecipe(); err != nil {
		return err
	}
	req.Files = len(inputs)
	f, err := os.OpenFile(filepath.Join(dir, rebuildRequestName), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	enc := gob.NewEncoder(&rebuildRequestWriter{out: f, left: rebuildRequestBytes})
	if err := enc.Encode(&req); err != nil {
		return err
	}
	for _, input := range inputs {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := enc.Encode(&input); err != nil {
			return err
		}
	}
	return nil
}

type rebuildRequestWriter struct {
	out  io.Writer
	left int
}

func (w *rebuildRequestWriter) Write(p []byte) (int, error) {
	if len(p) > w.left {
		return 0, errRebuildBuildRequestSize
	}
	n, err := w.out.Write(p)
	w.left -= n
	return n, err
}

func readRebuildBuildRequest(ctx context.Context, dir string) (req rebuildBuildRequest, inputs []rebuildInput, err error) {
	f, err := os.Open(filepath.Join(dir, rebuildRequestName))
	if err != nil {
		return req, nil, err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	st, err := f.Stat()
	if err != nil {
		return req, nil, err
	}
	if !st.Mode().IsRegular() || st.Size() > rebuildRequestBytes {
		return req, nil, errRebuildBuildRequestSize
	}
	dec := gob.NewDecoder(io.LimitReader(f, rebuildRequestBytes))
	if err := dec.Decode(&req); err != nil {
		return req, nil, fmt.Errorf("%w: decode request: %w", errRebuildBuildWire, err)
	}
	if err := store_pebble.ValidateIncarnation(req.Incarnation); err != nil {
		return req, nil, err
	}
	if !filepath.IsAbs(req.Root) || !filepath.IsAbs(req.ParseDir) || req.Files < 0 ||
		req.ParseLimit <= 0 || req.Bounds.CaptureBytes <= 0 || req.Bounds.FileBytes < req.Bounds.CaptureBytes || req.Bounds.Dir != "" ||
		req.Witness.Stage != store_pebble.StageManifest || len(req.Witness.Families) != 0 {
		return req, nil, fmt.Errorf("%w: request identity or bounds", errRebuildBuildWire)
	}
	if err := req.checkProjectRecipe(); err != nil {
		return req, nil, err
	}
	// Count is not an allocation hint supplied by the peer. Charge each
	// append to a record that actually decoded within the request file cap.
	for range req.Files {
		if err := ctx.Err(); err != nil {
			return req, nil, err
		}
		var input rebuildInput
		if err := dec.Decode(&input); err != nil {
			return req, nil, err
		}
		inputs = append(inputs, input)
	}
	if err := dec.DecodeValue(reflect.Value{}); err != io.EOF {
		return req, nil, fmt.Errorf("%w: trailing or unreadable request data: %v", errRebuildBuildWire, err)
	}
	return req, inputs, nil
}

type rebuildBuildReply struct {
	Files             []string
	Base              store_pebble.HeadDigest
	Witness           store_pebble.Witness
	Refusal           uint8
	Detail            string
	AttributionChains uint64 // present only on a refused attribution pass
	UnknownProvenance uint64 // present only on a refused provenance check
}

var rebuildFailureKinds = [...]error{
	ErrRebuildWorker, ErrBuildInputChanged, ErrBuildTransformBounds, errParseEntryCorrupt,
	errParseEntryConflict, errParseEntryTooLarge, store_pebble.ErrStageTableLimit,
	errRebuildBuildIncomplete, errRebuildBuildWire, errRebuildBuildRequestSize,
	store_pebble.ErrRelationsSeal, store_pebble.ErrRebuildReexports,
	store_pebble.ErrRebuildTemporalConst, store_pebble.ErrSearchSeal,
	resolver.ErrRebuildCppIncludeInput, store_pebble.ErrRebuildFileImports,
	store_pebble.ErrRebuildLinkInputSize, store_pebble.ErrRebuildLink,
	store_pebble.ErrRebuildPlaceholderSources, resolver.ErrRebuildCrossPackageGuard, store_pebble.ErrRebuildView,
	errRebuildInputSidecar, errRebuildGraphPathCollision, store_pebble.ErrInvalidIncarnation,
	errRebuildCallerAttribution, errRebuildGoEvidence, tsalias.ErrFrozenAliasUnproved, tsalias.ErrFrozenAliasPath,
	errRebuildResolverInputDenied, errRebuildResolverNamespace, errRebuildNpmInput, store_pebble.ErrRebuildImportDirectories,
	errRebuildCrossRepoUnavailable, store_pebble.ErrRebuildAttributionChain,
	resolver.ErrRebuildGoExternalRecordingPath, store_pebble.ErrRebuildUnknownProvenance, errRebuildModuleInput, errRebuildWorkspaceOutput,
	store_pebble.ErrReceiverMutationNonconvergent, store_pebble.ErrReceiverMutationInput, store_pebble.ErrRebuildReceiverMutation,
	store_pebble.ErrRebuildGRPC,
	store_pebble.ErrRebuildTemporal,
	store_pebble.ErrRebuildEventChannel,
	store_pebble.ErrRebuildSwiftObjC,
	store_pebble.ErrRebuildReactNativeBridge,
	store_pebble.ErrRebuildReactNativeNativePairing,
	store_pebble.ErrRebuildObserver,
	store_pebble.ErrRebuildClosureCollection,
	store_pebble.ErrRebuildReactSetState,
	store_pebble.ErrRebuildFlutterSetState,
	store_pebble.ErrRebuildKMPExpectActual,
	store_pebble.ErrRebuildExpoModuleBridge,
	store_pebble.ErrRebuildFabric,
	nil, // 55: retired MyBatis order refusal
	store_pebble.ErrRebuildMyBatis,
	nil, // 57: retired SQLCallsite order refusal
	store_pebble.ErrRebuildSQLCallsite,
	nil, // 59: retired StoreFactory order refusal
	store_pebble.ErrRebuildStoreFactory,
	store_pebble.ErrRebuildReduxThunk,
	store_pebble.ErrRebuildNgRxEffect,
	store_pebble.ErrRebuildObjectRegistry,
	store_pebble.ErrRebuildRTKQuery,
	store_pebble.ErrRebuildVuexDispatch,
	store_pebble.ErrRebuildCelery,
	store_pebble.ErrRebuildSpringEvents,
}

func buildRefusal(err error) rebuildBuildReply {
	r := rebuildBuildReply{Refusal: 1, Detail: err.Error()}
	limit := 8 << 10
	if errors.Is(err, errRebuildGraphPathCollision) {
		// Keep both quoted names even when all admitted path bytes need escaping.
		limit = 8*rebuildNamespaceKeyLimit + 128
	}
	if len(r.Detail) > limit {
		r.Detail = r.Detail[:limit]
	}
	for i, kind := range rebuildFailureKinds[1:] {
		if errors.Is(err, kind) {
			r.Refusal = uint8(i + 2)
			// The view wraps reducer errors as well as read failures.
			// Keep its code only when there is no more specific cause.
			if kind != store_pebble.ErrRebuildView {
				break
			}
		}
	}
	if chain, ok := errors.AsType[*store_pebble.RebuildAttributionChainError](err); ok {
		r.AttributionChains = chain.Count
	}
	if unknown, ok := errors.AsType[*store_pebble.RebuildUnknownProvenanceError](err); ok {
		r.UnknownProvenance = unknown.Count
	}
	return r
}

func writeRebuildBuildReply(out io.Writer, r rebuildBuildReply) error {
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(&r); err != nil {
		return err
	}
	if buf.Len() > rebuildReplyBytes {
		return fmt.Errorf("%w: stage frame too large", errRebuildBuildWire)
	}
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(buf.Len()))
	if _, err := out.Write(size[:]); err != nil {
		return err
	}
	_, err := buf.WriteTo(out)
	return err
}

func readRebuildBuildReply(in io.Reader) (r rebuildBuildReply, err error) {
	var size [4]byte
	if _, err := io.ReadFull(in, size[:]); err != nil {
		return r, err
	}
	n := binary.BigEndian.Uint32(size[:])
	if n == 0 || n > rebuildReplyBytes {
		return r, fmt.Errorf("%w: stage frame length %d", errRebuildBuildWire, n)
	}
	raw := make([]byte, n)
	if _, err := io.ReadFull(in, raw); err != nil {
		return r, fmt.Errorf("%w: truncated stage frame: %w", errRebuildBuildWire, err)
	}
	dec := gob.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&r); err != nil {
		return r, fmt.Errorf("%w: decode stage: %w", errRebuildBuildWire, err)
	}
	if err := dec.DecodeValue(reflect.Value{}); err != io.EOF {
		return r, fmt.Errorf("%w: trailing or unreadable stage data: %v", errRebuildBuildWire, err)
	}
	return r, nil
}
