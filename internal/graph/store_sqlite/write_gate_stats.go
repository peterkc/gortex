package store_sqlite

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"math/bits"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

const writeGateStatsInterval = 30 * time.Second

// Buckets double from one microsecond; the last covers time.Duration's range.
const writeGateHistogramBuckets = 55

type writeGateHistogram struct {
	buckets [writeGateHistogramBuckets]atomic.Uint64
	max     atomic.Int64
}

func (h *writeGateHistogram) record(d time.Duration) {
	bucket := 0
	if d > time.Microsecond {
		bucket = bits.Len64(uint64((d - 1) / time.Microsecond))
	}
	h.buckets[bucket].Add(1)
	for previous := h.max.Load(); int64(d) > previous; previous = h.max.Load() {
		if h.max.CompareAndSwap(previous, int64(d)) {
			break
		}
	}
}

type writeGateTiming struct {
	Samples    uint64 `json:"samples"`
	P50UpperNS int64  `json:"p50_upper_ns"`
	P95UpperNS int64  `json:"p95_upper_ns"`
	MaxNS      int64  `json:"max_ns"`
}

func (h *writeGateHistogram) snapshot() writeGateTiming {
	var buckets [writeGateHistogramBuckets]uint64
	var count uint64
	for i := range buckets {
		buckets[i] = h.buckets[i].Load()
		count += buckets[i]
	}
	maximum := h.max.Load()
	return writeGateTiming{
		Samples: count, MaxNS: maximum,
		P50UpperNS: writeGatePercentile(buckets, (count+1)/2, maximum),
		P95UpperNS: writeGatePercentile(buckets, count-count/20, maximum),
	}
}

// A bucket's upper bound is capped at the observed maximum, which is a
// tighter upper bound and keeps p50 <= p95 <= max even for a single sample.
func writeGatePercentile(buckets [writeGateHistogramBuckets]uint64, rank uint64, maximum int64) int64 {
	if rank == 0 {
		return 0
	}
	var count uint64
	for i, n := range buckets {
		count += n
		if count >= rank {
			upper := int64(math.MaxInt64)
			if i < writeGateHistogramBuckets-1 {
				upper = int64(time.Microsecond) << i
			}
			return min(upper, maximum)
		}
	}
	return maximum
}

type writeGateWaitPair struct {
	waits  atomic.Uint64
	waitNS atomic.Int64
}

type writeGateSiteStats struct {
	name            string
	acquisitions    atomic.Uint64
	tryLockFailures atomic.Uint64
	wait            writeGateHistogram
	hold            writeGateHistogram
	pairs           sync.Map // *writeGateSiteStats (nil means unknown) -> *writeGateWaitPair
}

func (s *writeGateSiteStats) recordWait(d time.Duration, holder *writeGateSiteStats, waiting bool) {
	s.wait.record(d)
	if waiting {
		pair, _ := s.pairs.Load(holder)
		p := pair.(*writeGateWaitPair)
		p.waits.Add(1)
		p.waitNS.Add(int64(d))
	}
}

type writeGateStats struct {
	sites      sync.Map // program counter -> *writeGateSiteStats; nil skips a gate frame
	registerMu sync.Mutex
	used       atomic.Bool
}

func (s *writeGateStats) register(pc uintptr, function, file string, line int) *writeGateSiteStats {
	s.registerMu.Lock()
	defer s.registerMu.Unlock()
	if cached, ok := s.sites.Load(pc); ok {
		return cached.(*writeGateSiteStats)
	}
	site := &writeGateSiteStats{name: fmt.Sprintf("%s (%s:%d)", function, filepath.Base(file), line)}
	site.pairs.Store((*writeGateSiteStats)(nil), &writeGateWaitPair{})
	site.pairs.Store(site, &writeGateWaitPair{})
	// Publish every pair before the new site can become a holder. Only first
	// registration allocates; acquisitions never allocate, even for a new pair.
	s.sites.Range(func(_, value any) bool {
		if other := value.(*writeGateSiteStats); other != nil {
			site.pairs.Store(other, &writeGateWaitPair{})
			other.pairs.Store(site, &writeGateWaitPair{})
		}
		return true
	})
	s.sites.Store(pc, site)
	return site
}

type writeGateSiteSnapshot struct {
	Site            string          `json:"site"`
	Acquisitions    uint64          `json:"acquisitions"`
	TryLockFailures uint64          `json:"try_lock_failures"`
	Wait            writeGateTiming `json:"wait"`
	Hold            writeGateTiming `json:"hold"`
}

type writeGatePairSnapshot struct {
	Waiter    string `json:"waiter"`
	Holder    string `json:"holder_seen_at_wait_start"`
	Waits     uint64 `json:"waits"`
	WaitSumNS int64  `json:"wait_sum_ns"`
}

type writeGateSnapshot struct {
	Reason string                  `json:"reason"`
	Sites  []writeGateSiteSnapshot `json:"sites"`
	Pairs  []writeGatePairSnapshot `json:"waiter_holder_pairs"`
}

func (s *writeGateStats) snapshot(reason string) writeGateSnapshot {
	result := writeGateSnapshot{Reason: reason, Sites: []writeGateSiteSnapshot{}, Pairs: []writeGatePairSnapshot{}}
	s.sites.Range(func(_, value any) bool {
		site := value.(*writeGateSiteStats)
		if site == nil {
			return true
		}
		result.Sites = append(result.Sites, writeGateSiteSnapshot{
			Site: site.name, Acquisitions: site.acquisitions.Load(),
			TryLockFailures: site.tryLockFailures.Load(), Wait: site.wait.snapshot(), Hold: site.hold.snapshot(),
		})
		site.pairs.Range(func(key, value any) bool {
			pair := value.(*writeGateWaitPair)
			if n := pair.waits.Load(); n != 0 {
				holder := "unknown"
				if observed := key.(*writeGateSiteStats); observed != nil {
					holder = observed.name
				}
				result.Pairs = append(result.Pairs, writeGatePairSnapshot{
					Waiter: site.name, Holder: holder, Waits: n, WaitSumNS: pair.waitNS.Load(),
				})
			}
			return true
		})
		return true
	})
	sort.Slice(result.Sites, func(i, j int) bool { return result.Sites[i].Site < result.Sites[j].Site })
	sort.Slice(result.Pairs, func(i, j int) bool {
		if result.Pairs[i].Waiter != result.Pairs[j].Waiter {
			return result.Pairs[i].Waiter < result.Pairs[j].Waiter
		}
		return result.Pairs[i].Holder < result.Pairs[j].Holder
	})
	return result
}

func (g *sqliteWriteGate) logStats(reason string) {
	used := g.stats.used.Swap(false)
	if !used && reason != "close" {
		return
	}
	data, err := json.Marshal(g.stats.snapshot(reason))
	if err != nil {
		log.Printf("store_sqlite: write gate stats encoding failed: %v", err)
		return
	}
	log.Printf("store_sqlite write_gate_stats %s", data)
}
