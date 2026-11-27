package indexer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_pebble"
	"github.com/zzet/gortex/internal/indexer/source"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/platform"
	"github.com/zzet/gortex/internal/resolver"
	"go.uber.org/zap"
)

const rebuildStageAck byte = 1

// runRebuildBuild starts a worker in the owner's prepared input lease. The
// process never outlives this call. The caller pins the directory owning
// the published witness for the duration of the run. publish can refuse any stage.
func runRebuildBuild(
	ctx context.Context, exe string, args []string, lease, headDir string, req rebuildBuildRequest,
	cfg config.IndexConfig, inputs []rebuildInput, publish func(context.Context, store_pebble.StageTables) error,
) (err error) {
	if !filepath.IsAbs(exe) || !filepath.IsAbs(lease) || !filepath.IsAbs(headDir) || publish == nil {
		return fmt.Errorf("%w: binary, build lease, head directory or publisher", errRebuildBuildWire)
	}
	if err := writeRebuildBuildRequest(ctx, lease, req, cfg, inputs); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, append(append([]string(nil), args...), lease)...)
	platform.ConfigureBackgroundCommand(cmd)
	acks, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	defer acks.Close()
	out, childOut, err := os.Pipe()
	if err != nil {
		return err
	}
	defer out.Close()
	var diagnostic rebuildWorkerDiagnostic
	cmd.Stdout, cmd.Stderr = childOut, io.MultiWriter(&diagnostic, os.Stderr)
	cmd.WaitDelay = time.Second // bound orphaned stderr after exit, as for transforms
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("%w: start process: %w", ErrRebuildWorker, errors.Join(err, childOut.Close()))
	}
	defer func() {
		if err != nil {
			cancel()
		}
		if waitErr := cmd.Wait(); waitErr != nil {
			exit := fmt.Errorf("%w: child exit: %w", ErrRebuildWorker, waitErr)
			if last := diagnostic.lastLine(); last != "" {
				exit = fmt.Errorf("%w; last stderr line: %q", exit, last)
			}
			err = errors.Join(err, exit)
		}
	}()
	if err := childOut.Close(); err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { _ = out.Close() })
	defer stop()
	return receiveRebuildStages(ctx, lease, req, out, acks, func(ctx context.Context, tables store_pebble.StageTables) error {
		if tables.Witness.Stage == store_pebble.StageManifest {
			if err := retainRebuildInputSidecar(ctx, headDir, lease, rebuildDigest(tables.Witness.FrozenInputs)); err != nil {
				return fmt.Errorf("%w: retain the manifest's frozen inputs: %w", ErrRebuildWorker, err)
			}
		}
		return publish(ctx, tables)
	})
}

// Keep only the start of the latest nonblank diagnostic, not a worker log.
// os/exec writes on one goroutine; the owner reads it only after Wait.
const rebuildDiagnosticBytes = 4 << 10

type rebuildWorkerDiagnostic struct {
	line, previous []byte
}

func (d *rebuildWorkerDiagnostic) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		line, rest, ended := bytes.Cut(p, []byte{'\n'})
		take := min(len(line), rebuildDiagnosticBytes-len(d.line))
		d.line = append(d.line, line[:take]...)
		if ended {
			if len(bytes.TrimSpace(d.line)) != 0 {
				d.line, d.previous = d.previous, d.line
			}
			d.line = d.line[:0]
		}
		p = rest
	}
	return n, nil
}

func (d *rebuildWorkerDiagnostic) lastLine() string {
	if line := bytes.TrimSpace(d.line); len(line) != 0 {
		return string(line)
	}
	return string(bytes.TrimSpace(d.previous))
}

func receiveRebuildStages(
	ctx context.Context, lease string, req rebuildBuildRequest, in io.Reader, acks io.Writer,
	publish func(context.Context, store_pebble.StageTables) error,
) error {
	next := store_pebble.StageManifest
	for {
		r, err := readRebuildBuildReply(in)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err == io.EOF {
			if next != store_pebble.StageSearch+1 {
				return fmt.Errorf("%w: %w: missing %s", ErrRebuildWorker, errRebuildBuildIncomplete, next)
			}
			return nil
		}
		if err != nil {
			return fmt.Errorf("%w: read stage: %w", ErrRebuildWorker, err)
		}
		if r.Refusal != 0 {
			if int(r.Refusal) > len(rebuildFailureKinds) || r.Witness.Stage != 0 || len(r.Files) != 0 {
				return fmt.Errorf("%w: malformed refusal", errRebuildBuildWire)
			}
			cause := rebuildFailureKinds[r.Refusal-1]
			if cause == nil {
				return fmt.Errorf("%w: retired refusal code %d", errRebuildBuildWire, r.Refusal)
			}
			if (cause == store_pebble.ErrRebuildAttributionChain) != (r.AttributionChains > 0) {
				return fmt.Errorf("%w: invalid attribution chain count", errRebuildBuildWire)
			}
			if (cause == store_pebble.ErrRebuildUnknownProvenance) != (r.UnknownProvenance > 0) {
				return fmt.Errorf("%w: invalid unknown provenance count", errRebuildBuildWire)
			}
			if r.AttributionChains != 0 {
				return fmt.Errorf("%w: %w: attribution_chains=%d: %s", ErrRebuildWorker, cause, r.AttributionChains, r.Detail)
			}
			if r.UnknownProvenance != 0 {
				return fmt.Errorf("%w: %w: unknown_provenance=%d: %s", ErrRebuildWorker, cause, r.UnknownProvenance, r.Detail)
			}
			return fmt.Errorf("%w: %w: %s", ErrRebuildWorker, cause, r.Detail)
		}
		if r.AttributionChains != 0 {
			return fmt.Errorf("%w: attribution chain count on a stage offer", errRebuildBuildWire)
		}
		if r.UnknownProvenance != 0 {
			return fmt.Errorf("%w: unknown provenance count on a stage offer", errRebuildBuildWire)
		}
		if next > store_pebble.StageSearch || r.Witness.Stage != next {
			return fmt.Errorf("%w: expected %s, got %s", store_pebble.ErrStageOrder, next, r.Witness.Stage)
		}
		if r.Witness.Version() != req.Witness.Version() || r.Witness.Profile != req.Witness.Profile ||
			r.Witness.FrozenInputs != req.Witness.FrozenInputs {
			return fmt.Errorf("%w: stage %s does not belong to the leased version", store_pebble.ErrStaleBase, next)
		}
		if (next >= store_pebble.StageRelations) != (r.Witness.Relations != nil) {
			return fmt.Errorf("%w: unexpected relation coverage at %s", errRebuildBuildWire, next)
		}
		// Do not trust any directory or incarnation received from the child.
		dir := filepath.Join(lease, next.String())
		st, err := os.Lstat(dir)
		if err != nil {
			return fmt.Errorf("%w: stage directory: %w", errRebuildBuildWire, err)
		}
		if !st.IsDir() {
			return fmt.Errorf("%w: invalid stage directory", errRebuildBuildWire)
		}
		tables := store_pebble.StageTables{Dir: dir, Files: r.Files, Base: r.Base, Witness: r.Witness, Incarnation: req.Incarnation}
		if err := publish(ctx, tables); err != nil {
			return err
		}
		if _, err := acks.Write([]byte{rebuildStageAck}); err != nil {
			return fmt.Errorf("%w: acknowledge %s: %w", ErrRebuildWorker, next, err)
		}
		next++
	}
}

// RunRebuildBuildWorker is the hidden command's process entry point. Only
// the owner pipe can acknowledge an ingest. Its EOF cancels work even if the
// worker is between stages. The lease belongs to the owner, never to a child
// asked to clean arbitrary paths after a failure.
func RunRebuildBuildWorker(ctx context.Context, lease string, in *os.File, output io.Writer, logger *zap.Logger, link store_pebble.RebuildLinkOptions) error {
	if !filepath.IsAbs(lease) {
		return fmt.Errorf("%w: build worker needs an absolute lease directory", errRebuildBuildWire)
	}
	ctx, cancel := context.WithCancelCause(ctx)
	acks := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		var packet [1]byte
		for {
			if _, err := io.ReadFull(in, packet[:]); err != nil {
				cancel(fmt.Errorf("%w: owner pipe: %w", errRebuildBuildWire, err))
				return
			}
			if packet[0] != rebuildStageAck {
				cancel(fmt.Errorf("%w: invalid stage acknowledgment", errRebuildBuildWire))
				return
			}
			select {
			case acks <- struct{}{}:
			case <-ctx.Done():
				return
			}
		}
	}()
	defer func() {
		cancel(nil)
		_ = in.Close()
		<-done
	}()
	send := func(tables store_pebble.StageTables) error {
		if err := context.Cause(ctx); err != nil {
			return err
		}
		r := rebuildBuildReply{Files: tables.Files, Base: tables.Base, Witness: tables.Witness}
		if err := writeRebuildBuildReply(output, r); err != nil {
			return err
		}
		select {
		case <-acks:
			return context.Cause(ctx)
		case <-ctx.Done():
			return context.Cause(ctx)
		}
	}
	if err := buildRebuildStages(ctx, lease, logger, store_pebble.OpenWorkerBuild, link, send); err != nil {
		return errors.Join(err, writeRebuildBuildReply(output, buildRefusal(err)))
	}
	return nil
}

// The worker and its native comparison must not install either provider
// path. Check the dependencies after construction, not provider availability.
func (idx *Indexer) checkRebuildProvidersOff() error {
	if idx.SemanticManager() != nil {
		return fmt.Errorf("%w: semantic manager installed for rebuild", errRebuildBuildWire)
	}
	if idx.resolver == nil || idx.resolver.LSPHelper() != nil {
		return fmt.Errorf("%w: rebuild requires a resolver with no LSP helper", errRebuildBuildWire)
	}
	return nil
}

func buildRebuildStages(
	ctx context.Context, lease string, logger *zap.Logger,
	openBuild func(string, store_pebble.Profile) (*store_pebble.BuildStore, error),
	link store_pebble.RebuildLinkOptions, send func(store_pebble.StageTables) error,
) (err error) {
	req, inputs, err := readRebuildBuildRequest(ctx, lease)
	if err != nil {
		return err
	}
	var classifications map[string]rebuildClassificationFact
	var gitPolicy rebuildGitPolicyProof
	var exactCheckout bool
	if digest := rebuildDigest(req.Witness.FrozenInputs); digest != (rebuildDigest{}) {
		if err := visitRebuildInputSidecar(ctx, lease, digest, func(row rebuildSidecarRow) error {
			if err := gitPolicy.add(row); err != nil {
				return err
			}
			if row.Kind == rebuildNamespaceCloseExactFact && string(row.Path) == "." {
				exactCheckout = true
			}
			if row.Classification == nil {
				return nil
			}
			key := string(row.Path)
			if _, duplicate := classifications[key]; duplicate {
				return fmt.Errorf("%w: repeated classification for %q", errRebuildInputSidecar, key)
			}
			if classifications == nil {
				classifications = make(map[string]rebuildClassificationFact)
			}
			classifications[key] = *row.Classification
			return nil
		}); err != nil {
			return err
		}
	}
	if err := gitPolicy.check(req.Witness.UsesGitIgnorePolicy); err != nil {
		return err
	}
	var cfg config.IndexConfig
	if err := json.Unmarshal(req.Config, &cfg); err != nil {
		return fmt.Errorf("read frozen index config: %w", err)
	}
	src, err := source.NewFilesystemSource(req.Root)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, src.Close()) }()
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	languages.RegisterCustomGrammars(reg, cfg.Grammars, logger)
	languages.RegisterExtractorPlugins(reg, cfg.ExtractorPlugins, logger)
	languages.RegisterFallbackChunkers(reg, cfg.FallbackChunkers, logger)
	private, err := openBuild(filepath.Join(lease, "working"), req.Witness.Profile)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, private.Close()) }()
	idx := New(private, reg, cfg, logger)
	defer idx.Close()
	if err := idx.checkRebuildProvidersOff(); err != nil {
		return err
	}
	idx.installRebuildParsePolicy(req.Policy)
	idx.SetRootPath(req.Root)
	idx.SetContentSource(src)
	req.Bounds.Dir = lease
	job := rebuildFileStages{
		workDir: lease, incarnation: req.Incarnation, inputs: inputs, recipes: req.Recipes, bounds: req.Bounds,
		witness: req.Witness, classifications: classifications,
	}
	store := rebuildParseStore{dir: req.ParseDir, limit: req.ParseLimit}
	counts, err := idx.buildRebuildFileStages(ctx, src, store, private, job, send)
	if c := counts.claims; err == nil || c != (store_pebble.RebuildSharedClaimStats{}) {
		logger.Info("rebuild file graph receipt claims",
			zap.Uint64("shadowed_id", c.Shadowed),
			zap.Uint64("shadowed_edge", c.ShadowedEdges),
			zap.Uint64("unexplained_id", c.UnexplainedNodes),
			zap.Uint64("accepted_string", c.AcceptedStrings),
			zap.Uint64("accepted_annotation", c.AcceptedAnnotations),
			zap.Uint64("accepted_column", c.AcceptedColumns),
			zap.Uint64("claim_scratch_bytes", c.ScratchBytes),
			zap.Uint64("claim_observed_pending_flush_bytes", c.SpillBytes),
			zap.Uint64("claim_peak_row_bytes", c.PeakRowBytes))
	}
	if err != nil {
		return err
	}
	if err := runRebuildFrozenMain(ctx, lease, rebuildDigest(req.Witness.FrozenInputs), private, counts.census, link, req.SkipPlaceholderSources); err != nil {
		return err
	}
	// Deferred file imports stay off. Like native cold materialization,
	// this pass is independent of whether main had a pending frontier.
	receipts := counts.census.receipts
	if err := runRebuildArgOf(ctx, private, receipts); err != nil {
		return err
	}
	if err := runRebuildReturnsTo(ctx, private, receipts); err != nil {
		return err
	}
	if err := resolver.RunRebuildImplements(ctx, private, receipts); err != nil {
		return err
	}
	if err := resolver.RunRebuildOverrides(ctx, private, receipts); err != nil {
		return err
	}
	// Legacy test/standalone captures without a checkout namespace were never
	// qualified to continue through the full native cold tail.
	if exactCheckout && cfg.Coverage.IsEnabled("modules") {
		digest := rebuildDigest(req.Witness.FrozenInputs)
		if err := runRebuildModules(ctx, lease, digest, private, inputs); err != nil {
			return err
		}
		if err := runRebuildPackageWorkspace(ctx, lease, digest, private); err != nil {
			return err
		}
	}
	if err := runRebuildTestFiles(ctx, private, receipts, classifications); err != nil {
		return err
	}
	if err := runRebuildTestSymbols(ctx, private, receipts); err != nil {
		return err
	}
	// The hierarchy entry-point predecessor stamps only C# type metadata;
	// neither capability election reads it.
	capability, err := private.RebuildCapabilities(ctx, receipts, processExecMechanism, rebuildProcessImport,
		rebuildReceiverRules(), 1<<20)
	if err != nil {
		return err
	}
	logger.Info("rebuild capabilities applied to private build",
		zap.Uint64("receiver_facts", capability.Facts),
		zap.Uint64("receiver_rounds", capability.Rounds),
		zap.Uint64("receiver_round_bound", capability.RoundBound),
		zap.Uint64("receiver_matches", capability.Matches),
		zap.Uint64("receiver_claims", capability.Emitted),
		zap.Uint64("receiver_spill_bytes", capability.SpillBytes))
	if exactCheckout {
		namespace, err := openRebuildResolverNamespace(ctx, lease, rebuildDigest(req.Witness.FrozenInputs))
		if err != nil {
			return err
		}
		predecessors := rebuildClonePredecessors{
			namespace: namespace, modules: cfg.Coverage.IsEnabled("modules"), fileClaims: &counts.claims,
		}
		// Native cold detection still banks pre-signed callables when raw
		// clone extraction is disabled.
		stats, err := applyRebuildCloneRelations(ctx, private, receipts, predecessors, cfg.Coverage.ClonesThreshold())
		if err != nil {
			return err
		}
		logger.Info("rebuild clone relations applied to private build",
			zap.Int("raw_bodies", stats.Signatures.Corpus),
			zap.Int("kept_signatures", stats.Signatures.Kept),
			zap.Int("signed_only", stats.Signatures.Signed),
			zap.Uint64("clone_pairs", stats.Direct.Pairs),
			zap.Int("diffused_pairs", stats.Diffused),
			zap.Bool("diffusion_capped", stats.DiffusionCapped),
			zap.Uint64("claim_scratch_bytes", stats.Signatures.Claims.ScratchBytes),
			zap.Uint64("claim_observed_pending_flush_bytes", stats.Signatures.Claims.SpillBytes),
			zap.Uint64("claim_peak_row_bytes", stats.Signatures.Claims.PeakRowBytes))
		// Positions 1–12 preserve node Language, Kind and Name used by the live
		// KMP and RN/Expo gates. Temporal can stamp node metadata.
		selection, err := resolver.FreezeRebuildFrameworkSelection(ctx, private.RebuildView(), idx.frameworkSynthesizerSelection())
		if err != nil {
			return err
		}
		grpc, err := resolver.RunRebuildGRPC(ctx, private, receipts, selection)
		if err != nil {
			return err
		}
		if grpc.Stubs != 0 {
			logger.Info("rebuild gRPC applied to private build",
				zap.Uint64("stubs", grpc.Stubs),
				zap.Uint64("registrations", grpc.Registrations),
				zap.Uint64("resolved", grpc.Resolved),
				zap.Uint64("moves", grpc.Moves))
		}
		temporal, err := resolver.RunRebuildTemporal(ctx, private, receipts, selection, 1<<20)
		if err != nil {
			return err
		}
		if temporal.WrapperRounds != 0 {
			logger.Info("rebuild Temporal applied to private build",
				zap.Int("wrapper_rounds", temporal.WrapperRounds),
				zap.Bool("wrapper_capped", temporal.WrapperCapped),
				zap.Uint64("wrapper_stubs", temporal.Wrappers),
				zap.Uint64("resolved", temporal.Resolved),
				zap.Uint64("moves", temporal.Moves),
				zap.Uint64("spill_bytes", temporal.SpillBytes))
		}
		eventChannel, err := resolver.RunRebuildEventChannel(ctx, private, receipts, selection)
		if err != nil {
			return err
		}
		if eventChannel.Pairs != 0 {
			logger.Info("rebuild event channel applied to private build",
				zap.Uint64("pairs", eventChannel.Pairs))
		}
		swiftObjC, err := resolver.RunRebuildSwiftObjC(ctx, private, receipts, selection)
		if err != nil {
			return err
		}
		if swiftObjC.Edges != 0 {
			logger.Info("rebuild Swift/ObjC applied to private build",
				zap.Uint64("edges", swiftObjC.Edges))
		}
		reactNative, err := resolver.RunRebuildReactNativeBridge(ctx, private, receipts, selection)
		if err != nil {
			return err
		}
		if reactNative.Edges != 0 {
			logger.Info("rebuild React Native bridge applied to private build",
				zap.Uint64("edges", reactNative.Edges))
		}
		nativePairing, err := resolver.RunRebuildReactNativeNativePairing(ctx, private, receipts, selection)
		if err != nil {
			return err
		}
		if nativePairing.Edges != 0 {
			logger.Info("rebuild React Native native pairing applied to private build",
				zap.Uint64("edges", nativePairing.Edges))
		}
		observer, err := resolver.RunRebuildObserver(ctx, private, receipts, selection)
		if err != nil {
			return err
		}
		if observer.Edges != 0 {
			logger.Info("rebuild Observer applied to private build",
				zap.Uint64("edges", observer.Edges))
		}
		closureCollection, err := resolver.RunRebuildClosureCollection(ctx, private, receipts, selection)
		if err != nil {
			return err
		}
		if closureCollection.Edges != 0 {
			logger.Info("rebuild closure collection applied to private build",
				zap.Uint64("edges", closureCollection.Edges))
		}
		reactSetState, err := resolver.RunRebuildReactSetState(ctx, private, receipts, selection)
		if err != nil {
			return err
		}
		if reactSetState.Edges != 0 {
			logger.Info("rebuild React setState applied to private build",
				zap.Uint64("edges", reactSetState.Edges))
		}
		flutterSetState, err := resolver.RunRebuildFlutterSetState(ctx, private, receipts, selection)
		if err != nil {
			return err
		}
		if flutterSetState.Edges != 0 {
			logger.Info("rebuild Flutter setState applied to private build",
				zap.Uint64("edges", flutterSetState.Edges))
		}
		kmp, err := resolver.RunRebuildKMPExpectActual(ctx, private, receipts, selection)
		if err != nil {
			return err
		}
		if kmp.Edges != 0 {
			logger.Info("rebuild KMP expect/actual applied to private build",
				zap.Uint64("edges", kmp.Edges))
		}
		expoModules, err := resolver.RunRebuildExpoModuleBridge(ctx, private, receipts, selection)
		if err != nil {
			return err
		}
		if expoModules.Edges != 0 {
			logger.Info("rebuild Expo module bridge applied to private build",
				zap.Uint64("edges", expoModules.Edges))
		}
		fabric, err := resolver.RunRebuildFabric(ctx, private, receipts, selection)
		if err != nil {
			return err
		}
		if fabric.Edges != 0 {
			logger.Info("rebuild Fabric applied to private build",
				zap.Uint64("edges", fabric.Edges))
		}
		mybatis, err := resolver.RunRebuildMyBatis(ctx, private, receipts, selection)
		if err != nil {
			return err
		}
		if mybatis.Stubs != 0 {
			logger.Info("rebuild MyBatis applied to private build",
				zap.Uint64("stubs", mybatis.Stubs), zap.Uint64("resolved", mybatis.Resolved),
				zap.Uint64("moves", mybatis.Moves))
		}
		sqlCallsite, err := resolver.RunRebuildSQLCallsites(ctx, private, receipts, selection)
		if err != nil {
			return err
		}
		if sqlCallsite.Stubs != 0 {
			logger.Info("rebuild SQL callsite applied to private build",
				zap.Uint64("stubs", sqlCallsite.Stubs), zap.Uint64("resolved", sqlCallsite.Resolved),
				zap.Uint64("moves", sqlCallsite.Moves))
		}
		storeFactory, err := resolver.RunRebuildStoreFactory(ctx, private, receipts, selection)
		if err != nil {
			return err
		}
		if storeFactory.Stubs != 0 {
			logger.Info("rebuild Store Factory applied to private build",
				zap.Uint64("stubs", storeFactory.Stubs), zap.Uint64("resolved", storeFactory.Resolved),
				zap.Uint64("moves", storeFactory.Moves))
		}
		reduxThunk, err := resolver.RunRebuildReduxThunk(ctx, private, receipts, selection)
		if err != nil {
			return err
		}
		if reduxThunk.Stubs != 0 {
			logger.Info("rebuild Redux Thunk applied to private build",
				zap.Uint64("stubs", reduxThunk.Stubs), zap.Uint64("resolved", reduxThunk.Resolved),
				zap.Uint64("moves", reduxThunk.Moves))
		}
		objectRegistry, err := resolver.RunRebuildObjectRegistry(ctx, private, receipts, selection)
		if err != nil {
			return err
		}
		if objectRegistry.Stubs != 0 {
			logger.Info("rebuild Object Registry applied to private build",
				zap.Uint64("stubs", objectRegistry.Stubs), zap.Uint64("resolved", objectRegistry.Resolved),
				zap.Uint64("moves", objectRegistry.Moves))
		}
		rtkQuery, err := resolver.RunRebuildRTKQuery(ctx, private, receipts, selection)
		if err != nil {
			return err
		}
		if rtkQuery.Moves != 0 {
			logger.Info("rebuild RTK Query applied to private build",
				zap.Uint64("resolved", rtkQuery.Resolved), zap.Uint64("moves", rtkQuery.Moves))
		}
		vuexDispatch, err := resolver.RunRebuildVuexDispatch(ctx, private, receipts, selection)
		if err != nil {
			return err
		}
		if vuexDispatch.Stubs != 0 {
			logger.Info("rebuild Vuex Dispatch applied to private build",
				zap.Uint64("stubs", vuexDispatch.Stubs), zap.Uint64("resolved", vuexDispatch.Resolved),
				zap.Uint64("moves", vuexDispatch.Moves))
		}
	}
	// The supported tails only changed private scratch. Relation projection
	// and its remaining tails, derived and search stages are incomplete.
	return errRebuildBuildIncomplete
}
