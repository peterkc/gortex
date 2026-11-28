package indexer

import (
	"context"
	"errors"
	"fmt"
	"iter"
)

var errRebuildContractIDs = errors.New("rebuild contract id comparison failed")

// compareRebuildContractIDs compares already-sorted unique keys without
// loading either set. Nil denotes an unbuilt input, not a built-empty set.
func compareRebuildContractIDs(ctx context.Context, expected, actual iter.Seq2[string, error]) error {
	if expected == nil || actual == nil {
		return fmt.Errorf("%w: missing id stream", errRebuildContractIDs)
	}
	nextExpected, stopExpected := iter.Pull2(expected)
	defer stopExpected()
	nextActual, stopActual := iter.Pull2(actual)
	defer stopActual()
	var previous string
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		want, err, haveExpected := nextExpected()
		if err != nil {
			return fmt.Errorf("%w: read expected ids: %w", errRebuildContractIDs, err)
		}
		got, err, haveActual := nextActual()
		if err != nil {
			return fmt.Errorf("%w: read actual ids: %w", errRebuildContractIDs, err)
		}
		if !haveExpected && !haveActual {
			return ctx.Err()
		}
		if haveExpected && want <= previous {
			return fmt.Errorf("%w: expected id %q after %q is empty, repeated or out of order", errRebuildContractIDs, want, previous)
		}
		switch {
		case !haveActual || haveExpected && want < got:
			return fmt.Errorf("%w: missing id %q", errRebuildContractIDs, want)
		case !haveExpected || got < want:
			return fmt.Errorf("%w: extra id %q", errRebuildContractIDs, got)
		}
		previous = want
	}
}
