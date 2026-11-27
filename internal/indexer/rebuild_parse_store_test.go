package indexer

import (
	"os"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_pebble"
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

func TestRebuildParseStoreContractReceiptBinding(t *testing.T) {
	store := rebuildParseStore{dir: t.TempDir(), limit: 1 << 20}
	const file = "src\xff.go"
	input := rebuildInput{Path: file, Language: "go", Content: rebuildDigest{1}}
	v2Key := input.parseKey(rebuildDigest{2})
	v3Key := input.parseKey(rebuildDigest{3})
	require.NotEqual(t, v2Key, v3Key)
	base := store_pebble.ParseFacts{File: graph.FileMetaRow{FilePath: file}, EffectiveLanguage: "go"}
	v2, err := store_pebble.EncodeParseFacts(base)
	require.NoError(t, err)
	require.NoError(t, store.write(v2Key, v2))
	_, err = store.read(v3Key)
	require.ErrorIs(t, err, os.ErrNotExist)
	facts := &contracts.FileFacts{Shapes: map[string]*contracts.Shape{}, Handlers: map[string]contracts.HandlerFacts{}}
	v3, err := store_pebble.EncodeContractParseFacts(base, facts)
	require.NoError(t, err)
	require.NoError(t, store.write(v3Key, v3))

	// Reopen the same private store; the original receipt is untouched.
	reader := rebuildParseStore{dir: store.dir, limit: store.limit}
	old, err := reader.read(v2Key)
	require.NoError(t, err)
	require.Equal(t, v2, old)
	candidate, err := reader.read(v3Key)
	require.NoError(t, err)
	decoded, got, _, err := store_pebble.DecodeContractParseFacts(candidate)
	require.NoError(t, err)
	require.Equal(t, base, decoded)
	require.Equal(t, facts, got)

	wrongKey := input.parseKey(rebuildDigest{4})
	wire, err := os.ReadFile(reader.entry(v3Key))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(reader.entry(wrongKey), wire, 0o600))
	_, err = reader.read(wrongKey)
	require.ErrorIs(t, err, errParseEntryCorrupt)
}
