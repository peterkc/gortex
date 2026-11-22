package resolver

import (
	"context"
	"errors"
	"iter"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
)

type rebuildLinkFrontier struct {
	view            store_pebble.RebuildView
	link            *store_pebble.RebuildLink
	opts            store_pebble.RebuildLinkOptions
	imports         rebuildMainImportInputs
	importDirs      *store_pebble.RebuildImportDirectories
	closeImportDirs func() error
}

// candidates streams one bounded request page. Visits are tentative until it
// returns nil. The reducer must propagate errors and not retain the frontier.
func (f *rebuildLinkFrontier) candidates(ctx context.Context, requests []store_pebble.LinkRequest, visit func(int, *graph.Node) error) (store_pebble.CandidatePageStats, error) {
	return f.link.SelectCandidatePage(ctx, f.view, requests, f.opts.SpillBytes, visit)
}

// importDirectories borrows the census for the rest of the serial frontier.
// Pull suspends WithImportDirectories inside its callback; closing the borrow
// resumes it through cleanup. Main page moves only edit edges, so a lazy
// census sees the same file identities as at candidate pass startup. Only
// reducers that leave the node rows unchanged may use this borrow.
func (f *rebuildLinkFrontier) importDirectories(ctx context.Context) (*store_pebble.RebuildImportDirectories, error) {
	if f.importDirs != nil {
		return f.importDirs, nil
	}
	var censusErr error
	next, stop := iter.Pull(func(yield func(*store_pebble.RebuildImportDirectories) bool) {
		_, censusErr = f.view.WithImportDirectories(ctx, f.opts.ScratchParent, f.opts.SpillBytes, func(dirs *store_pebble.RebuildImportDirectories) error {
			yield(dirs)
			return nil
		})
	})
	dirs, ok := next()
	if !ok {
		stop()
		return nil, censusErr
	}
	f.importDirs = dirs
	f.closeImportDirs = func() error {
		stop()
		return censusErr
	}
	return dirs, nil
}

// runRebuildLinkFrontier selects from each original pending page before starting
// the candidate pass. Candidate partitions freeze the entire working node view
// before the first reducer call. Only the private AllRepos name lookup sees
// intervening writes.
// The owner must discard its private work on any scan, reducer or close error.
func runRebuildLinkFrontier(ctx context.Context, build *store_pebble.BuildStore, opts store_pebble.RebuildLinkOptions,
	selectCallers func([]*graph.Edge) ([]*graph.Edge, error), reduce func(*rebuildLinkFrontier, []*graph.Edge) error,
) (err error) {
	f := rebuildLinkFrontier{view: build.RebuildView(), opts: opts}
	defer func() {
		if f.closeImportDirs != nil {
			err = errors.Join(err, f.closeImportDirs())
		}
		if f.link != nil {
			err = errors.Join(err, f.link.Close())
		}
	}()
	return f.view.VisitLinkPendingPages(ctx, resolvePendingPageRows, resolvePendingPageBytes, func(page []*graph.Edge) error {
		if selectCallers != nil {
			selected, err := selectCallers(page)
			if err != nil || len(selected) == 0 {
				return err
			}
			page = selected
		}
		if f.link == nil {
			link, err := store_pebble.StartRebuildLink(ctx, opts, func(yield func(*graph.Node) error) error {
				return f.view.VisitNodes(ctx, yield)
			})
			if err != nil {
				return err
			}
			f.link = link
		}
		return reduce(&f, page)
	})
}
