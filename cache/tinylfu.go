package cache

import (
	"math/bits"
	"sync"
	"sync/atomic"

	"github.com/cespare/xxhash/v2"
)

// ---------- Count-Min Sketch ----------

// countMinSketch is a probabilistic frequency counter using 4 rows of uint8 counters.
// Each Increment/Estimate hashes the key 4 times (one per row) and
// uses the minimum counter value as the frequency estimate.
type countMinSketch struct {
	rows  [4][]uint8
	width uint64
	mask  uint64 // width-1 (width is a power of two)
}

func newCountMinSketch(maxItems int) *countMinSketch {
	// Width = next power-of-two >= maxItems*10, minimum 4096. A large floor
	// keeps false-positive collisions rare even for very small caches in
	// tests and avoids pathological admission outcomes.
	w := nextPow2(uint64(maxItems) * 10)
	if w < 4096 {
		w = 4096
	}
	s := &countMinSketch{
		width: w,
		mask:  w - 1,
	}
	for i := range s.rows {
		s.rows[i] = make([]uint8, w)
	}
	return s
}

// cmsHashes returns 4 independent indices for the given key using double
// hashing. h1 is the primary hash, h2 is a secondary (rotated) hash; row i
// uses (h1 + i*h2). This ensures two keys that collide in one row are
// unlikely to collide in all four rows.
func (s *countMinSketch) cmsHashes(key string) [4]uint64 {
	h := xxhash.Sum64String(key)
	h1 := h
	h2 := (h >> 32) | (h << 32)
	if h2 == 0 {
		h2 = 0x9E3779B97F4A7C15 // golden ratio fallback
	}
	var out [4]uint64
	for i := 0; i < 4; i++ {
		out[i] = (h1 + uint64(i)*h2) & s.mask
	}
	return out
}

func (s *countMinSketch) increment(key string) {
	idx := s.cmsHashes(key)
	for i := range s.rows {
		if s.rows[i][idx[i]] < 255 {
			s.rows[i][idx[i]]++
		}
	}
}

func (s *countMinSketch) estimate(key string) uint8 {
	idx := s.cmsHashes(key)
	var min uint8 = 255
	for i := range s.rows {
		if s.rows[i][idx[i]] < min {
			min = s.rows[i][idx[i]]
		}
	}
	return min
}

// halve divides every counter by 2 (right-shift), used for periodic decay.
func (s *countMinSketch) halve() {
	for i := range s.rows {
		for j := range s.rows[i] {
			s.rows[i][j] >>= 1
		}
	}
}

func (s *countMinSketch) reset() {
	for i := range s.rows {
		for j := range s.rows[i] {
			s.rows[i][j] = 0
		}
	}
}

// ---------- Doorkeeper (Bloom filter) ----------

// doorkeeper is a simple Bloom filter that acts as a first-level frequency
// gate: an item must appear at least twice (once to set the bloom, once
// to pass) before it increments the sketch.
type doorkeeper struct {
	bits []uint64
	n    uint64 // number of uint64 words
	mask uint64 // n*64 - 1 (total bits, power of two)
}

func newDoorkeeper(maxItems int) *doorkeeper {
	// ~8 bits per item, rounded up to power-of-two total bits.
	totalBits := nextPow2(uint64(maxItems) * 8)
	if totalBits < 64 {
		totalBits = 64
	}
	nWords := totalBits / 64
	return &doorkeeper{
		bits: make([]uint64, nWords),
		n:    nWords,
		mask: totalBits - 1,
	}
}

// add sets the bloom bits for key, returning true if the key was already present.
func (d *doorkeeper) add(key string) bool {
	h := xxhash.Sum64String(key)
	h1 := h & d.mask
	h2 := (h>>32 | h<<32) & d.mask

	w1, b1 := h1/64, h1%64
	w2, b2 := h2/64, h2%64

	present := (d.bits[w1]>>b1)&1 == 1 && (d.bits[w2]>>b2)&1 == 1

	d.bits[w1] |= 1 << b1
	d.bits[w2] |= 1 << b2
	return present
}

// contains checks membership without mutating.
func (d *doorkeeper) contains(key string) bool {
	h := xxhash.Sum64String(key)
	h1 := h & d.mask
	h2 := (h>>32 | h<<32) & d.mask

	w1, b1 := h1/64, h1%64
	w2, b2 := h2/64, h2%64

	return (d.bits[w1]>>b1)&1 == 1 && (d.bits[w2]>>b2)&1 == 1
}

func (d *doorkeeper) reset() {
	for i := range d.bits {
		d.bits[i] = 0
	}
}

// ---------- TinyLFU ----------

// TinyLFU is a frequency-based admission policy. Before inserting a new item
// it compares the newcomer's frequency with the would-be victim's frequency,
// admitting the newcomer only if it appears more popular.
type TinyLFU struct {
	mu       sync.Mutex
	sketch   *countMinSketch
	door     *doorkeeper
	counter  atomic.Int64
	maxItems int64
}

func newTinyLFU(maxItems int) *TinyLFU {
	// Sample window: classic TinyLFU uses W ≈ 10×cache size. We also enforce
	// a floor so small caches don't decay before frequencies accumulate.
	sampleWindow := int64(maxItems) * 10
	if sampleWindow < 1000 {
		sampleWindow = 1000
	}
	return &TinyLFU{
		sketch:   newCountMinSketch(maxItems),
		door:     newDoorkeeper(maxItems),
		maxItems: sampleWindow,
	}
}

// Increment records an access for the given key. This should be called on
// every cache Get (hit or miss) to build up frequency data.
func (t *TinyLFU) Increment(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	// Doorkeeper: the first time a key is seen it only sets the bloom bits.
	// Only on the second (and subsequent) sighting do we increment the sketch.
	if t.door.add(key) {
		t.sketch.increment(key)
	}

	// Periodic decay: after maxItems increments, halve everything.
	c := t.counter.Add(1)
	if c >= t.maxItems {
		t.counter.Store(0)
		t.sketch.halve()
		t.door.reset()
	}
}

// Admit returns true if newKey should be admitted, comparing its estimated
// frequency against victimKey's estimated frequency. Newcomers win on ties,
// so an empty sketch (cold start) still admits new items (preserving LRU
// behavior). Only when the victim has been observed strictly more than the
// newcomer is the newcomer rejected — this is what prevents scan pollution.
func (t *TinyLFU) Admit(newKey, victimKey string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	newFreq := t.sketch.estimate(newKey)
	victimFreq := t.sketch.estimate(victimKey)
	return newFreq >= victimFreq
}

// Estimate returns the estimated frequency for key (mainly for testing).
func (t *TinyLFU) Estimate(key string) uint8 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sketch.estimate(key)
}

// ---------- helpers ----------

func nextPow2(v uint64) uint64 {
	if v == 0 {
		return 1
	}
	return 1 << bits.Len64(v-1)
}
