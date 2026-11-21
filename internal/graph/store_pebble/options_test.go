package store_pebble

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/cockroachdb/pebble/v2"
	"github.com/cockroachdb/pebble/v2/bloom"
	"github.com/cockroachdb/pebble/v2/sstable"
)

func readSavedOptions(t *testing.T, dir string) pebble.Options {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "OPTIONS-*"))
	if err != nil || len(files) != 1 {
		t.Fatalf("want one saved options file, got %v: %v", files, err)
	}
	raw, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	bloomPolicy := bloom.FilterPolicy(10)
	var options pebble.Options
	err = options.Parse(string(raw), &pebble.ParseHooks{NewFilterPolicy: func(name string) (pebble.FilterPolicy, error) {
		switch name {
		case bloomPolicy.Name():
			return bloomPolicy, nil
		case pebble.NoFilterPolicy.Name():
			return pebble.NoFilterPolicy, nil
		default:
			return nil, fmt.Errorf("unexpected filter policy %q", name)
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	return options
}

func TestSavedHeadOptions(t *testing.T) {
	dir := t.TempDir()
	db, err := pebble.Open(dir, headOptions())
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	options := readSavedOptions(t, dir)
	bloomPolicy := bloom.FilterPolicy(10)
	if options.DisableWAL || options.MemTableSize != 4<<20 {
		t.Fatalf("saved head options: disable WAL=%t, memtable=%d", options.DisableWAL, options.MemTableSize)
	}
	for i, level := range options.Levels {
		expected := bloomPolicy.Name()
		if i == 6 {
			expected = pebble.NoFilterPolicy.Name()
		}
		if level.FilterPolicy == nil || level.FilterPolicy.Name() != expected {
			t.Errorf("L%d filter policy = %v, want %s", i, level.FilterPolicy, expected)
		}
		if level.Compression == nil || level.Compression() != sstable.SnappyCompression {
			t.Errorf("L%d did not save Snappy", i)
		}
	}
}

func TestBuildBlockCaches(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "head")
	b, err := OpenBuild(dir, FullProfile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if b != nil {
			if err := b.Close(); err != nil {
				t.Error(err)
			}
		}
	})

	pendingDir := filepath.Join(dir, "pending")
	for _, tc := range []struct {
		name string
		dir  string
		db   *pebble.DB
	}{
		{"head", dir, b.db},
		{"pending", pendingDir, b.pending},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Pebble saves the actual cache's maximum size, not the
			// requested size, in OPTIONS.
			if got := readSavedOptions(t, tc.dir).CacheSize; got != 32<<20 {
				t.Fatalf("opened block cache capacity = %d, want 32 MiB", got)
			}
			key := []byte("test-key")
			if err := tc.db.Set(key, []byte("test-value"), pebble.NoSync); err != nil {
				t.Fatal(err)
			}
			if err := tc.db.Flush(); err != nil {
				t.Fatal(err)
			}
			before := tc.db.Metrics().BlockCache.Hits
			for range 2 {
				_, closer, err := tc.db.Get(key)
				if err != nil {
					t.Fatal(err)
				}
				if err := closer.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if after := tc.db.Metrics().BlockCache.Hits; after <= before {
				t.Fatalf("SST reads did not hit the block cache: before=%d after=%d", before, after)
			}
		})
	}

	// With CacheSize and no supplied Cache, these DB handles hold the only
	// lifetime references. Closing them releases Pebble's caches.
	err = b.Close()
	b = nil
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(pendingDir); !os.IsNotExist(err) {
		t.Fatalf("pending directory still present or not accessible: %v", err)
	}
	opts := headOptions()
	opts.ReadOnly = true
	reopened, err := pebble.Open(dir, opts)
	if err != nil {
		t.Fatalf("head did not release its lock: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
}
