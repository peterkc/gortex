package store_pebble

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zeebo/blake3"
	"github.com/zzet/gortex/internal/graph"
)

// ErrSearchSeal marks a refused search stage and preserves the underlying cause.
var ErrSearchSeal = errors.New("pebble search stage seal failed")

// PrepareSearch replaces private symbol documents from the final logical node
// union and seals only the search family. Render selects the input for both
// exact and ranked search from the selected profile.
// The worker must finish all graph mutations before calling and must discard
// its lease on error. Dir is a fresh, absolute stage directory owned by that lease.
func (b *BuildStore) PrepareSearch(
	ctx context.Context, dir string, derived Witness, incarnation string,
	render func(*graph.Node) (string, bool),
) (tables StageTables, err error) {
	defer func() {
		if err != nil {
			tables = StageTables{}
			err = fmt.Errorf("%w: %w", ErrSearchSeal, err)
		}
	}()
	if !filepath.IsAbs(dir) || derived.Stage != StageDerived || derived.Profile != b.profile || incarnation == "" || render == nil {
		return StageTables{}, invalid("search needs its derived witness, stage directory and document renderer")
	}
	if len(derived.Unsupported) == 0 {
		return StageTables{}, invalid("search needs D64 unsupported family declarations")
	}
	if slices.ContainsFunc(derived.Families, func(f FamilyWitness) bool { return f.Family == familySymbol }) {
		return StageTables{}, invalid("search family already in derived witness")
	}
	raw, err := encodeWitness(derived)
	if err != nil {
		return StageTables{}, err
	}
	if err := ctx.Err(); err != nil {
		return StageTables{}, err
	}

	// Never mix preliminary tokens with the final search corpus.
	if err := b.withWrites(func() error {
		prefix := []byte{familySymbol}
		return b.pending.DeleteRange(prefix, prefixEnd(prefix), pebble.NoSync)
	}); err != nil {
		return StageTables{}, err
	}
	if err := b.RebuildView().VisitNodes(ctx, func(n *graph.Node) error {
		if !b.profile.keepsNode(n.Kind) {
			return nil
		}
		if text, ok := render(n); ok {
			return b.UpsertSymbolFTS(n.ID, text)
		}
		return nil
	}); err != nil {
		return StageTables{}, err
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	format := b.db.FormatMajorVersion().MaxTableFormat()
	gold := sortedRuns{dir: dir, name: "search", options: b.options, format: format, limit: goldBufferBytes}
	if err := b.writeSymbols(ctx, &gold, exactFromDocuments); err != nil {
		return StageTables{}, err
	}
	stream := goldWitness{digest: blake3.New()}
	paths, err := writeGoldTables(ctx, nil, &gold, goldFileCap-1, func(r row) error {
		if err := checkSymbolRow(r); err != nil {
			return err
		}
		stream.add(r)
		return nil
	}, func(ctx context.Context) error { return ctx.Err() })
	if err != nil {
		return StageTables{}, err
	}
	stream.flush()
	w := derived
	w.Stage = StageSearch
	w.Families = append(slices.Clone(derived.Families), stream.families...)
	slices.SortFunc(w.Families, func(a, b FamilyWitness) int { return int(a.Family) - int(b.Family) })
	for i, path := range paths {
		paths[i] = filepath.Base(path)
	}
	return StageTables{Dir: dir, Files: paths, Incarnation: incarnation, Base: blake3.Sum256(raw), Witness: w}, nil
}
