package cache

import (
	"context"
	"hash/fnv"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

// Predictor tracks URLs that have been observed to be uncacheable.
//
// It is a counting Bloom filter with a two-bucket time-based decay scheme.
// One bucket is "active" (receives all new writes); the other is the
// "shadow" bucket (still readable, but no new writes).
//
// Reads consider both buckets (taking the MAX of each counter pair), so a
// URL that was marked uncacheable in the shadow bucket stays classified
// until the next rotation clears it. This produces a sliding window: an
// entry lives for between one and two decay intervals depending on where
// in the cycle it was first marked.
//
// Every decayInterval the buckets are swapped and the newly-inactive bucket
// is zeroed. URLs that become cacheable again are eventually forgotten.
//
// False positives are acceptable: worst case we skip a cache lookup for a
// URL that has since become cacheable and just do an origin fetch.
// False negatives are also fine: worst case we do the normal cache lookup
// path we would have done anyway.
type Predictor struct {
	// Two-bucket counting filter. Each bucket is an array of uint32 counters
	// (we use uint32 rather than byte so that concurrent Mark/Check can use
	// atomic operations race-free without resorting to a full mutex on the
	// hot path).
	buckets   [2][]uint32
	bits      uint32        // number of counters per bucket (power of two)
	mask      uint32        // bits - 1
	active    atomic.Uint32 // 0 or 1 — which bucket is currently active
	size      uint32        // reported capacity
	decay     time.Duration
	threshold uint32        // required confidence count (default 2)

	// mu serializes bucket rotation against reads and writes. Reads and
	// writes take RLock; Rotate takes Lock. Counter mutations within RLock
	// are race-free because counters are touched via sync/atomic.
	mu     sync.RWMutex
	hits   atomic.Uint64
	misses atomic.Uint64
}

// NewPredictor creates a Predictor sized for approximately `capacity` distinct
// keys and a decay window of `decay`. Call StartDecay to begin automatic
// rotation.
//
// The internal bit array is sized to ~8x capacity rounded up to a power of
// two, which keeps the false-positive rate low while staying very cheap.
func NewPredictor(capacity int, decay time.Duration) *Predictor {
	if capacity <= 0 {
		capacity = 1024
	}
	// ~8 counters per tracked key; round up to a power of two.
	target := uint32(capacity) * 8
	bits := uint32(1)
	for bits < target {
		bits <<= 1
	}
	if bits < 1024 {
		bits = 1024
	}
	p := &Predictor{
		bits:      bits,
		mask:      bits - 1,
		size:      uint32(capacity),
		decay:     decay,
		threshold: 2,
	}
	p.buckets[0] = make([]uint32, bits)
	p.buckets[1] = make([]uint32, bits)
	return p
}

// hashKey returns two independent hashes derived from a single FNV-64a pass.
// We split the 64-bit hash into high/low halves and mask to bit width. Two
// hashes at fixed offsets give enough spread for a simple Bloom filter.
func (p *Predictor) hashKey(key string) (uint32, uint32) {
	h := fnv.New64a()
	// hash/fnv implements io.StringWriter, so we can feed the key without
	// the []byte(string) allocation on the hot path.
	if sw, ok := h.(io.StringWriter); ok {
		_, _ = sw.WriteString(key)
	} else {
		_, _ = h.Write([]byte(key))
	}
	sum := h.Sum64()
	h1 := uint32(sum) & p.mask
	h2 := uint32(sum>>32) & p.mask
	// Avoid the two positions collapsing on small masks.
	if h1 == h2 {
		h2 = (h2 + 1) & p.mask
	}
	return h1, h2
}

// counterMax caps counters so heavily-hit URLs do not accumulate unbounded
// values (which would make them effectively permanent). 255 is the saturating
// limit inherited from the original byte-based design.
const counterMax uint32 = 255

// MarkUncacheable records that the given key was observed to be uncacheable.
// Counters saturate at counterMax to prevent unbounded growth.
func (p *Predictor) MarkUncacheable(key string) {
	h1, h2 := p.hashKey(key)

	p.mu.RLock()
	defer p.mu.RUnlock()

	bucket := p.buckets[p.active.Load()]

	// Atomic saturating increment. We use CAS in a tight loop instead of a
	// blind AddUint32 so the counter can cap at counterMax without drifting.
	for {
		old := atomic.LoadUint32(&bucket[h1])
		if old >= counterMax {
			break
		}
		if atomic.CompareAndSwapUint32(&bucket[h1], old, old+1) {
			break
		}
	}
	for {
		old := atomic.LoadUint32(&bucket[h2])
		if old >= counterMax {
			break
		}
		if atomic.CompareAndSwapUint32(&bucket[h2], old, old+1) {
			break
		}
	}
}

// IsLikelyUncacheable reports whether the key has been observed to be
// uncacheable recently, based on the configured threshold. It considers
// both buckets (taking the max of each counter pair) so recently-marked
// entries survive the next rotation for one extra interval.
func (p *Predictor) IsLikelyUncacheable(key string) bool {
	h1, h2 := p.hashKey(key)

	p.mu.RLock()
	idx := p.active.Load()
	active := p.buckets[idx]
	shadow := p.buckets[idx^1]
	c1a := atomic.LoadUint32(&active[h1])
	c1b := atomic.LoadUint32(&shadow[h1])
	c2a := atomic.LoadUint32(&active[h2])
	c2b := atomic.LoadUint32(&shadow[h2])
	p.mu.RUnlock()

	c1 := c1a
	if c1b > c1 {
		c1 = c1b
	}
	c2 := c2a
	if c2b > c2 {
		c2 = c2b
	}

	if c1 >= p.threshold && c2 >= p.threshold {
		p.hits.Add(1)
		return true
	}
	p.misses.Add(1)
	return false
}

// Rotate swaps the active/shadow buckets and clears the new active bucket
// so it starts fresh. The old active bucket becomes the shadow and is still
// read for one more cycle before being cleared at the next rotation.
//
// Exposed primarily for tests; in production use StartDecay.
func (p *Predictor) Rotate() {
	p.mu.Lock()
	defer p.mu.Unlock()

	cur := p.active.Load()
	next := cur ^ 1
	// Clear the bucket we are about to promote to active. The old active
	// (cur) becomes the shadow and keeps its contents for one more cycle.
	// We hold the write lock, so no readers or writers can observe the
	// interim state — plain assignment is fine.
	nextBucket := p.buckets[next]
	for i := range nextBucket {
		nextBucket[i] = 0
	}
	p.active.Store(next)
}

// StartDecay begins periodic bucket rotation every decay interval.
// Blocks until ctx is cancelled; call in its own goroutine.
func (p *Predictor) StartDecay(ctx context.Context) {
	if p.decay <= 0 {
		return
	}
	ticker := time.NewTicker(p.decay)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.Rotate()
		}
	}
}

// Stats returns cumulative hit and miss counters for observability.
// "hit" = IsLikelyUncacheable returned true (we skipped the cache path).
// "miss" = IsLikelyUncacheable returned false (we did the normal lookup).
func (p *Predictor) Stats() (hits, misses uint64) {
	return p.hits.Load(), p.misses.Load()
}
