package indexer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"time"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/contracts"
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
	order := store_pebble.CoreStages()
	relations := slices.Index(order[:], store_pebble.StageRelations)
	contracts := slices.Index(order[:], store_pebble.StageContracts)
	next := 0
	for {
		r, err := readRebuildBuildReply(in)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err == io.EOF {
			if next < len(order) {
				return fmt.Errorf("%w: %w: missing %s", ErrRebuildWorker, errRebuildBuildIncomplete, order[next])
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
		if next == len(order) {
			return fmt.Errorf("%w: extra %s after search", store_pebble.ErrStageOrder, r.Witness.Stage)
		}
		want := order[next]
		if r.Witness.Stage != want {
			return fmt.Errorf("%w: expected %s, got %s", store_pebble.ErrStageOrder, want, r.Witness.Stage)
		}
		if r.Witness.Version() != req.Witness.Version() || r.Witness.Profile != req.Witness.Profile ||
			r.Witness.FrozenInputs != req.Witness.FrozenInputs {
			return fmt.Errorf("%w: stage %s does not belong to the leased version", store_pebble.ErrStaleBase, want)
		}
		if (next >= relations) != (r.Witness.Relations != nil) {
			return fmt.Errorf("%w: unexpected relation coverage at %s", errRebuildBuildWire, want)
		}
		if (next >= contracts) != (r.Witness.Contracts != nil) {
			return fmt.Errorf("%w: unexpected contract coverage at %s", errRebuildBuildWire, want)
		}
		// Do not trust any directory or incarnation received from the child.
		dir := filepath.Join(lease, want.String())
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
			return fmt.Errorf("%w: acknowledge %s: %w", ErrRebuildWorker, want, err)
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
		// After the last ACK, the owner waits for stdout EOF with stdin still
		// open. Do not start another blocking read on inherited stdin.
		for _, stage := range store_pebble.CoreStages() {
			if _, err := io.ReadFull(in, packet[:]); err != nil {
				cancel(fmt.Errorf("%w: owner pipe before %s acknowledgment: %w", errRebuildBuildWire, stage, err))
				return
			}
			if packet[0] != rebuildStageAck {
				cancel(fmt.Errorf("%w: invalid %s stage acknowledgment", errRebuildBuildWire, stage))
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
	var exactCheckout, rootDepFact bool
	var rootFact rebuildGoManifest
	if digest := rebuildDigest(req.Witness.FrozenInputs); digest != (rebuildDigest{}) {
		if err := visitRebuildInputSidecar(ctx, lease, digest, func(row rebuildSidecarRow) error {
			if err := gitPolicy.add(row); err != nil {
				return err
			}
			if row.Kind == rebuildNamespaceCloseExactFact && string(row.Path) == "." {
				exactCheckout = true
			}
			if row.Kind == rebuildGoManifestFact && string(row.Path) == "go.mod" {
				if rootDepFact {
					return fmt.Errorf("%w: duplicate root Go manifest", errRebuildGoEvidence)
				}
				rootDepFact = true
				rootFact = rebuildGoManifestFromSidecar(row)
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
		witness: req.Witness, classifications: classifications, retainFileGraph: exactCheckout,
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
	if exactCheckout && !rootDepFact {
		return fmt.Errorf("%w: manifest row %q not covered", errRebuildGoEvidence, "go.mod")
	}
	markIsolationAffected := func(file string, kind contracts.ContractType, language string) error {
		return private.MarkRebuildContractIsolationAffected(ctx, idx.repoPrefix, file, kind, language)
	}
	var earlyIDs rebuildEarlyContractIDs
	runMain := func() error {
		var earlyFacts *rebuildEarlyContractFacts
		var err error
		if exactCheckout {
			earlyFacts, err = idx.readRebuildEarlyContractFacts(ctx, private.RebuildView(), job, store, counts.census, rootFact.DirectDeps, markIsolationAffected)
			if err != nil {
				return err
			}
		}

		if err := runRebuildFrozenMain(ctx, lease, rebuildDigest(req.Witness.FrozenInputs), private, counts.census, classifications, link, req.SkipPlaceholderSources); err != nil {
			return err
		}
		// Rebuild leaves deferred-language file imports off. Native cold
		// materialization runs that turn before frameworks.
		// As in native cold materialization, dataflow runs even if main had
		// no pending frontier.
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
		if exactCheckout {
			ids, err := earlyFacts.finish(ctx, private.RebuildView(), idx.repoPrefix, idx.workspaceID, idx.projectID)
			if err != nil {
				return err
			}
			earlyIDs = ids
			if paths := earlyFacts.routerOverlapFiles; len(paths) != 0 {
				logger.Info("rebuild router native map-order differences", zap.Int("files", len(paths)))
				for _, path := range paths {
					logger.Debug("rebuild router native map-order difference", zap.String("path", path))
				}
			}
			earlyFacts = nil
		}
		return nil
	}
	runTail := func() error {
		if exactCheckout {
			if err := private.BeginRebuildContractOverlay(ctx, earlyIDs); err != nil {
				return err
			}
		}
		if err := idx.runRebuildPrivateTail(ctx, lease, private, req, cfg, &counts, classifications, exactCheckout, earlyIDs, logger); err != nil {
			return err
		}
		if exactCheckout {
			return private.EndRebuildContractOverlay(ctx)
		}
		return nil
	}
	// Legacy captures keep their private turn witnesses, but do not offer
	// Relations without an exact checkout and its isolated input cut.
	if !counts.publishRelations {
		if err := runMain(); err != nil {
			return err
		}
		if err := runTail(); err != nil {
			return err
		}
		return errRebuildBuildIncomplete
	}
	reader := rebuildContractReceiptReader{idx: idx, job: job, store: store, census: counts.census, linkView: private.RebuildView(),
		markIsolationAffected: markIsolationAffected,
		visitIsolationAffected: func(yield func(store_pebble.RebuildContractBatch) error) error {
			return private.VisitRebuildContractIsolationAffected(ctx, yield)
		},
	}
	deferred, err := reader.deferredContractLanguages()
	if err != nil {
		return err
	}
	var contractTables store_pebble.StageTables
	stats, err := reader.withRebuildContractSources(ctx, private, lease, rootFact, link.SpillBytes, func(read func(func(contracts.Contract) error) error) error {
		if err := runMain(); err != nil {
			return err
		}
		return reader.withRebuildContractProducer(ctx, private, read, earlyIDs, func(_ iter.Seq2[store_pebble.RebuildContractBatch, error], seal func(string, store_pebble.RelationsBase) (store_pebble.StageTables, error)) error {
			if err := runTail(); err != nil {
				return err
			}
			if err := private.RestoreRebuildDeclarations(ctx); err != nil {
				return err
			}
			if err := private.CheckRebuildIsolationCoverage(deferred); err != nil {
				return err
			}
			relationsDir := filepath.Join(lease, store_pebble.StageRelations.String())
			if err := os.Mkdir(relationsDir, 0o700); err != nil {
				return err
			}
			relations, err := private.PrepareRelations(ctx, relationsDir, counts.fileGraphBase, reader.extractorOf)
			if err != nil {
				return err
			}
			base, err := private.RetainRelations(ctx, counts.fileGraphBase, relations)
			if err != nil {
				return err
			}
			if err := send(relations); err != nil {
				return err
			}
			contractsDir := filepath.Join(lease, store_pebble.StageContracts.String())
			if err := os.Mkdir(contractsDir, 0o700); err != nil {
				return err
			}
			contractTables, err = seal(contractsDir, base)
			return err
		})
	})
	if err != nil {
		return err
	}
	logger.Info("rebuild contract source capture",
		zap.Int("records", stats.Records), zap.Int("peak_batch_bytes", stats.PeakBatchBytes),
		zap.Int("spill_runs", stats.SpillRuns), zap.Int64("spill_bytes", stats.SpillBytes))
	if err := send(contractTables); err != nil {
		return err
	}
	derivedDir := filepath.Join(lease, store_pebble.StageDerived.String())
	if err := os.Mkdir(derivedDir, 0o700); err != nil {
		return err
	}
	derived, err := store_pebble.PrepareDeferredDerived(derivedDir, contractTables)
	if err != nil {
		return err
	}
	if err := send(derived); err != nil {
		return err
	}
	searchDir := filepath.Join(lease, store_pebble.StageSearch.String())
	if err := os.Mkdir(searchDir, 0o700); err != nil {
		return err
	}
	search, err := idx.prepareRebuildSearch(ctx, private, searchDir, derived.Witness, req.Incarnation, req.ProjectName)
	if err != nil {
		return err
	}
	return send(search)
}

// runRebuildPrivateTail preserves the native post-contract-ID turn order.
func (idx *Indexer) runRebuildPrivateTail(ctx context.Context, lease string, private *store_pebble.BuildStore,
	req rebuildBuildRequest, cfg config.IndexConfig, counts *rebuildParsedCounts, classifications map[string]rebuildClassificationFact,
	exactCheckout bool, earlyIDs rebuildEarlyContractIDs, logger *zap.Logger,
) error {
	receipts := counts.census.receipts
	skips := selectRebuildCheckedSkips(classifications, exactCheckout)
	if err := runRebuildTestFiles(ctx, private, receipts, skips); err != nil {
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
			namespace: namespace, fileClaims: &counts.claims, contracts: earlyIDs, heldCloneByLanguage: counts.census.heldCloneByLanguage,
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
		requestedSelection := idx.frameworkSynthesizerSelection().WithRebuildEarlyContractIDs(earlyIDs)
		familyGate, err := resolver.FreezeRebuildFrameworkFamilyGate(ctx, private.RebuildView(), requestedSelection)
		if err != nil {
			return err
		}
		gdReceiverGate, err := resolver.FreezeRebuildGDScriptReceiverGate(ctx, private.RebuildView(), requestedSelection)
		if err != nil {
			return err
		}
		selection, err := resolver.FreezeRebuildFrameworkSelection(ctx, private.RebuildView(), requestedSelection)
		if err != nil {
			return err
		}
		language := func(file string) string {
			raw := counts.census.originByGraphPath[file]
			if id := counts.census.byPath[raw]; id != "" {
				return id
			}
			fact := classifications[raw]
			if fact.sizeSkipped() || fact.ContentSkipReason != "" {
				return fact.Language
			}
			return ""
		}
		if err := resolver.FreezeRebuildFnPointerInputs(ctx, private, selection); err != nil {
			return err
		}
		if err := resolver.FreezeRebuildMacroExpansionInputs(ctx, private, receipts, selection); err != nil {
			return err
		}
		if err := resolver.FreezeRebuildReactResolveInputs(ctx, private, selection); err != nil {
			return err
		}
		if err := resolver.FreezeRebuildFastAPIInputs(ctx, private, selection); err != nil {
			return err
		}
		if err := resolver.FreezeRebuildRailsInputs(ctx, private, selection); err != nil {
			return err
		}
		if err := resolver.FreezeRebuildUIKitInputs(ctx, private, selection); err != nil {
			return err
		}
		if err := resolver.FreezeRebuildFactoryChainInputs(ctx, private, selection); err != nil {
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
		if err := resolver.MarkRebuildFrameworkIsolation(ctx, private, requestedSelection, selection, resolver.SynthReactNative, language, receipts); err != nil {
			return err
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
		if err := resolver.MarkRebuildFrameworkIsolation(ctx, private, requestedSelection, selection, resolver.SynthExpoModules, language, receipts); err != nil {
			return err
		}
		expoModules, err := resolver.RunRebuildExpoModuleBridge(ctx, private, receipts, selection)
		if err != nil {
			return err
		}
		if expoModules.Edges != 0 {
			logger.Info("rebuild Expo module bridge applied to private build",
				zap.Uint64("edges", expoModules.Edges))
		}
		if err := resolver.MarkRebuildFrameworkIsolation(ctx, private, requestedSelection, selection, resolver.SynthFabric, language, receipts); err != nil {
			return err
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
		ngrxEffect, err := resolver.RunRebuildNgRxEffect(ctx, private, receipts, selection)
		if err != nil {
			return err
		}
		if ngrxEffect.Stubs != 0 {
			logger.Info("rebuild NgRx Effect applied to private build",
				zap.Uint64("stubs", ngrxEffect.Stubs), zap.Uint64("resolved", ngrxEffect.Resolved),
				zap.Uint64("moves", ngrxEffect.Moves))
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
		celery, err := resolver.RunRebuildCelery(ctx, private, receipts, selection)
		if err != nil {
			return err
		}
		if celery.Stubs != 0 {
			logger.Info("rebuild Celery applied to private build",
				zap.Uint64("stubs", celery.Stubs), zap.Uint64("resolved", celery.Resolved),
				zap.Uint64("moves", celery.Moves))
		}
		spring, err := resolver.RunRebuildSpringEvents(ctx, private, receipts, selection)
		if err != nil {
			return err
		}
		if spring.Stubs != 0 {
			logger.Info("rebuild Spring events applied to private build",
				zap.Uint64("stubs", spring.Stubs), zap.Uint64("resolved", spring.Resolved),
				zap.Uint64("moves", spring.Moves), zap.Uint64("adds", spring.Adds))
		}
		mediatr, err := resolver.RunRebuildMediatR(ctx, private, receipts, selection)
		if err != nil {
			return err
		}
		if mediatr.Stubs != 0 {
			logger.Info("rebuild MediatR applied to private build",
				zap.Uint64("stubs", mediatr.Stubs), zap.Uint64("resolved", mediatr.Resolved),
				zap.Uint64("moves", mediatr.Moves), zap.Uint64("adds", mediatr.Adds))
		}
		csharpDI, err := resolver.RunRebuildCSharpDIIfaces(ctx, private, receipts, selection)
		if err != nil {
			return err
		}
		if csharpDI != 0 {
			logger.Info("rebuild C# DI interfaces applied to private build", zap.Uint64("edges", csharpDI))
		}
		csharpIface, err := resolver.RunRebuildCSharpIfaceDispatch(ctx, private, receipts, selection)
		if err != nil {
			return err
		}
		if csharpIface != 0 {
			logger.Info("rebuild C# interface dispatch applied to private build", zap.Uint64("edges", csharpIface))
		}
		efModels, err := resolver.RunRebuildEFCoreModels(ctx, private, receipts, selection)
		if err != nil {
			return err
		}
		if efModels != 0 {
			logger.Info("rebuild EF Core models applied to private build", zap.Uint64("models", efModels))
		}
		sidekiq, err := resolver.RunRebuildSidekiq(ctx, private, receipts, selection)
		if err != nil {
			return err
		}
		if sidekiq.Stubs != 0 {
			logger.Info("rebuild Sidekiq applied to private build",
				zap.Uint64("stubs", sidekiq.Stubs), zap.Uint64("resolved", sidekiq.Resolved),
				zap.Uint64("moves", sidekiq.Moves))
		}
		laravel, err := resolver.RunRebuildLaravelEvent(ctx, private, receipts, selection)
		if err != nil {
			return err
		}
		if laravel.Stubs != 0 {
			logger.Info("rebuild Laravel event applied to private build",
				zap.Uint64("stubs", laravel.Stubs), zap.Uint64("resolved", laravel.Resolved),
				zap.Uint64("moves", laravel.Moves), zap.Uint64("adds", laravel.Adds))
		}
		fnPointer, err := resolver.RunRebuildFnPointerStage(ctx, private, receipts, selection)
		if err != nil {
			return err
		}
		if fnPointer.Sweeps != 0 {
			logger.Info("rebuild function pointer applied to private build",
				zap.Uint64("resolved", fnPointer.Resolved), zap.Uint64("ref_moves", fnPointer.RefMoves),
				zap.Uint64("moves", fnPointer.CallMoves), zap.Uint64("adds", fnPointer.Adds),
				zap.Int("sweeps", fnPointer.Sweeps))
		}
		macroExpansion, err := resolver.RunRebuildMacroExpansion(ctx, private, receipts, selection)
		if err != nil {
			return err
		}
		if macroExpansion.Sites != 0 {
			logger.Info("rebuild macro expansion applied to private build",
				zap.Uint64("sites", macroExpansion.Sites), zap.Uint64("owned", macroExpansion.Owned),
				zap.Uint64("adds", macroExpansion.Adds))
		}
		ginOffers, err := resolver.RunRebuildGinMiddleware(ctx, private, receipts, selection)
		if err != nil {
			return err
		}
		if ginOffers != 0 {
			logger.Info("rebuild Gin middleware applied to private build", zap.Uint64("offers", ginOffers))
		}
		express, err := resolver.RunRebuildExpress(ctx, private, receipts, selection)
		if err != nil {
			return err
		}
		if express.Stubs != 0 {
			logger.Info("rebuild Express applied to private build",
				zap.Uint64("stubs", express.Stubs), zap.Uint64("moves", express.Moves))
		}
		reactResolveMoves, err := resolver.RunRebuildReactResolve(ctx, private, receipts, selection)
		if err != nil {
			return err
		}
		if reactResolveMoves != 0 {
			logger.Info("rebuild React hooks and context applied to private build", zap.Uint64("moves", reactResolveMoves))
		}
		fastapiMoves, err := resolver.RunRebuildFastAPI(ctx, private, receipts, selection)
		if err != nil {
			return err
		}
		if fastapiMoves != 0 {
			logger.Info("rebuild FastAPI applied to private build", zap.Uint64("moves", fastapiMoves))
		}
		rails, err := resolver.RunRebuildRails(ctx, private, receipts, selection)
		if err != nil {
			return err
		}
		if rails.Stubs != 0 {
			logger.Info("rebuild Rails applied to private build",
				zap.Uint64("stubs", rails.Stubs), zap.Uint64("moves", rails.Moves))
		}
		godotAutoloadMoves, err := resolver.RunRebuildGodotAutoload(ctx, private, receipts, selection)
		if err != nil {
			return err
		}
		if godotAutoloadMoves != 0 {
			logger.Info("rebuild Godot autoload applied to private build", zap.Uint64("moves", godotAutoloadMoves))
		}
		godotPreloadMoves, err := resolver.RunRebuildGodotPreloadAlias(ctx, private, receipts, selection)
		if err != nil {
			return err
		}
		if godotPreloadMoves != 0 {
			logger.Info("rebuild Godot preload alias applied to private build", zap.Uint64("moves", godotPreloadMoves))
		}
		swiftuiMoves, err := resolver.RunRebuildSwiftUI(ctx, private, receipts, selection)
		if err != nil {
			return err
		}
		if swiftuiMoves != 0 {
			logger.Info("rebuild SwiftUI applied to private build", zap.Uint64("moves", swiftuiMoves))
		}
		uikitMoves, err := resolver.RunRebuildUIKit(ctx, private, receipts, selection)
		if err != nil {
			return err
		}
		if uikitMoves != 0 {
			logger.Info("rebuild UIKit applied to private build", zap.Uint64("moves", uikitMoves))
		}
		vaporMoves, err := resolver.RunRebuildVapor(ctx, private, receipts, selection)
		if err != nil {
			return err
		}
		if vaporMoves != 0 {
			logger.Info("rebuild Vapor applied to private build", zap.Uint64("moves", vaporMoves))
		}
		goFrameMoves, err := resolver.RunRebuildGoFrame(ctx, private, receipts, selection)
		if err != nil {
			return err
		}
		if goFrameMoves != 0 {
			logger.Info("rebuild GoFrame applied to private build", zap.Uint64("moves", goFrameMoves))
		}
		if err := resolver.MarkRebuildFrameworkIsolation(ctx, private, requestedSelection, selection, resolver.SynthSvelteKitLoad, language, receipts); err != nil {
			return err
		}
		svelteKitAdds, err := resolver.RunRebuildSvelteKitLoad(ctx, private, receipts, selection, rebuildCheckedSkips(skips))
		if err != nil {
			return err
		}
		if svelteKitAdds != 0 {
			logger.Info("rebuild SvelteKit load applied to private build", zap.Uint64("adds", svelteKitAdds))
		}
		factoryMoves, err := resolver.RunRebuildFactoryChain(ctx, private, receipts, selection)
		if err != nil {
			return err
		}
		if factoryMoves != 0 {
			logger.Info("rebuild factory chain applied to private build", zap.Uint64("moves", factoryMoves))
		}
		fnValueAdds, err := resolver.RunRebuildFnValueCallbacks(ctx, private, receipts, selection)
		if err != nil {
			return err
		}
		if fnValueAdds != 0 {
			logger.Info("rebuild function-value callbacks applied to private build", zap.Uint64("adds", fnValueAdds))
		}
		valueRefMoves, err := resolver.RunRebuildValueRefs(ctx, private, receipts, selection)
		if err != nil {
			return err
		}
		if valueRefMoves != 0 {
			logger.Info("rebuild value refs applied to private build", zap.Uint64("moves", valueRefMoves))
		}
		familyDrops, err := resolver.RunRebuildFrameworkFamilyGate(ctx, private, familyGate)
		if err != nil {
			return err
		}
		if familyDrops != 0 {
			logger.Info("rebuild framework family gate applied to private build", zap.Uint64("drops", familyDrops))
		}
		djangoClaims, err := resolver.RunRebuildDjangoDescriptor(ctx, private, receipts, requestedSelection)
		if err != nil {
			return err
		}
		if djangoClaims != 0 {
			logger.Info("rebuild Django descriptor applied to private build", zap.Uint64("claims", djangoClaims))
		}
		gdDemotions, err := resolver.RunRebuildGDScriptReceiverGate(ctx, private, gdReceiverGate)
		if err != nil {
			return err
		}
		if gdDemotions != 0 {
			logger.Info("rebuild GDScript receiver gate applied to private build", zap.Uint64("demotions", gdDemotions))
		}
		rewriteNpm := rebuildExternalNpmRewrite(ctx, lease, rebuildDigest(req.Witness.FrozenInputs), counts.census, classifications, private)
		externalCalls, err := resolver.RunRebuildExternalCallSynthesis(ctx, private, receipts, req.ExternalCallsEnabled, requestedSelection, selection, rewriteNpm)
		if err != nil {
			return err
		}
		if externalCalls != 0 {
			logger.Info("rebuild external-call synthesis applied to private build", zap.Uint64("external_calls", externalCalls))
		}
	}
	if err := private.SettleRebuildPlatformCalls(ctx, func(p string) string {
		return counts.census.byPath[counts.census.originByGraphPath[p]]
	}); err != nil {
		return fmt.Errorf("settle platform calls: %w", err)
	}
	return nil
}

// selectRebuildCheckedSkips keeps deferred skip evidence out of every
// link-facing reader, including native filename-shaped framework roles.
func selectRebuildCheckedSkips(classifications map[string]rebuildClassificationFact, exactCheckout bool) map[string]rebuildClassificationFact {
	var skips map[string]rebuildClassificationFact
	for path, fact := range classifications {
		if exactCheckout && rebuildHoldsOutCheckedSkip(fact) {
			if skips == nil {
				skips = maps.Clone(classifications)
			}
			delete(skips, path)
		}
	}
	if skips == nil {
		return classifications
	}
	return skips
}
