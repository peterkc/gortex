package store_pebble

import (
	"context"
	"errors"

	"github.com/cockroachdb/pebble/v2"
)

var guardUsingsPrefix = []byte{'g', 'u'}

// RebuildGuardUsings holds the cold guard's C# visibility evidence on disk.
// Project directories and unit-level directives are not part of the head.
// The build coordinator uses one handle, serially, and must close it.
type RebuildGuardUsings struct{ db *pebble.DB }

func (b *BuildStore) OpenRebuildGuardUsings(ctx context.Context) (*RebuildGuardUsings, error) {
	if err := b.RebuildView().mutationError(); err != nil {
		return nil, err
	}
	occupied, err := rebuildGuardHasPrefix(ctx, b.pending, guardUsingsPrefix)
	if err != nil {
		return nil, err
	}
	if occupied {
		return nil, invalid("guard usings scratch range occupied")
	}
	return &RebuildGuardUsings{db: b.pending}, nil
}

func (u *RebuildGuardUsings) RecordProject(dir string) error {
	return u.db.Set(keyStrings([]byte{'g', 'u', 'p'}, dir), nil, pebble.NoSync)
}

func (u *RebuildGuardUsings) HasProject(dir string) (bool, error) {
	return pendingGet(u.db, keyStrings([]byte{'g', 'u', 'p'}, dir), func([]byte) (bool, error) { return true, nil })
}

func (u *RebuildGuardUsings) RecordGlobal(dir, target string, static bool) error {
	return u.db.Set(keyStrings(guardGlobalPrefix(dir, static), target), nil, pebble.NoSync)
}

func (u *RebuildGuardUsings) VisitGlobals(ctx context.Context, dir string, static bool, visit func(string) error) error {
	prefix := guardGlobalPrefix(dir, static)
	return pendingVisit(ctx, u.db, prefix, func(r row) error {
		name, rest, err := readKeyString(r.key[len(prefix):])
		if err != nil || len(rest) != 0 {
			return errors.Join(err, invalid("guard global using key"))
		}
		return visit(name)
	})
}

func guardGlobalPrefix(dir string, static bool) []byte {
	kind := byte('n')
	if static {
		kind = 's'
	}
	return keyStrings([]byte{'g', 'u', kind}, dir)
}

func (u *RebuildGuardUsings) Close() error {
	return u.db.DeleteRange(guardUsingsPrefix, prefixEnd(guardUsingsPrefix), pebble.NoSync)
}
