package indexer

import (
	"os"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRebuildParseStoreCreateAndReuse(t *testing.T) {
	s := rebuildParseStore{dir: t.TempDir(), limit: 64}
	key := rebuildDigest{1}
	record := []byte{'f', 1, 0, 2}
	data, err := s.read(key)
	require.ErrorIs(t, err, os.ErrNotExist)
	require.Nil(t, data)

	start := make(chan struct{})
	results := make(chan error, 2)
	var writers sync.WaitGroup
	for range cap(results) {
		writers.Go(func() {
			<-start
			results <- s.write(key, record)
		})
	}
	close(start)
	writers.Wait()
	close(results)
	for err := range results {
		require.NoError(t, err)
	}
	require.ErrorIs(t, s.write(key, []byte("different")), errParseEntryConflict)
	data, err = s.read(key)
	require.NoError(t, err)
	require.Equal(t, record, data)
	dir, err := os.ReadDir(s.dir)
	require.NoError(t, err)
	require.Len(t, dir, 1, "only one immutable entry, with no temporary files")
}

func TestRebuildParseStoreRefusesBadEntry(t *testing.T) {
	s := rebuildParseStore{dir: t.TempDir(), limit: 8}
	key, other := rebuildDigest{1}, rebuildDigest{2}
	payload := []byte("fact")
	require.NoError(t, s.write(key, payload))
	small := rebuildParseStore{dir: s.dir, limit: 3}
	got, err := small.read(key)
	require.ErrorIs(t, err, errParseEntryTooLarge)
	require.Nil(t, got)
	require.ErrorIs(t, small.write(other, payload), errParseEntryTooLarge)
	_, err = small.read(other)
	require.ErrorIs(t, err, os.ErrNotExist)

	saved, err := os.ReadFile(s.entry(key))
	require.NoError(t, err)
	require.Len(t, saved, parseEntryHeader+len(payload))
	require.NoError(t, os.WriteFile(s.entry(other), saved, 0o600))
	got, err = s.read(other)
	require.ErrorIs(t, err, errParseEntryCorrupt, "a valid payload under a different key is corrupt")
	require.Nil(t, got)

	saved[len(saved)-1] ^= 1
	require.NoError(t, os.WriteFile(s.entry(key), saved, 0o600))
	got, err = s.read(key)
	require.ErrorIs(t, err, errParseEntryCorrupt)
	require.Nil(t, got)
	require.ErrorIs(t, s.write(key, payload), errParseEntryCorrupt, "never overwrite corrupt evidence")
	got, err = os.ReadFile(s.entry(key))
	require.NoError(t, err)
	require.Equal(t, saved, got)
	dir, err := os.ReadDir(s.dir)
	require.NoError(t, err)
	require.Len(t, dir, 2)
}
