package resolver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

// ErrRebuildCrossPackageGuard marks a failed cold guard. All working writes
// remain provisional, including on a scratch cleanup failure.
var ErrRebuildCrossPackageGuard = errors.New("rebuild cross-package guard failed")

const rebuildGuardPageBytes = 16 << 20

type rebuildCrossPackageGuard struct {
	spool     resolveGuardSpool
	spillAt   int
	pageBytes int
	sealed    bool
}

type rebuildGuardStats struct {
	Pages, Recorded, Matched, Reverted, Sources int
	SpillBytes                                  int64
}

func newRebuildCrossPackageGuard(scratchParent string, spillAt int) *rebuildCrossPackageGuard {
	return &rebuildCrossPackageGuard{spool: resolveGuardSpool{scratchParent: scratchParent}, spillAt: spillAt}
}

// Append runs after a successful main reindex page. Keep all intended moves
// in main replay order, including candidates that lost to a stationary row
// or an earlier move. Append after any post-commit flag changes to the same
// job pointers. The native spool compares their intended payloads
// against the final live payload; rereading winners here would erase that test.
func (g *rebuildCrossPackageGuard) Append(ctx context.Context, batch []graph.EdgeReindex) error {
	if g.sealed || g.spillAt < 1 {
		return rebuildGuardError(errors.New("append outside the guard capture phase"))
	}
	for _, m := range batch {
		if err := ctx.Err(); err != nil {
			return rebuildGuardError(err)
		}
		e := m.Edge
		if e == nil || !guardCandidateJob(&reindexJob{kind: e.Kind, oldTo: m.OldTo, origin: e.Origin, confidence: e.Confidence}) {
			continue
		}
		record := resolveGuardRecord{
			From: e.From, CurrentTo: e.To, OldTo: m.OldTo, Kind: e.Kind, FilePath: e.FilePath, Line: e.Line,
			CrossRepo: e.CrossRepo, Confidence: e.Confidence, Origin: e.Origin, Payload: spoolSnapshotPersistedEdge(e),
		}
		bytes := resolveGuardRecordSize(record)
		g.pageBytes += bytes
		if g.pageBytes > rebuildGuardPageBytes {
			return rebuildGuardError(fmt.Errorf("%w: guard spool page", store_pebble.ErrRebuildLinkInputSize))
		}
		// Force an earlier spill in low-threshold witnesses. Native's own
		// tighter inline budget still applies at larger requested thresholds.
		if g.spool.file == nil && g.spool.bytes+bytes > g.spillAt {
			if err := g.spool.spill(); err != nil {
				return rebuildGuardError(err)
			}
		}
		if err := g.spool.append(record); err != nil {
			return rebuildGuardError(err)
		}
		if g.spool.count%resolvePendingPageRows == 0 {
			g.pageBytes = 0
		}
	}
	return rebuildGuardError(ctx.Err())
}

// Apply runs after the completed deferred-LSP writes are visible in the same
// private graph. A changed payload, including an LSP confirmation at the same
// key, cannot hydrate a stale heuristic job. If the LSP owner retains retry
// records, refresh must resnapshot the affected retries after each revert page.
// Pass nil only when no retry records are retained. The guard owns no LSP policy.
func (g *rebuildCrossPackageGuard) Apply(
	ctx context.Context, build *store_pebble.BuildStore, reconcileSources bool, refresh func([]lspSpoolRevert) error,
) (stats rebuildGuardStats, err error) {
	if g.sealed || g.spillAt < 1 {
		return stats, rebuildGuardError(errors.New("invalid guard apply"))
	}
	g.sealed = true
	defer func() {
		if err != nil {
			stats = rebuildGuardStats{}
			err = rebuildGuardError(err)
		}
	}()
	stats.Recorded = g.spool.count
	if stats.Recorded == 0 {
		return stats, ctx.Err()
	}
	stage := rebuildGuardStage{ctx: ctx, build: build, view: build.RebuildView()}
	defer func() {
		if stage.usings != nil {
			err = errors.Join(err, stage.usings.Close())
		}
	}()
	_, err = withRebuildImportClosure(ctx, stage.view, nil, func(closure *store_pebble.RebuildImportClosure) error {
		hasCallers, err := closure.AnyCaller(ctx)
		if err != nil || !hasCallers {
			return err
		}
		stage.closure = closure
		if g.spool.file != nil {
			if err := g.spool.beginRead(); err != nil {
				return err
			}
			info, err := g.spool.file.Stat()
			if err != nil {
				return err
			}
			stats.SpillBytes = info.Size()
		}
		for done := false; !done; {
			if err := ctx.Err(); err != nil {
				return err
			}
			records, exhausted, err := g.spool.nextPage(resolvePendingPageRows)
			if err != nil {
				return err
			}
			if err := stage.applyPage(records, reconcileSources, refresh, &stats); err != nil {
				return err
			}
			done = exhausted
		}
		return nil
	})
	return stats, err
}

// Close reaps native inline/file scratch and reports flush/close/unlink errors.
// The native spool's close helper intentionally does not return them.
func (g *rebuildCrossPackageGuard) Close() error {
	g.sealed = true
	s := &g.spool
	file, writer := s.file, s.writer
	*s = resolveGuardSpool{}
	if file == nil {
		return nil
	}
	return rebuildGuardError(errors.Join(writer.Flush(), file.Close(), os.Remove(file.Name())))
}

func rebuildGuardError(err error) error {
	if err != nil {
		return fmt.Errorf("%w: %w", ErrRebuildCrossPackageGuard, err)
	}
	return nil
}

type rebuildGuardStage struct {
	ctx     context.Context
	build   *store_pebble.BuildStore
	view    store_pebble.RebuildView
	closure *store_pebble.RebuildImportClosure
	usings  *store_pebble.RebuildGuardUsings // lazy, only for C# decisions that need visibility
}

func (s *rebuildGuardStage) applyPage(records []resolveGuardRecord, reconcile bool, refresh func([]lspSpoolRevert) error, stats *rebuildGuardStats) error {
	// Hydrate the complete page before changing any edge. Duplicate spool
	// records borrow the same mutable row, just as native site candidates do.
	live := make(map[graph.EdgeIdentity]*graph.Edge, len(records))
	matched := make([]*graph.Edge, len(records))
	for i, r := range records {
		if r.From == "" {
			continue // native exact-site projection excludes an empty source
		}
		payload := r.Payload.snapshot()
		key := payload.identity()
		e, hit := live[key]
		if !hit {
			var err error
			e, err = s.view.Edge(s.ctx, key)
			if err != nil {
				return err
			}
			// Keep only rows that match. A losing record may point at a
			// different payload much larger than its recorded job.
		}
		if payload.matches(e) {
			live[key] = e
			matched[i] = e
			stats.Matched++
		}
	}
	var moves []graph.EdgeReindex
	var sources []graph.PlaceholderRepoint
	var refreshes []lspSpoolRevert
	for i, r := range records {
		e := matched[i]
		if e == nil || e.Origin == graph.OriginLSPResolved {
			continue
		}
		caller, err := s.view.Node(s.ctx, e.From)
		if err != nil {
			return err
		}
		target, err := s.view.Node(s.ctx, r.CurrentTo)
		if err != nil {
			return err
		}
		file, prefix := e.FilePath, ""
		if caller != nil {
			prefix = caller.RepoPrefix
			if caller.FilePath != "" {
				file = caller.FilePath
			}
		}
		if file == "" || target == nil {
			continue
		}
		keep, err := s.keep(r, e, file, caller, target, prefix)
		if err != nil {
			return err
		}
		if keep {
			continue
		}
		old := e.To
		if reconcile {
			placeholder := r.OldTo
			if prefix != "" {
				placeholder = prefix + "/" + placeholder
			}
			sources = append(sources, graph.PlaceholderRepoint{OldFrom: old, FilePath: e.FilePath, Line: e.Line, NewFrom: placeholder})
		}
		e.To, e.Confidence, e.ConfidenceLabel = r.OldTo, 0, ""
		if e.Meta == nil {
			e.Meta = map[string]any{}
		}
		e.Meta["guard_reverted"] = true
		delete(e.Meta, "resolution")
		moves = append(moves, graph.EdgeReindex{Edge: e, OldTo: old})
		if refresh != nil {
			refreshes = append(refreshes, lspSpoolRevert{edge: e, oldBoundTo: old})
		}
	}
	sourceCount, err := s.build.ApplyRebuildGuardPage(s.ctx, moves, sources)
	if err != nil {
		return err
	}
	if len(refreshes) != 0 {
		if err := refresh(refreshes); err != nil {
			return err
		}
	}
	stats.Pages++
	stats.Reverted += len(moves)
	stats.Sources += sourceCount
	return nil
}

func (s *rebuildGuardStage) keep(r resolveGuardRecord, e *graph.Edge, file string, caller, target *graph.Node, repo string) (bool, error) {
	if target.FilePath == "" || filePathDir(file) == filePathDir(target.FilePath) || sameScopePackage(caller, target) {
		return true, nil
	}
	known, err := s.closure.HasCaller(s.ctx, file)
	if err != nil || !known {
		return !known, err
	}
	reachable, err := s.closure.Reachable(s.ctx, file, filePathDir(target.FilePath))
	if err != nil || reachable {
		return reachable, err
	}
	var visible csharpFileNS
	ns, _ := target.Meta["scope_ns"].(string)
	tag, _ := e.Meta["resolution"].(string)
	extension := tag == "extension_method" && isCSharpExtension(target)
	usingStatic := tag == "using_static" && csharpUsingStaticImportable(target)
	csharpLone := target.Language == "csharp" && ns != "" && strings.HasPrefix(graph.UnresolvedName(r.OldTo), "*.")
	if csharpLone || extension || usingStatic {
		visible, err = s.csharpVisibility(file)
		if err != nil {
			return false, err
		}
	}
	lone, err := s.loneMember(r.OldTo, e, target, repo, visible, ns)
	if err != nil || lone {
		return lone, err
	}
	if extension && rebuildGuardExtensionVisible(caller, target, visible, ns) {
		return true, nil
	}
	if usingStatic {
		_, ok := csharpUsingStaticOwner(visible, target)
		return ok, nil
	}
	return false, nil
}

func (s *rebuildGuardStage) loneMember(oldTo string, e *graph.Edge, target *graph.Node, repo string, visible csharpFileNS, ns string) (bool, error) {
	name := graph.UnresolvedName(oldTo)
	if !loneMemberLang(target.Language) || (target.Language != "java" && !strings.HasPrefix(name, "*.")) {
		return false, nil
	}
	if rt := edgeReceiverType(e); rt != "" {
		defined := false
		err := s.visitRepoName(rt, repo, func(n *graph.Node) {
			defined = defined || n.Kind == graph.KindType || n.Kind == graph.KindInterface
		})
		if err != nil || !defined {
			return false, err
		}
	}
	if target.Language == "csharp" && ns != "" && len(visible.imported) > 0 {
		_, enc := visible.enclosing[ns]
		_, imp := visible.imported[ns]
		if !enc && !imp {
			return false, nil
		}
	}
	n := 0
	err := s.visitRepoName(strings.TrimPrefix(name, "*."), repo, func(c *graph.Node) {
		if c.Language == target.Language && (c.Kind == graph.KindMethod || (target.Language == "java" && c.Kind == graph.KindFunction)) {
			n = min(n+1, 2)
		}
	})
	return n == 1, err
}

func (s *rebuildGuardStage) visitRepoName(name, repo string, visit func(*graph.Node)) error {
	return s.view.VisitNameCandidates(s.ctx, name, nil, func(n *graph.Node) error {
		if n.RepoPrefix == repo && len(withoutAttributedTerminals([]*graph.Node{n})) != 0 {
			visit(n)
		}
		return nil
	})
}
