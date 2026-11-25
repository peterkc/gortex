package store_pebble

import (
	"fmt"
	"strings"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/graph"
)

// The frontier exclusively owns this cache and
// must reset it after each precommit page. Cached full nodes are read-only.
// It is separate from the candidate reader: these nodes come directly from
// the working row and are not candidate-canonicalized.
const (
	rebuildCallerCacheBytes   = 16 << 20
	rebuildCallerCacheEntries = 896 // one Swiss-map table, also bounded by charge
)

type rebuildCallerCacheKey struct {
	id    string
	light bool
}

type rebuildCallerCacheEntry struct {
	node        *graph.Node
	rawBytes    int
	fingerprint [32]byte
}

type rebuildCallerPageCache struct {
	entries  map[rebuildCallerCacheKey]rebuildCallerCacheEntry
	retained int64
}

// WithCallerPageCache gives the serial precommit frontier a page-local raw
// node cache. It must reset this view at every page boundary; ordinary working
// views remain uncached, and live candidates never read from this cache.
func (v RebuildView) WithCallerPageCache() RebuildView {
	v.callerPage = &rebuildCallerPageCache{}
	return v
}

func (v RebuildView) ResetCallerPage() error {
	c := v.callerPage
	if c == nil {
		return nil
	}
	entries := c.entries
	c.entries, c.retained = nil, 0
	for key, entry := range entries {
		if entry.node == nil {
			continue
		}
		digest, err := v3Fingerprint(entry.node)
		if err != nil || digest != entry.fingerprint {
			return fmt.Errorf("%w: cached raw caller %q mutated", ErrRebuildView, key.id)
		}
	}
	return nil
}

// read is invoked inside the ordinary snapshot read (including the outer
// cancellation and mutation-error checks). For the light import path the
// logical value charge is debited before decoding or returning a hit, just
// as it is on the uncached path. A cached light read never satisfies a full
// read: light decoding deliberately does not parse metadata.
func (c *rebuildCallerPageCache) read(snap *pebble.Snapshot, id string, key []byte, light bool, chargeValue func(int) error) (*graph.Node, error) {
	cacheKey := rebuildCallerCacheKey{id: id, light: light}
	entry, hit := c.entries[cacheKey]
	if !hit && light {
		entry, hit = c.entries[rebuildCallerCacheKey{id: id}]
	}
	applyCharge := func(n int) error {
		if light {
			return chargeValue(n)
		}
		return nil
	}
	if hit {
		if err := applyCharge(entry.rawBytes); err != nil {
			return nil, err
		}
		if light && entry.node != nil && entry.node.Meta != nil {
			// A successful full decode also proves the light framing; the
			// import scan uses only the ID and file path, never metadata.
			node := *entry.node
			node.Meta = nil
			return &node, nil
		}
		return entry.node, nil
	}
	node, err := pendingGet(snap, key, func(raw []byte) (*graph.Node, error) {
		entry.rawBytes = len(raw)
		if err := applyCharge(len(raw)); err != nil {
			return nil, err
		}
		if light {
			return decodeNodeLight(raw)
		}
		return decodeNode(raw)
	})
	if err != nil {
		return nil, err
	}
	// The original caller still checks key identity and import ownership on
	// every path. Do not admit a malformed identity into the cache.
	if node != nil && node.ID != id {
		return node, nil
	}
	size := int64(len(id)) + 256 // conservative per-entry bookkeeping estimate
	if c.entries == nil {
		size += 2048 // first map allocation and growth slack
	}
	if len(c.entries) >= rebuildCallerCacheEntries || size > rebuildCallerCacheBytes-c.retained {
		return node, nil
	}
	charge := v3NodeCharge(node)
	if charge > rebuildCallerCacheBytes-c.retained-size {
		// Also bypass values for which the charge walk cannot prove a bound.
		return node, nil
	}
	size += charge
	if node != nil {
		digest, err := v3Fingerprint(node)
		if err != nil {
			// An uncheckable value still takes the ordinary raw read path.
			return node, nil
		}
		entry.fingerprint = digest
	}
	if c.entries == nil {
		c.entries = make(map[rebuildCallerCacheKey]rebuildCallerCacheEntry)
	}
	entry.node = node
	cacheKey.id = strings.Clone(cacheKey.id)
	c.entries[cacheKey] = entry
	c.retained += size
	return node, nil
}
