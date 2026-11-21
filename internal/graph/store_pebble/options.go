package store_pebble

import (
	"github.com/cockroachdb/pebble/v2"
	"github.com/cockroachdb/pebble/v2/bloom"
	"github.com/cockroachdb/pebble/v2/sstable"
)

// Pebble creates and owns one cache per DB when only CacheSize is set.
const buildBlockCacheBytes = 32 << 20

func headOptions() *pebble.Options {
	options := &pebble.Options{CacheSize: buildBlockCacheBytes, MemTableSize: 4 << 20}
	options.Levels[0].Compression = func() *sstable.CompressionProfile { return sstable.SnappyCompression }
	for i := range options.Levels[:6] {
		options.Levels[i].FilterPolicy = bloom.FilterPolicy(10)
	}
	// A nil policy inherits L5's filter, rather than disabling it.
	options.Levels[6].FilterPolicy = pebble.NoFilterPolicy
	options.EnsureDefaults()
	return options
}
